package web

import (
	"context"
	"errors"
	"fmt"
	"html"
	"html/template"
	"math"
	"net/http"
	"time"

	"github.com/marcelhoffs/anchor/internal/auth"
	"github.com/marcelhoffs/anchor/internal/i18n"
	"github.com/marcelhoffs/anchor/internal/store"
)

type langKey struct{}

// withUserLang makes r answer in u's chosen language before u is signed in
// all the way: between the password and the second factor.
func withUserLang(r *http.Request, u store.User) *http.Request {
	if l, ok := i18n.Parse(u.Settings.Language); ok {
		return r.WithContext(context.WithValue(r.Context(), langKey{}, l))
	}
	return r
}

// langOf is the language to answer r in: the user's choice, else the
// browser's.
func langOf(r *http.Request) i18n.Lang {
	if l, ok := r.Context().Value(langKey{}).(i18n.Lang); ok {
		return l
	}
	if u := currentUser(r); u != nil {
		if l, ok := i18n.Parse(u.Settings.Language); ok {
			return l
		}
	}
	return i18n.Match(r.Header.Get("Accept-Language"))
}

// errMsg is the message for a validation error from the store or auth,
// translated when the page renders.
func errMsg(err error) i18n.Msg {
	var v store.ValidationError
	if errors.As(err, &v) {
		return i18n.Msg{Format: v.Format, Args: v.Args}
	}
	var in auth.InputError
	if errors.As(err, &in) {
		return i18n.Msg{Format: in.Format, Args: in.Args}
	}
	return i18n.Msg{Format: err.Error()}
}

// retryText is how long until a rate limit lets someone try again.
func retryText(l i18n.Lang, d time.Duration) string {
	m := int(math.Ceil(d.Minutes()))
	if m <= 1 {
		return l.T("a minute")
	}
	return l.N(m, "%d minute", "%d minutes")
}

// langFuncs are the template functions that speak l. Pages are parsed once
// with English ones, then cloned per language with these.
func langFuncs(l i18n.Lang) template.FuncMap {
	return template.FuncMap{
		"lang": func() string { return string(l) },
		// t translates its literal format and fills in args. The result is
		// HTML: the translation is escaped, string args too, and template.HTML
		// args (strong, code) go in as they are.
		"t": func(format string, args ...any) template.HTML {
			s := html.EscapeString(l.Text(format))
			if len(args) > 0 {
				s = fmt.Sprintf(s, escapeArgs(args)...)
			}
			return template.HTML(s)
		},
		// tn is t for a plural: {{tn .N "%d bookmark" "%d bookmarks"}}, n any
		// integer. With no args, n fills in the verb.
		"tn": func(n any, one, other string, args ...any) (template.HTML, error) {
			count, err := toInt(n)
			if err != nil {
				return "", err
			}
			if len(args) == 0 {
				args = []any{count}
			}
			return template.HTML(fmt.Sprintf(html.EscapeString(l.Form(count, one, other)), escapeArgs(args)...)), nil
		},
		"ago":       func(t time.Time) string { return ago(l, t) },
		"date":      func(t time.Time) string { return dateText(l, t, true) },
		"dayMonth":  func(t time.Time) string { return dateText(l, t, false) },
		"roleLabel": func(r store.Role) string { return roleLabel(l, r) },
	}
}

// toInt is n as an int, for tn: counts come as int or int64.
func toInt(n any) (int, error) {
	switch v := n.(type) {
	case int:
		return v, nil
	case int64:
		return int(v), nil
	}
	return 0, fmt.Errorf("tn: %T is not a count", n)
}

// escapeArgs HTML-escapes t's args: template.HTML stays as it is, and
// counts stay numbers for %d; everything else is escaped text.
func escapeArgs(args []any) []any {
	out := make([]any, len(args))
	for i, a := range args {
		switch v := a.(type) {
		case template.HTML:
			out[i] = string(v)
		case int, int64:
			out[i] = v // for %d
		default:
			out[i] = html.EscapeString(fmt.Sprint(v))
		}
	}
	return out
}

// strong and code wrap a value for t: {{t "%s moved to the Archive." (strong .Title)}}.
// The value is a string, or HTML from t itself.
func strong(v any) template.HTML { return wrap("strong", v) }
func code(v any) template.HTML   { return wrap("code", v) }

func wrap(tag string, v any) template.HTML {
	s, ok := v.(template.HTML)
	if !ok {
		s = template.HTML(html.EscapeString(fmt.Sprint(v)))
	}
	return template.HTML("<"+tag+">") + s + template.HTML("</"+tag+">")
}

func ago(l i18n.Lang, t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return l.T("just now")
	case d < time.Hour:
		return l.N(int(d.Minutes()), "%d minute ago", "%d minutes ago")
	case d < 24*time.Hour:
		return l.N(int(d.Hours()), "%d hour ago", "%d hours ago")
	case d < 30*24*time.Hour:
		return l.N(int(d.Hours()/24), "%d day ago", "%d days ago")
	case d < 365*24*time.Hour:
		return l.N(int(d.Hours()/24/30), "%d month ago", "%d months ago")
	default:
		return l.N(int(d.Hours()/24/365), "%d year ago", "%d years ago")
	}
}

// months are the short month names, January first.
var months = map[i18n.Lang][12]string{
	"en": {"Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"},
	"nl": {"jan", "feb", "mrt", "apr", "mei", "jun", "jul", "aug", "sep", "okt", "nov", "dec"},
	"es": {"ene", "feb", "mar", "abr", "may", "jun", "jul", "ago", "sept", "oct", "nov", "dic"},
	"fr": {"janv.", "févr.", "mars", "avr.", "mai", "juin", "juil.", "août", "sept.", "oct.", "nov.", "déc."},
}

// dateText is "2 Jan 2006", or "2 Jan" without the year, in l.
func dateText(l i18n.Lang, t time.Time, year bool) string {
	s := fmt.Sprintf("%d %s", t.Day(), months[l][t.Month()-1])
	if year {
		s += fmt.Sprintf(" %d", t.Year())
	}
	return s
}

// roleLabel names a space role for people.
func roleLabel(l i18n.Lang, r store.Role) string {
	switch r {
	case store.RoleOwner:
		return l.T("Owner")
	case store.RoleWrite:
		return l.T("Can edit")
	}
	return l.T("Read only")
}
