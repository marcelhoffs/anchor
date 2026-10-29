// Package websearch knows the web search engines the search box can hand a
// query to, and the regions they can be asked to search in.
package websearch

import (
	"net/url"
	"strings"

	"golang.org/x/text/language"
)

// Engine is a web search engine. Names are brands, so they aren't translated.
type Engine struct{ ID, Name string }

// Engines are sorted by name; the first is the default.
var Engines = []Engine{
	{"duckduckgo", "DuckDuckGo"},
	{"ecosia", "Ecosia"},
	{"google", "Google"},
	{"qwant", "Qwant"},
}

// Origins are the engines' sites, for the Content-Security-Policy.
var Origins = []string{"https://duckduckgo.com", "https://www.ecosia.org", "https://www.google.com", "https://www.qwant.com"}

// Locale is a region to search in, named in its own language.
type Locale struct{ Code, Name string }

// Locales are the regions the engines all know, as language-COUNTRY.
var Locales = []Locale{
	{"da-DK", "Dansk (Danmark)"},
	{"de-AT", "Deutsch (Österreich)"},
	{"de-CH", "Deutsch (Schweiz)"},
	{"de-DE", "Deutsch (Deutschland)"},
	{"en-AU", "English (Australia)"},
	{"en-CA", "English (Canada)"},
	{"en-GB", "English (United Kingdom)"},
	{"en-IE", "English (Ireland)"},
	{"en-IN", "English (India)"},
	{"en-NZ", "English (New Zealand)"},
	{"en-US", "English (United States)"},
	{"es-AR", "Español (Argentina)"},
	{"es-ES", "Español (España)"},
	{"es-MX", "Español (México)"},
	{"fr-BE", "Français (Belgique)"},
	{"fr-CA", "Français (Canada)"},
	{"fr-CH", "Français (Suisse)"},
	{"fr-FR", "Français (France)"},
	{"it-CH", "Italiano (Svizzera)"},
	{"it-IT", "Italiano (Italia)"},
	{"nb-NO", "Norsk (Norge)"},
	{"nl-BE", "Nederlands (België)"},
	{"nl-NL", "Nederlands (Nederland)"},
	{"pl-PL", "Polski (Polska)"},
	{"pt-BR", "Português (Brasil)"},
	{"pt-PT", "Português (Portugal)"},
	{"fi-FI", "Suomi (Suomi)"},
	{"sv-SE", "Svenska (Sverige)"},
}

// Lookup returns the engine with id.
func Lookup(id string) (Engine, bool) {
	for _, e := range Engines {
		if e.ID == id {
			return e, true
		}
	}
	return Engine{}, false
}

// URL is the engine's results page for q, in locale (a Locales code, or ""
// to leave it to the engine) and with explicit results hidden if safe. The
// query comes last, so URL(e, "", …) is a prefix to append an encoded query to.
// Ecosia keeps region and SafeSearch in its own settings, not in the link.
func URL(engine, q, locale string, safe bool) string {
	lang, country, _ := strings.Cut(locale, "-")
	var base string
	var params [][2]string
	add := func(k, v string) { params = append(params, [2]string{k, v}) }
	switch engine {
	case "duckduckgo":
		base = "https://duckduckgo.com/"
		if locale != "" {
			// DuckDuckGo's regions are country-language, with its own codes
			// for the UK and Norwegian.
			region := strings.ToLower(country) + "-" + lang
			region = strings.NewReplacer("gb-", "uk-", "-nb", "-no").Replace(region)
			add("kl", region)
		}
		add("kp", map[bool]string{true: "1", false: "-2"}[safe])
	case "ecosia":
		base = "https://www.ecosia.org/search"
	case "google":
		base = "https://www.google.com/search"
		if locale != "" {
			add("hl", lang)
			add("gl", country)
		}
		add("safe", map[bool]string{true: "active", false: "off"}[safe])
	case "qwant":
		base = "https://www.qwant.com/"
		if locale != "" {
			add("locale", lang+"_"+country)
		}
		add("s", map[bool]string{true: "2", false: "0"}[safe])
	default:
		return ""
	}
	var b strings.Builder
	b.WriteString(base + "?")
	for _, p := range params {
		b.WriteString(url.QueryEscape(p[0]) + "=" + url.QueryEscape(p[1]) + "&")
	}
	b.WriteString("q=" + url.QueryEscape(q))
	return b.String()
}

// Match picks the Locales code for a browser's Accept-Language header: its
// first language that has one, in the region asked for or else the
// language's most likely one. It returns "" if none fits.
func Match(acceptLanguage string) string {
	tags, _, _ := language.ParseAcceptLanguage(acceptLanguage)
	has := func(code string) bool {
		for _, l := range Locales {
			if l.Code == code {
				return true
			}
		}
		return false
	}
	for _, t := range tags {
		base, _ := t.Base()
		region, _ := t.Region() // guessed when the tag has none
		if code := base.String() + "-" + region.String(); has(code) {
			return code
		}
		if likely, _ := language.Make(base.String()).Region(); has(base.String() + "-" + likely.String()) {
			return base.String() + "-" + likely.String()
		}
		for _, l := range Locales {
			if strings.HasPrefix(l.Code, base.String()+"-") {
				return l.Code
			}
		}
	}
	return ""
}
