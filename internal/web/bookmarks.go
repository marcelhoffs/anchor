package web

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/marcelhoffs/burrow/internal/search"
	"github.com/marcelhoffs/burrow/internal/store"
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
	view, heading := listView(q)
	d := data{
		"Query": q, "Bookmarks": list, "Tags": tags, "Counts": counts, "Here": r.URL.RequestURI(),
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
// "archive", "#<tag>", or "" for any other search) and the page heading.
func listView(q string) (view, heading string) {
	switch q = strings.TrimSpace(q); {
	case q == "":
		return "all", "All bookmarks"
	case q == "!fav":
		return "fav", "Favourites"
	case q == "!archived":
		return "archive", "Archive"
	case strings.HasPrefix(q, "#") && len(q) > 1 && len(strings.Fields(q)) == 1:
		return q, q
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
		"TagText": strings.Join(store.CleanTags(in.Tags), " "), "Favourite": in.IsFavourite,
	}
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
	s.render(w, r, http.StatusOK, "edit", d)
}

func (s *Server) createBookmark(w http.ResponseWriter, r *http.Request) {
	in := bookmarkForm(r)
	b, created, err := s.store.CreateBookmark(r.Context(), currentUser(r).ID, in)
	if err != nil {
		if isValidation(err) {
			d := formData(in)
			d["Error"] = err.Error()
			d["Next"] = safeNext(r.PostFormValue("next"))
			s.render(w, r, http.StatusUnprocessableEntity, "edit", d)
			return
		}
		s.serverError(w, r, err)
		return
	}
	if !created {
		http.Redirect(w, r, "/bookmarks/"+strconv.FormatInt(b.ID, 10)+"/edit?dup=1", http.StatusSeeOther)
		return
	}
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
	if r.URL.Query().Get("dup") != "" {
		d["Notice"] = "You already saved this URL."
	}
	s.render(w, r, http.StatusOK, "edit", d)
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
		s.render(w, r, http.StatusUnprocessableEntity, "edit", d)
		return
	}
	if !s.storeError(w, r, err) {
		return
	}
	http.Redirect(w, r, safeNext(r.PostFormValue("next")), http.StatusSeeOther)
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
