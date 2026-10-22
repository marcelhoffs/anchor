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

	"github.com/marcelhoffs/burrow/internal/store"
)

type pageTemplate = *template.Template

var funcs = template.FuncMap{
	"ago":     ago,
	"tagURL":  func(name string) string { return "/?q=" + url.QueryEscape("#"+name) },
	"join":    strings.Join,
	"display": func(b store.Bookmark) string { return cmp.Or(b.Title, b.URL) },
	"asset":   assetURL,
	"icon":    icon,
	"initial": initial,
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
	default:
		s.serverError(w, r, err)
	}
	return false
}

func idParam(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	return id, err == nil && id > 0
}

// safeNext returns next if it is a local path, so redirects can't be used to
// send users off-site.
func safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		return "/"
	}
	return next
}

func urlQueryEscape(s string) string { return url.QueryEscape(s) }

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

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit + " ago"
	}
	return strconv.Itoa(n) + " " + unit + "s ago"
}
