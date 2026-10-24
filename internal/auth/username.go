package auth

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

var usernameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

func ValidateUsername(u string) error {
	if !usernameRE.MatchString(u) {
		return inputError("Username must be 1–64 characters: letters, digits, '.', '_' or '-'.")
	}
	return nil
}

// InputError is a problem with what someone typed. Format and Args are kept
// apart so the web interface can translate them; Error is for the command
// line ("password must be at least 12 characters").
type InputError struct {
	Format string
	Args   []any
}

func (e InputError) Error() string {
	s := strings.TrimSuffix(fmt.Sprintf(e.Format, e.Args...), ".")
	r, n := utf8.DecodeRuneInString(s)
	return string(unicode.ToLower(r)) + s[n:]
}

// inputError's format must be a string literal: it is a translation key.
func inputError(format string, args ...any) error { return InputError{format, args} }
