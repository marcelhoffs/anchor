package web

import (
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/marcelhoffs/anchor/internal/i18n"
	"github.com/marcelhoffs/anchor/internal/search"
	"github.com/marcelhoffs/anchor/internal/store"
	"github.com/marcelhoffs/anchor/internal/websearch"
)

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	sc := currentScope(r)
	q := r.URL.Query().Get("q")
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	page = max(page, 1)
	web := webSearchFor(r)
	// search.js goes to the engine itself; this is for browsers without it.
	if web != nil && q != "" && r.URL.Query().Get("in") == web.ID {
		http.Redirect(w, r, web.URL+url.QueryEscape(q), http.StatusSeeOther)
		return
	}

	list, total, err := s.store.Search(r.Context(), sc, search.Parse(q), currentUser(r).Settings.Sort, pageSize, (page-1)*pageSize)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	tags, err := s.store.ListTags(r.Context(), sc)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	counts, err := s.store.CountBookmarks(r.Context(), sc)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	searches, err := s.store.ListSavedSearches(r.Context(), sc)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	pageURL := func(p int) string {
		v := url.Values{}
		if q != "" {
			v.Set("q", q)
		}
		if p > 1 {
			v.Set("page", strconv.Itoa(p))
		}
		if len(v) == 0 {
			return "/"
		}
		return "/?" + v.Encode()
	}
	pages := (total + pageSize - 1) / pageSize
	if page > pages && pages > 0 {
		http.Redirect(w, r, pageURL(pages), http.StatusSeeOther)
		return
	}
	view, heading := listView(q, searches)
	here := *r.URL
	query := here.Query()
	for _, p := range listParams {
		query.Del(p)
	}
	here.RawQuery = query.Encode()
	d := data{
		"Query": q, "Bookmarks": list, "Tags": tags, "Counts": counts, "SavedSearches": searches, "Here": here.RequestURI(),
		"View": view, "Heading": heading, "WebSearch": web, "TagColors": tagColors(tags),
		"Space": sc.SpaceID(), "InArchive": search.Parse(q).Archived,
	}
	if sc.CanWrite() {
		targets, err := s.spaceTargets(r, sc)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		d["Targets"] = targets
	}
	s.listNotice(r, sc, d)
	if pages > 1 {
		d["Pager"] = newPager(page, pages, total, pageURL)
	}
	s.render(w, r, http.StatusOK, "list", d)
}

// tagColors maps each tag's name to its colour, for the tags on the rows.
func tagColors(tags []store.Tag) map[string]string {
	out := make(map[string]string, len(tags))
	for _, t := range tags {
		out[t.Name] = t.Color
	}
	return out
}

// webSearch is the search box's web option.
type webSearch struct {
	websearch.Engine
	URL string // the engine's results page, to append the encoded query to
}

// webSearchFor is the user's web search, or nil while it is off.
func webSearchFor(r *http.Request) *webSearch {
	st := currentUser(r).Settings
	if st.WebSearch != "on" {
		return nil
	}
	e, _ := websearch.Lookup(st.Engine)
	locale := st.SearchLocale
	if locale == "auto" {
		locale = websearch.Match(r.Header.Get("Accept-Language"))
	}
	return &webSearch{e, websearch.URL(e.ID, "", locale, st.SafeSearch == "on")}
}

// pager is what the list template needs to draw page controls.
type pager struct {
	From, To, Total int
	PrevURL         string // "" on the first page
	NextURL         string // "" on the last page
}

// newPager links the pages either side of the current one.
func newPager(page, pages, total int, pageURL func(int) string) pager {
	p := pager{From: (page-1)*pageSize + 1, To: min(page*pageSize, total), Total: total}
	if page > 1 {
		p.PrevURL = pageURL(page - 1)
	}
	if page < pages {
		p.NextURL = pageURL(page + 1)
	}
	return p
}

// listView names the sidebar entry a query corresponds to ("all", "fav",
// "archive", "#<tag>", "search:<id>", or "" for any other search) and the
// page heading.
func listView(q string, searches []store.SavedSearch) (view string, heading i18n.Msg) {
	switch q = strings.TrimSpace(q); {
	case q == "":
		return "all", i18n.M("All bookmarks")
	case q == "!fav":
		return "fav", i18n.M("Favourites")
	case q == "!archived":
		return "archive", i18n.M("Archive")
	case q == "!untagged":
		return "", i18n.M("Untagged bookmarks")
	}
	if p := search.Parse(q); len(p.Tags) == 1 && search.TagQuery(p.Tags[0]) == q {
		return "#" + p.Tags[0], i18n.Raw(p.Tags[0])
	}
	for _, b := range searches {
		if b.Query == q {
			return "search:" + strconv.FormatInt(b.ID, 10), i18n.Raw(b.Name)
		}
	}
	return "", i18n.M("Search results")
}

func bookmarkForm(r *http.Request) store.BookmarkInput {
	return store.BookmarkInput{
		URL:         r.PostFormValue("url"),
		Title:       r.PostFormValue("title"),
		Description: r.PostFormValue("description"),
		Notes:       r.PostFormValue("notes"),
		Tags:        []string{r.PostFormValue("tags")},
		IsFavourite: r.PostFormValue("favourite") != "",
	}
}

func formData(in store.BookmarkInput) data {
	return data{
		"URL": in.URL, "BTitle": in.Title, "Description": in.Description, "Notes": in.Notes,
		"TagText": strings.Join(store.CleanTags(in.Tags), ", "), "Favourite": in.IsFavourite,
	}
}

// bookmarkScope is the user's scope in the space that holds bookmark id;
// false after answering (404 when they aren't a member there).
func (s *Server) bookmarkScope(w http.ResponseWriter, r *http.Request, id int64) (store.Scope, bool) {
	sc, err := s.store.BookmarkScope(r.Context(), currentUser(r).ID, id)
	return sc, s.storeError(w, r, err)
}

// renderEdit shows the add/edit form for a bookmark in sc's space. The tags
// field suggests the space's tags as they type, most used first.
func (s *Server) renderEdit(w http.ResponseWriter, r *http.Request, sc store.Scope, status int, d data) {
	tags, err := s.store.ListTags(r.Context(), sc)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	slices.SortStableFunc(tags, func(a, b store.Tag) int { return b.Count - a.Count })
	names, colors := make([]string, len(tags)), make([]string, len(tags))
	for i, t := range tags {
		names[i], colors[i] = t.Name, t.Color
	}
	d["TagNames"] = strings.Join(names, ",") // tag names never contain commas
	d["TagColors"] = strings.Join(colors, ",")
	s.render(w, r, status, "edit", d)
}

// addTarget is the space a new bookmark goes into, and its name: the space
// the user is looking at when they can add to it, else their personal one.
// A form names its target in "space", so a switch in another tab between
// showing the form and saving it changes nothing.
func (s *Server) addTarget(r *http.Request) (store.Scope, i18n.Msg, error) {
	nav := currentNav(r)
	if id, err := strconv.ParseInt(r.PostFormValue("space"), 10, 64); err == nil && id > 0 {
		sc, err := s.store.Access(r.Context(), currentUser(r).ID, id)
		if err != nil {
			return store.Scope{}, i18n.Msg{}, err
		}
		for _, sp := range nav.Spaces {
			if sp.ID == id {
				return sc, i18n.Raw(sp.Name), nil
			}
		}
		return sc, i18n.M("Personal"), nil
	}
	if nav.Scope.CanWrite() {
		return nav.Scope, nav.label(), nil
	}
	sc, err := s.store.PersonalScope(r.Context(), currentUser(r).ID)
	return sc, i18n.M("Personal"), err
}

// newBookmark shows the add form, prefilled from ?url=&title=&description=
// (which is how the bookmarklet calls it), or from Android's share sheet,
// which adds &text= (see sharedLink). An already-saved URL goes straight to
// its edit page.
func (s *Server) newBookmark(w http.ResponseWriter, r *http.Request) {
	sc, name, err := s.addTarget(r)
	if !s.storeError(w, r, err) {
		return
	}
	q := r.URL.Query()
	raw, description := sharedLink(q.Get("url"), q.Get("title"), q.Get("description"), q.Get("text"))
	if raw != "" {
		b, err := s.store.FindBookmarkByURL(r.Context(), sc, raw)
		if err == nil {
			http.Redirect(w, r, "/bookmarks/"+strconv.FormatInt(b.ID, 10)+"/edit?dup=1", http.StatusSeeOther)
			return
		}
		if !errors.Is(err, store.ErrNotFound) {
			s.serverError(w, r, err)
			return
		}
	}
	d := formData(store.BookmarkInput{URL: raw, Title: q.Get("title"), Description: description})
	d["Next"] = safeNext(q.Get("next"))
	d["Target"], d["TargetName"] = sc.SpaceID(), name
	s.renderEdit(w, r, sc, http.StatusOK, d)
}

func (s *Server) createBookmark(w http.ResponseWriter, r *http.Request) {
	sc, name, err := s.addTarget(r)
	if !s.storeError(w, r, err) {
		return
	}
	in := bookmarkForm(r)
	b, created, err := s.store.CreateBookmark(r.Context(), sc, in)
	if isValidation(err) {
		d := formData(in)
		d["Error"] = errMsg(err)
		d["Next"] = safeNext(r.PostFormValue("next"))
		d["Target"], d["TargetName"] = sc.SpaceID(), name
		s.renderEdit(w, r, sc, http.StatusUnprocessableEntity, d)
		return
	}
	if !s.storeError(w, r, err) {
		return
	}
	if !created {
		http.Redirect(w, r, "/bookmarks/"+strconv.FormatInt(b.ID, 10)+"/edit?dup=1", http.StatusSeeOther)
		return
	}
	s.fetcher.Notify()
	http.Redirect(w, r, safeNext(r.PostFormValue("next")), http.StatusSeeOther)
}

func (s *Server) editBookmark(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r)
	if !ok {
		s.notFound(w, r)
		return
	}
	sc, ok := s.bookmarkScope(w, r, id)
	if !ok {
		return
	}
	if !sc.CanWrite() {
		s.storeError(w, r, store.ErrForbidden) // a reader has nothing to edit here
		return
	}
	b, err := s.store.GetBookmark(r.Context(), sc, id)
	if !s.storeError(w, r, err) {
		return
	}
	d := formData(store.BookmarkInput{
		URL: b.URL, Title: b.Title, Description: b.Description, Notes: b.Notes,
		Tags: b.Tags, IsFavourite: b.IsFavourite,
	})
	d["Bookmark"] = b
	d["Next"] = safeNext(r.URL.Query().Get("next"))
	if b.IsShared {
		d["ShareURL"] = s.shareURL(b)
	}
	if r.URL.Query().Get("dup") != "" {
		d["Notice"] = i18n.M("You already saved this URL.")
	} else if n := bookmarkNotices[r.URL.Query().Get("done")]; n.Format != "" {
		d["Notice"] = n
	}
	s.renderEdit(w, r, sc, http.StatusOK, d)
}

func (s *Server) updateBookmark(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r)
	if !ok {
		s.notFound(w, r)
		return
	}
	sc, ok := s.bookmarkScope(w, r, id)
	if !ok {
		return
	}
	in := bookmarkForm(r)
	_, err := s.store.UpdateBookmark(r.Context(), sc, id, in)
	if errors.Is(err, store.ErrConflict) || isValidation(err) {
		b, gerr := s.store.GetBookmark(r.Context(), sc, id)
		if !s.storeError(w, r, gerr) {
			return
		}
		d := formData(in)
		d["Bookmark"] = b
		d["Next"] = safeNext(r.PostFormValue("next"))
		if errors.Is(err, store.ErrConflict) {
			d["Error"] = i18n.M("Another bookmark already has this URL.")
		} else {
			d["Error"] = errMsg(err)
		}
		s.renderEdit(w, r, sc, http.StatusUnprocessableEntity, d)
		return
	}
	if !s.storeError(w, r, err) {
		return
	}
	s.fetcher.Notify() // in case the URL changed
	http.Redirect(w, r, safeNext(r.PostFormValue("next")), http.StatusSeeOther)
}

var bookmarkNotices = map[string]i18n.Msg{
	"shared":    i18n.M("Anyone with the link can now see this bookmark's title, link and description."),
	"unshared":  i18n.M("The public link no longer works."),
	"refetched": i18n.M("Fetching the page again. Empty fields and the icon are filled in when it's done."),
}

// shareURL is a shared bookmark's public link.
func (s *Server) shareURL(b store.Bookmark) string {
	return s.cfg.BaseURL.String() + "/s/" + b.ShareToken
}

func (s *Server) shareBookmark(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r)
	if !ok {
		s.notFound(w, r)
		return
	}
	sc, ok := s.bookmarkScope(w, r, id)
	if !ok {
		return
	}
	on := r.PostFormValue("on") == "1"
	if !s.storeError(w, r, s.store.SetShared(r.Context(), sc, id, on)) {
		return
	}
	done := "unshared"
	if on {
		done = "shared"
	}
	http.Redirect(w, r, "/bookmarks/"+strconv.FormatInt(id, 10)+"/edit?done="+done+"&next="+url.QueryEscape(safeNext(r.PostFormValue("next"))), http.StatusSeeOther)
}

func (s *Server) refetchBookmark(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r)
	if !ok {
		s.notFound(w, r)
		return
	}
	sc, ok := s.bookmarkScope(w, r, id)
	if !ok {
		return
	}
	if !s.storeError(w, r, s.store.Refetch(r.Context(), sc, id)) {
		return
	}
	s.fetcher.Notify()
	http.Redirect(w, r, "/bookmarks/"+strconv.FormatInt(id, 10)+"/edit?done=refetched&next="+url.QueryEscape(safeNext(r.PostFormValue("next"))), http.StatusSeeOther)
}

// sharedBookmark is the public page behind a share link.
func (s *Server) sharedBookmark(w http.ResponseWriter, r *http.Request) {
	b, err := s.store.SharedBookmark(r.Context(), chi.URLParam(r, "token"))
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	w.Header().Set("X-Robots-Tag", "noindex")
	w.Header().Set("Referrer-Policy", "no-referrer")
	s.render(w, r, http.StatusOK, "shared", data{"Shared": b})
}

// favicon serves a stored site icon to a user who can see a bookmark using it.
func (s *Server) favicon(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r)
	if !ok {
		s.notFound(w, r)
		return
	}
	mime, img, err := s.store.Favicon(r.Context(), currentUser(r).ID, id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Write(img)
}

func (s *Server) deleteBookmark(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r)
	if !ok {
		s.notFound(w, r)
		return
	}
	sc, ok := s.bookmarkScope(w, r, id)
	if !ok {
		return
	}
	if !s.storeError(w, r, s.store.DeleteBookmark(r.Context(), sc, id)) {
		return
	}
	http.Redirect(w, r, safeNext(r.PostFormValue("next")), http.StatusSeeOther)
}

// bulkFlags are the toolbar's flag actions: the flag and its new value.
var bulkFlags = map[string]struct {
	flag store.Flag
	on   bool
}{
	"favourite":   {store.FlagFavourite, true},
	"unfavourite": {store.FlagFavourite, false},
	"archive":     {store.FlagArchived, true},
	"unarchive":   {store.FlagArchived, false},
}

// listNotices are the list's notices after a toolbar action (?done=).
var listNotices = map[string]i18n.Msg{
	"none":     i18n.M("Select one or more bookmarks first."),
	"edit-one": i18n.M("Select one bookmark to edit."),
}

// bulkBookmarks does one of the list toolbar's actions to the selected
// bookmarks (the "id" fields), in the space the list showed ("from"), and
// goes back to the list. The action is in "action"; copy and move name
// their target in "space", and tag (which toggles it) its id in "tag".
func (s *Server) bulkBookmarks(w http.ResponseWriter, r *http.Request) {
	ctx, uid := r.Context(), currentUser(r).ID
	fromID, _ := strconv.ParseInt(r.PostFormValue("from"), 10, 64)
	sc, err := s.store.Access(ctx, uid, fromID)
	if !s.storeError(w, r, err) {
		return
	}
	var ids []int64
	for _, v := range r.PostForm["id"] {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			ids = append(ids, id)
		}
	}
	next := safeNext(r.PostFormValue("next"))
	back := func(params url.Values) {
		u, _ := url.Parse(next) // safeNext parsed it already
		v := u.Query()
		for k := range params {
			v.Set(k, params.Get(k))
		}
		u.RawQuery = v.Encode()
		http.Redirect(w, r, u.String(), http.StatusSeeOther)
	}
	action := r.FormValue("action")
	if len(ids) == 0 && action != "" {
		back(url.Values{"done": {"none"}})
		return
	}
	switch action {
	case "edit":
		if len(ids) != 1 {
			back(url.Values{"done": {"edit-one"}})
			return
		}
		http.Redirect(w, r, "/bookmarks/"+strconv.FormatInt(ids[0], 10)+"/edit?next="+url.QueryEscape(next), http.StatusSeeOther)
	case "favourite", "unfavourite", "archive", "unarchive":
		f := bulkFlags[action]
		if _, err := s.store.SetFlags(ctx, sc, ids, f.flag, f.on); !s.storeError(w, r, err) {
			return
		}
		if action != "archive" {
			back(nil)
			return
		}
		// The list offers to undo it.
		list := make([]string, len(ids))
		for i, id := range ids {
			list[i] = strconv.FormatInt(id, 10)
		}
		back(url.Values{"archived": {strings.Join(list, ",")}})
	case "delete":
		if _, err := s.store.DeleteBookmarks(ctx, sc, ids); !s.storeError(w, r, err) {
			return
		}
		back(nil)
	case "copy", "move":
		toID, _ := strconv.ParseInt(r.FormValue("space"), 10, 64)
		to, err := s.store.Access(ctx, uid, toID)
		if !s.storeError(w, r, err) {
			return
		}
		done, skipped, err := s.store.CopyBookmarks(ctx, sc, to, ids, action == "move")
		if !s.storeError(w, r, err) {
			return
		}
		back(url.Values{map[string]string{"copy": "copied", "move": "moved"}[action]: {strconv.Itoa(done)}, "skipped": {strconv.Itoa(skipped)}, "to": {strconv.FormatInt(toID, 10)}})
	case "tag":
		tagID, _ := strconv.ParseInt(r.FormValue("tag"), 10, 64)
		if _, _, err := s.store.ToggleTags(ctx, sc, ids, tagID); !s.storeError(w, r, err) {
			return
		}
		back(nil)
	default:
		http.Error(w, langOf(r).T("Unknown action"), http.StatusBadRequest)
	}
}

// listParams are the query parameters that carry a notice to the list; its
// own links leave them out.
var listParams = []string{"archived", "done", "copied", "moved", "skipped", "to"}

// spaceTarget is a space the list's bookmarks can be copied or moved to.
type spaceTarget struct {
	ID   int64
	Name string // "" for the personal space
	Icon string
}

// spaceTargets are the spaces other than sc's that the user can add to:
// their personal space first, then the shared ones by name.
func (s *Server) spaceTargets(r *http.Request, sc store.Scope) ([]spaceTarget, error) {
	var out []spaceTarget
	if !sc.IsPersonal() {
		own, err := s.store.PersonalScope(r.Context(), currentUser(r).ID)
		if err != nil {
			return nil, err
		}
		out = append(out, spaceTarget{ID: own.SpaceID()})
	}
	for _, sp := range currentNav(r).Spaces {
		if sp.ID != sc.SpaceID() && (sp.Role == store.RoleWrite || sp.Role == store.RoleOwner) {
			out = append(out, spaceTarget{sp.ID, sp.Name, sp.Icon})
		}
	}
	return out, nil
}

// copyNotice is the list's notice after copying or moving bookmarks.
type copyNotice struct {
	Moved         bool
	Done, Skipped int
	Space         spaceTarget
}

// listNotice fills in the list's notice after a toolbar action, from the
// page's query: an undo after archiving, the outcome of a copy or move, or
// a hint about the selection.
func (s *Server) listNotice(r *http.Request, sc store.Scope, d data) {
	q := r.URL.Query()
	if n, ok := listNotices[q.Get("done")]; ok {
		d["Notice"] = n
	}
	if !sc.CanWrite() {
		return
	}
	// Just archived from this page (see bulkBookmarks):
	// offer to undo it, for those still archived.
	var undo []store.Bookmark
	for _, v := range strings.Split(q.Get("archived"), ",") {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || len(undo) == store.MaxBatch {
			continue
		}
		if b, err := s.store.GetBookmark(r.Context(), sc, id); err == nil && b.IsArchived {
			undo = append(undo, b)
		}
	}
	if len(undo) > 0 {
		d["Undo"] = undo
	}
	for _, action := range []string{"copied", "moved"} {
		done, err := strconv.Atoi(q.Get(action))
		if err != nil {
			continue
		}
		skipped, _ := strconv.Atoi(q.Get("skipped"))
		to, _ := strconv.ParseInt(q.Get("to"), 10, 64)
		n := copyNotice{Moved: action == "moved", Done: done, Skipped: skipped, Space: spaceTarget{ID: to}}
		for _, sp := range currentNav(r).Spaces {
			if sp.ID == to {
				n.Space.Name = sp.Name
			}
		}
		d["Copied"] = n
	}
}

// tagURL is the list filtered to one tag.
func tagURL(name string) string { return "/?q=" + url.QueryEscape(search.TagQuery(name)) }

// tagBookmark adds a tag to a bookmark: the list page calls it when a
// bookmark's icon is dropped on a tag. It answers with the bookmark's tags,
// to redraw them, and the tag's new count.
func (s *Server) tagBookmark(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r)
	tagID, err := strconv.ParseInt(r.PostFormValue("tag"), 10, 64)
	if !ok || err != nil {
		s.notFound(w, r)
		return
	}
	sc, ok := s.bookmarkScope(w, r, id)
	if !ok {
		return
	}
	if _, err := s.store.AddTag(r.Context(), sc, id, tagID); !s.storeError(w, r, err) {
		return
	}
	b, err := s.store.GetBookmark(r.Context(), sc, id)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	tags, err := s.store.ListTags(r.Context(), sc)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	type tagLink struct {
		Name  string `json:"name"`
		URL   string `json:"url"`
		Color string `json:"color"`
	}
	out := struct {
		Tags  []tagLink `json:"tags"`
		Count int       `json:"count"`
	}{Tags: []tagLink{}}
	colors := tagColors(tags)
	for _, name := range b.Tags {
		out.Tags = append(out.Tags, tagLink{name, tagURL(name), colors[name]})
	}
	for _, t := range tags {
		if t.ID == tagID {
			out.Count = t.Count
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) tags(w http.ResponseWriter, r *http.Request) {
	s.renderTags(w, r, http.StatusOK, i18n.Msg{})
}

func (s *Server) renderTags(w http.ResponseWriter, r *http.Request, status int, problem i18n.Msg) {
	tags, err := s.store.ListTags(r.Context(), currentScope(r))
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, status, "tags", data{"Tags": tags, "Error": problem, "Colors": choices(langOf(r), store.TagColors, tagColorLabels)})
}

// tagColorLabels names each store.TagColors value on the Tags page.
var tagColorLabels = map[string]i18n.Msg{
	"blue": i18n.M("Blue"), "teal": i18n.M("Teal"), "green": i18n.M("Green"), "amber": i18n.M("Amber"),
	"orange": i18n.M("Orange"), "rose": i18n.M("Rose"), "purple": i18n.M("Purple"), "slate": i18n.M("Slate"),
}

func (s *Server) colorTag(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r)
	if !ok {
		s.notFound(w, r)
		return
	}
	err := s.store.SetTagColor(r.Context(), currentScope(r), id, r.PostFormValue("color"))
	if isValidation(err) {
		s.renderTags(w, r, http.StatusUnprocessableEntity, errMsg(err))
		return
	}
	if !s.storeError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/tags", http.StatusSeeOther)
}

func (s *Server) createTag(w http.ResponseWriter, r *http.Request) {
	_, err := s.store.CreateTag(r.Context(), currentScope(r), r.PostFormValue("name"))
	if isValidation(err) {
		s.renderTags(w, r, http.StatusUnprocessableEntity, errMsg(err))
		return
	}
	if !s.storeError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/tags", http.StatusSeeOther)
}

func (s *Server) renameTag(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r)
	if !ok {
		s.notFound(w, r)
		return
	}
	err := s.store.RenameTag(r.Context(), currentScope(r), id, r.PostFormValue("name"))
	if isValidation(err) {
		s.renderTags(w, r, http.StatusUnprocessableEntity, errMsg(err))
		return
	}
	if !s.storeError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/tags", http.StatusSeeOther)
}

func (s *Server) deleteTag(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r)
	if !ok {
		s.notFound(w, r)
		return
	}
	if !s.storeError(w, r, s.store.DeleteTag(r.Context(), currentScope(r), id)) {
		return
	}
	http.Redirect(w, r, "/tags", http.StatusSeeOther)
}

// isValidation reports whether err is an input problem worth showing to the
// user rather than an internal failure.
func isValidation(err error) bool {
	var v store.ValidationError
	return errors.As(err, &v)
}
