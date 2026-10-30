package web

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/marcelhoffs/anchor/internal/i18n"
	"github.com/marcelhoffs/anchor/internal/store"
)

var spaceNotices = map[string]i18n.Msg{
	"created":     i18n.M("Space created. Add the people you want to share it with."),
	"renamed":     i18n.M("Space renamed."),
	"icon":        i18n.M("Space icon changed."),
	"added":       i18n.M("Member added."),
	"updated":     i18n.M("Role changed."),
	"removed":     i18n.M("Member removed."),
	"transferred": i18n.M("The space has a new owner. You can still edit it."),
	"left":        i18n.M("You left the space."),
	"deleted":     i18n.M("Space deleted."),
}

// canCreateSpace reports whether the user may create shared spaces: admins
// always, everyone else unless an admin limited it to admins.
func (s *Server) canCreateSpace(r *http.Request) bool {
	if s.isAdmin(r) {
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
		http.Error(w, langOf(r).T("Only admins can create spaces."), http.StatusForbidden)
		return
	}
	sc, err := s.store.CreateSpace(r.Context(), currentUser(r).ID, r.PostFormValue("name"))
	if isValidation(err) {
		s.renderSpaces(w, r, http.StatusUnprocessableEntity, data{"Error": errMsg(err)})
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
	d["Icons"] = choices(langOf(r), append([]string{""}, store.SpaceIcons...), spaceIconLabels)
	s.render(w, r, status, "space", d)
}

// spaceDone answers a space change: back to the space page with a notice,
// or the page again with the problem when the input was refused.
func (s *Server) spaceDone(w http.ResponseWriter, r *http.Request, sc store.Scope, err error, done string) {
	if isValidation(err) {
		s.renderSpace(w, r, sc, http.StatusUnprocessableEntity, data{"Error": errMsg(err)})
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

func (s *Server) setSpaceIcon(w http.ResponseWriter, r *http.Request) {
	sc, ok := s.sharedScope(w, r)
	if !ok {
		return
	}
	s.spaceDone(w, r, sc, s.store.SetSpaceIcon(r.Context(), sc, r.PostFormValue("icon")), "icon")
}

// spaceIconLabels names the default ("") and each store.SpaceIcons value on
// a space's page.
var spaceIconLabels = map[string]i18n.Msg{
	"": i18n.M("Default"), "briefcase": i18n.M("Briefcase"), "house": i18n.M("House"), "users": i18n.M("People"),
	"folder": i18n.M("Folder"), "book-open": i18n.M("Book"), "graduation-cap": i18n.M("Graduation cap"),
	"code": i18n.M("Code"), "server": i18n.M("Server"), "cpu": i18n.M("Processor"), "globe": i18n.M("Globe"),
	"heart": i18n.M("Heart"), "heart-pulse": i18n.M("Heartbeat"), "stethoscope": i18n.M("Stethoscope"),
	"hospital": i18n.M("Hospital"), "ambulance": i18n.M("Ambulance"), "siren": i18n.M("Police siren"),
	"rocket": i18n.M("Rocket"), "shopping-cart": i18n.M("Shopping cart"), "music": i18n.M("Music"),
	"camera": i18n.M("Camera"), "gamepad-2": i18n.M("Game controller"), "wrench": i18n.M("Wrench"),
	"flask-conical": i18n.M("Flask"), "leaf": i18n.M("Leaf"), "building-2": i18n.M("Office building"),
	"trophy": i18n.M("Trophy"), "volleyball": i18n.M("Ball"), "sport-shoe": i18n.M("Running shoe"),
	"dumbbell": i18n.M("Dumbbell"), "bike": i18n.M("Bicycle"), "car": i18n.M("Car"), "bus": i18n.M("Bus"),
	"truck": i18n.M("Lorry"), "train-front": i18n.M("Train"), "plane": i18n.M("Aeroplane"),
	"plane-takeoff": i18n.M("Departing aeroplane"), "tower-control": i18n.M("Airport control tower"),
	"sailboat": i18n.M("Sailing boat"), "ship": i18n.M("Ship"),
}

func (s *Server) addMember(w http.ResponseWriter, r *http.Request) {
	sc, ok := s.sharedScope(w, r)
	if !ok {
		return
	}
	err := s.store.AddMember(r.Context(), sc, r.PostFormValue("username"), store.Role(r.PostFormValue("role")))
	s.spaceDone(w, r, sc, err, "added")
}

// memberSuggestions answers the add-member field's type-ahead with JSON:
// usernames matching ?q= that aren't in the space yet. Owners only.
func (s *Server) memberSuggestions(w http.ResponseWriter, r *http.Request) {
	sc, ok := s.sharedScope(w, r)
	if !ok {
		return
	}
	names, err := s.store.SuggestMembers(r.Context(), sc, r.URL.Query().Get("q"))
	if !s.storeError(w, r, err) {
		return
	}
	if names == nil {
		names = []string{}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(names)
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
