package web

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/marcelhoffs/burrow/internal/auth"
	"github.com/marcelhoffs/burrow/internal/store"
)

type ctxKey int

const (
	userKey ctxKey = iota
	sessionKey
	csrfKey
)

func currentUser(r *http.Request) *store.User {
	u, _ := r.Context().Value(userKey).(*store.User)
	return u
}

func currentSession(r *http.Request) *store.Session {
	ss, _ := r.Context().Value(sessionKey).(*store.Session)
	return ss
}

func csrfToken(r *http.Request) string {
	t, _ := r.Context().Value(csrfKey).(string)
	return t
}

const csp = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; object-src 'none'; " +
	"base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		s.log.Debug("request", "method", r.Method, "path", r.URL.Path, "status", ww.Status(),
			"duration", time.Since(start), "ip", s.clientIP(r))
	})
}

// limitBody caps form posts at 1 MiB.
func (s *Server) limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		next.ServeHTTP(w, r)
	})
}

// csrf implements signed double-submit tokens: the cookie holds
// random.HMAC(random), and every state-changing request must echo the same
// value in its "csrf" form field.
func (s *Server) csrf(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := ""
		if c, err := r.Cookie(csrfCookie); err == nil && s.validCSRF(c.Value) {
			token = c.Value
		}
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			if token == "" {
				token = s.newCSRF()
				http.SetCookie(w, &http.Cookie{
					Name: csrfCookie, Value: token, Path: "/", HttpOnly: true,
					Secure: s.cfg.SecureCookies(), SameSite: http.SameSiteLaxMode,
				})
			}
		default:
			form := r.PostFormValue("csrf")
			if token == "" || subtle.ConstantTimeCompare([]byte(form), []byte(token)) != 1 {
				http.Error(w, "Invalid or missing CSRF token. Reload the page and try again.", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), csrfKey, token)))
	})
}

func (s *Server) newCSRF() string {
	b := make([]byte, 32)
	rand.Read(b)
	raw := base64.RawURLEncoding.EncodeToString(b)
	return raw + "." + s.csrfSig(raw)
}

func (s *Server) validCSRF(v string) bool {
	raw, sig, ok := strings.Cut(v, ".")
	return ok && hmac.Equal([]byte(sig), []byte(s.csrfSig(raw)))
}

func (s *Server) csrfSig(raw string) string {
	m := hmac.New(sha256.New, s.csrfKey)
	m.Write([]byte(raw))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// loadSession attaches the logged-in user, if any, to the request.
func (s *Server) loadSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookie)
		if err != nil {
			next.ServeHTTP(w, r)
			return
		}
		ss, u, err := s.store.SessionByHash(r.Context(), auth.HashToken(c.Value))
		if err != nil {
			if err != store.ErrNotFound {
				s.log.Error("load session", "err", err)
			}
			next.ServeHTTP(w, r)
			return
		}
		if !ss.MFAPassed {
			// Half-logged-in session waiting for its second factor; not usable yet.
			next.ServeHTTP(w, r)
			return
		}
		if now := time.Now(); now.Sub(ss.LastSeenAt) > sessionTouchEvery {
			if err := s.store.TouchSession(r.Context(), ss.TokenHash, now, now.Add(sessionTTL)); err != nil {
				s.log.Error("touch session", "err", err)
			} else {
				s.setSessionCookie(w, c.Value)
			}
		}
		ctx := context.WithValue(r.Context(), userKey, &u)
		ctx = context.WithValue(ctx, sessionKey, &ss)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/", MaxAge: int(sessionTTL.Seconds()),
		HttpOnly: true, Secure: s.cfg.SecureCookies(), SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: s.cfg.SecureCookies(), SameSite: http.SameSiteLaxMode,
	})
}

// setupGate sends every request to /setup until the first account exists.
func (s *Server) setupGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.hasUsers.Load() {
			n, err := s.store.CountUsers(r.Context())
			if err != nil {
				s.serverError(w, r, err)
				return
			}
			if n > 0 {
				s.hasUsers.Store(true)
			} else if r.URL.Path != "/setup" {
				http.Redirect(w, r, "/setup", http.StatusSeeOther)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) requireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if currentUser(r) == nil {
			if r.Method != http.MethodGet {
				http.Error(w, "Not logged in", http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, "/login?next="+urlQueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// clientIP returns the peer address, or the right-most untrusted address in
// X-Forwarded-For when the peer is a trusted proxy.
func (s *Server) clientIP(r *http.Request) string {
	peer, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	ip := peer.Addr().Unmap()
	if !s.trusted(ip) {
		return ip.String()
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		hop, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			break
		}
		ip = hop.Unmap()
		if !s.trusted(ip) {
			break
		}
	}
	return ip.String()
}

func (s *Server) trusted(ip netip.Addr) bool {
	for _, p := range s.cfg.TrustedProxies {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}
