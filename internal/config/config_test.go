package config

import (
	"strings"
	"testing"
)

func TestOIDC(t *testing.T) {
	base := map[string]string{"ANCHOR_BASE_URL": "https://anchor.example", "ANCHOR_SECRET_KEY": "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="}
	for name, c := range map[string]struct {
		env     map[string]string
		wantErr string
		check   func(*Config) bool
	}{
		"off": {nil, "", func(c *Config) bool { return c.OIDC == nil }},
		"defaults": {map[string]string{"ANCHOR_OIDC_ISSUER": "https://auth.example/o/anchor/", "ANCHOR_OIDC_CLIENT_ID": "anchor"}, "", func(c *Config) bool {
			return c.OIDC.Name == "SSO" && c.OIDC.GroupsClaim == "groups" && !c.OIDC.Only &&
				strings.Join(c.OIDC.Scopes, " ") == "openid profile email"
		}},
		"scopes": {map[string]string{"ANCHOR_OIDC_ISSUER": "https://auth.example", "ANCHOR_OIDC_CLIENT_ID": "anchor", "ANCHOR_OIDC_SCOPES": "profile,groups email"}, "", func(c *Config) bool {
			return strings.Join(c.OIDC.Scopes, " ") == "openid profile groups email"
		}},
		"admin group": {map[string]string{"ANCHOR_OIDC_ISSUER": "https://auth.example", "ANCHOR_OIDC_CLIENT_ID": "anchor", "ANCHOR_OIDC_ADMIN_GROUP": "admins"}, "", func(c *Config) bool {
			return strings.Join(c.OIDC.Scopes, " ") == "openid profile email offline_access"
		}},
		"admin group, own offline_access": {map[string]string{"ANCHOR_OIDC_ISSUER": "https://auth.example", "ANCHOR_OIDC_CLIENT_ID": "anchor", "ANCHOR_OIDC_ADMIN_GROUP": "admins", "ANCHOR_OIDC_SCOPES": "offline_access profile"}, "", func(c *Config) bool {
			return strings.Join(c.OIDC.Scopes, " ") == "openid offline_access profile"
		}},
		"only":          {map[string]string{"ANCHOR_OIDC_ISSUER": "https://auth.example", "ANCHOR_OIDC_CLIENT_ID": "anchor", "ANCHOR_OIDC_ONLY": "true"}, "", func(c *Config) bool { return c.OIDC.Only }},
		"no client id":  {map[string]string{"ANCHOR_OIDC_ISSUER": "https://auth.example"}, "ANCHOR_OIDC_CLIENT_ID", nil},
		"bad issuer":    {map[string]string{"ANCHOR_OIDC_ISSUER": "auth.example", "ANCHOR_OIDC_CLIENT_ID": "anchor"}, "ANCHOR_OIDC_ISSUER", nil},
		"only, no oidc": {map[string]string{"ANCHOR_OIDC_ONLY": "true"}, "ANCHOR_OIDC_ONLY needs", nil},
	} {
		t.Run(name, func(t *testing.T) {
			for k, v := range base {
				t.Setenv(k, v)
			}
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			cfg, err := Load(true)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want %q", err, c.wantErr)
				}
				return
			}
			if err != nil || !c.check(cfg) {
				t.Fatalf("err %v, config %+v", err, cfg.OIDC)
			}
		})
	}
}
