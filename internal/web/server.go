// Package web serves Burrow's server-rendered HTML interface.
package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"embed"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/marcelhoffs/burrow/internal/config"
	"github.com/marcelhoffs/burrow/internal/store"
)

//go:embed templates static
var assets embed.FS

const (
	sessionCookie = "burrow_session"
	csrfCookie    = "burrow_csrf"
	sessionTTL    = 30 * 24 * time.Hour
	// A session's sliding expiry is pushed forward at most this often, so
	// browsing doesn't turn every page view into a database write.
	sessionTouchEvery = time.Hour
	pageSize          = 50
)

type Server struct {
	cfg      *config.Config
	store    *store.Store
	log      *slog.Logger
	pages    map[string]pageTemplate
	csrfKey  []byte
	hasUsers atomic.Bool
}

func New(cfg *config.Config, st *store.Store, log *slog.Logger) (*Server, error) {
	mac := hmac.New(sha256.New, cfg.SecretKey)
	mac.Write([]byte("burrow csrf v1"))
	s := &Server{cfg: cfg, store: st, log: log, csrfKey: mac.Sum(nil)}
	var err error
	if s.pages, err = loadTemplates(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer, s.logRequests, securityHeaders)

	static, _ := fs.Sub(assets, "static")
	r.Handle("/static/*", http.StripPrefix("/static/", cacheFor(24*time.Hour, http.FileServerFS(static))))
	r.Get("/healthz", s.healthz)

	r.Group(func(r chi.Router) {
		r.Use(s.limitBody, s.csrf, s.loadSession, s.setupGate)

		r.Get("/setup", s.setupForm)
		r.Post("/setup", s.setup)
		r.Get("/login", s.loginForm)
		r.Post("/login", s.login)
		r.Post("/logout", s.logout)

		r.Group(func(r chi.Router) {
			r.Use(s.requireUser)
			r.Get("/", s.list)
			r.Get("/bookmarks/new", s.newBookmark)
			r.Post("/bookmarks", s.createBookmark)
			r.Get("/bookmarks/{id}/edit", s.editBookmark)
			r.Post("/bookmarks/{id}", s.updateBookmark)
			r.Post("/bookmarks/{id}/delete", s.deleteBookmark)
			r.Post("/bookmarks/{id}/flag", s.flagBookmark)
			r.Get("/tags", s.tags)
			r.Post("/tags/{id}/rename", s.renameTag)
			r.Post("/tags/{id}/delete", s.deleteTag)
			r.Get("/settings", s.settings)
			r.Post("/settings/password", s.changePassword)
			r.Post("/settings/sessions/revoke", s.revokeSession)
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

func cacheFor(d time.Duration, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age="+strconv.Itoa(int(d.Seconds())))
		h.ServeHTTP(w, r)
	})
}
