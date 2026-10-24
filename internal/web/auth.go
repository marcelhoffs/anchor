package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/marcelhoffs/anchor/internal/auth"
	"github.com/marcelhoffs/anchor/internal/ratelimit"
	"github.com/marcelhoffs/anchor/internal/store"
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
	username, password, msg := newCredentials(r)
	if msg != "" {
		s.render(w, r, http.StatusUnprocessableEntity, "setup", data{"Error": msg, "Username": username})
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
	s.startSession(w, r, u, "/")
}

// newCredentials reads and checks a username, password and confirmation;
// msg is a user-facing problem, or "".
func newCredentials(r *http.Request) (username, password, msg string) {
	username = strings.TrimSpace(r.PostFormValue("username"))
	password = r.PostFormValue("password")
	switch {
	case auth.ValidateUsername(username) != nil:
		msg = sentence(auth.ValidateUsername(username))
	case auth.ValidatePassword(password) != nil:
		msg = sentence(auth.ValidatePassword(password))
	case password != r.PostFormValue("confirm"):
		msg = "Passwords do not match."
	}
	return username, password, msg
}

func (s *Server) loginForm(w http.ResponseWriter, r *http.Request) {
	if currentUser(r) != nil {
		http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusSeeOther)
		return
	}
	s.render(w, r, http.StatusOK, "login", data{
		"Next": r.URL.Query().Get("next"), "OpenSignup": s.signupMode(r.Context()) == "open",
	})
}

func retryText(d time.Duration) string {
	m := int(math.Ceil(d.Minutes()))
	if m <= 1 {
		return "a minute"
	}
	return fmt.Sprintf("%d minutes", m)
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	username := strings.TrimSpace(r.PostFormValue("username"))
	password := r.PostFormValue("password")
	next := r.PostFormValue("next")
	ip := s.clientIP(r)
	userKey := ip + "|" + strings.ToLower(username)
	fail := func(status int, msg string) {
		s.render(w, r, status, "login", data{"Error": msg, "Username": username, "Next": next,
			"OpenSignup": s.signupMode(r.Context()) == "open"})
	}

	// Attempts are limited per IP and username, and per IP overall. Each one
	// counts before the password is checked, so a burst of parallel guesses
	// can't all slip in under the limit; signing in resets the username's.
	for _, l := range []struct {
		lim *ratelimit.Limiter
		key string
	}{{s.loginUser, userKey}, {s.loginIP, ip}} {
		if ok, retry := l.lim.Allow(l.key); !ok {
			w.Header().Set("Retry-After", fmt.Sprint(int(retry.Seconds())+1))
			fail(http.StatusTooManyRequests, "Too many failed attempts. Try again in "+retryText(retry)+".")
			return
		}
	}

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
		fail(http.StatusUnauthorized, invalidLogin)
		return
	}
	s.loginUser.Reset(userKey)

	m, err := s.store.MFAStatus(r.Context(), u.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if m.Enabled() {
		s.startPendingLogin(w, r, u, next)
		return
	}
	s.store.RecordLogin(r.Context(), u.ID)
	s.startSession(w, r, u, next)
}

// dropBrowserSession deletes whatever session the browser's cookie names, so
// every login issues a fresh token (no session fixation).
func (s *Server) dropBrowserSession(r *http.Request) {
	if c, err := r.Cookie(s.sessionCookie); err == nil {
		if old, _, err := s.store.SessionByHash(r.Context(), auth.HashToken(c.Value)); err == nil {
			s.store.DeleteSession(r.Context(), old.UserID, old.TokenHash)
		}
	}
}

func (s *Server) newSession(w http.ResponseWriter, r *http.Request, u store.User, mfaPassed bool, ttl time.Duration) error {
	s.dropBrowserSession(r)
	token, hash := auth.NewToken(32)
	now := time.Now()
	ss := store.Session{
		TokenHash: hash, UserID: u.ID, MFAPassed: mfaPassed,
		UserAgent: truncate(r.UserAgent(), 256), IP: s.clientIP(r),
		CreatedAt: now, ExpiresAt: now.Add(ttl), LastSeenAt: now,
	}
	if mfaPassed {
		ss.VerifiedAt = now
	}
	if err := s.store.CreateSession(r.Context(), ss); err != nil {
		return err
	}
	s.setSessionCookie(w, token)
	s.setCSRFCookie(w, s.newCSRF(token)) // the old one was bound to the old session
	return nil
}

// startSession signs u in fully and redirects to next.
func (s *Server) startSession(w http.ResponseWriter, r *http.Request, u store.User, next string) {
	if err := s.newSession(w, r, u, true, sessionTTL); err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, safeNext(next), http.StatusSeeOther)
}

// startPendingLogin records a passed password check and asks for the second
// factor. The pending session can do nothing else and expires in minutes.
func (s *Server) startPendingLogin(w http.ResponseWriter, r *http.Request, u store.User, next string) {
	if err := s.newSession(w, r, u, false, pendingTTL); err != nil {
		s.serverError(w, r, err)
		return
	}
	to := "/login/2fa"
	if next != "" {
		to += "?next=" + urlQueryEscape(safeNext(next))
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if ss := currentSession(r); ss != nil {
		if err := s.store.DeleteSession(r.Context(), ss.UserID, ss.TokenHash); err != nil {
			s.serverError(w, r, err)
			return
		}
	} else {
		s.dropBrowserSession(r) // a pending 2FA login being abandoned
	}
	s.clearSessionCookie(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// signupAllowed reports whether a signup may proceed, and with which invite.
// Closed: never. Invite: only with a valid invite. Open: always (an invite is
// still consumed if given).
func (s *Server) signupAllowed(r *http.Request, token string) (inviteHash []byte, ok bool) {
	mode := s.signupMode(r.Context())
	if token != "" {
		inviteHash = auth.HashToken(token)
		if !s.store.InviteValid(r.Context(), inviteHash) {
			inviteHash = nil
		}
	}
	switch mode {
	case "open":
		return inviteHash, true
	case "invite":
		return inviteHash, inviteHash != nil
	}
	return nil, false
}

func (s *Server) signupForm(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("invite")
	if _, ok := s.signupAllowed(r, token); !ok {
		s.render(w, r, http.StatusNotFound, "signup_closed", data{"HadInvite": token != ""})
		return
	}
	s.render(w, r, http.StatusOK, "signup", data{"Invite": token})
}

func (s *Server) signup(w http.ResponseWriter, r *http.Request) {
	token := r.PostFormValue("invite")
	inviteHash, ok := s.signupAllowed(r, token)
	if !ok {
		s.render(w, r, http.StatusNotFound, "signup_closed", data{"HadInvite": token != ""})
		return
	}
	if allowed, retry := s.signupIP.Allow(s.clientIP(r)); !allowed {
		w.Header().Set("Retry-After", fmt.Sprint(int(retry.Seconds())+1))
		s.render(w, r, http.StatusTooManyRequests, "signup", data{"Invite": token,
			"Error": "Too many sign-ups from your network. Try again in " + retryText(retry) + "."})
		return
	}
	username, password, msg := newCredentials(r)
	if msg != "" {
		s.render(w, r, http.StatusUnprocessableEntity, "signup", data{"Error": msg, "Username": username, "Invite": token})
		return
	}
	u, err := s.store.SignUp(r.Context(), username, auth.HashPassword(password), inviteHash)
	switch {
	case errors.Is(err, store.ErrConflict):
		s.render(w, r, http.StatusUnprocessableEntity, "signup", data{"Error": "That username is taken.", "Username": username, "Invite": token})
		return
	case errors.Is(err, store.ErrInvalidInvite):
		s.render(w, r, http.StatusNotFound, "signup_closed", data{"HadInvite": true})
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}
	s.store.RecordLogin(r.Context(), u.ID)
	s.startSession(w, r, u, "/")
}

func (s *Server) settings(w http.ResponseWriter, r *http.Request) {
	s.renderSettings(w, r, http.StatusOK, data{"Notice": settingsNotices[r.URL.Query().Get("done")]})
}

var settingsNotices = map[string]string{
	"revoked":     "Session revoked.",
	"appearance":  "Settings saved.",
	"totp-off":    "Authenticator app removed. Your other sessions were signed out.",
	"passkey":     "Passkey added.",
	"passkey-off": "Passkey removed. Your other sessions were signed out.",
	"token-off":   "Token revoked.",
	"confirmed":   "Thanks, you're confirmed for the next 10 minutes.",
}

// accentLabels names each store.Accents value in the appearance picker.
var accentLabels = map[string]string{
	"indigo": "Indigo", "terracotta": "Terracotta", "yellow": "Yellow", "emerald": "Emerald",
	"forest": "Forest", "ocean": "Ocean", "rose": "Rose", "graphite": "Graphite",
}

type choice struct{ Value, Label string }

// fontLabels names each store.Fonts value in the appearance picker.
var fontLabels = map[string]string{
	"inter": "Inter", "system": "System", "roboto": "Roboto", "open-sans": "Open Sans", "lato": "Lato",
}

// fontFiles are the files under /static/ each store.Fonts value loads,
// which the layout preloads. The system font loads none.
var fontFiles = map[string][]string{
	"inter":     {"fonts/inter-latin.woff2"},
	"roboto":    {"fonts/roboto-latin.woff2"},
	"open-sans": {"fonts/open-sans-latin.woff2"},
	"lato":      {"fonts/lato-latin-400.woff2", "fonts/lato-latin-700.woff2"},
}

func fontChoices() []choice {
	out := make([]choice, len(store.Fonts))
	for i, f := range store.Fonts {
		out[i] = choice{f, fontLabels[f]}
	}
	return out
}

func accentChoices() []choice {
	out := make([]choice, len(store.Accents))
	for i, a := range store.Accents {
		out[i] = choice{a, accentLabels[a]}
	}
	return out
}

func (s *Server) renderSettings(w http.ResponseWriter, r *http.Request, status int, d data) {
	d["Accents"] = accentChoices()
	d["Fonts"] = fontChoices()
	d["Bookmarklet"] = s.bookmarklet()
	d["Tab"] = "profile"
	s.render(w, r, status, "settings", d)
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	u, cur := currentUser(r), currentSession(r)
	render := func(status int, msg string) { s.renderSecurity(w, r, status, data{"Error": msg}) }
	fail := func(msg string) { render(http.StatusUnprocessableEntity, msg) }
	// Wrong current passwords count with failed second factors, so a stolen
	// session can't be used to guess the password.
	if s.mfaBlocked(w, r, *u, false, render) {
		return
	}
	if auth.VerifyPassword(u.PasswordHash, r.PostFormValue("current")) != nil {
		fail("Current password is wrong.")
		return
	}
	s.mfaUser.Reset(fmt.Sprint(u.ID))
	password := r.PostFormValue("password")
	if err := auth.ValidatePassword(password); err != nil {
		fail(sentence(err))
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
	http.Redirect(w, r, "/settings/security?done=password", http.StatusSeeOther)
}

func (s *Server) saveAppearance(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	// A field the form didn't send keeps its current value.
	keep := func(field, cur string) string {
		if v := r.PostFormValue(field); v != "" {
			return v
		}
		return cur
	}
	err := s.store.SaveSettings(r.Context(), u.ID, store.Settings{
		Accent:       keep("accent", u.Settings.Accent),
		Theme:        keep("theme", u.Settings.Theme),
		Font:         keep("font", u.Settings.Font),
		Sidebar:      keep("sidebar", u.Settings.Sidebar),
		Sort:         keep("sort", u.Settings.Sort),
		Descriptions: keep("descriptions", u.Settings.Descriptions),
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

// bookmarklet returns a javascript: link that opens the add form prefilled
// with the current page. html/template blocks javascript: URLs unless they
// are marked safe, which is fine here: the only input is the configured
// base URL, embedded as a JSON string literal.
func (s *Server) bookmarklet() template.URL {
	base, _ := json.Marshal(s.cfg.BaseURL.String() + "/bookmarks/new?url=")
	return template.URL("javascript:void(window.open(" + string(base) +
		"+encodeURIComponent(location.href)+'&title='+encodeURIComponent(document.title)))")
}

// sentence turns a Go error ("password must be …") into a message for the
// page ("Password must be ….").
func sentence(err error) string {
	msg := err.Error()
	if msg == "" {
		return msg
	}
	r, size := utf8.DecodeRuneInString(msg)
	return string(unicode.ToUpper(r)) + strings.TrimSuffix(msg[size:], ".") + "."
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
