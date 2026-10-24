package web

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/marcelhoffs/anchor/internal/auth"
	"github.com/marcelhoffs/anchor/internal/config"
	"github.com/marcelhoffs/anchor/internal/db"
	"github.com/marcelhoffs/anchor/internal/fetch"
	"github.com/marcelhoffs/anchor/internal/jobs"
	"github.com/marcelhoffs/anchor/internal/store"
)

type env struct {
	t       *testing.T
	srv     *httptest.Server
	store   *store.Store
	db      *db.DB
	server  *Server
	worker  *jobs.Worker
	fetcher *jobs.Fetcher
	origin  string // the configured base URL (WebAuthn origin)
}

func newEnv(t *testing.T) *env {
	t.Helper()
	return newEnvHost(t, "localhost")
}

// newEnvHost configures the base URL with host (the listener is always
// 127.0.0.1).
func newEnvHost(t *testing.T, host string) *env {
	t.Helper()
	return newEnvWith(t, host, nil)
}

// newEnvWith lets configure change the settings before the server starts.
func newEnvWith(t *testing.T, host string, configure func(*config.Config)) *env {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if err := d.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	st := store.New(d)
	ts := httptest.NewUnstartedServer(nil)
	// The base URL names localhost, not the listener's IP: a WebAuthn RP ID
	// must be a host name.
	_, port, _ := net.SplitHostPort(ts.Listener.Addr().String())
	base, _ := url.Parse("http://" + host + ":" + port)
	cfg := &config.Config{BaseURL: base, SecretKey: make([]byte, 32)}
	if configure != nil {
		configure(cfg)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	worker := jobs.NewWorker(st, log)
	// Tests fetch from httptest servers on 127.0.0.1.
	fetcher := jobs.NewFetcher(st, fetch.New(true), log)
	s, err := New(cfg, st, log, worker, fetcher)
	if err != nil {
		t.Fatal(err)
	}
	ts.Config.Handler = s.Handler()
	ts.Start()
	t.Cleanup(ts.Close)
	return &env{t: t, srv: ts, store: st, db: d, server: s, worker: worker, fetcher: fetcher, origin: base.String()}
}

type client struct {
	e    *env
	http *http.Client
}

func (e *env) client() *client {
	jar, _ := cookiejar.New(nil)
	return &client{e: e, http: &http.Client{
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// get returns the status, body and Location header.
func (c *client) get(path string) (int, string, string) {
	c.e.t.Helper()
	res, err := c.http.Get(c.e.srv.URL + path)
	if err != nil {
		c.e.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b), res.Header.Get("Location")
}

func (c *client) csrf() string {
	u, _ := url.Parse(c.e.srv.URL)
	for _, ck := range c.http.Jar.Cookies(u) {
		if ck.Name == csrfCookie {
			return ck.Value
		}
	}
	c.get("/login") // any GET sets the cookie
	return c.csrf()
}

// post submits a form with the client's CSRF token.
func (c *client) post(path string, form url.Values) (int, string, string) {
	c.e.t.Helper()
	if form == nil {
		form = url.Values{}
	}
	if !form.Has("csrf") {
		form.Set("csrf", c.csrf())
	}
	res, err := c.http.PostForm(c.e.srv.URL+path, form)
	if err != nil {
		c.e.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b), res.Header.Get("Location")
}

func (e *env) user(name string) {
	e.t.Helper()
	if _, err := e.store.CreateUser(context.Background(), name, auth.HashPassword(name+"-password"), false); err != nil {
		e.t.Fatal(err)
	}
}

// tags creates tags for a user, as on the Tags page.
func (e *env) tags(user string, names ...string) {
	e.t.Helper()
	for _, n := range names {
		if _, err := e.store.CreateTag(context.Background(), personal(e.store, e.userID(user)), n); err != nil {
			e.t.Fatal(err)
		}
	}
}

func (e *env) login(name string) *client {
	e.t.Helper()
	c := e.client()
	if code, body, _ := c.post("/login", url.Values{"username": {name}, "password": {name + "-password"}}); code != http.StatusSeeOther {
		e.t.Fatalf("login %s: %d %s", name, code, body)
	}
	return c
}

func TestSetupFlow(t *testing.T) {
	e := newEnv(t)
	c := e.client()
	if code, _, loc := c.get("/"); code != http.StatusSeeOther || loc != "/setup" {
		t.Fatalf("empty instance should redirect to /setup, got %d %q", code, loc)
	}
	if code, body, _ := c.post("/setup", url.Values{"username": {"root"}, "password": {"short"}, "confirm": {"short"}}); code != http.StatusUnprocessableEntity || !strings.Contains(body, "at least 10") {
		t.Fatalf("weak password accepted: %d", code)
	}
	if code, _, loc := c.post("/setup", url.Values{"username": {"root"}, "password": {"long enough pw"}, "confirm": {"long enough pw"}}); code != http.StatusSeeOther || loc != "/" {
		t.Fatalf("setup: %d %q", code, loc)
	}
	if code, body, _ := c.get("/"); code != http.StatusOK || !strings.Contains(body, `action="/logout"`) || !strings.Contains(body, "root") {
		t.Fatalf("setup should log the admin in: %d", code)
	}
	// Setup is closed once an account exists.
	other := e.client()
	if code, _, loc := other.post("/setup", url.Values{"username": {"evil"}, "password": {"long enough pw"}, "confirm": {"long enough pw"}}); code != http.StatusSeeOther || loc != "/login" {
		t.Fatalf("second setup: %d %q", code, loc)
	}
	if _, err := e.store.UserByUsername(context.Background(), "evil"); err != store.ErrNotFound {
		t.Fatalf("second setup created a user: %v", err)
	}
}

func TestLoginLogoutAndCSRF(t *testing.T) {
	e := newEnv(t)
	e.user("alice")
	c := e.client()
	if code, _, loc := c.get("/tags"); code != http.StatusSeeOther || loc != "/login?next=%2Ftags" {
		t.Fatalf("anonymous /tags: %d %q", code, loc)
	}
	if code, body, _ := c.post("/login", url.Values{"username": {"alice"}, "password": {"wrong"}}); code != http.StatusUnauthorized || !strings.Contains(body, invalidLogin.Format) {
		t.Fatalf("bad password: %d", code)
	}
	if code, _, _ := c.post("/login", url.Values{"username": {"nobody"}, "password": {"wrong"}}); code != http.StatusUnauthorized {
		t.Fatalf("unknown user: %d", code)
	}
	if code, _, _ := c.post("/login", url.Values{"username": {"alice"}, "password": {"alice-password"}, "csrf": {"forged"}}); code != http.StatusForbidden {
		t.Fatalf("forged CSRF token accepted: %d", code)
	}
	if code, _, loc := c.post("/login", url.Values{"username": {"alice"}, "password": {"alice-password"}, "next": {"//evil.example"}}); code != http.StatusSeeOther || loc != "/" {
		t.Fatalf("login with off-site next: %d %q", code, loc)
	}
	if code, _, _ := c.get("/tags"); code != http.StatusOK {
		t.Fatalf("logged-in /tags: %d", code)
	}
	if code, _, _ := c.post("/bookmarks", url.Values{"url": {"https://x.org"}, "csrf": {""}}); code != http.StatusForbidden {
		t.Fatalf("missing CSRF token accepted: %d", code)
	}
	c.post("/logout", nil)
	if code, _, _ := c.get("/tags"); code != http.StatusSeeOther {
		t.Fatalf("still logged in after logout: %d", code)
	}
}

func TestSecurityHeaders(t *testing.T) {
	e := newEnv(t)
	res, err := http.Get(e.srv.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if csp := res.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") || !strings.Contains(csp, "script-src 'self'") {
		t.Fatalf("CSP = %q", csp)
	}
}

func TestBookmarkFlow(t *testing.T) {
	e := newEnv(t)
	e.user("alice")
	c := e.login("alice")
	e.tags("alice", "go", "reading", "golang")

	if code, body, _ := c.post("/bookmarks", url.Values{"url": {"javascript:alert(1)"}}); code != http.StatusUnprocessableEntity || !strings.Contains(body, "valid http") {
		t.Fatalf("javascript: URL accepted: %d", code)
	}
	if code, _, loc := c.post("/bookmarks", url.Values{"url": {"https://go.dev/doc"}, "title": {"Go docs <script>"}, "tags": {"go, reading"}}); code != http.StatusSeeOther || loc != "/" {
		t.Fatalf("create: %d %q", code, loc)
	}
	_, body, _ := c.get("/")
	if !strings.Contains(body, "Go docs &lt;script&gt;") || !strings.Contains(body, ">reading</a>") {
		t.Fatalf("list missing bookmark or not escaped:\n%s", body)
	}
	_, body, _ = c.get("/?q=" + url.QueryEscape("#go docs"))
	if !strings.Contains(body, "Go docs") {
		t.Fatal("search by tag + text found nothing")
	}

	// The same URL again goes to the existing bookmark.
	_, _, loc := c.post("/bookmarks", url.Values{"url": {"go.dev/doc"}})
	id := regexp.MustCompile(`^/bookmarks/(\d+)/edit\?dup=1$`).FindStringSubmatch(loc)
	if id == nil {
		t.Fatalf("duplicate create redirected to %q", loc)
	}
	if code, _, loc := c.get("/bookmarks/new?url=" + url.QueryEscape("https://go.dev/doc")); code != http.StatusSeeOther || !strings.HasSuffix(loc, "?dup=1") {
		t.Fatalf("bookmarklet on saved URL: %d %q", code, loc)
	}

	if code, _, _ := c.post("/bookmarks/"+id[1], url.Values{"url": {"https://go.dev/doc"}, "title": {"Renamed"}, "tags": {"golang"}}); code != http.StatusSeeOther {
		t.Fatalf("update: %d", code)
	}
	c.post("/bookmarks/"+id[1]+"/flag", url.Values{"flag": {"archived"}, "on": {"1"}})
	if _, body, _ := c.get("/"); strings.Contains(body, "Renamed") {
		t.Fatal("archived bookmark still in the main list")
	}
	if _, body, _ := c.get("/?q=%21archived"); !strings.Contains(body, "Renamed") {
		t.Fatal("archived bookmark missing from archive")
	}
	if code, _, _ := c.post("/bookmarks/"+id[1]+"/delete", nil); code != http.StatusSeeOther {
		t.Fatalf("delete: %d", code)
	}
	if code, _, _ := c.get("/bookmarks/" + id[1] + "/edit"); code != http.StatusNotFound {
		t.Fatalf("deleted bookmark still editable: %d", code)
	}
}

// User B must never read or change user A's rows through any endpoint.
func TestCrossUserIsolation(t *testing.T) {
	e := newEnv(t)
	e.user("alice")
	e.user("bob")
	alice, bob := e.login("alice"), e.login("bob")
	e.tags("alice", "alicetag")

	alice.post("/bookmarks", url.Values{"url": {"https://alice.example/secret"}, "title": {"Alice secret"}, "tags": {"alicetag"}})
	ctx := context.Background()
	a, _ := e.store.UserByUsername(ctx, "alice")
	bm, err := e.store.FindBookmarkByURL(ctx, personal(e.store, a.ID), "https://alice.example/secret")
	if err != nil {
		t.Fatal(err)
	}
	tags, _ := e.store.ListTags(ctx, personal(e.store, a.ID))
	bid, tid := itoa(bm.ID), itoa(tags[0].ID)

	if _, body, _ := alice.get("/tags"); !strings.Contains(body, `href="/?q=%23alicetag"`) {
		t.Fatal("leak check below would be vacuous: alice's own tag link not rendered as expected")
	}
	for _, path := range []string{"/", "/?q=secret", "/?q=%23alicetag", "/?q=site%3Aalice.example", "/tags"} {
		if _, body, _ := bob.get(path); strings.Contains(body, "Alice secret") || strings.Contains(body, `href="/?q=%23alicetag"`) {
			t.Errorf("GET %s leaks alice's data to bob", path)
		}
	}
	if code, _, _ := bob.get("/bookmarks/" + bid + "/edit"); code != http.StatusNotFound {
		t.Errorf("bob GET edit: %d", code)
	}
	if code, _, loc := bob.get("/bookmarks/new?url=" + url.QueryEscape("https://alice.example/secret")); code != http.StatusOK {
		t.Errorf("bob's add form for alice's URL redirected to %q (leaks that it exists)", loc)
	}
	for path, form := range map[string]url.Values{
		"/bookmarks/" + bid:             {"url": {"https://bob.example"}, "title": {"pwned"}},
		"/bookmarks/" + bid + "/flag":   {"flag": {"favourite"}, "on": {"1"}},
		"/bookmarks/" + bid + "/delete": nil,
		"/tags/" + tid + "/rename":      {"name": {"pwned"}},
		"/tags/" + tid + "/delete":      nil,
	} {
		if code, _, _ := bob.post(path, form); code != http.StatusNotFound {
			t.Errorf("bob POST %s: %d, want 404", path, code)
		}
	}
	// Bob saving the same URL creates his own bookmark rather than touching alice's.
	if code, _, loc := bob.post("/bookmarks", url.Values{"url": {"https://alice.example/secret"}, "title": {"Bob copy"}}); code != http.StatusSeeOther || loc != "/" {
		t.Errorf("bob create same URL: %d %q", code, loc)
	}
	// Bob can't revoke alice's sessions either.
	sessions, _ := e.store.ListSessions(ctx, a.ID)
	bob.post("/settings/sessions/revoke", url.Values{"id": {hexOf(sessions[0].TokenHash)}})

	got, err := e.store.GetBookmark(ctx, personal(e.store, a.ID), bm.ID)
	if err != nil || got.Title != "Alice secret" || got.IsFavourite || len(got.Tags) != 1 || got.Tags[0] != "alicetag" {
		t.Fatalf("alice's bookmark changed: %+v %v", got, err)
	}
	if code, _, _ := alice.get("/tags"); code != http.StatusOK {
		t.Fatalf("alice was logged out by bob: %d", code)
	}
}

func TestChangePasswordRevokesOtherSessions(t *testing.T) {
	e := newEnv(t)
	e.user("alice")
	laptop, phone := e.login("alice"), e.login("alice")
	if code, body, _ := laptop.post("/settings/password", url.Values{"current": {"wrong"}, "password": {"new password 1"}, "confirm": {"new password 1"}}); code != http.StatusUnprocessableEntity || !strings.Contains(body, "Current password is wrong") {
		t.Fatalf("wrong current password: %d", code)
	}
	if _, body, _ := laptop.get("/settings/security"); !strings.Contains(body, `action="/settings/password"`) {
		t.Fatal("password form is not on the Security tab")
	}
	if _, body, _ := laptop.get("/settings"); strings.Contains(body, `action="/settings/password"`) {
		t.Fatal("password form is still on the Appearance tab")
	}
	if code, _, loc := laptop.post("/settings/password", url.Values{"current": {"alice-password"}, "password": {"new password 1"}, "confirm": {"new password 1"}}); code != http.StatusSeeOther || loc != "/settings/security?done=password" {
		t.Fatalf("change password: %d", code)
	}
	if code, _, _ := laptop.get("/settings"); code != http.StatusOK {
		t.Fatalf("current session lost: %d", code)
	}
	if code, _, _ := phone.get("/settings"); code != http.StatusSeeOther {
		t.Fatalf("other session survived password change: %d", code)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func hexOf(b []byte) string { return hex.EncodeToString(b) }

func TestAppearancePreference(t *testing.T) {
	e := newEnv(t)
	e.user("alice")
	anon := e.client()
	if _, body, _ := anon.get("/login"); !strings.Contains(body, `data-accent="ocean" data-theme="system"`) {
		t.Fatal("logged-out pages should use ocean and the other defaults")
	}
	c := e.login("alice")
	if _, body, _ := c.get("/"); !strings.Contains(body, `data-accent="indigo" data-theme="system"`) {
		t.Fatal("new users should get the defaults")
	}
	if code, _, loc := c.post("/settings/appearance", url.Values{"accent": {"terracotta"}, "theme": {"dark"}}); code != http.StatusSeeOther || loc != "/settings?done=appearance" {
		t.Fatalf("save appearance: %d %q", code, loc)
	}
	_, body, _ := c.get("/settings")
	if !strings.Contains(body, `data-accent="terracotta" data-theme="dark"`) || !strings.Contains(body, `value="terracotta" checked`) {
		t.Fatal("saved appearance not applied")
	}
	if code, _, _ := c.post("/settings/appearance", url.Values{"accent": {"red"}, "theme": {"dark"}}); code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown accent: %d", code)
	}
	// Preferences are per user.
	e.user("bob")
	if _, body, _ := e.login("bob").get("/"); !strings.Contains(body, `data-accent="indigo"`) {
		t.Fatal("alice's preference leaked to bob")
	}
}

// The CSP forbids inline styles, inline scripts and event-handler
// attributes, so no page may contain them. Scripts must be empty tags that
// load a file from /static/.
func cspViolation(body string) string {
	if m := regexp.MustCompile(`(?i)\sstyle=|<style|\son[a-z]+=`).FindString(body); m != "" {
		return m
	}
	tags := regexp.MustCompile(`(?i)<script[^>]*>`).FindAllString(body, -1)
	external := regexp.MustCompile(`(?i)<script src="/static/[^"]+"></script>`).FindAllString(body, -1)
	if len(tags) != len(external) {
		return "inline <script>"
	}
	return ""
}

func TestPagesAreCSPClean(t *testing.T) {
	e := newEnv(t)
	e.user("alice")
	c := e.login("alice")
	e.tags("alice", "go", "home lab")
	c.post("/bookmarks", url.Values{"url": {"https://go.dev"}, "title": {"Go"}, "tags": {"go, home lab"}})
	c.post("/bookmarks/1/share", url.Values{"on": {"1"}})
	c.post("/bundles", url.Values{"name": {"Go"}, "query": {"#go site:go.dev"}})
	b, _ := e.store.GetBookmark(context.Background(), personal(e.store, e.userID("alice")), 1)
	for _, path := range []string{"/", "/?q=nothing", "/bookmarks/new", "/bookmarks/1/edit", "/tags", "/settings", "/nope",
		"/settings/security", "/settings/tokens", "/settings/data", "/reauth", "/bundles", "/?q=%23go+site%3Ago.dev"} {
		_, body, _ := c.get(path)
		if m := cspViolation(body); m != "" {
			t.Errorf("%s contains %q", path, m)
		}
	}
	for _, path := range []string{"/login", "/setup", "/s/" + b.ShareToken} {
		if _, body, _ := e.client().get(path); cspViolation(body) != "" {
			t.Errorf("%s has inline style or script", path)
		}
	}
}

func TestVersionedAssets(t *testing.T) {
	e := newEnv(t)
	for _, name := range []string{"style.css", "icons.svg", "fonts/inter-latin.woff2"} {
		res, err := http.Get(e.srv.URL + assetURL(name))
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusOK || !strings.Contains(res.Header.Get("Cache-Control"), "immutable") {
			t.Errorf("%s: %d %q", name, res.StatusCode, res.Header.Get("Cache-Control"))
		}
	}
}

// A preload is only used when its URL is the one the stylesheet asks for.
func TestFontPreloadMatchesStylesheet(t *testing.T) {
	c := newEnv(t).admin()
	_, body, _ := c.get("/")
	css, _ := fs.ReadFile(assets, "static/style.css")
	src := regexp.MustCompile(`url\("([^"]+\.woff2)"\)`).FindStringSubmatch(string(css))
	if src == nil || !strings.Contains(body, `<link rel="preload" href="/static/`+src[1]+`"`) {
		t.Fatalf("no preload for the stylesheet's font %v", src)
	}
}

func TestFontChoice(t *testing.T) {
	c := newEnv(t).admin()
	preloads := func(body string) []string {
		var out []string
		for _, m := range regexp.MustCompile(`<link rel="preload" href="/static/([^"]+)"`).FindAllStringSubmatch(body, -1) {
			out = append(out, m[1])
		}
		return out
	}
	if _, body, _ := c.get("/"); !strings.Contains(body, `data-font="inter"`) || len(preloads(body)) != 1 {
		t.Fatalf("default font: preloads %v", preloads(body))
	}
	if code, _, _ := c.post("/settings/appearance", url.Values{"font": {"lato"}}); code != http.StatusSeeOther {
		t.Fatalf("save font: %d", code)
	}
	_, body, _ := c.get("/")
	if !strings.Contains(body, `data-font="lato"`) || strings.Join(preloads(body), " ") != "fonts/lato-latin-400.woff2 fonts/lato-latin-700.woff2" {
		t.Fatalf("lato: preloads %v", preloads(body))
	}
	// Saving another setting keeps the font.
	c.post("/settings/appearance", url.Values{"theme": {"dark"}})
	if _, body, _ := c.get("/"); !strings.Contains(body, `data-font="lato"`) {
		t.Fatal("font lost when saving the theme")
	}
	c.post("/settings/appearance", url.Values{"font": {"system"}})
	if _, body, _ := c.get("/"); len(preloads(body)) != 0 {
		t.Fatalf("system font preloads %v", preloads(body))
	}
	if code, _, _ := c.post("/settings/appearance", url.Values{"font": {"comic-sans"}}); code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown font: %d", code)
	}
}

// Every font can be picked, switched to in CSS, and has the files it loads.
func TestFontsAreComplete(t *testing.T) {
	css, _ := fs.ReadFile(assets, "static/style.css")
	for _, f := range store.Fonts {
		if fontLabels[f].Format == "" {
			t.Errorf("%s: no label", f)
		}
		if f != store.Fonts[0] && !strings.Contains(string(css), `[data-font="`+f+`"]`) {
			t.Errorf("%s: no data-font rule", f)
		}
		if !strings.Contains(string(css), ".font-sample."+f+" ") {
			t.Errorf("%s: no sample style", f)
		}
		for _, file := range fontFiles[f] {
			if _, err := fs.Stat(assets, "static/"+file); err != nil {
				t.Errorf("%s: %v", f, err)
			}
			if !strings.Contains(string(css), `url("`+file+`")`) {
				t.Errorf("%s: %s not in the stylesheet", f, file)
			}
		}
	}
}

// Icon-only row actions show what they do on hover.
func TestRowActionTooltips(t *testing.T) {
	c := newEnv(t).admin()
	c.post("/bookmarks", url.Values{"url": {"https://go.dev/"}, "title": {"Go"}})
	_, body, _ := c.get("/")
	row := body[strings.Index(body, `class="bm-actions"`):]
	row = row[:strings.Index(row, `class="icon-btn bm-more"`)]
	for _, tip := range []string{"Archive", "Edit", "Delete", "Add to favourites"} {
		if !strings.Contains(row, `title="`+tip+`"`) {
			t.Errorf("no %q tooltip", tip)
		}
	}
}

// Delete and archive submit only from inside a confirmation popover.
func TestDestructiveActionsNeedConfirmation(t *testing.T) {
	e := newEnv(t)
	e.user("alice")
	c := e.login("alice")
	c.post("/bookmarks", url.Values{"url": {"https://a.example"}, "title": {"Keep"}})
	c.post("/bookmarks", url.Values{"url": {"https://b.example"}, "title": {"Old"}})
	c.post("/bookmarks/2/flag", url.Values{"flag": {"archived"}, "on": {"1"}})

	// The submit buttons for delete/archive must sit inside the popover that
	// the row's trigger button opens; the row itself must not post directly.
	confirmed := func(body, id, action string) bool {
		trigger := strings.Contains(body, `popovertarget="`+id+`"`)
		start := strings.Index(body, `id="`+id+`"`)
		if !trigger || start < 0 {
			return false
		}
		end := strings.Index(body[start:], "</form>")
		return end > 0 && strings.Contains(body[start:start+end], `action="`+action+`"`)
	}
	_, body, _ := c.get("/")
	if !confirmed(body, "delete-1", "/bookmarks/1/delete") || !confirmed(body, "archive-1", "/bookmarks/1/flag") {
		t.Fatal("list: delete/archive are not behind a confirmation")
	}
	if n := strings.Count(body, `action="/bookmarks/1/delete"`); n != 1 {
		t.Fatalf("list has %d delete forms for bookmark 1, want only the confirmed one", n)
	}
	_, body, _ = c.get("/?q=%21archived")
	if strings.Contains(body, `id="archive-2"`) || !strings.Contains(body, `aria-label="Unarchive Old"`) {
		t.Fatal("archive view: unarchive should be one click")
	}
	if !confirmed(body, "delete-2", "/bookmarks/2/delete") {
		t.Fatal("archive view: delete is not behind a confirmation")
	}
	_, body, _ = c.get("/bookmarks/1/edit")
	if !confirmed(body, "delete-bookmark", "/bookmarks/1/delete") {
		t.Fatal("edit page: delete is not behind a confirmation")
	}
}

// Every accent the store accepts must be offered, labelled, and styled.
func TestEveryAccentIsStyled(t *testing.T) {
	css, err := assets.ReadFile("static/style.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range store.Accents {
		if accentLabels[a].Format == "" {
			t.Errorf("accent %q has no label", a)
		}
		if a != store.Accents[0] && !strings.Contains(string(css), `:root[data-accent="`+a+`"]`) {
			t.Errorf("accent %q has no colour tokens", a)
		}
		if !strings.Contains(string(css), ".swatch."+a+" ") {
			t.Errorf("accent %q has no swatch", a)
		}
		// The favicon uses the accent's light-mode colour.
		if c, ok := iconColours[a]; !ok || !strings.Contains(string(css), "--accent: light-dark("+c[0]+",") {
			t.Errorf("accent %q has no favicon colours matching its CSS", a)
		}
	}
	e := newEnv(t)
	e.user("alice")
	c := e.login("alice")
	if code, _, _ := c.post("/settings/appearance", url.Values{"accent": {"forest"}, "theme": {"system"}}); code != http.StatusSeeOther {
		t.Fatalf("save forest: %d", code)
	}
	_, body, _ := c.get("/settings")
	if !strings.Contains(body, `data-accent="forest"`) || strings.Count(body, `name="accent"`) != len(store.Accents) {
		t.Fatal("settings page doesn't offer every accent")
	}
	// The favicon and header logo follow the accent.
	if !strings.Contains(body, `<link rel="icon" href="/icon/forest.svg?v=`) || !strings.Contains(body, `<svg class="brand-mark"`) {
		t.Fatal("app icon doesn't follow the accent")
	}
	res, err := http.Get(e.srv.URL + "/icon/forest.svg")
	if err != nil || res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "image/svg+xml" {
		t.Fatalf("favicon: %v %v", res, err)
	}
	svg, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if !strings.Contains(string(svg), `fill="#2d6a4f"`) || strings.Contains(string(svg), `class="eye"`) {
		t.Fatalf("favicon = %s", svg)
	}
	for _, bad := range []string{"/icon/violet.svg", "/icon/forest", "/icon/..%2fstyle.css"} {
		if code, _, _ := e.client().get(bad); code != http.StatusNotFound {
			t.Errorf("%s: %d", bad, code)
		}
	}
	// A saved accent that no longer exists falls back to the default.
	e.db.W.Exec(`UPDATE users SET settings = '{"accent":"violet"}' WHERE username = 'alice'`)
	if _, body, _ := c.get("/"); !strings.Contains(body, `data-accent="indigo"`) {
		t.Fatal("removed accent didn't fall back to indigo")
	}
}

func TestSidebarPreference(t *testing.T) {
	e := newEnv(t)
	e.user("alice")
	c := e.login("alice")
	if _, body, _ := c.get("/"); !strings.Contains(body, `data-sidebar="left"`) {
		t.Fatal("default sidebar should be left")
	}
	if code, _, _ := c.post("/settings/appearance", url.Values{"accent": {"rose"}, "theme": {"dark"}, "sidebar": {"right"}}); code != http.StatusSeeOther {
		t.Fatalf("save sidebar: %d", code)
	}
	_, body, _ := c.get("/")
	if !strings.Contains(body, `data-sidebar="right"`) {
		t.Fatal("sidebar preference not applied")
	}
	// The navigation still comes first in the page for keyboard and screen readers.
	if strings.Index(body, `aria-label="Views"`) > strings.Index(body, `role="search"`) {
		t.Fatal("sidebar moved in the markup, not just visually")
	}
	if _, body, _ := c.get("/settings"); !strings.Contains(body, `value="right" checked`) {
		t.Fatal("settings page doesn't show the saved sidebar")
	}
	// A form that omits a field keeps it.
	c.post("/settings/appearance", url.Values{"accent": {"indigo"}})
	if u, _ := e.store.UserByUsername(context.Background(), "alice"); u.Settings.Sidebar != "right" || u.Settings.Theme != "dark" {
		t.Fatalf("partial save changed other fields: %+v", u.Settings)
	}
	if code, _, _ := c.post("/settings/appearance", url.Values{"sidebar": {"top"}}); code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown sidebar: %d", code)
	}
	css, _ := assets.ReadFile("static/style.css")
	if !strings.Contains(string(css), `:root[data-sidebar="right"] .sidebar { order: 2; }`) {
		t.Fatal("no CSS for the right-hand sidebar")
	}
}

func TestListPreferences(t *testing.T) {
	e := newEnv(t)
	e.user("alice")
	c := e.login("alice")
	ctx := context.Background()
	e.store.CreateBookmark(ctx, personal(e.store, e.userID("alice")), store.BookmarkInput{URL: "https://a.com", Title: "Aardvark"})
	e.store.CreateBookmark(ctx, personal(e.store, e.userID("alice")), store.BookmarkInput{URL: "https://z.com", Title: "Zebra", Description: "Stripes and more"})
	_, body, _ := c.get("/")
	if strings.Index(body, "Zebra") > strings.Index(body, "Aardvark") || !strings.Contains(body, "Stripes and more") {
		t.Fatal("default should be newest first, with descriptions")
	}
	if code, _, _ := c.post("/settings/appearance", url.Values{"sort": {"title"}, "descriptions": {"hide"}}); code != http.StatusSeeOther {
		t.Fatalf("save list settings: %d", code)
	}
	_, body, _ = c.get("/")
	if strings.Index(body, "Aardvark") > strings.Index(body, "Zebra") {
		t.Fatal("not sorted by title")
	}
	if strings.Contains(body, "Stripes and more") {
		t.Fatal("description shown although hidden")
	}
	if _, body, _ := c.get("/settings"); !strings.Contains(body, `value="title" checked`) || !strings.Contains(body, `value="hide" checked`) {
		t.Fatal("settings page doesn't show the saved list settings")
	}
	if code, _, _ := c.post("/settings/appearance", url.Values{"sort": {"random"}}); code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown sort: %d", code)
	}
}

func TestPagination(t *testing.T) {
	e := newEnv(t)
	e.user("alice")
	c := e.login("alice")
	id := e.userID("alice")
	for i := range 2*pageSize + 20 {
		e.store.CreateBookmark(context.Background(), personal(e.store, id), store.BookmarkInput{URL: "https://example.com/" + strconv.Itoa(i)})
	}
	_, body, _ := c.get("/")
	if n := strings.Count(body, `class="pager"`); n != 2 {
		t.Fatalf("%d pagers, want one above and one below the list", n)
	}
	if !strings.Contains(body, "1–50 of 120") || !strings.Contains(body, `href="/?page=2" rel="next"`) || strings.Contains(body, `rel="prev"`) {
		t.Fatal("first page controls")
	}
	_, body, _ = c.get("/?page=3")
	if !strings.Contains(body, "101–120 of 120") || strings.Contains(body, `rel="next"`) || !strings.Contains(body, `href="/?page=2" rel="prev"`) {
		t.Fatal("last page controls")
	}
	if code, _, loc := c.get("/?page=9"); code != http.StatusSeeOther || loc != "/?page=3" {
		t.Fatalf("past the last page: %d %q", code, loc)
	}
	// Few enough for one page: no controls.
	e.user("bob")
	if _, body, _ := e.login("bob").get("/"); strings.Contains(body, `class="pager"`) {
		t.Fatal("pager on a single page")
	}
}

func TestPagerLinks(t *testing.T) {
	pageURL := func(n int) string { return strconv.Itoa(n) }
	for _, c := range []struct {
		page       int
		prev, next string
	}{{1, "", "2"}, {6, "5", "7"}, {20, "19", ""}} {
		p := newPager(c.page, 20, 1000, pageURL)
		if p.PrevURL != c.prev || p.NextURL != c.next {
			t.Errorf("page %d: prev %q next %q, want %q %q", c.page, p.PrevURL, p.NextURL, c.prev, c.next)
		}
	}
	if p := newPager(20, 20, 1000, pageURL); p.From != 951 || p.To != 1000 {
		t.Errorf("last page shows %d–%d", p.From, p.To)
	}
}

func TestTagsDrawerOnPhones(t *testing.T) {
	e := newEnv(t)
	e.user("alice")
	c := e.login("alice")
	e.tags("alice", "homelab")
	e.store.CreateBookmark(context.Background(), personal(e.store, e.userID("alice")), store.BookmarkInput{URL: "https://a.com", Tags: []string{"homelab"}})
	_, body, _ := c.get("/")
	if !strings.Contains(body, `popovertarget="tag-drawer"`) || !strings.Contains(body, `id="tag-drawer"`) {
		t.Fatal("no Tags button and drawer")
	}
	if n := strings.Count(body, `href="/?q=%23homelab"`); n < 2 {
		t.Fatalf("tag listed %d times, want in the sidebar and the drawer", n)
	}
	css, _ := assets.ReadFile("static/style.css")
	if !strings.Contains(string(css), ".side-tags { display: none; }") || !strings.Contains(string(css), ".tags-btn { display: inline-flex; }") {
		t.Fatal("phones don't swap the sidebar tags for the drawer")
	}
}

func TestSearchHelp(t *testing.T) {
	e := newEnv(t)
	e.user("alice")
	_, body, _ := e.login("alice").get("/")
	if !strings.Contains(body, `aria-describedby="search-tips"`) || !strings.Contains(body, `id="search-tips" class="tooltip" role="tooltip"`) {
		t.Fatal("no search help")
	}
	for _, syntax := range []string{"#homelab", "-#homelab", "site:github.com", "!fav", "!shared", "!archived", "after:", "before:"} {
		if !strings.Contains(body, "<code>"+syntax) {
			t.Errorf("search help doesn't explain %s", syntax)
		}
	}
	if strings.Contains(body, `class="hints"`) {
		t.Fatal("the Try bar is still there")
	}
}

func TestTagTypeAhead(t *testing.T) {
	e := newEnv(t)
	e.user("alice")
	c := e.login("alice")
	e.tags("alice", "go", "home lab")
	c.post("/bookmarks", url.Values{"url": {"https://a.com"}, "tags": {"go, home lab"}})
	c.post("/bookmarks", url.Values{"url": {"https://b.com"}, "tags": {"home lab"}})
	for _, path := range []string{"/bookmarks/new", "/bookmarks/1/edit"} {
		_, body, _ := c.get(path)
		// Most used first.
		if !strings.Contains(body, `data-tags="home lab,go"`) || !strings.Contains(body, `role="combobox"`) {
			t.Errorf("%s: no tag suggestions", path)
		}
		if !strings.Contains(body, `<script src="/static/tags.js?v=`) {
			t.Errorf("%s: tags.js not loaded", path)
		}
	}
	// An invalid form shows the field again with its suggestions.
	if code, body, _ := c.post("/bookmarks", url.Values{"url": {"not a url"}}); code != http.StatusUnprocessableEntity || !strings.Contains(body, `data-tags="home lab,go"`) {
		t.Fatalf("error page: %d", code)
	}
}

// The user menu on the admin page is a popover, so the table's scroll box
// can't clip it.
func TestAdminUserMenuIsAPopover(t *testing.T) {
	e := newEnv(t)
	root := e.admin()
	e.user("alice")
	id := itoa(e.userID("alice"))
	_, body, _ := root.get("/admin/users")
	if !strings.Contains(body, `popovertarget="user-menu-`+id+`"`) || !strings.Contains(body, `id="user-menu-`+id+`" class="menu row-menu" popover`) {
		t.Fatal("user menu isn't a popover")
	}
	if strings.Contains(body, `<details class="row-menu"`) {
		t.Fatal("old details menu still there")
	}
}

func TestTagsMustBeCreatedFirst(t *testing.T) {
	e := newEnv(t)
	e.user("alice")
	c := e.login("alice")

	// Unknown tags are refused, and the form keeps what was typed.
	code, body, _ := c.post("/bookmarks", url.Values{"url": {"https://a.com"}, "title": {"A"}, "tags": {"home lab, go"}})
	if code != http.StatusUnprocessableEntity || !strings.Contains(body, "Create them under Tags first") || !strings.Contains(body, `value="home lab, go"`) {
		t.Fatalf("unknown tags: %d", code)
	}

	// Create them on the Tags page.
	for _, name := range []string{"home lab", "go"} {
		if code, _, loc := c.post("/tags", url.Values{"name": {name}}); code != http.StatusSeeOther || loc != "/tags" {
			t.Fatalf("create %q: %d %q", name, code, loc)
		}
	}
	if code, body, _ := c.post("/tags", url.Values{"name": {"Home Lab"}}); code != http.StatusUnprocessableEntity || !strings.Contains(body, "already a tag") {
		t.Fatalf("duplicate: %d", code)
	}
	if code, body, _ := c.post("/tags", url.Values{"name": {"a, b"}}); code != http.StatusUnprocessableEntity || !strings.Contains(body, "commas") {
		t.Fatalf("comma: %d", code)
	}
	_, body, _ = c.get("/tags")
	if !strings.Contains(body, `value="home lab"`) {
		t.Fatal("new tag not listed")
	}

	if code, _, _ := c.post("/bookmarks", url.Values{"url": {"https://a.com"}, "title": {"A"}, "tags": {"home lab, go"}}); code != http.StatusSeeOther {
		t.Fatalf("create with existing tags: %d", code)
	}
	// A tag with spaces links to a quoted search, which shows as its view.
	link := "/?q=" + url.QueryEscape(`#"home lab"`)
	_, body, _ = c.get("/")
	if !strings.Contains(body, `href="/?q=%23%22home&#43;lab%22"`) { // html/template writes + as &#43;
		t.Fatal("tag link isn't a quoted search")
	}
	_, body, _ = c.get(link)
	if !strings.Contains(body, "<h1>home lab</h1>") || !strings.Contains(body, ">A</a>") {
		t.Fatal("spaced tag view")
	}
	if _, body, _ := c.get("/bookmarks/1/edit"); !strings.Contains(body, `value="go, home lab"`) {
		t.Fatal("edit form doesn't show comma-separated tags")
	}

	// Deleting the bookmark keeps the tags.
	c.post("/bookmarks/1/delete", nil)
	if tags, _ := e.store.ListTags(context.Background(), personal(e.store, e.userID("alice"))); len(tags) != 2 {
		t.Fatalf("tags after delete: %+v", tags)
	}

	// The API refuses unknown tags too, and can create them.
	api := apiClient{t, e.srv.URL, apiToken(t, c, "write")}
	if code, out := api.do("POST", "/bookmarks", map[string]any{"url": "https://b.com", "tags": []string{"new one"}}); code != http.StatusUnprocessableEntity || !strings.Contains(out["detail"].(string), `"new one"`) {
		t.Fatalf("api unknown tag: %d %v", code, out)
	}
	if code, out := api.do("POST", "/tags", map[string]any{"name": "new one"}); code != http.StatusCreated || out["name"] != "new one" {
		t.Fatalf("api create tag: %d %v", code, out)
	}
	if code, _ := api.do("POST", "/bookmarks", map[string]any{"url": "https://b.com", "tags": []string{"new one"}}); code != http.StatusCreated {
		t.Fatalf("api create with tag: %d", code)
	}
}

func TestFooterLinksSource(t *testing.T) {
	e := newEnv(t)
	// Every page offers the source, signed in or not (AGPL section 13).
	for path, c := range map[string]*client{"/login": e.client(), "/": e.admin()} {
		if _, body, _ := c.get(path); !strings.Contains(body, `href="`+sourceURL+`"`) {
			t.Fatalf("%s: no source code link in the footer", path)
		}
	}
}

func TestFooterShowsVersion(t *testing.T) {
	defer func(v string) { Version = v }(Version)
	c := newEnv(t).admin()
	for v, link := range map[string]string{
		"dev":               "",
		"v1.2.3":            sourceURL + "/releases/tag/v1.2.3",
		"v1.3.0-rc.1":       sourceURL + "/releases/tag/v1.3.0-rc.1",
		"v1.2.3-4-gabc1234": "",
		"v1.2.3-dirty":      "",
	} {
		Version = v
		_, body, _ := c.get("/")
		if link == "" {
			if !strings.Contains(body, "Anchor "+v+" is free software") {
				t.Errorf("%s: version not shown", v)
			}
		} else if !strings.Contains(body, `Anchor <a href="`+link+`">`+v+`</a>`) {
			t.Errorf("%s: no link to %s", v, link)
		}
	}
}

// personal is a user's scope in their own space.
func personal(st *store.Store, userID int64) store.Scope {
	sc, err := st.PersonalScope(context.Background(), userID)
	if err != nil {
		panic(err)
	}
	return sc
}

func TestIconsDrawFromThePage(t *testing.T) {
	_, body, _ := newEnv(t).admin().get("/")
	if !strings.Contains(body, `<symbol id="i-star"`) || !strings.Contains(body, `<use href="#i-star">`) {
		t.Fatal("icons don't use the sprite in the page")
	}
	// Every icon a template asks for is in the sprite, or it draws nothing.
	files, _ := fs.Glob(assets, "templates/*.html")
	for _, f := range files {
		b, _ := fs.ReadFile(assets, f)
		for _, m := range regexp.MustCompile(`\{\{icon "([^"]+)"`).FindAllStringSubmatch(string(b), -1) {
			if !strings.Contains(string(iconSprite), `<symbol id="i-`+m[1]+`"`) {
				t.Errorf("%s: no icon %q", f, m[1])
			}
		}
	}
}

func TestViewBarAboveSearch(t *testing.T) {
	c := newEnv(t).admin()
	_, body, _ := c.get("/?q=" + url.QueryEscape("!fav"))
	bar := strings.Index(body, `<nav class="view-bar"`)
	if bar < strings.Index(body, `class="content"`) || bar > strings.Index(body, `role="search"`) {
		t.Fatal("no view bar above the search box")
	}
	if !strings.Contains(body[bar:], `<a href="/?q=%21fav" aria-current="page">`) {
		t.Fatal("view bar doesn't mark Favourites")
	}
}

func TestClearFiltersInSearchBox(t *testing.T) {
	c := newEnv(t).admin()
	if _, body, _ := c.get("/"); strings.Contains(body, `class="icon-btn search-clear"`) {
		t.Fatal("clear filters with no filter")
	}
	_, body, _ := c.get("/?q=" + url.QueryEscape("!fav"))
	form := body[strings.Index(body, `role="search"`):]
	form = form[:strings.Index(form, "</form>")]
	if !strings.Contains(form, `<a class="icon-btn search-clear" href="/" aria-label="Clear filters">`) {
		t.Fatal("no clear filters in the search box")
	}
}

func TestLoginPageHasNoTopBar(t *testing.T) {
	e := newEnv(t)
	e.user("alice")
	_, body, _ := e.client().get("/login")
	if strings.Contains(body, `<header class="top">`) || !strings.Contains(body, `<div class="auth-brand"><svg class="brand-mark"`) {
		t.Fatal("login page: want the app's mark and no top bar")
	}
	if _, body, _ := e.login("alice").get("/"); !strings.Contains(body, `<header class="top">`) || strings.Contains(body, "auth-brand") {
		t.Fatal("signed-in page lost its top bar")
	}
}

func (c *client) postJSON(path string, body []byte) (int, map[string]any) {
	c.e.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, c.e.srv.URL+path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", c.csrf())
	res, err := c.http.Do(req)
	if err != nil {
		c.e.t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func find(t *testing.T, body, pattern string) string {
	t.Helper()
	m := regexp.MustCompile(pattern).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("pattern %q not found in page", pattern)
	}
	return m[1]
}

func (e *env) admin() *client {
	e.t.Helper()
	if _, err := e.store.CreateUser(context.Background(), "root", auth.HashPassword("root-password"), true); err != nil {
		e.t.Fatal(err)
	}
	return e.login("root")
}
