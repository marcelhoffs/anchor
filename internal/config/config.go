// Package config loads Burrow's settings from BURROW_* environment variables.
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"net/url"
	"os"
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
}

// Load reads the environment. When requireServe is false, the settings only
// the HTTP server needs (base URL, secret key) are optional, so CLI
// subcommands like migrate and backup work without them.
func Load(requireServe bool) (*Config, error) {
	c := &Config{
		DataDir:       env("BURROW_DATA_DIR", "/data"),
		Listen:        env("BURROW_LISTEN", ":8080"),
		AdminUser:     os.Getenv("BURROW_ADMIN_USER"),
		AdminPassword: os.Getenv("BURROW_ADMIN_PASSWORD"),
	}
	var errs []error

	switch strings.ToLower(env("BURROW_LOG_LEVEL", "info")) {
	case "debug":
		c.LogLevel = slog.LevelDebug
	case "info":
		c.LogLevel = slog.LevelInfo
	case "warn":
		c.LogLevel = slog.LevelWarn
	default:
		errs = append(errs, errors.New("BURROW_LOG_LEVEL must be debug, info or warn"))
	}

	for _, s := range strings.FieldsFunc(os.Getenv("BURROW_TRUSTED_PROXIES"), func(r rune) bool { return r == ',' || r == ' ' }) {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			errs = append(errs, fmt.Errorf("BURROW_TRUSTED_PROXIES: %w", err))
			continue
		}
		c.TrustedProxies = append(c.TrustedProxies, p.Masked())
	}

	switch v := strings.ToLower(os.Getenv("BURROW_SIGNUP")); v {
	case "", "closed", "invite", "open":
		c.Signup = v
	default:
		errs = append(errs, errors.New("BURROW_SIGNUP must be closed, invite or open"))
	}
	switch v := strings.ToLower(os.Getenv("BURROW_REQUIRE_2FA")); v {
	case "":
	case "true", "1", "yes":
		c.Require2FA = new(bool)
		*c.Require2FA = true
	case "false", "0", "no":
		c.Require2FA = new(bool)
	default:
		errs = append(errs, errors.New("BURROW_REQUIRE_2FA must be true or false"))
	}

	if raw := os.Getenv("BURROW_BASE_URL"); raw != "" {
		u, err := url.Parse(strings.TrimRight(raw, "/"))
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, errors.New("BURROW_BASE_URL must be an absolute http(s) URL"))
		} else {
			c.BaseURL = u
		}
	} else if requireServe {
		errs = append(errs, errors.New("BURROW_BASE_URL is required"))
	}

	if raw := os.Getenv("BURROW_SECRET_KEY"); raw != "" {
		key, err := base64.StdEncoding.DecodeString(raw)
		if err != nil || len(key) < 32 {
			errs = append(errs, errors.New("BURROW_SECRET_KEY must be at least 32 bytes, base64-encoded (openssl rand -base64 32)"))
		} else {
			c.SecretKey = key
		}
	} else if requireServe {
		errs = append(errs, errors.New("BURROW_SECRET_KEY is required"))
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
