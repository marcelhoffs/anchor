package web

import (
	"bytes"
	"context"
	"html/template"
	"io"
	"io/fs"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/marcelhoffs/anchor/internal/i18n"
	"github.com/marcelhoffs/anchor/internal/store"
)

// A Go or template string literal: "…" with escapes.
const lit = `("(?:[^"\\]|\\.)*")`

var (
	// In templates: {{t "…"}}, (t "…"), and tn's two forms.
	tmplT  = regexp.MustCompile(`[{(]t ` + lit)
	tmplTN = regexp.MustCompile(`[{(]tn [^"]*?` + lit + ` ` + lit)
	// In Go: i18n.M("…"), l.T("…"), l.N(n, "…", "…"), and the store's and
	// auth's input errors.
	goT  = regexp.MustCompile(`(?:i18n\.M|\.T|\binvalid|\binputError)\(` + lit)
	goTN = regexp.MustCompile(`\.N\([^"]*?` + lit + `, ` + lit)
)

// messageKeys finds every message the interface translates, as catalog
// keys: the English text, or "one|other" for a plural.
func messageKeys(t *testing.T) map[string]bool {
	t.Helper()
	keys := map[string]bool{}
	unquote := func(s string) string {
		u, err := strconv.Unquote(s)
		if err != nil {
			t.Fatalf("bad literal %s: %v", s, err)
		}
		return u
	}
	scan := func(src string, single, plural *regexp.Regexp) {
		for _, m := range single.FindAllStringSubmatch(src, -1) {
			keys[unquote(m[1])] = true
		}
		for _, m := range plural.FindAllStringSubmatch(src, -1) {
			keys[unquote(m[1])+"|"+unquote(m[2])] = true
		}
	}
	files, _ := fs.Glob(assets, "templates/*.html")
	for _, f := range files {
		b, _ := fs.ReadFile(assets, f)
		scan(string(b), tmplT, tmplTN)
	}
	for _, dir := range []string{".", "../store", "../auth"} {
		gofiles, _ := filepath.Glob(filepath.Join(dir, "*.go"))
		for _, f := range gofiles {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			scan(string(b), goT, goTN)
		}
	}
	return keys
}

// verbs are a format's fmt verbs by argument index, so a translation may
// reorder them with %[n]s.
func verbs(s string) []string {
	var out []string
	n := 0
	for _, m := range regexp.MustCompile(`%(?:\[(\d+)\])?[a-z]`).FindAllStringSubmatch(strings.ReplaceAll(s, "%%", ""), -1) {
		if m[1] != "" {
			n, _ = strconv.Atoi(m[1])
		} else {
			n++
		}
		out = append(out, strconv.Itoa(n)+m[0][len(m[0])-1:])
	}
	slices.Sort(out)
	return out
}

func TestCatalogsComplete(t *testing.T) {
	keys := messageKeys(t)
	if len(keys) < 400 {
		t.Fatalf("found only %d messages: is the extraction broken?", len(keys))
	}
	for _, l := range i18n.Langs[1:] {
		have := i18n.Keys(l)
		for k := range keys {
			if !have[k] {
				t.Errorf("%s: missing %q", l, k)
			}
		}
		for k := range have {
			if !keys[k] {
				t.Errorf("%s: %q is no longer used", l, k)
			}
		}
		for k := range keys {
			if !have[k] {
				continue
			}
			one, other, plural := i18n.Plural(k)
			if plural != l.IsPlural(k) {
				t.Errorf("%s: %q should be %s", l, k, map[bool]string{true: "a [one, other] pair", false: "a string"}[plural])
				continue
			}
			if !plural {
				if got := l.Text(k); !slices.Equal(verbs(got), verbs(k)) {
					t.Errorf("%s: %q has other verbs than %q", l, got, k)
				}
				continue
			}
			// 1 is singular and 2 plural in every language here.
			for n, en := range map[int]string{1: one, 2: other} {
				if got := l.Form(n, one, other); !slices.Equal(verbs(got), verbs(en)) {
					t.Errorf("%s: %q has other verbs than %q", l, got, en)
				}
			}
		}
	}
}

// Each language renders the pages in it, and the user's choice beats the
// browser's.
func TestPagesInEachLanguage(t *testing.T) {
	e := newEnv(t)
	c := e.admin() // with no users, /login sends everyone to /setup
	for lang, want := range map[string]string{"nl": "Inloggen", "es": "Iniciar sesión", "fr": "Se connecter"} {
		req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/login", nil)
		req.Header.Set("Accept-Language", lang+";q=0.9,en;q=0.5")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b := new(strings.Builder)
		_, _ = io.Copy(b, res.Body)
		res.Body.Close()
		if !strings.Contains(b.String(), `<html lang="`+lang+`"`) || !strings.Contains(b.String(), want) {
			t.Errorf("%s: login page not translated", lang)
		}
	}
	if code, _, _ := c.post("/settings/appearance", url.Values{"language": {"nl"}}); code != http.StatusSeeOther {
		t.Fatalf("save language: %d", code)
	}
	if _, body, _ := c.get("/"); !strings.Contains(body, `<html lang="nl"`) || !strings.Contains(body, "Alle bladwijzers") {
		t.Fatal("chosen language not used")
	}
	if code, _, _ := c.post("/settings/appearance", url.Values{"language": {"de"}}); code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown language: %d", code)
	}
}

// Every page renders in every language: no template errors, and no fmt
// errors (%!d) from a translation whose verbs don't fit the values.
func TestEveryPageInEveryLanguage(t *testing.T) {
	e := newEnv(t)
	c := e.admin()
	e.user("alice")
	c.post("/tags", url.Values{"name": {"go"}})
	c.post("/bookmarks", url.Values{"url": {"https://go.dev/"}, "title": {"Go & friends"}, "tags": {"go"}})
	c.post("/bookmarks/1/share", url.Values{"on": {"1"}})
	c.post("/bookmarks/1/flag", url.Values{"flag": {"favourite"}, "on": {"1"}})
	c.post("/saved-searches", url.Values{"name": {"Go"}, "query": {"#go"}})
	c.post("/spaces", url.Values{"name": {"Team"}})
	c.post("/admin/settings", url.Values{"signup": {"invite"}})
	c.post("/admin/invites", url.Values{"note": {"friend"}})
	ctx := context.Background()
	uid := e.userID("root")
	e.store.CreateAPIToken(ctx, uid, "script", []byte("hash"), []string{"read"}, time.Now().Add(time.Hour))
	spaces, _ := e.store.ListSpaces(ctx, uid)
	team, _ := e.store.Access(ctx, uid, spaces[0].ID)
	e.store.AddMember(ctx, team, "alice", store.RoleRead)
	b, _ := e.store.GetBookmark(ctx, personal(e.store, uid), 1)

	pages := []string{"/", "/?q=%23go", "/?q=nothing", "/?q=%21archived", "/tags", "/saved-searches", "/bookmarks/new",
		"/bookmarks/1/edit", "/settings", "/settings/security", "/settings/tokens", "/settings/data", "/spaces",
		spaceURL(spaces[0].ID), "/admin/", "/admin/users", "/admin/invites", "/admin/spaces", "/reauth",
		"/s/" + b.ShareToken, "/no-such-page"}
	anon := e.client()
	for _, lang := range i18n.Langs {
		c.post("/settings/appearance", url.Values{"language": {string(lang)}})
		for _, p := range pages {
			code, body, _ := c.get(p)
			if code != http.StatusOK && !(p == "/no-such-page" && code == http.StatusNotFound) {
				t.Errorf("%s %s: %d", lang, p, code)
			}
			// Unknown paths skip the session, so their 404 follows the browser.
			if strings.Contains(body, "%!") || p != "/no-such-page" && !strings.Contains(body, `<html lang="`+string(lang)+`"`) {
				t.Errorf("%s %s: bad page:\n%s", lang, p, body)
			}
		}
		// Pages that answer a form.
		for _, f := range []struct {
			path string
			form url.Values
		}{
			{"/tags", url.Values{"name": {"bad,name"}}},
			{"/bookmarks", url.Values{"url": {"not a url"}}},
		} {
			if _, body, _ := c.post(f.path, f.form); strings.Contains(body, "%!") {
				t.Errorf("%s POST %s: fmt error", lang, f.path)
			}
		}
		// Pages without a session follow the browser.
		for _, p := range []string{"/signup", "/offline"} {
			req, _ := http.NewRequest(http.MethodGet, e.srv.URL+p, nil)
			req.Header.Set("Accept-Language", string(lang))
			res, err := anon.http.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(res.Body)
			res.Body.Close()
			if strings.Contains(string(body), "%!") || !strings.Contains(string(body), `<html lang="`+string(lang)+`"`) {
				t.Errorf("%s %s: bad page", lang, p)
			}
		}
	}
}

// Between the password and the second factor, pages already speak the
// user's chosen language, not the browser's.
func TestSecondFactorInUsersLanguage(t *testing.T) {
	e := newEnv(t)
	e.user("alice")
	a := e.login("alice")
	enableTOTP(t, a)
	a.post("/settings/appearance", url.Values{"language": {"fr"}})

	c := e.passwordStep("alice") // the test client sends no Accept-Language
	if _, body, _ := c.get("/login/2fa"); !strings.Contains(body, `<html lang="fr"`) {
		t.Fatal("2FA page not in the user's language")
	}
	if _, body, _ := c.post("/login/2fa/totp", url.Values{"code": {"000000"}}); !strings.Contains(body, "Ce code n") {
		t.Fatal("2FA error not in the user's language")
	}
}

// Each kind of refused import gets its own message.
func TestImportErrorMessages(t *testing.T) {
	e := newEnv(t)
	c := e.admin()
	for file, want := range map[string]string{
		`{"format":"anchor-bookmarks","version":9,"bookmarks":[]}`: "newer version of Anchor",
		`{"format":"something-else"}`:                              "isn&#39;t a valid Anchor JSON export",
		`{not json`:                                                "isn&#39;t a valid Anchor JSON export",
		"just some text":                                           "Export your bookmarks from a browser as HTML",
	} {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		mw.WriteField("csrf", c.csrf())
		fw, _ := mw.CreateFormFile("file", "bookmarks")
		fw.Write([]byte(file))
		mw.Close()
		req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/settings/import", &buf)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		res, err := c.http.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(string(body), want) {
			t.Errorf("%s: %d, want %q", file, res.StatusCode, want)
		}
	}
}

// t escapes every value that isn't HTML or a count, whatever its type.
func TestTranslateEscapesValues(t *testing.T) {
	type name string
	tf := langFuncs(i18n.English)["t"].(func(string, ...any) template.HTML)
	got := tf("%s, %s, %s, %d", name("<b>"), "<i>", strong("<u>"), 3)
	if want := template.HTML("&lt;b&gt;, &lt;i&gt;, <strong>&lt;u&gt;</strong>, 3"); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
