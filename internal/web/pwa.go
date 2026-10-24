package web

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/marcelhoffs/anchor/internal/i18n"
)

// Anchor installs as an app: a web app manifest per accent and language, PNG
// icons in static/icons (see generate.sh there), and a service worker that
// caches only static files and an offline page, never anyone's bookmarks.

// themeColours are each accent's --accent in light and dark mode (see
// style.css): the browser's and app window's toolbar colour.
var themeColours = map[string][2]string{
	"indigo":     {"#4f46e5", "#8b8cf8"},
	"terracotta": {"#c2410c", "#fb8b4f"},
	"yellow":     {"#eab308", "#facc15"},
	"emerald":    {"#047857", "#34d399"},
	"forest":     {"#2d6a4f", "#95c9a8"},
	"ocean":      {"#0369a1", "#38bdf8"},
	"rose":       {"#be123c", "#fb7185"},
	"graphite":   {"#1e293b", "#e2e8f0"},
}

// themeColor is one <meta name="theme-color">; Media is empty when it
// applies whatever the colour scheme.
type themeColor struct{ Color, Media string }

// themeColors follows the theme setting: one colour for light or dark, both
// with a media query for system.
func themeColors(accent, theme string) []themeColor {
	c := themeColours[accent]
	switch theme {
	case "light":
		return []themeColor{{Color: c[0]}}
	case "dark":
		return []themeColor{{Color: c[1]}}
	}
	return []themeColor{
		{c[0], "(prefers-color-scheme: light)"},
		{c[1], "(prefers-color-scheme: dark)"},
	}
}

func manifestURL(accent, lang string) string {
	return "/app.webmanifest?" + url.Values{"accent": {accent}, "lang": {lang}, "v": {assetVersion}}.Encode()
}

func appleTouchIconURL(accent string) string {
	return assetURL("icons/apple-touch-" + accent + ".png")
}

type manifestIcon struct {
	Src     string `json:"src"`
	Sizes   string `json:"sizes"`
	Type    string `json:"type"`
	Purpose string `json:"purpose,omitempty"`
}

// manifest serves /app.webmanifest?accent=…&lang=…, public like the favicon.
func (s *Server) manifest(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	accent := q.Get("accent")
	colours, ok := iconColours[accent]
	if !ok {
		http.NotFound(w, r)
		return
	}
	lang, ok := i18n.Parse(q.Get("lang"))
	if !ok {
		lang = i18n.English
	}
	icons := []manifestIcon{{Src: appIconURL(accent), Sizes: "any", Type: "image/svg+xml"}}
	for _, size := range []string{"192", "512"} {
		icons = append(icons,
			manifestIcon{Src: assetURL("icons/icon-" + accent + "-" + size + ".png"), Sizes: size + "x" + size, Type: "image/png"},
			manifestIcon{Src: assetURL("icons/maskable-" + accent + "-" + size + ".png"), Sizes: size + "x" + size, Type: "image/png", Purpose: "maskable"})
	}
	m := map[string]any{
		"id":               "/",
		"start_url":        "/",
		"scope":            "/",
		"display":          "standalone",
		"name":             "Anchor",
		"short_name":       "Anchor",
		"description":      lang.T("Your bookmarks, with tags, search and shared spaces."),
		"lang":             string(lang),
		"theme_color":      colours[0],
		"background_color": "#f8fafc", // --bg in light mode
		"icons":            icons,
		"shortcuts": []map[string]any{{
			"name":  lang.T("Add bookmark"),
			"url":   "/bookmarks/new",
			"icons": []manifestIcon{{Src: assetURL("icons/icon-" + accent + "-192.png"), Sizes: "192x192", Type: "image/png"}},
		}},
		// Android's share sheet opens the add form, filled in (see newBookmark).
		"share_target": map[string]any{
			"action": "/bookmarks/new",
			"method": "GET",
			"params": map[string]string{"url": "url", "title": "title", "text": "text"},
		},
	}
	w.Header().Set("Content-Type", "application/manifest+json")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	json.NewEncoder(w).Encode(m)
}

// precache is what the service worker stores on install: the versioned CSS
// and scripts, the fonts (style.css asks for them unversioned), the
// signed-out favicon and the offline page.
var precache = func() []string {
	list := []string{"/offline", appIconURL("ocean")}
	fs.WalkDir(assets, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		name := strings.TrimPrefix(p, "static/")
		switch path.Ext(name) {
		case ".css", ".js":
			if name != "sw.js" {
				list = append(list, assetURL(name))
			}
		case ".woff2":
			list = append(list, "/static/"+name)
		}
		return nil
	})
	return list
}()

// serviceWorker serves static/sw.js at /sw.js, so its scope is the whole
// site, with this build's asset version in it: a deploy changes the file,
// and browsers then install the new worker and drop the old cache.
func (s *Server) serviceWorker(w http.ResponseWriter, r *http.Request) {
	b, err := fs.ReadFile(assets, "static/sw.js")
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	list, _ := json.Marshal(precache)
	src := strings.Replace(string(b), `"__VERSION__"`, strconv.Quote(assetVersion), 1)
	src = strings.Replace(src, "__PRECACHE__", string(list), 1)
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write([]byte(src))
}

// offline is the page the service worker shows when the network is gone. It
// is fetched without a session, so it never holds anyone's data.
func (s *Server) offline(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, "offline", nil)
}

var webLink = regexp.MustCompile(`(?i)https?://\S+`)

// sharedLink makes sense of what Android's share sheet sends: many apps put
// the link in text and leave url empty. The link comes out of text, and
// what's left becomes the description, unless it only repeats the title.
func sharedLink(rawURL, title, description, text string) (string, string) {
	text = strings.TrimSpace(text)
	if rawURL == "" {
		rawURL = webLink.FindString(text)
	}
	if rawURL != "" {
		text = strings.TrimSpace(strings.Replace(text, rawURL, "", 1))
	}
	if description == "" && text != title {
		description = text
	}
	return rawURL, description
}
