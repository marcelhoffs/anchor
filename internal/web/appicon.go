package web

import (
	"html/template"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

// squirrel is the Burrow app icon's glyph (from the icon design), drawn in
// currentColor on a 100×100 rounded square; its eye shows the background.
const squirrel = `<g transform="translate(15.36 11.52) scale(0.8)" fill="currentColor"><g transform="matrix(-1 0 0 1 100 0)">` +
	`<path d="M50 82 C96 86 96 50 74 46 C58 43 52 22 70 14 C80 10 90 16 86 24" fill="none" stroke="currentColor" stroke-width="16" stroke-linecap="round" stroke-linejoin="round"/>` +
	`<ellipse cx="44" cy="66" rx="16" ry="20"/><ellipse cx="33" cy="44" rx="13" ry="11"/><circle cx="23" cy="47" r="6"/>` +
	`<path d="M30 37 L35 25 Q37 22 39 25 L44 35 Z"/><ellipse cx="51" cy="77" rx="13" ry="10"/><ellipse cx="40" cy="88" rx="13" ry="3.5"/>` +
	`<ellipse cx="28" cy="62" rx="5" ry="4"/><circle class="eye" cx="31" cy="41.5" r="2"/></g></g>`

// iconColours are each accent's light-mode --accent and --on-accent (see
// style.css): the favicon's background and squirrel.
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
		`<rect width="100" height="100" rx="22"/>` + squirrel + `</svg>`)
}

// appIconSVG is the favicon in an accent's colours.
func appIconSVG(bg, fg string) string {
	return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 100" color="` + fg + `">` +
		`<rect width="100" height="100" rx="22" fill="` + bg + `"/>` +
		strings.Replace(squirrel, `class="eye"`, `fill="`+bg+`"`, 1) + `</svg>`
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
