package web

import (
	"context"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/marcelhoffs/burrow/internal/auth"
	"github.com/marcelhoffs/burrow/internal/config"
	"github.com/marcelhoffs/burrow/internal/db"
	"github.com/marcelhoffs/burrow/internal/store"
)

type env struct {
	t     *testing.T
	srv   *httptest.Server
	store *store.Store
}

func newEnv(t *testing.T) *env {
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
	base, _ := url.Parse("http://" + ts.Listener.Addr().String())
	cfg := &config.Config{BaseURL: base, SecretKey: make([]byte, 32)}
	s, err := New(cfg, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ts.Config.Handler = s.Handler()
	ts.Start()
	t.Cleanup(ts.Close)
	return &env{t: t, srv: ts, store: st}
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
	if code, body, _ := c.post("/login", url.Values{"username": {"alice"}, "password": {"wrong"}}); code != http.StatusUnauthorized || !strings.Contains(body, invalidLogin) {
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

	if code, body, _ := c.post("/bookmarks", url.Values{"url": {"javascript:alert(1)"}}); code != http.StatusUnprocessableEntity || !strings.Contains(body, "valid http") {
		t.Fatalf("javascript: URL accepted: %d", code)
	}
	if code, _, loc := c.post("/bookmarks", url.Values{"url": {"https://go.dev/doc"}, "title": {"Go docs <script>"}, "tags": {"go, reading"}}); code != http.StatusSeeOther || loc != "/" {
		t.Fatalf("create: %d %q", code, loc)
	}
	_, body, _ := c.get("/")
	if !strings.Contains(body, "Go docs &lt;script&gt;") || !strings.Contains(body, "#reading") {
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

	alice.post("/bookmarks", url.Values{"url": {"https://alice.example/secret"}, "title": {"Alice secret"}, "tags": {"alicetag"}})
	ctx := context.Background()
	a, _ := e.store.UserByUsername(ctx, "alice")
	bm, err := e.store.FindBookmarkByURL(ctx, a.ID, "https://alice.example/secret")
	if err != nil {
		t.Fatal(err)
	}
	tags, _ := e.store.ListTags(ctx, a.ID)
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

	got, err := e.store.GetBookmark(ctx, a.ID, bm.ID)
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
	if code, _, _ := laptop.post("/settings/password", url.Values{"current": {"alice-password"}, "password": {"new password 1"}, "confirm": {"new password 1"}}); code != http.StatusSeeOther {
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
	if _, body, _ := anon.get("/login"); !strings.Contains(body, `data-accent="indigo" data-theme="system"`) {
		t.Fatal("logged-out pages should use the defaults")
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

// The CSP forbids inline styles and scripts, so no page may contain them.
func TestPagesAreCSPClean(t *testing.T) {
	e := newEnv(t)
	e.user("alice")
	c := e.login("alice")
	c.post("/bookmarks", url.Values{"url": {"https://go.dev"}, "title": {"Go"}, "tags": {"go"}})
	inline := regexp.MustCompile(`(?i)\sstyle=|<style|<script|\son[a-z]+=`)
	for _, path := range []string{"/", "/?q=nothing", "/bookmarks/new", "/bookmarks/1/edit", "/tags", "/settings", "/nope"} {
		_, body, _ := c.get(path)
		if m := inline.FindString(body); m != "" {
			t.Errorf("%s contains %q", path, m)
		}
	}
	for _, path := range []string{"/login", "/setup"} {
		if _, body, _ := e.client().get(path); inline.MatchString(body) {
			t.Errorf("%s has inline style or script", path)
		}
	}
}

func TestVersionedAssets(t *testing.T) {
	e := newEnv(t)
	for _, name := range []string{"style.css", "icons.svg", "icon.svg", "fonts/inter-latin.woff2"} {
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
