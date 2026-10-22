package web

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/netip"
	"net/url"
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

// limitBody caps request bodies: 1 MiB, or 32 MiB for bookmark imports.
func (s *Server) limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limit := int64(maxFormBody)
		if r.URL.Path == "/settings/import" {
			limit = maxImportBody
		}
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		next.ServeHTTP(w, r)
	})
}

// csrf implements signed double-submit tokens: the cookie holds
// random.HMAC(random), and every state-changing request must echo the same
// value in its "csrf" form field, or (for JSON calls from our own scripts) in
// an X-CSRF-Token header.
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
			sent := r.Header.Get("X-CSRF-Token")
			if sent == "" {
				sent = r.PostFormValue("csrf")
			}
			if token == "" || subtle.ConstantTimeCompare([]byte(sent), []byte(token)) != 1 {
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

// pendingLogin returns the session and user of a login that passed the
// password check and is waiting for its second factor.
func (s *Server) pendingLogin(r *http.Request) (store.Session, store.User, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return store.Session{}, store.User{}, false
	}
	ss, u, err := s.store.SessionByHash(r.Context(), auth.HashToken(c.Value))
	if err != nil || ss.MFAPassed {
		return store.Session{}, store.User{}, false
	}
	return ss, u, true
}

func (s *Server) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !currentUser(r).IsAdmin {
			s.notFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// enforce2FA sends users without a second factor to set one up while the
// instance requires 2FA. Only the pages needed to do that stay reachable.
func (s *Server) enforce2FA(next http.Handler) http.Handler {
	allowed := func(p string) bool {
		for _, prefix := range []string{"/settings/security", "/settings/totp", "/settings/passkeys", "/settings/recovery", "/reauth"} {
			if strings.HasPrefix(p, prefix) {
				return true
			}
		}
		return false
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if allowed(r.URL.Path) || !s.require2FA(r.Context()) {
			next.ServeHTTP(w, r)
			return
		}
		m, err := s.store.MFAStatus(r.Context(), currentUser(r).ID)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		if m.Enabled() {
			next.ServeHTTP(w, r)
			return
		}
		http.Redirect(w, r, "/settings/security?required=1", http.StatusSeeOther)
	})
}

// requireRecentAuth makes sensitive actions ask for the password (or a
// second factor, for users who have one) if the session hasn't proved itself
// in the last reauthWindow.
func (s *Server) requireRecentAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if time.Since(currentSession(r).VerifiedAt) <= reauthWindow {
			next.ServeHTTP(w, r)
			return
		}
		// Come back to the page the form was on; the action itself is re-sent
		// by the user once confirmed.
		back := "/settings/security"
		if ref, err := url.Parse(r.Referer()); err == nil && ref.Host == r.Host && ref.Path != "" {
			back = safeNext(ref.RequestURI())
		}
		to := "/reauth?next=" + url.QueryEscape(back)
		if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"error": "Please confirm it's you first.", "redirect": to})
			return
		}
		http.Redirect(w, r, to, http.StatusSeeOther)
	})
}
