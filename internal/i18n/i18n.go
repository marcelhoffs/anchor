// Package i18n translates Anchor's web interface. A message is keyed by its
// English text, so English needs no catalog; every other language has one in
// locales/<code>.json. A plural is keyed "one|other" (the English forms) and
// translated as a [one, other] pair.
package i18n

import (
	"embed"
	"encoding/json"
	"fmt"
	"path"
	"strconv"
	"strings"
)

// Lang is a language Anchor speaks: a lower-case ISO 639-1 code.
type Lang string

const English Lang = "en"

// Langs lists the languages, English (the fallback) first.
var Langs = []Lang{English, "nl", "es", "fr"}

// names are each language's name in that language, for the picker.
var names = map[Lang]string{"en": "English", "nl": "Nederlands", "es": "Español", "fr": "Français"}

// Name is the language's name in itself: "Nederlands", "Français".
func (l Lang) Name() string { return names[l] }

//go:embed locales/*.json
var files embed.FS

// entry is one translation; other is set only for a plural.
type entry struct{ one, other string }

func (e *entry) UnmarshalJSON(b []byte) error {
	if err := json.Unmarshal(b, &e.one); err == nil {
		return nil
	}
	var pair [2]string
	if err := json.Unmarshal(b, &pair); err != nil {
		return fmt.Errorf("want a string or a [one, other] pair: %w", err)
	}
	e.one, e.other = pair[0], pair[1]
	return nil
}

var catalogs = func() map[Lang]map[string]entry {
	out := map[Lang]map[string]entry{}
	for _, l := range Langs[1:] {
		b, err := files.ReadFile(path.Join("locales", string(l)+".json"))
		if err != nil {
			panic(err)
		}
		c := map[string]entry{}
		if err := json.Unmarshal(b, &c); err != nil {
			panic(fmt.Sprintf("locales/%s.json: %v", l, err))
		}
		out[l] = c
	}
	return out
}()

// Keys returns the keys of a language's catalog, for tests.
func Keys(l Lang) map[string]bool {
	out := map[string]bool{}
	for k := range catalogs[l] {
		out[k] = true
	}
	return out
}

// Plural reports whether a catalog key is a plural and, if so, its English forms.
func Plural(key string) (one, other string, ok bool) {
	return strings.Cut(key, "|")
}

// IsPlural reports whether l's translation of key is a [one, other] pair.
func (l Lang) IsPlural(key string) bool { return catalogs[l][key].other != "" }

// Text is the translation of format into l, or format itself without one.
func (l Lang) Text(format string) string {
	if e, ok := catalogs[l][format]; ok && e.one != "" {
		return e.one
	}
	return format
}

// Form is the translated plural form for n: one and other are the English.
func (l Lang) Form(n int, one, other string) string {
	if e, ok := catalogs[l][one+"|"+other]; ok && e.other != "" {
		one, other = e.one, e.other
	}
	if l.isOne(n) {
		return one
	}
	return other
}

// T translates format into l and fills in args, like fmt.Sprintf.
func (l Lang) T(format string, args ...any) string {
	if len(args) == 0 {
		return l.Text(format)
	}
	return fmt.Sprintf(l.Text(format), args...)
}

// N translates the plural for n. With no args, n fills in the form's verb.
func (l Lang) N(n int, one, other string, args ...any) string {
	if len(args) == 0 {
		args = []any{n}
	}
	return fmt.Sprintf(l.Form(n, one, other), args...)
}

// isOne is the plural rule: French counts 0 as singular too.
func (l Lang) isOne(n int) bool {
	if l == "fr" {
		return n == 0 || n == 1
	}
	return n == 1
}

// Parse returns the language for a code like "nl", or false.
func Parse(code string) (Lang, bool) {
	for _, l := range Langs {
		if string(l) == code {
			return l, true
		}
	}
	return "", false
}

// Match picks the best language for an Accept-Language header, or English.
func Match(header string) Lang {
	best, bestQ := English, 0.0
	for _, part := range strings.Split(header, ",") {
		tag, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		q := 1.0
		if v, ok := strings.CutPrefix(strings.TrimSpace(params), "q="); ok {
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				q = f
			}
		}
		base, _, _ := strings.Cut(strings.ToLower(tag), "-")
		if l, ok := Parse(base); ok && q > bestQ {
			best, bestQ = l, q
		}
	}
	return best
}

// Msg is a message to translate once the reader's language is known: a
// handler builds one, the page renders it.
type Msg struct {
	Format string
	Args   []any
}

// M makes a Msg. Messages are found for translation by this call, so its
// format must be a string literal.
func M(format string, args ...any) Msg { return Msg{format, args} }

// In translates m into l; the zero Msg is "".
func (m Msg) In(l Lang) string {
	if m.Format == "" {
		return ""
	}
	return l.T(m.Format, m.Args...)
}

// Raw is a Msg shown as it is: a name, not a message to translate.
func Raw(s string) Msg { return Msg{"%s", []any{s}} }
