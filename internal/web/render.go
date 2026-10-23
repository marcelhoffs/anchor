package web

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"github.com/marcelhoffs/anchor/internal/search"
	"github.com/marcelhoffs/anchor/internal/store"
)

type pageTemplate = *template.Template

// sourceURL is where the footer sends people for Anchor's source. The AGPL
// asks whoever runs a changed version as a service to offer its source to
// its users: a fork points this at its own repository.
const sourceURL = "https://github.com/marcelhoffs/anchor"

var funcs = template.FuncMap{
	"sourceURL":  func() string { return sourceURL },
	"ago":        ago,
	"count":      count,
	"tagURL":     func(name string) string { return "/?q=" + url.QueryEscape(search.TagQuery(name)) },
	"bundleURL":  bundleURL,
	"dict":       dict,
	"brandMark":  brandMark,
	"appIconURL": appIconURL,
	"join":       strings.Join,
	"display":    func(b store.Bookmark) string { return cmp.Or(b.Title, b.URL) },
	"asset":      assetURL,
	"icon":       icon,
	"initial":    initial,
	"bytes":      humanBytes,
	"roleLabel":  roleLabel,
}

// roleLabel names a space role for people.
func roleLabel(r store.Role) string {
	switch r {
	case store.RoleOwner:
		return "Owner"
	case store.RoleWrite:
		return "Can edit"
	}
	return "Read only"
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

// icon renders an icon from the static sprite. Only ever called from
// templates with literal arguments.
func icon(name string, class ...string) template.HTML {
	cls := strings.Join(append([]string{"icon"}, class...), " ")
	return template.HTML(`<svg class="` + cls + `" aria-hidden="true" focusable="false"><use href="` +
		assetURL("icons.svg") + "#" + name + `"></use></svg>`)
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

// loadTemplates parses each page together with the shared layout.
func loadTemplates() (map[string]pageTemplate, error) {
	files, err := fs.Glob(assets, "templates/*.html")
	if err != nil {
		return nil, err
	}
	pages := map[string]pageTemplate{}
	for _, f := range files {
		name := path.Base(f)
		if name == "layout.html" {
			continue
		}
		t, err := template.New(name).Funcs(funcs).ParseFS(assets, "templates/layout.html", f)
		if err != nil {
			return nil, err
		}
		pages[strings.TrimSuffix(name, ".html")] = t
	}
	return pages, nil
}

// data is what every page template receives; handlers add their own keys.
type data map[string]any

func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, page string, d data) {
	t, ok := s.pages[page]
	if !ok {
		s.serverError(w, r, fmt.Errorf("no template %q", page))
		return
	}
	if d == nil {
		d = data{}
	}
	u := currentUser(r)
	d["User"] = u
	d["CSRF"] = csrfToken(r)
	d["Path"] = r.URL.Path
	prefs := store.Settings{}.Normalized()
	if u != nil {
		prefs = u.Settings
	}
	d["Prefs"] = prefs
	d["PasskeysAvailable"] = s.webauthn != nil
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
	http.Error(w, "Internal server error", http.StatusInternalServerError)
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
		http.Error(w, "You can read these bookmarks but not change them.", http.StatusForbidden)
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

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d.Minutes()), "minute")
	case d < 24*time.Hour:
		return plural(int(d.Hours()), "hour")
	case d < 30*24*time.Hour:
		return plural(int(d.Hours()/24), "day")
	case d < 365*24*time.Hour:
		return plural(int(d.Hours()/24/30), "month")
	default:
		return plural(int(d.Hours()/24/365), "year")
	}
}

func plural(n int, unit string) string { return count(n, unit) + " ago" }

// count is "1 bookmark", "2 bookmarks".
func count(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return strconv.Itoa(n) + " " + unit + "s"
}
