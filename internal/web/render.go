package web

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"github.com/marcelhoffs/anchor/internal/i18n"
	"github.com/marcelhoffs/anchor/internal/store"
)

type pageTemplate = *template.Template

// sourceURL is where the footer sends people for Anchor's source. The AGPL
// asks whoever runs a changed version as a service to offer its source to
// its users: a fork points this at its own repository.
const sourceURL = "https://github.com/marcelhoffs/anchor"

// Version is the release tag Anchor was built from, set at build time with
// -ldflags "-X github.com/marcelhoffs/anchor/internal/web.Version=v1.2.3".
var Version = "dev"

// untagged matches what git describe adds to a build between releases
// (v1.2.3-4-gabc1234, maybe -dirty): such a version has no release page.
var untagged = regexp.MustCompile(`-[0-9]+-g[0-9a-f]+(-dirty)?$|-dirty$`)

// releaseURL is the GitHub release page of the running version, or "" when
// it isn't a release.
func releaseURL() string {
	if Version == "dev" || untagged.MatchString(Version) {
		return ""
	}
	return sourceURL + "/releases/tag/" + url.PathEscape(Version)
}

var funcs = template.FuncMap{
	"sourceURL":  func() string { return sourceURL },
	"version":    func() string { return Version },
	"releaseURL": releaseURL,
	// The footer's links, as values for its translated sentence.
	"versionLink": func() template.HTML {
		if u := releaseURL(); u != "" {
			return template.HTML(`<a href="` + html.EscapeString(u) + `">` + html.EscapeString(Version) + `</a>`)
		}
		return template.HTML(html.EscapeString(Version))
	},
	"licenseLink": func() template.HTML {
		return template.HTML(`<a href="` + sourceURL + `/blob/main/LICENSE" rel="license">AGPL-3.0</a>`)
	},
	"tagURL":     tagURL,
	"bundleURL":  bundleURL,
	"dict":       dict,
	"brandMark":  brandMark,
	"appIconURL": appIconURL,
	"join":       strings.Join,
	"display":    func(b store.Bookmark) string { return cmp.Or(b.Title, b.URL) },
	"asset":      assetURL,
	"fontFiles":  func(font string) []string { return fontFiles[font] },
	"iconSprite": func() template.HTML { return iconSprite },
	"icon":       icon,
	"initial":    initial,
	"bytes":      humanBytes,
	"strong":     strong,
	"code":       code,
	"secretCode": func(s string) template.HTML {
		return template.HTML(`<code class="secret">` + html.EscapeString(s) + `</code>`)
	},
	// link is an <a> for t: (link "/tags" (t "Tags")).
	"link": func(href string, text template.HTML) template.HTML {
		return template.HTML(`<a href="` + html.EscapeString(href) + `">` + string(text) + `</a>`)
	},
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return strconv.FormatFloat(float64(n)/float64(div), 'f', 1, 64) + " " + string("KMGTPE"[exp]) + "B"
}

// assetVersion changes whenever any embedded static file does, so asset URLs
// can be cached forever and still update on deploy.
var assetVersion = func() string {
	h := sha256.New()
	fs.WalkDir(assets, "static", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := fs.ReadFile(assets, p)
			h.Write([]byte(p))
			h.Write(b)
		}
		return err
	})
	return hex.EncodeToString(h.Sum(nil))[:12]
}()

func assetURL(name string) string { return "/static/" + name + "?v=" + assetVersion }

// iconSprite is static/icons.svg to put in every page, so icons draw with
// the page: an external sprite fills in only after the first paint, and
// the icons flash on every navigation. Its ids get an "i-" prefix to keep
// clear of the page's own.
var iconSprite = func() template.HTML {
	b, err := fs.ReadFile(assets, "static/icons.svg")
	if err != nil {
		panic(err)
	}
	s := strings.Replace(string(b), "<svg ", `<svg class="sprite" aria-hidden="true" `, 1)
	return template.HTML(strings.ReplaceAll(s, `<symbol id="`, `<symbol id="i-`))
}()

// icon renders an icon from the sprite in the page. Only ever called from
// templates with literal arguments.
func icon(name string, class ...string) template.HTML {
	cls := strings.Join(append([]string{"icon"}, class...), " ")
	return template.HTML(`<svg class="` + cls + `" aria-hidden="true" focusable="false"><use href="#i-` +
		name + `"></use></svg>`)
}

// initial is the upper-cased first letter or digit of s, for letter avatars.
func initial(s string) string {
	s = strings.TrimPrefix(s, "www.")
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return string(unicode.ToUpper(r))
		}
	}
	if r, _ := utf8.DecodeRuneInString(s); r != utf8.RuneError {
		return string(r)
	}
	return "?"
}

// loadTemplates parses each page together with the shared layout, then
// clones it for each language with that language's functions.
func loadTemplates() (map[i18n.Lang]map[string]pageTemplate, error) {
	files, err := fs.Glob(assets, "templates/*.html")
	if err != nil {
		return nil, err
	}
	out := map[i18n.Lang]map[string]pageTemplate{}
	for _, l := range i18n.Langs {
		out[l] = map[string]pageTemplate{}
	}
	for _, f := range files {
		name := path.Base(f)
		if name == "layout.html" {
			continue
		}
		t, err := template.New(name).Funcs(funcs).Funcs(langFuncs(i18n.English)).ParseFS(assets, "templates/layout.html", f)
		if err != nil {
			return nil, err
		}
		for _, l := range i18n.Langs {
			c, err := t.Clone()
			if err != nil {
				return nil, err
			}
			out[l][strings.TrimSuffix(name, ".html")] = c.Funcs(langFuncs(l))
		}
	}
	return out, nil
}

// authPages show no top bar to a signed-out visitor, only the app's mark
// above their card: there is nothing to navigate to yet.
var authPages = map[string]bool{"login": true, "signup": true, "setup": true, "signup_closed": true, "verify": true}

// data is what every page template receives; handlers add their own keys.
type data map[string]any

func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, page string, d data) {
	lang := langOf(r)
	t, ok := s.pages[lang][page]
	if !ok {
		s.serverError(w, r, fmt.Errorf("no template %q", page))
		return
	}
	if d == nil {
		d = data{}
	}
	// Messages from handlers (notices, errors, headings) are translated now.
	// Only top-level values: a Msg inside a struct or slice isn't seen.
	for k, v := range d {
		if m, ok := v.(i18n.Msg); ok {
			d[k] = m.In(lang)
		}
	}
	u := currentUser(r)
	d["User"] = u
	d["CSRF"] = csrfToken(r)
	d["Path"] = r.URL.Path
	d["Bare"] = u == nil && authPages[page]
	prefs := store.Settings{}.Normalized()
	if u != nil {
		prefs = u.Settings
	}
	d["Prefs"] = prefs
	d["PasskeysAvailable"] = s.webauthn != nil
	d["SSO"] = s.cfg.OIDC // nil when single sign-on is off
	// Pages outside the signed-in area have no space, so nothing is read-only.
	nav := currentNav(r)
	d["Nav"] = nav
	d["CanWrite"] = nav == nil || nav.Scope.CanWrite()
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "layout", d); err != nil {
		s.serverError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	buf.WriteTo(w)
}

func (s *Server) serverError(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	http.Error(w, langOf(r).T("Internal server error"), http.StatusInternalServerError)
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusNotFound, "notfound", nil)
}

// storeError maps store errors to responses; it reports whether err was nil.
func (s *Server) storeError(w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case err == nil:
		return true
	case errors.Is(err, store.ErrNotFound):
		s.notFound(w, r)
	case errors.Is(err, store.ErrForbidden):
		http.Error(w, langOf(r).T("You can read these bookmarks but not change them."), http.StatusForbidden)
	default:
		s.serverError(w, r, err)
	}
	return false
}

func idParam(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	return id, err == nil && id > 0
}

// safeNext returns next if it is a path on this site, else "/", so redirects
// can't send users off-site. It refuses anything a browser could read as
// another host: //host, a backslash, and control characters, which browsers
// strip (a tab between two slashes would become //host).
func safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") ||
		strings.ContainsFunc(next, func(r rune) bool { return r == '\\' || unicode.IsControl(r) }) {
		return "/"
	}
	if u, err := url.Parse(next); err != nil || u.Scheme != "" || u.Host != "" {
		return "/"
	}
	return next
}

func urlQueryEscape(s string) string { return url.QueryEscape(s) }

// dict builds a map from key, value pairs, for passing several values to a
// shared template: {{template "confirm" dict "ID" "x" "Title" "…"}}.
func dict(kv ...any) map[string]any {
	m := make(map[string]any, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i].(string)] = kv[i+1]
	}
	return m
}
