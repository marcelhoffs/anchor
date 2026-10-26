package web

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/marcelhoffs/anchor/internal/auth"
	"github.com/marcelhoffs/anchor/internal/i18n"
	"github.com/marcelhoffs/anchor/internal/store"
)

const inviteTTL = 7 * 24 * time.Hour

var adminNotices = map[string]i18n.Msg{
	"disable":       i18n.M("User disabled and signed out everywhere."),
	"enable":        i18n.M("User enabled."),
	"make-admin":    i18n.M("User is now an admin."),
	"remove-admin":  i18n.M("Admin rights removed."),
	"reset-2fa":     i18n.M("Two-factor authentication reset. The user was signed out everywhere."),
	"delete":        i18n.M("Account deleted."),
	"delete-spaces": i18n.M("Account deleted. The shared spaces they owned are now yours."),
	"invite-off":    i18n.M("Invite revoked."),
	"settings":      i18n.M("Settings saved."),
	"space-owner":   i18n.M("The space has a new owner."),
	"space-delete":  i18n.M("Space deleted."),
}

// adminSpaces lists every shared space: names, members and sizes, never
// the bookmarks inside.
func (s *Server) adminSpaces(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.AdminListSpaces(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	creation, _ := s.store.InstanceSetting(r.Context(), store.SettingSpaceCreation, "everyone")
	s.render(w, r, http.StatusOK, "admin_spaces", data{
		"AdminTab": "spaces", "SpaceList": list, "SpaceCreation": creation, "Notice": adminNotices[r.URL.Query().Get("done")],
	})
}

// adminSaveSpaceCreation sets who may create shared spaces.
func (s *Server) adminSaveSpaceCreation(w http.ResponseWriter, r *http.Request) {
	who := r.PostFormValue("space_creation")
	if who != "admins" {
		who = "everyone"
	}
	if err := s.store.SetInstanceSetting(r.Context(), store.SettingSpaceCreation, who); err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/admin/spaces?done=settings", http.StatusSeeOther)
}

// adminSpaceOwner hands a shared space to one of its members, for instance
// when its owner's account was disabled.
func (s *Server) adminSpaceOwner(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r)
	uid, err := strconv.ParseInt(r.PostFormValue("user_id"), 10, 64)
	if !ok || err != nil {
		s.notFound(w, r)
		return
	}
	if !s.storeError(w, r, s.store.AdminTransferSpace(r.Context(), id, uid)) {
		return
	}
	http.Redirect(w, r, "/admin/spaces?done=space-owner", http.StatusSeeOther)
}

func (s *Server) adminDeleteSpace(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r)
	if !ok {
		s.notFound(w, r)
		return
	}
	if !s.storeError(w, r, s.store.AdminDeleteSpace(r.Context(), id)) {
		return
	}
	http.Redirect(w, r, "/admin/spaces?done=space-delete", http.StatusSeeOther)
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
	linked, err := s.store.LinkedUsers(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	d["AdminTab"] = "users"
	d["Users"] = users
	d["Stats"] = stats
	d["Linked"] = linked // users who sign in with single sign-on
	s.render(w, r, status, "admin_users", d)
}

// showSecret renders a value that is shown exactly once (a generated
// password or an invite link).
func (s *Server) showSecret(w http.ResponseWriter, r *http.Request, tab string, title, message i18n.Msg, value string) {
	s.render(w, r, http.StatusOK, "admin_secret", data{
		"AdminTab": tab, "SecretTitle": title, "Message": message, "Value": value,
	})
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
			data{"Error": i18n.M("Use Settings to change your own account.")})
		return
	}
	if (action == "make-admin" || action == "remove-admin") && s.groupAdmins() {
		s.renderAdminUsers(w, r, http.StatusConflict, data{"Error": i18n.M("Admin rights come from the %s group at %s.", s.cfg.OIDC.AdminGroup, s.cfg.OIDC.Name)})
		return
	}
	switch action {
	case "disable":
		err = s.store.SetActive(r.Context(), id, false)
	case "enable":
		err = s.store.SetActive(r.Context(), id, true)
	case "make-admin":
		err = s.store.SetAdmin(r.Context(), id, true)
	case "remove-admin":
		err = s.store.SetAdmin(r.Context(), id, false)
	case "reset-2fa":
		err = s.store.ResetMFA(r.Context(), id)
	case "delete":
		var moved int
		if moved, err = s.store.DeleteUser(r.Context(), id, me.ID); moved > 0 {
			action = "delete-spaces"
		}
	case "reset-password":
		password := auth.RandomPassword()
		if err := s.store.SetPassword(r.Context(), id, auth.HashPassword(password), nil); err != nil {
			s.serverError(w, r, err)
			return
		}
		if id == me.ID {
			s.clearSessionCookie(w)
		}
		s.showSecret(w, r, "users", i18n.M("New password for %s", target.Username),
			i18n.M("They were signed out everywhere. Give them this password; it is shown only once."), password)
		return
	default:
		s.notFound(w, r)
		return
	}
	if isValidation(err) {
		s.renderAdminUsers(w, r, http.StatusUnprocessableEntity, data{"Error": errMsg(err)})
		return
	}
	if !s.storeError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/admin/users?done="+action, http.StatusSeeOther)
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
	if s.signupMode(r.Context()) == "closed" {
		// The form is hidden then; the page says why.
		http.Redirect(w, r, "/admin/invites", http.StatusSeeOther)
		return
	}
	token, hash := auth.NewToken(24)
	note := truncate(strings.TrimSpace(r.PostFormValue("note")), 100)
	if err := s.store.CreateInvite(r.Context(), currentUser(r).ID, note, hash, time.Now().Add(inviteTTL)); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.showSecret(w, r, "invites", i18n.M("Invite link created"),
		i18n.M("Send this link to the person you're inviting. It works once and expires in 7 days. It is shown only once."),
		s.cfg.BaseURL.String()+"/signup?invite="+token)
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
	if s.cfg.Signup == "" {
		mode := r.PostFormValue("signup")
		valid := false
		for _, m := range store.SignupModes {
			valid = valid || m == mode
		}
		if !valid {
			s.renderAdminSettings(w, r, http.StatusUnprocessableEntity, data{"Error": i18n.M("Unknown signup mode.")})
			return
		}
		if err := s.store.SetInstanceSetting(ctx, store.SettingSignup, mode); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	if s.cfg.Require2FA == nil {
		req := strconv.FormatBool(r.PostFormValue("require_2fa") != "")
		if err := s.store.SetInstanceSetting(ctx, store.SettingRequire2FA, req); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	http.Redirect(w, r, "/admin/?done=settings", http.StatusSeeOther)
}
