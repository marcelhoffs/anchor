package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/marcelhoffs/anchor/internal/auth"
	"github.com/marcelhoffs/anchor/internal/i18n"
	"github.com/marcelhoffs/anchor/internal/ratelimit"
	"github.com/marcelhoffs/anchor/internal/store"
)

var invalidLogin = i18n.M("Invalid username or password.")

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
	if s.passwordsOff(w, r) {
		return
	}
	username, password, msg := newCredentials(r)
	if msg.Format != "" {
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
// msg is a user-facing problem, or the zero Msg.
func newCredentials(r *http.Request) (username, password string, msg i18n.Msg) {
	username = strings.TrimSpace(r.PostFormValue("username"))
	password = r.PostFormValue("password")
	switch {
	case auth.ValidateUsername(username) != nil:
		msg = errMsg(auth.ValidateUsername(username))
	case auth.ValidatePassword(password) != nil:
		msg = errMsg(auth.ValidatePassword(password))
	case password != r.PostFormValue("confirm"):
		msg = i18n.M("Passwords do not match.")
	}
	return username, password, msg
}

func (s *Server) loginForm(w http.ResponseWriter, r *http.Request) {
	if currentUser(r) != nil {
		http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusSeeOther)
		return
	}
	s.renderLogin(w, r, http.StatusOK, data{"Next": r.URL.Query().Get("next")})
}

// renderLogin shows the login page, which offers sign-up when it's open.
func (s *Server) renderLogin(w http.ResponseWriter, r *http.Request, status int, d data) {
	d["OpenSignup"] = s.signupMode(r.Context()) == "open"
	s.render(w, r, status, "login", d)
}

// passwordsOff answers a password sign-in or sign-up while single sign-on
// is the only way in; it reports whether it did.
func (s *Server) passwordsOff(w http.ResponseWriter, r *http.Request) bool {
	if s.cfg.OIDC == nil || !s.cfg.OIDC.Only {
		return false
	}
	s.notFound(w, r)
	return true
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if s.passwordsOff(w, r) {
		return
	}
	username := strings.TrimSpace(r.PostFormValue("username"))
	password := r.PostFormValue("password")
	next := r.PostFormValue("next")
	ip := s.clientIP(r)
	userKey := ip + "|" + strings.ToLower(username)
	fail := func(status int, msg i18n.Msg) {
		s.renderLogin(w, r, status, data{"Error": msg, "Username": username, "Next": next})
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
			fail(http.StatusTooManyRequests, i18n.M("Too many failed attempts. Try again in %s.", retryText(langOf(r), retry)))
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

// newSession signs u in: fully (mfaPassed) or waiting for a second factor;
// sso marks a sign-in through the OpenID Connect provider, and ssoTokens are
// its tokens, sealed. verified is when it last proved who it is (see
// requireRecentAuth), or zero.
func (s *Server) newSession(w http.ResponseWriter, r *http.Request, u store.User, mfaPassed bool, ttl time.Duration, sso bool, ssoTokens []byte, verified time.Time) error {
	s.dropBrowserSession(r)
	token, hash := auth.NewToken(32)
	now := time.Now()
	ss := store.Session{
		TokenHash: hash, UserID: u.ID, MFAPassed: mfaPassed, SSO: sso, SSOTokens: ssoTokens,
		UserAgent: truncate(r.UserAgent(), 256), IP: s.clientIP(r),
		CreatedAt: now, ExpiresAt: now.Add(ttl), LastSeenAt: now, VerifiedAt: verified,
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
	if err := s.newSession(w, r, u, true, sessionTTL, false, nil, time.Now()); err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, safeNext(next), http.StatusSeeOther)
}

// startPendingLogin records a passed password check and asks for the second
// factor. The pending session can do nothing else and expires in minutes.
func (s *Server) startPendingLogin(w http.ResponseWriter, r *http.Request, u store.User, next string) {
	if err := s.newSession(w, r, u, false, pendingTTL, false, nil, time.Time{}); err != nil {
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
	if s.passwordsOff(w, r) {
		return
	}
	token := r.PostFormValue("invite")
	inviteHash, ok := s.signupAllowed(r, token)
	if !ok {
		s.render(w, r, http.StatusNotFound, "signup_closed", data{"HadInvite": token != ""})
		return
	}
	if allowed, retry := s.signupIP.Allow(s.clientIP(r)); !allowed {
		w.Header().Set("Retry-After", fmt.Sprint(int(retry.Seconds())+1))
		s.render(w, r, http.StatusTooManyRequests, "signup", data{"Invite": token,
			"Error": i18n.M("Too many sign-ups from your network. Try again in %s.", retryText(langOf(r), retry))})
		return
	}
	username, password, msg := newCredentials(r)
	if msg.Format != "" {
		s.render(w, r, http.StatusUnprocessableEntity, "signup", data{"Error": msg, "Username": username, "Invite": token})
		return
	}
	u, err := s.store.SignUp(r.Context(), username, auth.HashPassword(password), inviteHash)
	switch {
	case errors.Is(err, store.ErrConflict):
		s.render(w, r, http.StatusUnprocessableEntity, "signup", data{"Error": i18n.M("That username is taken."), "Username": username, "Invite": token})
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

var settingsNotices = map[string]i18n.Msg{
	"revoked":     i18n.M("Session revoked."),
	"appearance":  i18n.M("Settings saved."),
	"totp-off":    i18n.M("Authenticator app removed. Your other sessions were signed out."),
	"passkey":     i18n.M("Passkey added."),
	"passkey-off": i18n.M("Passkey removed. Your other sessions were signed out."),
	"token-off":   i18n.M("Token revoked."),
	"confirmed":   i18n.M("Thanks, you're confirmed for the next 10 minutes."),
}

// accentLabels names each store.Accents value in the appearance picker.
var accentLabels = map[string]i18n.Msg{
	"indigo": i18n.M("Indigo"), "terracotta": i18n.M("Terracotta"), "yellow": i18n.M("Yellow"),
	"emerald": i18n.M("Emerald"), "forest": i18n.M("Forest"), "ocean": i18n.M("Ocean"),
	"rose": i18n.M("Rose"), "graphite": i18n.M("Graphite"),
}

// fontLabels names each store.Fonts value in the appearance picker; only
// System is a word to translate.
var fontLabels = map[string]i18n.Msg{
	"inter": {Format: "Inter"}, "system": i18n.M("System"), "roboto": {Format: "Roboto"},
	"open-sans": {Format: "Open Sans"},
}

// fontFiles are the files under /static/ each store.Fonts value loads,
// which the layout preloads. The system font loads none.
var fontFiles = map[string][]string{
	"inter":     {"fonts/inter-latin.woff2"},
	"roboto":    {"fonts/roboto-latin.woff2"},
	"open-sans": {"fonts/open-sans-latin.woff2"},
}

type choice struct{ Value, Label string }

// choices labels values in l, from labels.
func choices(l i18n.Lang, values []string, labels map[string]i18n.Msg) []choice {
	out := make([]choice, len(values))
	for i, v := range values {
		out[i] = choice{v, labels[v].In(l)}
	}
	return out
}

// languageChoices names each language in itself, so anyone can find theirs.
func languageChoices(l i18n.Lang) []choice {
	out := []choice{{"auto", l.T("Automatic")}}
	for _, lang := range i18n.Langs {
		out = append(out, choice{string(lang), lang.Name()})
	}
	return out
}

func (s *Server) renderSettings(w http.ResponseWriter, r *http.Request, status int, d data) {
	l := langOf(r)
	d["Accents"] = choices(l, store.Accents, accentLabels)
	d["Fonts"] = choices(l, store.Fonts, fontLabels)
	d["Languages"] = languageChoices(l)
	d["Bookmarklet"] = s.bookmarklet()
	d["Tab"] = "appearance"
	s.render(w, r, status, "settings", d)
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	u, cur := currentUser(r), currentSession(r)
	render := func(status int, msg i18n.Msg) { s.renderSecurity(w, r, status, data{"Error": msg}) }
	fail := func(msg i18n.Msg) { render(http.StatusUnprocessableEntity, msg) }
	// Wrong current passwords count with failed second factors, so a stolen
	// session can't be used to guess the password.
	if s.mfaBlocked(w, r, *u, false, render) {
		return
	}
	if !u.HasPassword() {
		// An account made by single sign-on sets its first password: there's
		// no current one to check, so it proves itself at the provider.
		if time.Since(cur.VerifiedAt) > reauthWindow {
			http.Redirect(w, r, "/reauth?next=%2Fsettings%2Fsecurity", http.StatusSeeOther)
			return
		}
	} else if auth.VerifyPassword(u.PasswordHash, r.PostFormValue("current")) != nil {
		fail(i18n.M("Current password is wrong."))
		return
	}
	s.mfaUser.Reset(fmt.Sprint(u.ID))
	password := r.PostFormValue("password")
	if err := auth.ValidatePassword(password); err != nil {
		fail(errMsg(err))
		return
	}
	if password != r.PostFormValue("confirm") {
		fail(i18n.M("New passwords do not match."))
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
		Language:     keep("language", u.Settings.Language),
		Sidebar:      keep("sidebar", u.Settings.Sidebar),
		Sort:         keep("sort", u.Settings.Sort),
		Descriptions: keep("descriptions", u.Settings.Descriptions),
	})
	if isValidation(err) {
		s.renderSettings(w, r, http.StatusUnprocessableEntity, data{"Error": errMsg(err)})
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

// truncate cuts s to at most n bytes, at a character boundary.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
