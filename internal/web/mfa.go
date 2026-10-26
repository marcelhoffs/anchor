package web

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/marcelhoffs/anchor/internal/auth"
	"github.com/marcelhoffs/anchor/internal/i18n"
	"github.com/marcelhoffs/anchor/internal/store"
)

const totpIssuer = "Anchor"

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
	return true, s.store.UpdatePasskeyUse(r.Context(), u.ID, cred.ID, b)
}

// beginPasskeyLogin starts a WebAuthn login ceremony for u under key.
func (s *Server) beginPasskeyLogin(w http.ResponseWriter, r *http.Request, u store.User, key string) {
	if s.passkeysOff(w, r) {
		return
	}
	wu, err := s.webauthnUser(r, u)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, langOf(r).T("Something went wrong."))
		return
	}
	if len(wu.creds) == 0 {
		jsonError(w, http.StatusBadRequest, langOf(r).T("No passkeys are registered."))
		return
	}
	opts, sd, err := s.webauthn.BeginLogin(wu)
	if err != nil {
		s.log.Error("begin passkey login", "err", err)
		jsonError(w, http.StatusInternalServerError, langOf(r).T("Could not start passkey sign-in."))
		return
	}
	s.cer.put(key, *sd)
	writeJSON(w, http.StatusOK, opts)
}

// mfaBlocked counts a second-factor attempt before it is checked, and
// answers it if it is over the limit; it reports whether it did. Passing
// resets the count.
func (s *Server) mfaBlocked(w http.ResponseWriter, r *http.Request, u store.User, asJSON bool, render func(int, i18n.Msg)) bool {
	ok, retry := s.mfaUser.Allow(fmt.Sprint(u.ID))
	if ok {
		return false
	}
	msg := i18n.M("Too many failed attempts. Try again in %s.", retryText(langOf(r), retry))
	w.Header().Set("Retry-After", fmt.Sprint(int(retry.Seconds())+1))
	if asJSON {
		jsonError(w, http.StatusTooManyRequests, msg.In(langOf(r)))
	} else {
		render(http.StatusTooManyRequests, msg)
	}
	return true
}

// --- Second factor at login ---

func (s *Server) renderVerify(w http.ResponseWriter, r *http.Request, status int, mode string, u store.User, next string, problem i18n.Msg) {
	m, err := s.store.MFAStatus(r.Context(), u.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	action := map[string]string{"login": "/login/2fa", "reauth": "/reauth"}[mode]
	s.render(w, r, status, "verify", data{
		"Mode": mode, "Next": next, "Error": problem, "MFA": m, "Who": u.Username, "Action": action,
		"PasskeyFinish": action + "/passkey/finish?next=" + urlQueryEscape(next),
		"SSOLinked":     mode == "reauth" && s.ssoLinked(r.Context(), &u),
		"SSOOnly":       mode == "reauth" && s.ssoReauthOnly(r),
		"HasPassword":   u.HasPassword(),
	})
}

func (s *Server) login2FAForm(w http.ResponseWriter, r *http.Request) {
	r, _, u, ok := s.pendingLogin(r)
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	s.renderVerify(w, r, http.StatusOK, "login", u, r.URL.Query().Get("next"), i18n.Msg{})
}

func (s *Server) login2FA(w http.ResponseWriter, r *http.Request, method string, check func(store.User) (bool, error)) {
	r, _, u, ok := s.pendingLogin(r)
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	next := r.PostFormValue("next")
	render := func(status int, msg i18n.Msg) { s.renderVerify(w, r, status, "login", u, next, msg) }
	if s.mfaBlocked(w, r, u, false, render) {
		return
	}
	passed, err := check(u)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if !passed {
		render(http.StatusUnauthorized, i18n.M("That code didn't work. Check it and try again."))
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
	r, ss, u, ok := s.pendingLogin(r)
	if !ok {
		jsonError(w, http.StatusUnauthorized, langOf(r).T("Your sign-in expired. Start again."))
		return
	}
	s.beginPasskeyLogin(w, r, u, ceremonyKey("login", ss.TokenHash))
}

func (s *Server) login2FAPasskeyFinish(w http.ResponseWriter, r *http.Request) {
	r, ss, u, ok := s.pendingLogin(r)
	if !ok {
		jsonError(w, http.StatusUnauthorized, langOf(r).T("Your sign-in expired. Start again."))
		return
	}
	if s.mfaBlocked(w, r, u, true, nil) {
		return
	}
	passed, err := s.checkPasskey(r, u, ceremonyKey("login", ss.TokenHash))
	if err != nil {
		s.log.Error("passkey login", "err", err)
		jsonError(w, http.StatusInternalServerError, langOf(r).T("Something went wrong."))
		return
	}
	if !passed {
		jsonError(w, http.StatusUnauthorized, langOf(r).T("That passkey wasn't accepted. Try again."))
		return
	}
	s.completeLogin(w, r, u, "passkey", r.URL.Query().Get("next"), true)
}

// completeLogin turns a pending login into a full session with a new token.
func (s *Server) completeLogin(w http.ResponseWriter, r *http.Request, u store.User, method, next string, asJSON bool) {
	s.mfaUser.Reset(fmt.Sprint(u.ID))
	s.store.RecordLogin(r.Context(), u.ID)
	if err := s.newSession(w, r, u, true, sessionTTL, false, nil, time.Now()); err != nil {
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
	s.renderVerify(w, r, http.StatusOK, "reauth", *currentUser(r), r.URL.Query().Get("next"), i18n.Msg{})
}

func (s *Server) reauth(w http.ResponseWriter, r *http.Request, check func(store.User) (bool, error), asJSON bool) {
	u, cur := *currentUser(r), currentSession(r)
	next := r.PostFormValue("next")
	if asJSON {
		next = r.URL.Query().Get("next")
	}
	render := func(status int, msg i18n.Msg) { s.renderVerify(w, r, status, "reauth", u, next, msg) }
	if s.ssoReauthOnly(r) {
		// The page only offers the provider; see ssoReauthOnly.
		if asJSON {
			jsonError(w, http.StatusForbidden, langOf(r).T("Confirm with %s", s.cfg.OIDC.Name))
		} else {
			render(http.StatusForbidden, i18n.M("Confirm with %s", s.cfg.OIDC.Name))
		}
		return
	}
	if s.mfaBlocked(w, r, u, asJSON, render) {
		return
	}
	passed, err := check(u)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if !passed {
		if asJSON {
			jsonError(w, http.StatusUnauthorized, langOf(r).T("That passkey wasn't accepted. Try again."))
		} else {
			render(http.StatusUnauthorized, i18n.M("That didn't work. Check it and try again."))
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
	if s.ssoReauthOnly(r) {
		jsonError(w, http.StatusForbidden, langOf(r).T("Confirm with %s", s.cfg.OIDC.Name))
		return
	}
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

var securityNotices = map[string]i18n.Msg{
	"password":     i18n.M("Password changed. Your other sessions were signed out."),
	"revoked":      i18n.M("Session revoked."),
	"totp":         i18n.M("Authenticator app is set up."),
	"totp-off":     i18n.M("Authenticator app removed. Your other sessions were signed out."),
	"passkey":      i18n.M("Passkey added."),
	"passkey-off":  i18n.M("Passkey removed. Your other sessions were signed out."),
	"confirmed":    i18n.M("Thanks, you're confirmed for the next 10 minutes. Go ahead."),
	"recovery-low": i18n.M("You're running low on recovery codes. Generate new ones below."),
	"sso-linked":   i18n.M("Your account is linked: you can now sign in with single sign-on."),
	"sso-unlinked": i18n.M("Single sign-on is unlinked from your account."),
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
	d["SSOLinked"] = s.ssoLinked(r.Context(), u)
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

func (s *Server) renderTOTPSetup(w http.ResponseWriter, r *http.Request, status int, setup auth.TOTPSetup, problem i18n.Msg) {
	s.render(w, r, status, "totp_setup", data{
		"Tab": "security", "Secret": setup.Secret, "QR": template.URL(setup.QR), "Error": problem,
	})
}

func (s *Server) totpStart(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u.TOTPEnabled {
		s.renderSecurity(w, r, http.StatusConflict, data{"Error": i18n.M("An authenticator app is already set up.")})
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
	s.renderTOTPSetup(w, r, http.StatusOK, setup, i18n.Msg{})
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
			i18n.M("That code didn't match. Check your device's clock and try the newest code."))
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
		s.renderSecurity(w, r, http.StatusConflict, data{"Error": i18n.M("Set up an authenticator app or a passkey first.")})
		return
	}
	s.showNewRecoveryCodes(w, r)
}

// Removing a factor: lastRequiredFactor checks it is allowed, and
// afterFactorRemoved cleans up: recovery codes go once no factor is left,
// and every other session is signed out.
var errLastFactor = i18n.M("Two-factor authentication is required here. Add another method before removing this one.")

// lastRequiredFactor reports whether removing one of before's factors would
// leave none where the instance requires two-factor authentication.
func (s *Server) lastRequiredFactor(r *http.Request, before store.MFAStatus) bool {
	return s.require2FA(r.Context()) && before.Passkeys+boolInt(before.TOTP) <= 1
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
	if s.lastRequiredFactor(r, m) {
		s.renderSecurity(w, r, http.StatusConflict, data{"Error": errLastFactor})
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
	if s.passkeysOff(w, r) {
		return
	}
	u := currentUser(r)
	wu, err := s.webauthnUser(r, *u)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, langOf(r).T("Something went wrong."))
		return
	}
	exclude := make([]webauthn.Credential, len(wu.creds))
	copy(exclude, wu.creds)
	opts, sd, err := s.webauthn.BeginRegistration(wu, webauthn.WithExclusions(webauthn.Credentials(exclude).CredentialDescriptors()))
	if err != nil {
		s.log.Error("begin passkey registration", "err", err)
		jsonError(w, http.StatusInternalServerError, langOf(r).T("Could not start passkey setup."))
		return
	}
	s.cer.put(ceremonyKey("register", currentSession(r).TokenHash), *sd)
	writeJSON(w, http.StatusOK, opts)
}

func (s *Server) passkeyRegisterFinish(w http.ResponseWriter, r *http.Request) {
	if s.passkeysOff(w, r) {
		return
	}
	u := currentUser(r)
	sd, ok := s.cer.take(ceremonyKey("register", currentSession(r).TokenHash))
	if !ok {
		jsonError(w, http.StatusBadRequest, langOf(r).T("Passkey setup expired. Try again."))
		return
	}
	wu, err := s.webauthnUser(r, *u)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, langOf(r).T("Something went wrong."))
		return
	}
	cred, err := s.webauthn.FinishRegistration(wu, sd, r)
	if err != nil {
		s.log.Info("passkey registration rejected", "user", u.Username, "err", err)
		jsonError(w, http.StatusBadRequest, langOf(r).T("That passkey couldn't be registered. Try again."))
		return
	}
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		name = langOf(r).T("Passkey")
	}
	b, _ := jsonMarshal(cred)
	if err := s.store.AddPasskey(r.Context(), u.ID, truncate(name, 60), cred.ID, b); err != nil {
		if isValidation(err) {
			jsonError(w, http.StatusConflict, errMsg(err).In(langOf(r)))
			return
		}
		jsonError(w, http.StatusInternalServerError, langOf(r).T("Something went wrong."))
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
	if s.lastRequiredFactor(r, m) {
		s.renderSecurity(w, r, http.StatusConflict, data{"Error": errLastFactor})
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
