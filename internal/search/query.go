// Package search parses the search box syntax into a structured query.
//
//	kubernetes ingress     full-text, all terms (prefix match)
//	"exact phrase"         phrase match
//	#homelab -#archive     has tag / lacks tag
//	#"home lab"            a tag with spaces (also -#"home lab")
//	site:github.com        domain (and subdomains)
//	!fav !shared           flags; !archived searches the archive instead
//	after:2026-01-01       saved on or after; before: saved before
package search

import (
	"strings"
	"time"
	"unicode"
)

type Query struct {
	Terms    []string // free-text words
	Phrases  []string // quoted phrases
	Tags     []string
	NotTags  []string
	Sites    []string
	Fav      bool
	Shared   bool
	Archived bool
	After    time.Time // zero = unset
	Before   time.Time // zero = unset
}

func Parse(s string) Query {
	var q Query
	for _, tok := range tokenize(s) {
		if tok.quoted {
			if hasWordChar(tok.text) {
				q.Phrases = append(q.Phrases, tok.text)
			}
			continue
		}
		t := tok.text
		lower := strings.ToLower(t)
		switch {
		case strings.HasPrefix(t, "-#") && len(t) > 2:
			q.NotTags = append(q.NotTags, t[2:])
		case strings.HasPrefix(t, "#") && len(t) > 1:
			q.Tags = append(q.Tags, t[1:])
		case lower == "!fav" || lower == "!favourite" || lower == "!favorite":
			q.Fav = true
		case lower == "!shared":
			q.Shared = true
		case lower == "!archived":
			q.Archived = true
		case strings.HasPrefix(lower, "site:") && len(t) > 5:
			if site := cleanSite(lower[5:]); site != "" {
				q.Sites = append(q.Sites, site)
			}
		case strings.HasPrefix(lower, "after:") && parseDate(t[6:]) != nil:
			q.After = *parseDate(t[6:])
		case strings.HasPrefix(lower, "before:") && parseDate(t[7:]) != nil:
			q.Before = *parseDate(t[7:])
		default:
			if hasWordChar(t) {
				q.Terms = append(q.Terms, t)
			}
		}
	}
	return q
}

// FTS returns the FTS5 MATCH expression for the free-text part, or "" when
// there is none. Every term and phrase is quoted so user input can never be
// read as FTS5 operators; bare terms get prefix matching.
func (q Query) FTS() string {
	parts := make([]string, 0, len(q.Terms)+len(q.Phrases))
	for _, t := range q.Terms {
		parts = append(parts, quote(t)+"*")
	}
	for _, p := range q.Phrases {
		parts = append(parts, quote(p))
	}
	return strings.Join(parts, " ")
}

// TagQuery is the search for one tag, quoted when its name has spaces.
func TagQuery(name string) string {
	if strings.ContainsFunc(name, unicode.IsSpace) {
		return `#"` + name + `"`
	}
	return "#" + name
}

func quote(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

type token struct {
	text   string
	quoted bool
}

// tokenize splits on whitespace outside quotes. A quote right after # or -#
// quotes a tag name, which stays one unquoted token: #"home lab".
func tokenize(s string) []token {
	var toks []token
	var b strings.Builder
	inQuote, tagQuote := false, false
	flush := func(quoted bool) {
		if b.Len() > 0 {
			toks = append(toks, token{b.String(), quoted})
			b.Reset()
		}
	}
	for _, r := range s {
		switch {
		case r == '"' && !inQuote && (b.String() == "#" || b.String() == "-#"):
			inQuote, tagQuote = true, true
		case r == '"':
			flush(inQuote && !tagQuote)
			inQuote, tagQuote = !inQuote, false
		case unicode.IsSpace(r) && !inQuote:
			flush(false)
		default:
			b.WriteRune(r)
		}
	}
	flush(inQuote && !tagQuote)
	return toks
}

func hasWordChar(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) >= 0
}

// cleanSite accepts "github.com", "https://github.com/foo" or "www.github.com".
func cleanSite(s string) string {
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimPrefix(s, "www.")
}

func parseDate(s string) *time.Time {
	t, err := time.ParseInLocation("2006-01-02", s, time.UTC)
	if err != nil {
		return nil
	}
	return &t
}
