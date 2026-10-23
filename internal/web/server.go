// Package web serves Anchor's server-rendered HTML interface and its REST API.
package web

import (
	"context"
	"embed"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/marcelhoffs/anchor/internal/auth"
	"github.com/marcelhoffs/anchor/internal/config"
	"github.com/marcelhoffs/anchor/internal/ratelimit"
	"github.com/marcelhoffs/anchor/internal/store"
)

//go:embed templates static
var assets embed.FS

const (
	// Cookie names; over https they get the __Host- prefix, which makes the
	// browser insist on Secure, Path=/ and no Domain.
	sessionCookie = "anchor_session"
	csrfCookie    = "anchor_csrf"
	sessionTTL    = 30 * 24 * time.Hour
	// A password-checked session waiting for its second factor lives this long.
	pendingTTL = 5 * time.Minute
	// Sensitive actions (2FA changes, API tokens, admin actions) need the
	// session to have proved itself this recently.
	reauthWindow = 10 * time.Minute
	// A session's sliding expiry is pushed forward at most this often, so
	// browsing doesn't turn every page view into a database write.
	sessionTouchEvery = time.Hour
	pageSize          = 50

	maxFormBody   = 1 << 20  // 1 MiB
	maxImportBody = 32 << 20 // 32 MiB
)

// JobNotifier is told when background work is queued.
type JobNotifier interface{ Notify() }

type Server struct {
	cfg      *config.Config
	store    *store.Store
	log      *slog.Logger
	jobs     JobNotifier
	fetcher  JobNotifier // told when a bookmark needs its page details
	pages    map[string]pageTemplate
	keys     auth.Keys
	csrfKey  []byte
	webauthn *webauthn.WebAuthn // nil when passkeys are unavailable
	cer      ceremonies
	hasUsers atomic.Bool

	sessionCookie, csrfCookie string // see the constants

	// Failed logins per IP+username and per IP; failed second factors per
	// user; requests per API token; signups per IP.
	loginUser, loginIP, mfaUser, apiToken, signupIP *ratelimit.Limiter
}

func New(cfg *config.Config, st *store.Store, log *slog.Logger, jobs, fetcher JobNotifier) (*Server, error) {
	keys := auth.NewKeys(cfg.SecretKey)
	// Passkeys are bound to a domain name. Reached by IP address, Anchor runs
	// without them (TOTP and recovery codes still work).
	wa, err := webauthn.New(&webauthn.Config{
		RPID:          cfg.BaseURL.Hostname(),
		RPDisplayName: "Anchor",
		RPOrigins:     []string{cfg.BaseURL.Scheme + "://" + cfg.BaseURL.Host},
	})
	if err != nil {
		log.Warn("passkeys disabled: ANCHOR_BASE_URL must use a domain name", "base_url", cfg.BaseURL.String(), "err", err)
		wa = nil
	}
	s := &Server{
		cfg: cfg, store: st, log: log, jobs: jobs, fetcher: fetcher, keys: keys, csrfKey: keys.Derive("csrf"), webauthn: wa,
		cer:       ceremonies{m: map[string]ceremony{}},
		loginUser: ratelimit.New(10, 15*time.Minute),
		loginIP:   ratelimit.New(100, 15*time.Minute),
		mfaUser:   ratelimit.New(10, 15*time.Minute),
		apiToken:  ratelimit.New(600, time.Minute),
		signupIP:  ratelimit.New(10, time.Hour),
	}
	s.sessionCookie, s.csrfCookie = sessionCookie, csrfCookie
	if cfg.SecureCookies() {
		s.sessionCookie, s.csrfCookie = "__Host-"+sessionCookie, "__Host-"+csrfCookie
	}
	if s.pages, err = loadTemplates(); err != nil {
		return nil, err
	}
	return s, nil
}

// Run does periodic housekeeping until ctx ends.
func (s *Server) Run(ctx context.Context) {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			for _, l := range []*ratelimit.Limiter{s.loginUser, s.loginIP, s.mfaUser, s.apiToken, s.signupIP} {
				l.Sweep()
			}
			s.cer.sweep()
		}
	}
}

func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer, s.logRequests, s.securityHeaders)

	static, _ := fs.Sub(assets, "static")
	// Only static files are compressed: compressing pages that carry the CSRF
	// token next to text the user typed would allow BREACH-style guessing.
	r.With(middleware.Compress(5, "text/css", "text/javascript", "application/javascript", "image/svg+xml")).
		Handle("/static/*", http.StripPrefix("/static/", cacheFor(24*time.Hour, http.FileServerFS(static))))
	r.Get("/healthz", s.healthz)
	r.Get("/icon/{file}", s.appIcon)
	r.Route("/api/v1", s.apiRoutes)

	r.Group(func(r chi.Router) {
		r.Use(s.limitBody, s.csrf, s.loadSession, s.setupGate)

		r.Get("/setup", s.setupForm)
		r.Post("/setup", s.setup)
		r.Get("/login", s.loginForm)
		r.Post("/login", s.login)
		r.Post("/logout", s.logout)
		r.Get("/signup", s.signupForm)
		r.Post("/signup", s.signup)

		// Second factor after a password: these use the pending session.
		r.Get("/login/2fa", s.login2FAForm)
		r.Post("/login/2fa/totp", s.login2FATOTP)
		r.Post("/login/2fa/recovery", s.login2FARecovery)
		r.Post("/login/2fa/passkey/begin", s.login2FAPasskeyBegin)
		r.Post("/login/2fa/passkey/finish", s.login2FAPasskeyFinish)

		// A shared bookmark's public page.
		r.Get("/s/{token}", s.sharedBookmark)

		// Site icons, one request per bookmark row: signed in is all they
		// need, not the spaces and 2FA checks pages do.
		r.With(s.requireUser).Get("/favicons/{id}", s.favicon)

		r.Group(func(r chi.Router) {
			r.Use(s.requireUser, s.enforce2FA, s.loadScope)
			r.Get("/", s.list)
			r.Get("/bookmarks/new", s.newBookmark)
			r.Post("/bookmarks", s.createBookmark)
			r.Get("/bookmarks/{id}/edit", s.editBookmark)
			r.Post("/bookmarks/{id}", s.updateBookmark)
			r.Post("/bookmarks/{id}/delete", s.deleteBookmark)
			r.Post("/bookmarks/{id}/flag", s.flagBookmark)
			r.Post("/bookmarks/{id}/share", s.shareBookmark)
			r.Post("/bookmarks/{id}/refetch", s.refetchBookmark)
			r.Get("/bundles", s.bundles)
			r.Post("/bundles", s.createBundle)
			r.Post("/bundles/{id}", s.updateBundle)
			r.Post("/bundles/{id}/delete", s.deleteBundle)
			r.Get("/tags", s.tags)
			r.Post("/tags", s.createTag)
			r.Post("/tags/{id}/rename", s.renameTag)
			r.Post("/tags/{id}/delete", s.deleteTag)

			r.Get("/settings", s.settings)
			r.Post("/settings/password", s.changePassword)
			r.Post("/settings/appearance", s.saveAppearance)
			r.Get("/settings/security", s.security)
			r.Post("/settings/sessions/revoke", s.revokeSession)
			r.Get("/settings/tokens", s.tokens)
			r.Post("/settings/tokens/{id}/delete", s.deleteToken)
			r.Get("/settings/data", s.dataPage)
			r.Post("/settings/import", s.importFile)
			r.Get("/settings/export", s.exportFile)

			r.Get("/reauth", s.reauthForm)
			r.Post("/reauth/password", s.reauthPassword)
			r.Post("/reauth/totp", s.reauthTOTP)
			r.Post("/reauth/recovery", s.reauthRecovery)
			r.Post("/reauth/passkey/begin", s.reauthPasskeyBegin)
			r.Post("/reauth/passkey/finish", s.reauthPasskeyFinish)

			// Changing how you sign in, or minting credentials, needs a recent
			// password or second-factor check.
			r.Group(func(r chi.Router) {
				r.Use(s.requireRecentAuth)
				r.Post("/settings/totp/start", s.totpStart)
				r.Post("/settings/totp/enable", s.totpEnable)
				r.Post("/settings/totp/disable", s.totpDisable)
				r.Post("/settings/passkeys/begin", s.passkeyRegisterBegin)
				r.Post("/settings/passkeys/finish", s.passkeyRegisterFinish)
				r.Post("/settings/passkeys/{id}/delete", s.passkeyDelete)
				r.Post("/settings/recovery", s.recoveryRegenerate)
				r.Post("/settings/tokens", s.createToken)
				// Handing a space over, or deleting it with everything in it.
				r.Post("/spaces/{id}/members/{uid}/owner", s.transferSpace)
				r.Post("/spaces/{id}/delete", s.deleteSpace)
			})

			r.Post("/space", s.selectSpace)
			r.Get("/spaces", s.spaces)
			r.Post("/spaces", s.createSpace)
			r.Get("/spaces/{id}", s.space)
			r.Post("/spaces/{id}/rename", s.renameSpace)
			r.Post("/spaces/{id}/members", s.addMember)
			r.Get("/spaces/{id}/members/suggest", s.memberSuggestions)
			r.Post("/spaces/{id}/members/{uid}/{action}", s.memberAction)
			r.Post("/spaces/{id}/leave", s.leaveSpace)

			r.Route("/admin", func(r chi.Router) {
				r.Use(s.requireAdmin)
				r.Get("/", s.adminUsers)
				r.Get("/invites", s.adminInvites)
				r.Get("/settings", s.adminSettings)
				r.Get("/spaces", s.adminSpaces)
				r.Group(func(r chi.Router) {
					r.Use(s.requireRecentAuth)
					r.Post("/users", s.adminCreateUser)
					r.Post("/users/{id}/{action}", s.adminUserAction)
					r.Post("/invites", s.adminCreateInvite)
					r.Post("/invites/{id}/delete", s.adminDeleteInvite)
					r.Post("/settings", s.adminSaveSettings)
					r.Post("/spaces/{id}/owner", s.adminSpaceOwner)
					r.Post("/spaces/{id}/delete", s.adminDeleteSpace)
				})
			})
		})
	})
	r.NotFound(s.notFound)
	return r
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Ping(r.Context()); err != nil {
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte("ok\n"))
}

// cacheFor sets a cache lifetime of d, or a year for versioned (?v=) URLs,
// whose content never changes.
func cacheFor(d time.Duration, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("v") != "" {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "public, max-age="+strconv.Itoa(int(d.Seconds())))
		}
		h.ServeHTTP(w, r)
	})
}

// signupMode is the environment's choice if set, else the admin's.
func (s *Server) signupMode(ctx context.Context) string {
	if s.cfg.Signup != "" {
		return s.cfg.Signup
	}
	v, err := s.store.InstanceSetting(ctx, store.SettingSignup, "closed")
	if err != nil {
		s.log.Error("read signup setting", "err", err)
		return "closed"
	}
	return v
}

// require2FA is the environment's choice if set, else the admin's.
func (s *Server) require2FA(ctx context.Context) bool {
	if s.cfg.Require2FA != nil {
		return *s.cfg.Require2FA
	}
	v, err := s.store.InstanceSetting(ctx, store.SettingRequire2FA, "false")
	if err != nil {
		s.log.Error("read 2FA setting", "err", err)
		return false
	}
	return v == "true"
}
