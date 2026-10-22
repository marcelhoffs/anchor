package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/marcelhoffs/burrow/internal/auth"
	"github.com/marcelhoffs/burrow/internal/store"
)

const inviteTTL = 7 * 24 * time.Hour

var adminNotices = map[string]string{
	"disable":      "User disabled and signed out everywhere.",
	"enable":       "User enabled.",
	"make-admin":   "User is now an admin.",
	"remove-admin": "Admin rights removed.",
	"reset-2fa":    "Two-factor authentication reset. The user was signed out everywhere.",
	"invite-off":   "Invite revoked.",
	"settings":     "Settings saved.",
	"confirmed":    "Thanks, you're confirmed for the next 10 minutes. Go ahead.",
}

func (s *Server) adminUsers(w http.ResponseWriter, r *http.Request) {
	s.renderAdminUsers(w, r, http.StatusOK, data{"Notice": adminNotices[r.URL.Query().Get("done")]})
}

func (s *Server) renderAdminUsers(w http.ResponseWriter, r *http.Request, status int, d data) {
	users, err := s.store.ListUsers(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	stats, err := s.store.Stats(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	d["AdminTab"] = "users"
	d["Users"] = users
	d["Stats"] = stats
	s.render(w, r, status, "admin_users", d)
}

// showSecret renders a value that is shown exactly once (a generated
// password or an invite link).
func (s *Server) showSecret(w http.ResponseWriter, r *http.Request, tab, title, message, value string) {
	s.render(w, r, http.StatusOK, "admin_secret", data{
		"AdminTab": tab, "SecretTitle": title, "Message": message, "Value": value,
	})
}

func (s *Server) adminCreateUser(w http.ResponseWriter, r *http.Request) {
	username := strings.TrimSpace(r.PostFormValue("username"))
	if err := auth.ValidateUsername(username); err != nil {
		s.renderAdminUsers(w, r, http.StatusUnprocessableEntity, data{"Error": err.Error()})
		return
	}
	password := auth.RandomPassword()
	u, err := s.store.CreateUser(r.Context(), username, auth.HashPassword(password), r.PostFormValue("admin") != "")
	if errors.Is(err, store.ErrConflict) {
		s.renderAdminUsers(w, r, http.StatusUnprocessableEntity, data{"Error": "That username is taken."})
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.audit(r, store.AuditEntry{UserID: u.ID, ActorID: currentUser(r).ID, Event: store.EventUserCreated})
	s.showSecret(w, r, "users", "User "+u.Username+" created",
		"Give them this password. It is shown only once; they can change it under Settings.", password)
}

func (s *Server) adminUserAction(w http.ResponseWriter, r *http.Request) {
	me := currentUser(r)
	id, ok := idParam(r)
	if !ok {
		s.notFound(w, r)
		return
	}
	target, err := s.store.UserByID(r.Context(), id)
	if !s.storeError(w, r, err) {
		return
	}
	action := chi.URLParam(r, "action")
	if id == me.ID && action != "reset-password" {
		s.renderAdminUsers(w, r, http.StatusUnprocessableEntity,
			data{"Error": "Use Settings to change your own account."})
		return
	}
	entry := store.AuditEntry{UserID: id, ActorID: me.ID}
	switch action {
	case "disable":
		err, entry.Event = s.store.SetActive(r.Context(), id, false), store.EventUserDisabled
	case "enable":
		err, entry.Event = s.store.SetActive(r.Context(), id, true), store.EventUserEnabled
	case "make-admin":
		err, entry.Event = s.store.SetAdmin(r.Context(), id, true), store.EventAdminGranted
	case "remove-admin":
		err, entry.Event = s.store.SetAdmin(r.Context(), id, false), store.EventAdminRevoked
	case "reset-2fa":
		err, entry.Event = s.store.ResetMFA(r.Context(), id), store.EventMFAReset
	case "reset-password":
		password := auth.RandomPassword()
		if err := s.store.SetPassword(r.Context(), id, auth.HashPassword(password), nil); err != nil {
			s.serverError(w, r, err)
			return
		}
		entry.Event = store.EventPasswordReset
		s.audit(r, entry)
		if id == me.ID {
			s.clearSessionCookie(w)
		}
		s.showSecret(w, r, "users", "New password for "+target.Username,
			"They were signed out everywhere. Give them this password; it is shown only once.", password)
		return
	default:
		s.notFound(w, r)
		return
	}
	if isValidation(err) {
		s.renderAdminUsers(w, r, http.StatusUnprocessableEntity, data{"Error": err.Error()})
		return
	}
	if !s.storeError(w, r, err) {
		return
	}
	s.audit(r, entry)
	http.Redirect(w, r, "/admin/?done="+action, http.StatusSeeOther)
}

func (s *Server) adminInvites(w http.ResponseWriter, r *http.Request) {
	s.renderAdminInvites(w, r, http.StatusOK, data{"Notice": adminNotices[r.URL.Query().Get("done")]})
}

func (s *Server) renderAdminInvites(w http.ResponseWriter, r *http.Request, status int, d data) {
	invites, err := s.store.ListInvites(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	d["AdminTab"] = "invites"
	d["Invites"] = invites
	d["SignupMode"] = s.signupMode(r.Context())
	d["Now"] = time.Now()
	s.render(w, r, status, "admin_invites", d)
}

func (s *Server) adminCreateInvite(w http.ResponseWriter, r *http.Request) {
	token, hash := auth.NewToken(24)
	note := truncate(strings.TrimSpace(r.PostFormValue("note")), 100)
	if err := s.store.CreateInvite(r.Context(), currentUser(r).ID, note, hash, time.Now().Add(inviteTTL)); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.audit(r, store.AuditEntry{ActorID: currentUser(r).ID, Event: store.EventInviteCreated, Detail: note})
	msg := "Send this link to the person you're inviting. It works once and expires in 7 days. It is shown only once."
	if mode := s.signupMode(r.Context()); mode == "closed" {
		msg += " Note: signup is closed, so the link won't work until you allow invites under Instance."
	}
	s.showSecret(w, r, "invites", "Invite link created", msg, s.cfg.BaseURL.String()+"/signup?invite="+token)
}

func (s *Server) adminDeleteInvite(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r)
	if !ok {
		s.notFound(w, r)
		return
	}
	if !s.storeError(w, r, s.store.DeleteInvite(r.Context(), id)) {
		return
	}
	s.audit(r, store.AuditEntry{ActorID: currentUser(r).ID, Event: store.EventInviteRevoked})
	http.Redirect(w, r, "/admin/invites?done=invite-off", http.StatusSeeOther)
}

func (s *Server) adminSettings(w http.ResponseWriter, r *http.Request) {
	s.renderAdminSettings(w, r, http.StatusOK, data{"Notice": adminNotices[r.URL.Query().Get("done")]})
}

func (s *Server) renderAdminSettings(w http.ResponseWriter, r *http.Request, status int, d data) {
	d["AdminTab"] = "settings"
	d["SignupMode"] = s.signupMode(r.Context())
	d["Require2FA"] = s.require2FA(r.Context())
	d["SignupFromEnv"] = s.cfg.Signup != ""
	d["Require2FAFromEnv"] = s.cfg.Require2FA != nil
	s.render(w, r, status, "admin_settings", d)
}

func (s *Server) adminSaveSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var changed []string
	if s.cfg.Signup == "" {
		mode := r.PostFormValue("signup")
		valid := false
		for _, m := range store.SignupModes {
			valid = valid || m == mode
		}
		if !valid {
			s.renderAdminSettings(w, r, http.StatusUnprocessableEntity, data{"Error": "Unknown signup mode."})
			return
		}
		if err := s.store.SetInstanceSetting(ctx, store.SettingSignup, mode); err != nil {
			s.serverError(w, r, err)
			return
		}
		changed = append(changed, "signup="+mode)
	}
	if s.cfg.Require2FA == nil {
		req := strconv.FormatBool(r.PostFormValue("require_2fa") != "")
		if err := s.store.SetInstanceSetting(ctx, store.SettingRequire2FA, req); err != nil {
			s.serverError(w, r, err)
			return
		}
		changed = append(changed, "require_2fa="+req)
	}
	s.audit(r, store.AuditEntry{ActorID: currentUser(r).ID, Event: store.EventSettingsChanged, Detail: strings.Join(changed, ", ")})
	http.Redirect(w, r, "/admin/settings?done=settings", http.StatusSeeOther)
}

func (s *Server) adminAudit(w http.ResponseWriter, r *http.Request) {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	page = max(page, 1)
	const per = 100
	rows, more, err := s.store.ListAudit(r.Context(), per, (page-1)*per)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	d := data{"AdminTab": "audit", "Entries": rows}
	if page > 1 {
		d["PrevURL"] = "/admin/audit?page=" + strconv.Itoa(page-1)
	}
	if more {
		d["NextURL"] = "/admin/audit?page=" + strconv.Itoa(page+1)
	}
	s.render(w, r, http.StatusOK, "admin_audit", d)
}
