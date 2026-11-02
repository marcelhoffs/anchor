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

// pageBackgrounds are each accent's --bg in light and dark mode (see
// style.css), which Android's status and navigation bars take on so they
// blend with the page.
var pageBackgrounds = map[string][2]string{
	"terracotta": {"#fff7f2", "#1b0a03"},
	"violet":     {"#f8f6ff", "#0e0b16"},
	"emerald":    {"#f3fcfb", "#00110d"},
	"ocean":      {"#f3fbff", "#010f1a"},
	"forest":     {"#f5fbfb", "#070f0e"},
	"rose":       {"#fff6f8", "#19070c"},
	"graphite":   {"#f7fafe", "#0c0d11"},
	"sapphire":   {"#f4f7ff", "#060b1c"},
	"bordeaux":   {"#fdf6f8", "#160709"},
}

// waveBackgrounds are each accent's --bg in light and dark mode in the wave
// style (see style.css).
var waveBackgrounds = map[string][2]string{
	"violet":     {"#f6f4fc", "#0d0b16"},
	"terracotta": {"#f9f3f0", "#140b08"},
	"emerald":    {"#f0f5f4", "#04110e"},
	"ocean":      {"#f0f5f7", "#040f16"},
	"forest":     {"#f2f5f3", "#0c100f"},
	"rose":       {"#f9f0f2", "#14090c"},
	"graphite":   {"#f1f1f2", "#121315"},
	"sapphire":   {"#f1f3fa", "#080d16"},
	"bordeaux":   {"#f6f1f3", "#120c10"},
}

// backgrounds are the accent's page backgrounds, light and dark, in a style.
func backgrounds(accent, style string) [2]string {
	if style == "wave" {
		return waveBackgrounds[accent]
	}
	return pageBackgrounds[accent]
}

// themeColor is one <meta name="theme-color">; Media is empty when it
// applies whatever the colour scheme.
type themeColor struct{ Color, Media string }

// themeColors follows the theme setting: one colour for light or dark, both
// with a media query for system.
func themeColors(accent, theme, style string) []themeColor {
	c := backgrounds(accent, style)
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

// manifestURL has no asset version: the manifest is built in Go, so it can
// change without any static file changing. It's revalidated instead.
func manifestURL(accent, lang, theme, style string) string {
	return "/app.webmanifest?" + url.Values{"accent": {accent}, "lang": {lang}, "theme": {theme}, "style": {style}}.Encode()
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

// manifest serves /app.webmanifest?accent=…&lang=…&theme=…&style=…, public like the
// favicon. Chrome on Android colours an installed app's navigation bar with
// theme_color, fixed at install, so it can't follow the phone's dark mode.
// The light theme gets the light background; dark and system get the dark
// one (leaving the colours out for system gave a white bar, not the phone's).
func (s *Server) manifest(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	accent := q.Get("accent")
	if _, ok := iconColours[accent]; !ok {
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
		"id":          "/",
		"start_url":   "/",
		"scope":       "/",
		"display":     "standalone",
		"name":        "Anchor",
		"short_name":  "Anchor",
		"description": lang.T("Your bookmarks, with tags, search and shared spaces."),
		"lang":        string(lang),
		"icons":       icons,
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
	bgs := backgrounds(accent, q.Get("style"))
	bg := bgs[1]
	if q.Get("theme") == "light" {
		bg = bgs[0]
	}
	m["theme_color"], m["background_color"] = bg, bg
	w.Header().Set("Content-Type", "application/manifest+json")
	w.Header().Set("Cache-Control", "no-cache")
	json.NewEncoder(w).Encode(m)
}

// precache is what the service worker stores on install: the versioned CSS
// and scripts, the fonts and the wave style's halftone (style.css asks for
// them unversioned), the signed-out favicon and the offline page. The cache
// is renamed on every deploy, so they're fetched again then.
var precache = func() []string {
	list := []string{"/offline", appIconURL(signedOutPrefs.Accent)}
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
		case ".svg":
			if name == "halftone.svg" {
				list = append(list, "/static/"+name)
			}
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
