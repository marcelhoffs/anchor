package web

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/marcelhoffs/anchor/internal/auth"
	"github.com/marcelhoffs/anchor/internal/i18n"
	"github.com/marcelhoffs/anchor/internal/store"
)

type ctxKey int

const (
	userKey ctxKey = iota
	sessionKey
	csrfKey
	scopeKey
)

func currentUser(r *http.Request) *store.User {
	u, _ := r.Context().Value(userKey).(*store.User)
	return u
}

func currentSession(r *http.Request) *store.Session {
	ss, _ := r.Context().Value(sessionKey).(*store.Session)
	return ss
}

// spaceNav is the space the user is looking at, and the shared spaces the
// top bar's switcher offers (it shows only when there are any).
type spaceNav struct {
	Scope  store.Scope
	Name   string // the shared space's name; "" for the personal space
	Spaces []store.Space
}

// label is the space's name for a page: Personal is translated.
func (n *spaceNav) label() i18n.Msg {
	if n.Name == "" {
		return i18n.M("Personal")
	}
	return i18n.Raw(n.Name)
}

func currentNav(r *http.Request) *spaceNav {
	n, _ := r.Context().Value(scopeKey).(*spaceNav)
	return n
}

// currentScope is the signed-in user's access to the space they're looking
// at. Pages about one bookmark use that bookmark's space instead (see
// bookmarkScope), so a link to it works whichever space is selected.
func currentScope(r *http.Request) store.Scope {
	if n := currentNav(r); n != nil {
		return n.Scope
	}
	return store.Scope{}
}

// loadScope puts the user's current space on the request, after
// requireUser: the one the session selected while they're still a member,
// else their personal space.
func (s *Server) loadScope(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, u := r.Context(), currentUser(r)
		spaces, err := s.store.ListSpaces(ctx, u.ID)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		nav := &spaceNav{Spaces: spaces}
		if id := currentSession(r).SpaceID; id != 0 {
			if sc, err := s.store.Access(ctx, u.ID, id); err == nil {
				for _, sp := range spaces {
					if sp.ID == id {
						nav.Scope, nav.Name = sc, sp.Name
					}
				}
			}
		}
		if nav.Scope.SpaceID() == 0 {
			if nav.Scope, err = s.store.PersonalScope(ctx, u.ID); err != nil {
				s.serverError(w, r, err)
				return
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, scopeKey, nav)))
	})
}

func csrfToken(r *http.Request) string {
	t, _ := r.Context().Value(csrfKey).(string)
	return t
}

const csp = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; object-src 'none'; " +
	"base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	https := s.cfg.SecureCookies()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=(), browsing-topics=()")
		if https {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
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
// random.HMAC(random, session cookie), and every state-changing request must
// echo the same value in its "csrf" form field, or (for JSON calls from our
// own scripts) in an X-CSRF-Token header. Binding it to the session means a
// token another host under the same domain plants (possible without the
// __Host- prefix, over http) isn't valid for the victim's session; signing
// in or out makes the next page load issue a new one.
func (s *Server) csrf(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session := ""
		if c, err := r.Cookie(s.sessionCookie); err == nil {
			session = c.Value
		}
		token := ""
		if c, err := r.Cookie(s.csrfCookie); err == nil && s.validCSRF(c.Value, session) {
			token = c.Value
		}
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			if token == "" {
				token = s.newCSRF(session)
				s.setCSRFCookie(w, token)
			}
		default:
			sent := r.Header.Get("X-CSRF-Token")
			if sent == "" {
				sent = r.PostFormValue("csrf")
			}
			if token == "" || subtle.ConstantTimeCompare([]byte(sent), []byte(token)) != 1 {
				http.Error(w, langOf(r).T("Invalid or missing CSRF token. Reload the page and try again."), http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), csrfKey, token)))
	})
}

func (s *Server) setCSRFCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name: s.csrfCookie, Value: token, Path: "/", HttpOnly: true,
		Secure: s.cfg.SecureCookies(), SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) newCSRF(session string) string {
	b := make([]byte, 32)
	rand.Read(b)
	raw := base64.RawURLEncoding.EncodeToString(b)
	return raw + "." + s.csrfSig(raw, session)
}

func (s *Server) validCSRF(v, session string) bool {
	raw, sig, ok := strings.Cut(v, ".")
	return ok && hmac.Equal([]byte(sig), []byte(s.csrfSig(raw, session)))
}

func (s *Server) csrfSig(raw, session string) string {
	m := hmac.New(sha256.New, s.csrfKey)
	m.Write([]byte(raw + "\x00" + session))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// loadSession attaches the logged-in user, if any, to the request.
func (s *Server) loadSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(s.sessionCookie)
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
			expires := now.Add(sessionTTL)
			if end := ss.CreatedAt.Add(sessionMaxAge); end.Before(expires) {
				expires = end
			}
			if err := s.store.TouchSession(r.Context(), ss.TokenHash, now, expires); err != nil {
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
		Name: s.sessionCookie, Value: token, Path: "/", MaxAge: int(sessionTTL.Seconds()),
		HttpOnly: true, Secure: s.cfg.SecureCookies(), SameSite: http.SameSiteLaxMode,
	})
}

// clearSessionCookie signs the browser out, with a CSRF token to match.
func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: s.sessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: s.cfg.SecureCookies(), SameSite: http.SameSiteLaxMode,
	})
	s.setCSRFCookie(w, s.newCSRF(""))
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
			} else if r.URL.Path != "/setup" && !strings.HasPrefix(r.URL.Path, "/login/oidc") {
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
				http.Error(w, langOf(r).T("Not logged in"), http.StatusUnauthorized)
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
// password check and is waiting for its second factor, and r set to answer
// in that user's language.
func (s *Server) pendingLogin(r *http.Request) (*http.Request, store.Session, store.User, bool) {
	c, err := r.Cookie(s.sessionCookie)
	if err != nil {
		return r, store.Session{}, store.User{}, false
	}
	ss, u, err := s.store.SessionByHash(r.Context(), auth.HashToken(c.Value))
	if err != nil || ss.MFAPassed {
		return r, store.Session{}, store.User{}, false
	}
	return withUserLang(r, u), ss, u, true
}

// requireAdmin lets only admins in (see isAdmin); anyone else gets the
// same not-found page as for any page that isn't theirs. With an admin
// group, a session signed in through the provider asks it on every request
// here, and is refused when the provider can't answer.
func (s *Server) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.groupAdmins() && currentSession(r).SSO {
			switch err := s.checkGroup(r); {
			case errors.Is(err, errSSOEnded):
				s.endSSOSession(w, r)
				return
			case err != nil:
				s.oidc.checkFailed.Store(time.Now().UnixNano())
				s.log.Warn("oidc: checking the admin group", "user", currentUser(r).Username, "err", err)
				http.Error(w, langOf(r).T("%s couldn't confirm your admin rights. Try again in a moment.", s.cfg.OIDC.Name),
					http.StatusServiceUnavailable)
				return
			}
		}
		if !s.isAdmin(r) {
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
		for _, prefix := range []string{"/settings/security", "/settings/totp", "/settings/passkeys", "/settings/recovery"} {
			if strings.HasPrefix(p, prefix) {
				return true
			}
		}
		return false
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A sign-in through the provider had its second factor there.
		if allowed(r.URL.Path) || currentSession(r).SSO || !s.require2FA(r.Context()) {
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
