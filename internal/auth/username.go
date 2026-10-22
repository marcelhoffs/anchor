package auth

import (
	"errors"
	"regexp"
)

var usernameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

func ValidateUsername(u string) error {
	if !usernameRE.MatchString(u) {
		return errors.New("username must be 1–64 characters: letters, digits, '.', '_' or '-'")
	}
	return nil
}
