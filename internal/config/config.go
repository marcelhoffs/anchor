// Package config loads Anchor's settings from ANCHOR_* environment variables.
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"net/url"
	"os"
	"slices"
	"strings"
)

type Config struct {
	BaseURL        *url.URL
	SecretKey      []byte
	DataDir        string
	Listen         string
	TrustedProxies []netip.Prefix
	LogLevel       slog.Level
	AdminUser      string
	AdminPassword  string

	// Signup and Require2FA, when set in the environment, override what an
	// admin chooses in the UI. "" / nil = not set.
	Signup     string
	Require2FA *bool

	// Fetch is which addresses page details may be fetched from: "public"
	// (default), "all" (also private networks) or "off".
	Fetch string
	// IconFallback lists the public icon services asked, in order, for a
	// site's icon when the site gives none: "duckduckgo", "google". Empty
	// (the default) is off. A service learns the bookmarked domain, so it is
	// opt-in.
	IconFallback []string

	// OIDC is single sign-on through an OpenID Connect provider; nil = off.
	OIDC *OIDC
}

// OIDC configures sign-in through an OpenID Connect provider.
type OIDC struct {
	Issuer       string
	ClientID     string
	ClientSecret string // "" for a public client (PKCE only)
	Name         string // the provider's name on the sign-in button
	// AdminGroup, when set, makes members of that group admins and everyone
	// else not, checked at sign-in through the provider and as the session
	// is used.
	AdminGroup  string
	GroupsClaim string   // the ID token claim with the groups
	Scopes      []string // asked for; always includes openid
	// Only hides password sign-in and sign-up; the command line still works.
	Only bool
}

// Load reads the environment. When requireServe is false, the settings only
// the HTTP server needs (base URL, secret key) are optional, so CLI
// subcommands like migrate and backup work without them.
func Load(requireServe bool) (*Config, error) {
	c := &Config{
		DataDir:       env("ANCHOR_DATA_DIR", "/data"),
		Listen:        env("ANCHOR_LISTEN", ":8080"),
		AdminUser:     os.Getenv("ANCHOR_ADMIN_USER"),
		AdminPassword: os.Getenv("ANCHOR_ADMIN_PASSWORD"),
	}
	var errs []error

	switch strings.ToLower(env("ANCHOR_LOG_LEVEL", "info")) {
	case "debug":
		c.LogLevel = slog.LevelDebug
	case "info":
		c.LogLevel = slog.LevelInfo
	case "warn":
		c.LogLevel = slog.LevelWarn
	default:
		errs = append(errs, errors.New("ANCHOR_LOG_LEVEL must be debug, info or warn"))
	}

	for _, s := range strings.FieldsFunc(os.Getenv("ANCHOR_TRUSTED_PROXIES"), func(r rune) bool { return r == ',' || r == ' ' }) {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			errs = append(errs, fmt.Errorf("ANCHOR_TRUSTED_PROXIES: %w", err))
			continue
		}
		c.TrustedProxies = append(c.TrustedProxies, p.Masked())
	}

	switch v := strings.ToLower(os.Getenv("ANCHOR_SIGNUP")); v {
	case "", "closed", "invite", "open":
		c.Signup = v
	default:
		errs = append(errs, errors.New("ANCHOR_SIGNUP must be closed, invite or open"))
	}
	switch v := strings.ToLower(os.Getenv("ANCHOR_REQUIRE_2FA")); v {
	case "":
	case "true", "1", "yes":
		c.Require2FA = new(bool)
		*c.Require2FA = true
	case "false", "0", "no":
		c.Require2FA = new(bool)
	default:
		errs = append(errs, errors.New("ANCHOR_REQUIRE_2FA must be true or false"))
	}

	switch c.Fetch = strings.ToLower(env("ANCHOR_FETCH", "public")); c.Fetch {
	case "public", "all", "off":
	default:
		errs = append(errs, errors.New("ANCHOR_FETCH must be public, all or off"))
	}
	if v := strings.ToLower(strings.TrimSpace(os.Getenv("ANCHOR_FAVICON_FALLBACK"))); v != "" && v != "off" {
		for _, s := range strings.Split(v, ",") {
			switch s = strings.TrimSpace(s); s {
			case "duckduckgo", "google":
				c.IconFallback = append(c.IconFallback, s)
			default:
				errs = append(errs, errors.New("ANCHOR_FAVICON_FALLBACK must be off, or duckduckgo and/or google (comma-separated)"))
			}
		}
	}

	if issuer := strings.TrimSpace(os.Getenv("ANCHOR_OIDC_ISSUER")); issuer != "" {
		c.OIDC = &OIDC{
			Issuer:       issuer,
			ClientID:     os.Getenv("ANCHOR_OIDC_CLIENT_ID"),
			ClientSecret: os.Getenv("ANCHOR_OIDC_CLIENT_SECRET"),
			Name:         env("ANCHOR_OIDC_NAME", "SSO"),
			AdminGroup:   os.Getenv("ANCHOR_OIDC_ADMIN_GROUP"),
			GroupsClaim:  env("ANCHOR_OIDC_GROUPS_CLAIM", "groups"),
			Scopes:       []string{"openid"},
		}
		for _, sc := range strings.FieldsFunc(env("ANCHOR_OIDC_SCOPES", "openid profile email"), func(r rune) bool { return r == ',' || r == ' ' }) {
			if sc != "openid" {
				c.OIDC.Scopes = append(c.OIDC.Scopes, sc)
			}
		}
		// With an admin group, sessions check it again with a refresh token,
		// which some providers (Authentik) only hand out for offline_access.
		if c.OIDC.AdminGroup != "" && !slices.Contains(c.OIDC.Scopes, "offline_access") {
			c.OIDC.Scopes = append(c.OIDC.Scopes, "offline_access")
		}
		if u, err := url.Parse(issuer); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			errs = append(errs, errors.New("ANCHOR_OIDC_ISSUER must be the provider's http(s) issuer URL"))
		}
		if c.OIDC.ClientID == "" {
			errs = append(errs, errors.New("ANCHOR_OIDC_CLIENT_ID is required with ANCHOR_OIDC_ISSUER"))
		}
	}
	switch v := strings.ToLower(os.Getenv("ANCHOR_OIDC_ONLY")); v {
	case "", "false", "0", "no":
	case "true", "1", "yes":
		if c.OIDC == nil {
			errs = append(errs, errors.New("ANCHOR_OIDC_ONLY needs ANCHOR_OIDC_ISSUER"))
		} else {
			c.OIDC.Only = true
		}
	default:
		errs = append(errs, errors.New("ANCHOR_OIDC_ONLY must be true or false"))
	}

	if raw := os.Getenv("ANCHOR_BASE_URL"); raw != "" {
		u, err := url.Parse(strings.TrimRight(raw, "/"))
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, errors.New("ANCHOR_BASE_URL must be an absolute http(s) URL"))
		} else {
			c.BaseURL = u
		}
	} else if requireServe {
		errs = append(errs, errors.New("ANCHOR_BASE_URL is required"))
	}

	if raw := os.Getenv("ANCHOR_SECRET_KEY"); raw != "" {
		key, err := base64.StdEncoding.DecodeString(raw)
		if err != nil || len(key) < 32 {
			errs = append(errs, errors.New("ANCHOR_SECRET_KEY must be at least 32 bytes, base64-encoded (openssl rand -base64 32)"))
		} else {
			c.SecretKey = key
		}
	} else if requireServe {
		errs = append(errs, errors.New("ANCHOR_SECRET_KEY is required"))
	}

	return c, errors.Join(errs...)
}

// SecureCookies reports whether cookies should carry the Secure attribute.
func (c *Config) SecureCookies() bool {
	return c.BaseURL != nil && c.BaseURL.Scheme == "https"
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
