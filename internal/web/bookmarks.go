package web

import (
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/marcelhoffs/anchor/internal/search"
	"github.com/marcelhoffs/anchor/internal/store"
)

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	q := r.URL.Query().Get("q")
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	page = max(page, 1)

	list, total, err := s.store.Search(r.Context(), u.ID, search.Parse(q), u.Settings.Sort, pageSize, (page-1)*pageSize)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	tags, err := s.store.ListTags(r.Context(), u.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	counts, err := s.store.CountBookmarks(r.Context(), u.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	bundles, err := s.store.ListBundles(r.Context(), u.ID)
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
	Links           []pageLink
}

// pageLink is one numbered page; Num 0 marks a gap ("…").
type pageLink struct {
	Num     int
	URL     string
	Current bool
}

// newPager links the first and last page and those within two of the
// current one, with gaps for the rest.
func newPager(page, pages, total int, pageURL func(int) string) pager {
	p := pager{From: (page-1)*pageSize + 1, To: min(page*pageSize, total), Total: total}
	if page > 1 {
		p.PrevURL = pageURL(page - 1)
	}
	if page < pages {
		p.NextURL = pageURL(page + 1)
	}
	for n := 1; n <= pages; n++ {
		switch {
		case n == 1 || n == pages || (n >= page-2 && n <= page+2):
			p.Links = append(p.Links, pageLink{Num: n, URL: pageURL(n), Current: n == page})
		case p.Links[len(p.Links)-1].Num != 0:
			p.Links = append(p.Links, pageLink{})
		}
	}
	return p
}

// listView names the sidebar entry a query corresponds to ("all", "fav",
// "archive", "#<tag>", "bundle:<id>", or "" for any other search) and the
// page heading.
func listView(q string, bundles []store.Bundle) (view, heading string) {
	switch q = strings.TrimSpace(q); {
	case q == "":
		return "all", "All bookmarks"
	case q == "!fav":
		return "fav", "Favourites"
	case q == "!archived":
		return "archive", "Archive"
	}
	if p := search.Parse(q); len(p.Tags) == 1 && search.TagQuery(p.Tags[0]) == q {
		return "#" + p.Tags[0], "# " + p.Tags[0] // as the sidebar shows it
	}
	for _, b := range bundles {
		if b.Query == q {
			return "bundle:" + strconv.FormatInt(b.ID, 10), b.Name
		}
	}
	return "", "Search results"
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

// renderEdit shows the add/edit form. The tags field suggests the user's
// tags as they type, most used first.
func (s *Server) renderEdit(w http.ResponseWriter, r *http.Request, status int, d data) {
	tags, err := s.store.ListTags(r.Context(), currentUser(r).ID)
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

// newBookmark shows the add form, prefilled from ?url=&title=&description=
// (which is how the bookmarklet calls it). An already-saved URL goes
// straight to its edit page.
func (s *Server) newBookmark(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if raw := q.Get("url"); raw != "" {
		b, err := s.store.FindBookmarkByURL(r.Context(), currentUser(r).ID, raw)
		if err == nil {
			http.Redirect(w, r, "/bookmarks/"+strconv.FormatInt(b.ID, 10)+"/edit?dup=1", http.StatusSeeOther)
			return
		}
		if !errors.Is(err, store.ErrNotFound) {
			s.serverError(w, r, err)
			return
		}
	}
	d := formData(store.BookmarkInput{URL: q.Get("url"), Title: q.Get("title"), Description: q.Get("description")})
	d["Next"] = safeNext(q.Get("next"))
	s.renderEdit(w, r, http.StatusOK, d)
}

func (s *Server) createBookmark(w http.ResponseWriter, r *http.Request) {
	in := bookmarkForm(r)
	b, created, err := s.store.CreateBookmark(r.Context(), currentUser(r).ID, in)
	if err != nil {
		if isValidation(err) {
			d := formData(in)
			d["Error"] = err.Error()
			d["Next"] = safeNext(r.PostFormValue("next"))
			s.renderEdit(w, r, http.StatusUnprocessableEntity, d)
			return
		}
		s.serverError(w, r, err)
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
	b, err := s.store.GetBookmark(r.Context(), currentUser(r).ID, id)
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
		d["Notice"] = "You already saved this URL."
	} else if n := bookmarkNotices[r.URL.Query().Get("done")]; n != "" {
		d["Notice"] = n
	}
	s.renderEdit(w, r, http.StatusOK, d)
}

func (s *Server) updateBookmark(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r)
	if !ok {
		s.notFound(w, r)
		return
	}
	u := currentUser(r)
	in := bookmarkForm(r)
	_, err := s.store.UpdateBookmark(r.Context(), u.ID, id, in)
	if errors.Is(err, store.ErrConflict) || isValidation(err) {
		b, gerr := s.store.GetBookmark(r.Context(), u.ID, id)
		if !s.storeError(w, r, gerr) {
			return
		}
		d := formData(in)
		d["Bookmark"] = b
		d["Next"] = safeNext(r.PostFormValue("next"))
		d["Error"] = err.Error()
		if errors.Is(err, store.ErrConflict) {
			d["Error"] = "Another bookmark already has this URL."
		}
		s.renderEdit(w, r, http.StatusUnprocessableEntity, d)
		return
	}
	if !s.storeError(w, r, err) {
		return
	}
	s.fetcher.Notify() // in case the URL changed
	http.Redirect(w, r, safeNext(r.PostFormValue("next")), http.StatusSeeOther)
}

var bookmarkNotices = map[string]string{
	"shared":    "Anyone with the link can now see this bookmark's title, link and description.",
	"unshared":  "The public link no longer works.",
	"refetched": "Fetching the page again. Empty fields and the icon are filled in when it's done.",
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
	on := r.PostFormValue("on") == "1"
	if !s.storeError(w, r, s.store.SetShared(r.Context(), currentUser(r).ID, id, on)) {
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
	if !s.storeError(w, r, s.store.Refetch(r.Context(), currentUser(r).ID, id)) {
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

// favicon serves a stored site icon to a user who has a bookmark using it.
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
	if !s.storeError(w, r, s.store.DeleteBookmark(r.Context(), currentUser(r).ID, id)) {
		return
	}
	http.Redirect(w, r, safeNext(r.PostFormValue("next")), http.StatusSeeOther)
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
		http.Error(w, "Unknown flag", http.StatusBadRequest)
		return
	}
	err := s.store.SetFlag(r.Context(), currentUser(r).ID, id, flag, r.PostFormValue("on") == "1")
	if !s.storeError(w, r, err) {
		return
	}
	http.Redirect(w, r, safeNext(r.PostFormValue("next")), http.StatusSeeOther)
}

func (s *Server) tags(w http.ResponseWriter, r *http.Request) {
	s.renderTags(w, r, http.StatusOK, "")
}

func (s *Server) renderTags(w http.ResponseWriter, r *http.Request, status int, errMsg string) {
	tags, err := s.store.ListTags(r.Context(), currentUser(r).ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, status, "tags", data{"Tags": tags, "Error": errMsg})
}

func (s *Server) createTag(w http.ResponseWriter, r *http.Request) {
	_, err := s.store.CreateTag(r.Context(), currentUser(r).ID, r.PostFormValue("name"))
	if isValidation(err) {
		s.renderTags(w, r, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if err != nil {
		s.serverError(w, r, err)
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
	err := s.store.RenameTag(r.Context(), currentUser(r).ID, id, r.PostFormValue("name"))
	if isValidation(err) {
		s.renderTags(w, r, http.StatusUnprocessableEntity, err.Error())
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
	if !s.storeError(w, r, s.store.DeleteTag(r.Context(), currentUser(r).ID, id)) {
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
