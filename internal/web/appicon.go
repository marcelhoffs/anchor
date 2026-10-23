package web

import (
	"html/template"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

// glyph is the Anchor app icon's mark, a "</>", drawn in currentColor on a
// 100×100 rounded square.
const glyph = `<g fill="none" stroke="currentColor" stroke-width="9" stroke-linecap="round" stroke-linejoin="round">` +
	`<path d="M33 32 L16 50 L33 68"/><path d="M67 32 L84 50 L67 68"/><path d="M56 25 L44 75"/></g>`

// iconColours are each accent's light-mode --accent and --on-accent (see
// style.css): the favicon's background and glyph.
var iconColours = map[string][2]string{
	"indigo":     {"#4f46e5", "#ffffff"},
	"terracotta": {"#c2410c", "#ffffff"},
	"yellow":     {"#eab308", "#1c1917"},
	"emerald":    {"#047857", "#ffffff"},
	"forest":     {"#2d6a4f", "#ffffff"},
	"ocean":      {"#0369a1", "#ffffff"},
	"rose":       {"#be123c", "#ffffff"},
	"graphite":   {"#1e293b", "#ffffff"},
}

// brandMark is the header logo; CSS colours it with the current accent.
func brandMark() template.HTML {
	return template.HTML(`<svg class="brand-mark" viewBox="0 0 100 100" width="32" height="32" aria-hidden="true" focusable="false">` +
		`<rect width="100" height="100" rx="22"/>` + glyph + `</svg>`)
}

// appIconSVG is the favicon in an accent's colours.
func appIconSVG(bg, fg string) string {
	return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 100" color="` + fg + `">` +
		`<rect width="100" height="100" rx="22" fill="` + bg + `"/>` +
		glyph + `</svg>`
}

func appIconURL(accent string) string { return "/icon/" + accent + ".svg?v=" + assetVersion }

// appIcon serves /icon/<accent>.svg, the favicon for that accent.
func (s *Server) appIcon(w http.ResponseWriter, r *http.Request) {
	name, ok := strings.CutSuffix(chi.URLParam(r, "file"), ".svg")
	c, known := iconColours[name]
	if !ok || !known {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Write([]byte(appIconSVG(c[0], c[1])))
}
