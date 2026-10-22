package web

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/marcelhoffs/burrow/internal/auth"
	"github.com/marcelhoffs/burrow/internal/store"
)

const invalidLogin = "Invalid username or password."

func (s *Server) setupForm(w http.ResponseWriter, r *http.Request) {
	if s.hasUsers.Load() {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	s.render(w, r, http.StatusOK, "setup", nil)
}

func (s *Server) setup(w http.ResponseWriter, r *http.Request) {
	if s.hasUsers.Load() {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	username := strings.TrimSpace(r.PostFormValue("username"))
	password := r.PostFormValue("password")
	fail := func(msg string) {
		s.render(w, r, http.StatusUnprocessableEntity, "setup", data{"Error": msg, "Username": username})
	}
	if err := auth.ValidateUsername(username); err != nil {
		fail(err.Error())
		return
	}
	if err := auth.ValidatePassword(password); err != nil {
		fail(err.Error())
		return
	}
	if password != r.PostFormValue("confirm") {
		fail("Passwords do not match.")
		return
	}
	u, err := s.store.CreateFirstAdmin(r.Context(), username, auth.HashPassword(password))
	if errors.Is(err, store.ErrConflict) {
		s.hasUsers.Store(true)
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.hasUsers.Store(true)
	s.log.Info("first admin created", "user", u.Username)
	s.startSession(w, r, u)
}

func (s *Server) loginForm(w http.ResponseWriter, r *http.Request) {
	if currentUser(r) != nil {
		http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusSeeOther)
		return
	}
	s.render(w, r, http.StatusOK, "login", data{"Next": r.URL.Query().Get("next")})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	username := strings.TrimSpace(r.PostFormValue("username"))
	password := r.PostFormValue("password")
	next := r.PostFormValue("next")

	u, err := s.store.UserByUsername(r.Context(), username)
	switch {
	case errors.Is(err, store.ErrNotFound):
		auth.VerifyDummy(password)
	case err != nil:
		s.serverError(w, r, err)
		return
	default:
		if auth.VerifyPassword(u.PasswordHash, password) != nil || !u.IsActive {
			err = auth.ErrMismatch
		}
	}
	if err != nil {
		s.log.Info("login failed", "user", username, "ip", s.clientIP(r))
		s.render(w, r, http.StatusUnauthorized, "login", data{"Error": invalidLogin, "Username": username, "Next": next})
		return
	}
	s.log.Info("login", "user", u.Username, "ip", s.clientIP(r))
	s.startSession(w, r, u)
}

// startSession issues a fresh session token (replacing any the browser already
// had) and redirects to the requested page.
func (s *Server) startSession(w http.ResponseWriter, r *http.Request, u store.User) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		if old, _, err := s.store.SessionByHash(r.Context(), auth.HashToken(c.Value)); err == nil {
			s.store.DeleteSession(r.Context(), old.UserID, old.TokenHash)
		}
	}
	token, hash := auth.NewToken(32)
	now := time.Now()
	err := s.store.CreateSession(r.Context(), store.Session{
		TokenHash: hash,
		UserID:    u.ID,
		MFAPassed: true, // no second factors until M2
		UserAgent: truncate(r.UserAgent(), 256),
		IP:        s.clientIP(r),
		CreatedAt: now, ExpiresAt: now.Add(sessionTTL), LastSeenAt: now,
	})
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.setSessionCookie(w, token)
	http.Redirect(w, r, safeNext(r.PostFormValue("next")), http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if ss := currentSession(r); ss != nil {
		if err := s.store.DeleteSession(r.Context(), ss.UserID, ss.TokenHash); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	s.clearSessionCookie(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) settings(w http.ResponseWriter, r *http.Request) {
	s.renderSettings(w, r, http.StatusOK, data{"Notice": settingsNotices[r.URL.Query().Get("done")]})
}

var settingsNotices = map[string]string{
	"password":   "Password changed. Your other sessions were signed out.",
	"revoked":    "Session revoked.",
	"appearance": "Appearance saved.",
}

type sessionRow struct {
	store.Session
	ID      string
	Current bool
}

func (s *Server) renderSettings(w http.ResponseWriter, r *http.Request, status int, d data) {
	u, cur := currentUser(r), currentSession(r)
	list, err := s.store.ListSessions(r.Context(), u.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	rows := make([]sessionRow, len(list))
	for i, ss := range list {
		rows[i] = sessionRow{ss, hex.EncodeToString(ss.TokenHash), bytes.Equal(ss.TokenHash, cur.TokenHash)}
	}
	d["Sessions"] = rows
	d["MinPassword"] = auth.MinPasswordLen
	d["Bookmarklet"] = s.bookmarklet()
	s.render(w, r, status, "settings", d)
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	u, cur := currentUser(r), currentSession(r)
	fail := func(msg string) { s.renderSettings(w, r, http.StatusUnprocessableEntity, data{"Error": msg}) }
	if auth.VerifyPassword(u.PasswordHash, r.PostFormValue("current")) != nil {
		fail("Current password is wrong.")
		return
	}
	password := r.PostFormValue("password")
	if err := auth.ValidatePassword(password); err != nil {
		fail(err.Error())
		return
	}
	if password != r.PostFormValue("confirm") {
		fail("New passwords do not match.")
		return
	}
	if err := s.store.SetPassword(r.Context(), u.ID, auth.HashPassword(password), cur.TokenHash); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.log.Info("password changed", "user", u.Username, "ip", s.clientIP(r))
	http.Redirect(w, r, "/settings?done=password", http.StatusSeeOther)
}

func (s *Server) saveAppearance(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	err := s.store.SaveSettings(r.Context(), u.ID, store.Settings{
		Accent: r.PostFormValue("accent"),
		Theme:  r.PostFormValue("theme"),
	})
	if isValidation(err) {
		s.renderSettings(w, r, http.StatusUnprocessableEntity, data{"Error": err.Error()})
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/settings?done=appearance", http.StatusSeeOther)
}

func (s *Server) revokeSession(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	hash, err := hex.DecodeString(r.PostFormValue("id"))
	if err != nil {
		http.Error(w, "Bad session id", http.StatusBadRequest)
		return
	}
	if err := s.store.DeleteSession(r.Context(), u.ID, hash); err != nil {
		s.serverError(w, r, err)
		return
	}
	if bytes.Equal(hash, currentSession(r).TokenHash) {
		s.clearSessionCookie(w)
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/settings?done=revoked", http.StatusSeeOther)
}

// bookmarklet returns a javascript: link that opens the add form prefilled
// with the current page. html/template blocks javascript: URLs unless they
// are marked safe, which is fine here: the only input is the configured
// base URL, embedded as a JSON string literal.
func (s *Server) bookmarklet() template.URL {
	base, _ := json.Marshal(s.cfg.BaseURL.String() + "/bookmarks/new?url=")
	return template.URL("javascript:void(window.open(" + string(base) +
		"+encodeURIComponent(location.href)+'&title='+encodeURIComponent(document.title)))")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
