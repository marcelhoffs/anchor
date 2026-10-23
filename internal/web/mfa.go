package web

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/marcelhoffs/burrow/internal/auth"
	"github.com/marcelhoffs/burrow/internal/store"
)

const totpIssuer = "Burrow"

// checkTOTP verifies a code against u's enabled authenticator, using each
// time step at most once.
func (s *Server) checkTOTP(r *http.Request, u store.User, code string) (bool, error) {
	if !u.TOTPEnabled || u.TOTPSecret == nil {
		return false, nil
	}
	secret, err := s.keys.Open("totp", u.TOTPSecret)
	if err != nil {
		return false, fmt.Errorf("open TOTP secret: %w", err)
	}
	step, ok := auth.VerifyTOTP(string(secret), code, u.TOTPLastStep, time.Now())
	if !ok {
		return false, nil
	}
	return s.store.UseTOTPStep(r.Context(), u.ID, step)
}

func (s *Server) checkRecovery(r *http.Request, u store.User, code string) (bool, error) {
	return s.store.UseRecoveryCode(r.Context(), u.ID, auth.HashRecoveryCode(code))
}

// checkPasskey finishes a WebAuthn login ceremony for u.
func (s *Server) checkPasskey(r *http.Request, u store.User, key string) (bool, error) {
	if s.webauthn == nil {
		return false, nil
	}
	sd, ok := s.cer.take(key)
	if !ok {
		return false, nil
	}
	wu, err := s.webauthnUser(r, u)
	if err != nil {
		return false, err
	}
	cred, err := s.webauthn.FinishLogin(wu, sd, r)
	if err != nil {
		s.log.Info("passkey assertion rejected", "user", u.Username, "err", err)
		return false, nil
	}
	b, _ := jsonMarshal(cred)
	return true, s.store.UpdatePasskeyUse(r.Context(), u.ID, cred.ID, cred.Authenticator.SignCount, b)
}

// beginPasskeyLogin starts a WebAuthn login ceremony for u under key.
func (s *Server) beginPasskeyLogin(w http.ResponseWriter, r *http.Request, u store.User, key string) {
	if s.passkeysOff(w) {
		return
	}
	wu, err := s.webauthnUser(r, u)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "Something went wrong.")
		return
	}
	if len(wu.creds) == 0 {
		jsonError(w, http.StatusBadRequest, "No passkeys are registered.")
		return
	}
	opts, sd, err := s.webauthn.BeginLogin(wu)
	if err != nil {
		s.log.Error("begin passkey login", "err", err)
		jsonError(w, http.StatusInternalServerError, "Could not start passkey sign-in.")
		return
	}
	s.cer.put(key, *sd)
	writeJSON(w, http.StatusOK, opts)
}

// mfaBlocked answers a rate-limited second-factor attempt; it reports
// whether it did.
func (s *Server) mfaBlocked(w http.ResponseWriter, r *http.Request, u store.User, asJSON bool, render func(int, string)) bool {
	blocked, retry := s.mfaUser.Blocked(fmt.Sprint(u.ID))
	if !blocked {
		return false
	}
	msg := "Too many failed attempts. Try again in " + retryText(retry) + "."
	w.Header().Set("Retry-After", fmt.Sprint(int(retry.Seconds())+1))
	if asJSON {
		jsonError(w, http.StatusTooManyRequests, msg)
	} else {
		render(http.StatusTooManyRequests, msg)
	}
	return true
}

// --- Second factor at login ---

func (s *Server) renderVerify(w http.ResponseWriter, r *http.Request, status int, mode string, u store.User, next, errMsg string) {
	m, err := s.store.MFAStatus(r.Context(), u.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	action := map[string]string{"login": "/login/2fa", "reauth": "/reauth"}[mode]
	s.render(w, r, status, "verify", data{
		"Mode": mode, "Next": next, "Error": errMsg, "MFA": m, "Who": u.Username, "Action": action,
		"PasskeyFinish": action + "/passkey/finish?next=" + urlQueryEscape(next),
	})
}

func (s *Server) login2FAForm(w http.ResponseWriter, r *http.Request) {
	_, u, ok := s.pendingLogin(r)
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	s.renderVerify(w, r, http.StatusOK, "login", u, r.URL.Query().Get("next"), "")
}

func (s *Server) login2FA(w http.ResponseWriter, r *http.Request, method string, check func(store.User) (bool, error)) {
	_, u, ok := s.pendingLogin(r)
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	next := r.PostFormValue("next")
	render := func(status int, msg string) { s.renderVerify(w, r, status, "login", u, next, msg) }
	if s.mfaBlocked(w, r, u, false, render) {
		return
	}
	passed, err := check(u)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if !passed {
		s.mfaUser.Hit(fmt.Sprint(u.ID))
		render(http.StatusUnauthorized, "That code didn't work. Check it and try again.")
		return
	}
	s.completeLogin(w, r, u, method, next, false)
}

func (s *Server) login2FATOTP(w http.ResponseWriter, r *http.Request) {
	s.login2FA(w, r, "totp", func(u store.User) (bool, error) { return s.checkTOTP(r, u, r.PostFormValue("code")) })
}

func (s *Server) login2FARecovery(w http.ResponseWriter, r *http.Request) {
	s.login2FA(w, r, "recovery code", func(u store.User) (bool, error) { return s.checkRecovery(r, u, r.PostFormValue("code")) })
}

func (s *Server) login2FAPasskeyBegin(w http.ResponseWriter, r *http.Request) {
	ss, u, ok := s.pendingLogin(r)
	if !ok {
		jsonError(w, http.StatusUnauthorized, "Your sign-in expired. Start again.")
		return
	}
	s.beginPasskeyLogin(w, r, u, ceremonyKey("login", ss.TokenHash))
}

func (s *Server) login2FAPasskeyFinish(w http.ResponseWriter, r *http.Request) {
	ss, u, ok := s.pendingLogin(r)
	if !ok {
		jsonError(w, http.StatusUnauthorized, "Your sign-in expired. Start again.")
		return
	}
	if s.mfaBlocked(w, r, u, true, nil) {
		return
	}
	passed, err := s.checkPasskey(r, u, ceremonyKey("login", ss.TokenHash))
	if err != nil {
		s.log.Error("passkey login", "err", err)
		jsonError(w, http.StatusInternalServerError, "Something went wrong.")
		return
	}
	if !passed {
		s.mfaUser.Hit(fmt.Sprint(u.ID))
		jsonError(w, http.StatusUnauthorized, "That passkey wasn't accepted. Try again.")
		return
	}
	s.completeLogin(w, r, u, "passkey", r.URL.Query().Get("next"), true)
}

// completeLogin turns a pending login into a full session with a new token.
func (s *Server) completeLogin(w http.ResponseWriter, r *http.Request, u store.User, method, next string, asJSON bool) {
	s.mfaUser.Reset(fmt.Sprint(u.ID))
	s.store.RecordLogin(r.Context(), u.ID)
	if err := s.newSession(w, r, u, true, sessionTTL); err != nil {
		s.serverError(w, r, err)
		return
	}
	next = safeNext(next)
	if method == "recovery code" {
		if m, err := s.store.MFAStatus(r.Context(), u.ID); err == nil && m.RecoveryLeft <= 2 {
			next = "/settings/security?done=recovery-low"
		}
	}
	if asJSON {
		writeJSON(w, http.StatusOK, map[string]string{"redirect": next})
		return
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// --- Re-authentication before sensitive actions ---

func (s *Server) reauthForm(w http.ResponseWriter, r *http.Request) {
	s.renderVerify(w, r, http.StatusOK, "reauth", *currentUser(r), r.URL.Query().Get("next"), "")
}

func (s *Server) reauth(w http.ResponseWriter, r *http.Request, check func(store.User) (bool, error), asJSON bool) {
	u, cur := *currentUser(r), currentSession(r)
	next := r.PostFormValue("next")
	if asJSON {
		next = r.URL.Query().Get("next")
	}
	render := func(status int, msg string) { s.renderVerify(w, r, status, "reauth", u, next, msg) }
	if s.mfaBlocked(w, r, u, asJSON, render) {
		return
	}
	passed, err := check(u)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if !passed {
		s.mfaUser.Hit(fmt.Sprint(u.ID))
		if asJSON {
			jsonError(w, http.StatusUnauthorized, "That passkey wasn't accepted. Try again.")
		} else {
			render(http.StatusUnauthorized, "That didn't work. Check it and try again.")
		}
		return
	}
	s.mfaUser.Reset(fmt.Sprint(u.ID))
	if err := s.store.MarkVerified(r.Context(), cur.TokenHash); err != nil {
		s.serverError(w, r, err)
		return
	}
	next = withNotice(safeNext(next), "confirmed")
	if asJSON {
		writeJSON(w, http.StatusOK, map[string]string{"redirect": next})
		return
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// withNotice adds done=<notice> to a local URL that has no query yet.
func withNotice(next, notice string) string {
	if strings.Contains(next, "?") {
		return next
	}
	return next + "?done=" + notice
}

// reauthPassword is only for users without a second factor; everyone else
// must confirm with one ("API tokens bypass 2FA, so creating one requires a
// fresh 2FA check").
func (s *Server) reauthPassword(w http.ResponseWriter, r *http.Request) {
	s.reauth(w, r, func(u store.User) (bool, error) {
		if m, err := s.store.MFAStatus(r.Context(), u.ID); err != nil || m.Enabled() {
			return false, err
		}
		return auth.VerifyPassword(u.PasswordHash, r.PostFormValue("password")) == nil, nil
	}, false)
}

func (s *Server) reauthTOTP(w http.ResponseWriter, r *http.Request) {
	s.reauth(w, r, func(u store.User) (bool, error) { return s.checkTOTP(r, u, r.PostFormValue("code")) }, false)
}

func (s *Server) reauthRecovery(w http.ResponseWriter, r *http.Request) {
	s.reauth(w, r, func(u store.User) (bool, error) { return s.checkRecovery(r, u, r.PostFormValue("code")) }, false)
}

func (s *Server) reauthPasskeyBegin(w http.ResponseWriter, r *http.Request) {
	s.beginPasskeyLogin(w, r, *currentUser(r), ceremonyKey("reauth", currentSession(r).TokenHash))
}

func (s *Server) reauthPasskeyFinish(w http.ResponseWriter, r *http.Request) {
	key := ceremonyKey("reauth", currentSession(r).TokenHash)
	s.reauth(w, r, func(u store.User) (bool, error) { return s.checkPasskey(r, u, key) }, true)
}

// --- Security settings ---

type sessionRow struct {
	store.Session
	ID      string
	Current bool
}

var securityNotices = map[string]string{
	"password":     "Password changed. Your other sessions were signed out.",
	"revoked":      "Session revoked.",
	"totp":         "Authenticator app is set up.",
	"totp-off":     "Authenticator app removed. Your other sessions were signed out.",
	"passkey":      "Passkey added.",
	"passkey-off":  "Passkey removed. Your other sessions were signed out.",
	"confirmed":    "Thanks, you're confirmed for the next 10 minutes. Go ahead.",
	"recovery-low": "You're running low on recovery codes. Generate new ones below.",
}

func (s *Server) security(w http.ResponseWriter, r *http.Request) {
	s.renderSecurity(w, r, http.StatusOK, data{"Notice": securityNotices[r.URL.Query().Get("done")]})
}

func (s *Server) renderSecurity(w http.ResponseWriter, r *http.Request, status int, d data) {
	u, cur := currentUser(r), currentSession(r)
	m, err := s.store.MFAStatus(r.Context(), u.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	keys, err := s.store.ListPasskeys(r.Context(), u.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	list, err := s.store.ListSessions(r.Context(), u.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	rows := make([]sessionRow, len(list))
	for i, ss := range list {
		rows[i] = sessionRow{ss, hex.EncodeToString(ss.TokenHash), bytes.Equal(ss.TokenHash, cur.TokenHash)}
	}
	d["Tab"] = "security"
	d["MFA"] = m
	d["Passkeys"] = keys
	d["Sessions"] = rows
	d["Required"] = s.require2FA(r.Context()) && !m.Enabled()
	d["MinPassword"] = auth.MinPasswordLen
	s.render(w, r, status, "security", d)
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
	http.Redirect(w, r, "/settings/security?done=revoked", http.StatusSeeOther)
}

func (s *Server) renderTOTPSetup(w http.ResponseWriter, r *http.Request, status int, setup auth.TOTPSetup, errMsg string) {
	s.render(w, r, status, "totp_setup", data{
		"Tab": "security", "Secret": setup.Secret, "QR": template.URL(setup.QR), "Error": errMsg,
	})
}

func (s *Server) totpStart(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u.TOTPEnabled {
		s.renderSecurity(w, r, http.StatusConflict, data{"Error": "An authenticator app is already set up."})
		return
	}
	setup, err := auth.NewTOTP(totpIssuer, u.Username)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	sealed, err := s.keys.Seal("totp", []byte(setup.Secret))
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if err := s.store.StartTOTP(r.Context(), u.ID, sealed); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.renderTOTPSetup(w, r, http.StatusOK, setup, "")
}

func (s *Server) totpEnable(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u.TOTPEnabled || u.TOTPSecret == nil {
		http.Redirect(w, r, "/settings/security", http.StatusSeeOther)
		return
	}
	secret, err := s.keys.Open("totp", u.TOTPSecret)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	step, ok := auth.VerifyTOTP(string(secret), r.PostFormValue("code"), 0, time.Now())
	if !ok {
		setup, err := auth.TOTPSetupFor(string(secret), totpIssuer, u.Username)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		s.renderTOTPSetup(w, r, http.StatusUnprocessableEntity, setup,
			"That code didn't match. Check your device's clock and try the newest code.")
		return
	}
	if err := s.store.EnableTOTP(r.Context(), u.ID, step); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.firstFactorAdded(w, r, "/settings/security?done=totp")
}

// firstFactorAdded shows fresh recovery codes if the user has none yet, and
// otherwise redirects to done.
func (s *Server) firstFactorAdded(w http.ResponseWriter, r *http.Request, done string) {
	u := currentUser(r)
	m, err := s.store.MFAStatus(r.Context(), u.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if m.RecoveryLeft > 0 {
		http.Redirect(w, r, done, http.StatusSeeOther)
		return
	}
	s.showNewRecoveryCodes(w, r)
}

func (s *Server) showNewRecoveryCodes(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	codes, hashes := auth.NewRecoveryCodes()
	if err := s.store.ReplaceRecoveryCodes(r.Context(), u.ID, hashes); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "recovery_codes", data{"Tab": "security", "Codes": codes})
}

func (s *Server) recoveryRegenerate(w http.ResponseWriter, r *http.Request) {
	m, err := s.store.MFAStatus(r.Context(), currentUser(r).ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if !m.Enabled() {
		s.renderSecurity(w, r, http.StatusConflict, data{"Error": "Set up an authenticator app or a passkey first."})
		return
	}
	s.showNewRecoveryCodes(w, r)
}

// removingFactor checks that removing one factor is allowed, and after the
// removal cleans up: recovery codes go once no factor is left, and every
// other session is signed out.
func (s *Server) removingFactor(r *http.Request, before store.MFAStatus) error {
	if s.require2FA(r.Context()) && before.Passkeys+boolInt(before.TOTP) <= 1 {
		return errors.New("Two-factor authentication is required here. Add another method before removing this one.")
	}
	return nil
}

func (s *Server) afterFactorRemoved(r *http.Request) error {
	u, cur := currentUser(r), currentSession(r)
	m, err := s.store.MFAStatus(r.Context(), u.ID)
	if err != nil {
		return err
	}
	if !m.Enabled() {
		if err := s.store.ReplaceRecoveryCodes(r.Context(), u.ID, nil); err != nil {
			return err
		}
	}
	return s.store.DeleteOtherSessions(r.Context(), u.ID, cur.TokenHash)
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (s *Server) totpDisable(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	m, err := s.store.MFAStatus(r.Context(), u.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if err := s.removingFactor(r, m); err != nil {
		s.renderSecurity(w, r, http.StatusConflict, data{"Error": err.Error()})
		return
	}
	if err := s.store.DisableTOTP(r.Context(), u.ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	if err := s.afterFactorRemoved(r); err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/settings/security?done=totp-off", http.StatusSeeOther)
}

func (s *Server) passkeyRegisterBegin(w http.ResponseWriter, r *http.Request) {
	if s.passkeysOff(w) {
		return
	}
	u := currentUser(r)
	wu, err := s.webauthnUser(r, *u)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "Something went wrong.")
		return
	}
	exclude := make([]webauthn.Credential, len(wu.creds))
	copy(exclude, wu.creds)
	opts, sd, err := s.webauthn.BeginRegistration(wu, webauthn.WithExclusions(webauthn.Credentials(exclude).CredentialDescriptors()))
	if err != nil {
		s.log.Error("begin passkey registration", "err", err)
		jsonError(w, http.StatusInternalServerError, "Could not start passkey setup.")
		return
	}
	s.cer.put(ceremonyKey("register", currentSession(r).TokenHash), *sd)
	writeJSON(w, http.StatusOK, opts)
}

func (s *Server) passkeyRegisterFinish(w http.ResponseWriter, r *http.Request) {
	if s.passkeysOff(w) {
		return
	}
	u := currentUser(r)
	sd, ok := s.cer.take(ceremonyKey("register", currentSession(r).TokenHash))
	if !ok {
		jsonError(w, http.StatusBadRequest, "Passkey setup expired. Try again.")
		return
	}
	wu, err := s.webauthnUser(r, *u)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "Something went wrong.")
		return
	}
	cred, err := s.webauthn.FinishRegistration(wu, sd, r)
	if err != nil {
		s.log.Info("passkey registration rejected", "user", u.Username, "err", err)
		jsonError(w, http.StatusBadRequest, "That passkey couldn't be registered. Try again.")
		return
	}
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		name = "Passkey"
	}
	b, _ := jsonMarshal(cred)
	if err := s.store.AddPasskey(r.Context(), u.ID, truncate(name, 60), cred.ID, cred.PublicKey, cred.Authenticator.SignCount, b); err != nil {
		if isValidation(err) {
			jsonError(w, http.StatusConflict, err.Error())
			return
		}
		jsonError(w, http.StatusInternalServerError, "Something went wrong.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"redirect": "/settings/security?done=passkey"})
}

func (s *Server) passkeyDelete(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	id, ok := idParam(r)
	if !ok {
		s.notFound(w, r)
		return
	}
	m, err := s.store.MFAStatus(r.Context(), u.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if err := s.removingFactor(r, m); err != nil {
		s.renderSecurity(w, r, http.StatusConflict, data{"Error": err.Error()})
		return
	}
	if !s.storeError(w, r, s.store.DeletePasskey(r.Context(), u.ID, id)) {
		return
	}
	if err := s.afterFactorRemoved(r); err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/settings/security?done=passkey-off", http.StatusSeeOther)
}
