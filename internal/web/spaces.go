package web

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/marcelhoffs/anchor/internal/store"
)

var spaceNotices = map[string]string{
	"created":     "Space created. Add the people you want to share it with.",
	"renamed":     "Space renamed.",
	"added":       "Member added.",
	"updated":     "Role changed.",
	"removed":     "Member removed.",
	"transferred": "The space has a new owner. You can still edit it.",
	"left":        "You left the space.",
	"deleted":     "Space deleted.",
}

// canCreateSpace reports whether the user may create shared spaces: admins
// always, everyone else unless an admin limited it to admins.
func (s *Server) canCreateSpace(r *http.Request) bool {
	if currentUser(r).IsAdmin {
		return true
	}
	who, err := s.store.InstanceSetting(r.Context(), store.SettingSpaceCreation, "everyone")
	return err == nil && who != "admins"
}

func (s *Server) spaces(w http.ResponseWriter, r *http.Request) {
	s.renderSpaces(w, r, http.StatusOK, data{"Notice": spaceNotices[r.URL.Query().Get("done")]})
}

func (s *Server) renderSpaces(w http.ResponseWriter, r *http.Request, status int, d data) {
	list, err := s.store.ListSpaces(r.Context(), currentUser(r).ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	d["Spaces"] = list
	d["CanCreate"] = s.canCreateSpace(r)
	s.render(w, r, status, "spaces", d)
}

func (s *Server) createSpace(w http.ResponseWriter, r *http.Request) {
	if !s.canCreateSpace(r) {
		http.Error(w, "Only admins can create spaces.", http.StatusForbidden)
		return
	}
	sc, err := s.store.CreateSpace(r.Context(), currentUser(r).ID, r.PostFormValue("name"))
	if isValidation(err) {
		s.renderSpaces(w, r, http.StatusUnprocessableEntity, data{"Error": err.Error()})
		return
	}
	if !s.storeError(w, r, err) {
		return
	}
	http.Redirect(w, r, spaceURL(sc.SpaceID())+"?done=created", http.StatusSeeOther)
}

func spaceURL(id int64) string { return "/spaces/" + strconv.FormatInt(id, 10) }

// selectSpace switches the space this session looks at: "space" is a shared
// space's id, or empty for the personal space. Then the list shows it.
func (s *Server) selectSpace(w http.ResponseWriter, r *http.Request) {
	ctx, u := r.Context(), currentUser(r)
	var sc store.Scope
	var err error
	if id, perr := strconv.ParseInt(r.PostFormValue("space"), 10, 64); perr == nil && id > 0 {
		sc, err = s.store.Access(ctx, u.ID, id)
	} else {
		sc, err = s.store.PersonalScope(ctx, u.ID)
	}
	if !s.storeError(w, r, err) || !s.storeError(w, r, s.store.SelectSpace(ctx, currentSession(r).TokenHash, sc)) {
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// sharedScope is the user's scope in the shared space in the URL; false
// after answering 404 when they aren't a member (or it's a personal space).
func (s *Server) sharedScope(w http.ResponseWriter, r *http.Request) (store.Scope, bool) {
	id, ok := idParam(r)
	if !ok {
		s.notFound(w, r)
		return store.Scope{}, false
	}
	sc, err := s.store.Access(r.Context(), currentUser(r).ID, id)
	if err == nil && sc.IsPersonal() {
		err = store.ErrNotFound
	}
	return sc, s.storeError(w, r, err)
}

func (s *Server) space(w http.ResponseWriter, r *http.Request) {
	sc, ok := s.sharedScope(w, r)
	if !ok {
		return
	}
	s.renderSpace(w, r, sc, http.StatusOK, data{"Notice": spaceNotices[r.URL.Query().Get("done")]})
}

func (s *Server) renderSpace(w http.ResponseWriter, r *http.Request, sc store.Scope, status int, d data) {
	sp, err := s.store.GetSpace(r.Context(), sc)
	if !s.storeError(w, r, err) {
		return
	}
	members, err := s.store.Members(r.Context(), sc)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	d["Space"] = sp
	d["Members"] = members
	d["IsOwner"] = sc.Role() == store.RoleOwner
	s.render(w, r, status, "space", d)
}

// spaceDone answers a space change: back to the space page with a notice,
// or the page again with the problem when the input was refused.
func (s *Server) spaceDone(w http.ResponseWriter, r *http.Request, sc store.Scope, err error, done string) {
	if isValidation(err) {
		s.renderSpace(w, r, sc, http.StatusUnprocessableEntity, data{"Error": err.Error()})
		return
	}
	if !s.storeError(w, r, err) {
		return
	}
	http.Redirect(w, r, spaceURL(sc.SpaceID())+"?done="+done, http.StatusSeeOther)
}

func (s *Server) renameSpace(w http.ResponseWriter, r *http.Request) {
	sc, ok := s.sharedScope(w, r)
	if !ok {
		return
	}
	s.spaceDone(w, r, sc, s.store.RenameSpace(r.Context(), sc, r.PostFormValue("name")), "renamed")
}

func (s *Server) addMember(w http.ResponseWriter, r *http.Request) {
	sc, ok := s.sharedScope(w, r)
	if !ok {
		return
	}
	err := s.store.AddMember(r.Context(), sc, r.PostFormValue("username"), store.Role(r.PostFormValue("role")))
	s.spaceDone(w, r, sc, err, "added")
}

func memberParam(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "uid"), 10, 64)
	return id, err == nil && id > 0
}

// memberAction changes a member's role (write, read) or removes them.
func (s *Server) memberAction(w http.ResponseWriter, r *http.Request) {
	sc, ok := s.sharedScope(w, r)
	if !ok {
		return
	}
	uid, ok := memberParam(r)
	if !ok {
		s.notFound(w, r)
		return
	}
	switch action := chi.URLParam(r, "action"); action {
	case "write", "read":
		s.spaceDone(w, r, sc, s.store.SetRole(r.Context(), sc, uid, store.Role(action)), "updated")
	case "remove":
		s.spaceDone(w, r, sc, s.store.RemoveMember(r.Context(), sc, uid), "removed")
	default:
		s.notFound(w, r)
	}
}

func (s *Server) transferSpace(w http.ResponseWriter, r *http.Request) {
	sc, ok := s.sharedScope(w, r)
	if !ok {
		return
	}
	uid, ok := memberParam(r)
	if !ok {
		s.notFound(w, r)
		return
	}
	s.spaceDone(w, r, sc, s.store.TransferSpace(r.Context(), sc, uid), "transferred")
}

func (s *Server) leaveSpace(w http.ResponseWriter, r *http.Request) {
	sc, ok := s.sharedScope(w, r)
	if !ok {
		return
	}
	if err := s.store.LeaveSpace(r.Context(), sc); err != nil {
		s.spaceDone(w, r, sc, err, "")
		return
	}
	http.Redirect(w, r, "/spaces?done=left", http.StatusSeeOther)
}

func (s *Server) deleteSpace(w http.ResponseWriter, r *http.Request) {
	sc, ok := s.sharedScope(w, r)
	if !ok {
		return
	}
	if !s.storeError(w, r, s.store.DeleteSpace(r.Context(), sc)) {
		return
	}
	http.Redirect(w, r, "/spaces?done=deleted", http.StatusSeeOther)
}
