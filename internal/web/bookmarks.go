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
)

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	sc := currentScope(r)
	q := r.URL.Query().Get("q")
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	page = max(page, 1)

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
	bundles, err := s.store.ListBundles(r.Context(), sc)
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
	view, heading := listView(q, bundles)
	d := data{
		"Query": q, "Bookmarks": list, "Tags": tags, "Counts": counts, "Bundles": bundles, "Here": r.URL.RequestURI(),
		"View": view, "Heading": heading,
	}
	if pages > 1 {
		d["Pager"] = newPager(page, pages, total, pageURL)
	}
	s.render(w, r, http.StatusOK, "list", d)
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
// "archive", "#<tag>", "bundle:<id>", or "" for any other search) and the
// page heading.
func listView(q string, bundles []store.Bundle) (view string, heading i18n.Msg) {
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
	for _, b := range bundles {
		if b.Query == q {
			return "bundle:" + strconv.FormatInt(b.ID, 10), i18n.Raw(b.Name)
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
	names := make([]string, len(tags))
	for i, t := range tags {
		names[i] = t.Name
	}
	d["TagNames"] = strings.Join(names, ",") // tag names never contain commas
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
		Name string `json:"name"`
		URL  string `json:"url"`
	}
	out := struct {
		Tags  []tagLink `json:"tags"`
		Count int       `json:"count"`
	}{Tags: []tagLink{}}
	for _, name := range b.Tags {
		out.Tags = append(out.Tags, tagLink{name, tagURL(name)})
	}
	for _, t := range tags {
		if t.ID == tagID {
			out.Count = t.Count
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) flagBookmark(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r)
	if !ok {
		s.notFound(w, r)
		return
	}
	flag := store.Flag(r.PostFormValue("flag"))
	switch flag {
	case store.FlagFavourite, store.FlagArchived:
	default:
		http.Error(w, langOf(r).T("Unknown flag"), http.StatusBadRequest)
		return
	}
	sc, ok := s.bookmarkScope(w, r, id)
	if !ok {
		return
	}
	err := s.store.SetFlag(r.Context(), sc, id, flag, r.PostFormValue("on") == "1")
	if !s.storeError(w, r, err) {
		return
	}
	http.Redirect(w, r, safeNext(r.PostFormValue("next")), http.StatusSeeOther)
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
	s.render(w, r, status, "tags", data{"Tags": tags, "Error": problem})
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
