package store

import (
	"crypto/sha256"
	"net/url"
	"strings"
)

var ErrInvalidURL = ValidationError{"Enter a valid http or https URL."}

// NormalizeURL cleans up a user-supplied URL so equivalent spellings dedupe to
// the same hash, and returns it together with its lower-cased host. Only http
// and https are accepted; a missing scheme means https.
func NormalizeURL(raw string) (normalized, domain string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", ErrInvalidURL
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", ErrInvalidURL
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", "", ErrInvalidURL
	}
	host := strings.ToLower(u.Hostname())
	if host == "" || strings.ContainsAny(host, " \t") {
		return "", "", ErrInvalidURL
	}
	port := u.Port()
	if (u.Scheme == "http" && port == "80") || (u.Scheme == "https" && port == "443") {
		port = ""
	}
	if strings.Contains(host, ":") { // IPv6 literal
		u.Host = "[" + host + "]"
	} else {
		u.Host = host
	}
	if port != "" {
		u.Host += ":" + port
	}
	if u.Path == "" {
		u.Path = "/"
	}
	return u.String(), strings.TrimSuffix(host, "."), nil
}

func urlHash(normalized string) []byte {
	h := sha256.Sum256([]byte(normalized))
	return h[:]
}
