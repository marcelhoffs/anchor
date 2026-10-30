package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/marcelhoffs/anchor/internal/auth"
	"github.com/marcelhoffs/anchor/internal/search"
	"github.com/marcelhoffs/anchor/internal/store"
)

// The REST API: JSON under /api/v1, authenticated only by bearer tokens
// (never cookies, so it needs no CSRF protection).

type apiCtxKey struct{}

type apiAuth struct {
	user  store.User
	token store.APIToken
	scope store.Scope // the space the request works on
}

func apiCaller(r *http.Request) apiAuth {
	return r.Context().Value(apiCtxKey{}).(apiAuth)
}

// problem writes an RFC 9457 problem response.
func problem(w http.ResponseWriter, status int, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"type": "about:blank", "title": http.StatusText(status), "status": status, "detail": detail,
	})
}

func (s *Server) apiRoutes(r chi.Router) {
	r.Use(s.apiAuthenticate)
	r.Get("/user", s.apiUser)
	r.Get("/spaces", s.apiListSpaces)
	r.Get("/bookmarks", s.apiListBookmarks)
	r.Get("/bookmarks/check", s.apiCheckBookmark)
	r.Get("/bookmarks/{id}", s.apiGetBookmark)
	r.Get("/tags", s.apiListTags)
	r.Get("/saved-searches", s.apiListSavedSearches)
	r.Get("/export", s.apiExport)
	r.Get("/jobs/{id}", s.apiGetJob)
	r.Group(func(r chi.Router) {
		r.Use(requireScope("write"))
		r.Post("/bookmarks", s.apiCreateBookmark)
		r.Patch("/bookmarks/{id}", s.apiUpdateBookmark)
		r.Delete("/bookmarks/{id}", s.apiDeleteBookmark)
		r.Post("/bookmarks/{id}/archive", s.apiArchiveBookmark)
		r.Post("/bookmarks/{id}/share", s.apiShareBookmark)
		r.Post("/tags", s.apiCreateTag)
		r.Patch("/tags/{id}", s.apiUpdateTag)
		r.Delete("/tags/{id}", s.apiDeleteTag)
		r.Post("/import", s.apiImport)
	})
	r.NotFound(func(w http.ResponseWriter, r *http.Request) { problem(w, http.StatusNotFound, "No such endpoint.") })
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		problem(w, http.StatusMethodNotAllowed, "Method not allowed here.")
	})
}

func (s *Server) apiAuthenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || raw == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="anchor"`)
			problem(w, http.StatusUnauthorized, "Send an API token as 'Authorization: Bearer <token>'. Create one under Settings → API tokens.")
			return
		}
		t, u, err := s.store.TokenByHash(r.Context(), auth.HashToken(strings.TrimSpace(raw)))
		if errors.Is(err, store.ErrNotFound) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="anchor", error="invalid_token"`)
			problem(w, http.StatusUnauthorized, "The token is invalid, expired or revoked.")
			return
		}
		if err != nil {
			s.log.Error("api auth", "err", err)
			problem(w, http.StatusInternalServerError, "Something went wrong.")
			return
		}
		if ok, retry := s.apiToken.Allow(strconv.FormatInt(t.ID, 10)); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
			problem(w, http.StatusTooManyRequests, "Rate limit is 600 requests per minute per token.")
			return
		}
		// Last use is recorded to the minute; skipping the write otherwise keeps
		// busy clients off the single database writer.
		if time.Since(t.LastUsedAt) > time.Minute {
			if err := s.store.TouchAPIToken(r.Context(), t.ID); err != nil {
				s.log.Error("touch api token", "err", err)
			}
		}
		limit := int64(maxFormBody)
		if r.URL.Path == "/api/v1/import" {
			limit = maxImportBody
		}
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		// ?space= picks the space the request works on; the personal one
		// when absent. A space the user isn't in doesn't exist for them.
		var sc store.Scope
		if raw := r.URL.Query().Get("space"); raw != "" {
			id, perr := strconv.ParseInt(raw, 10, 64)
			if perr != nil || id <= 0 {
				problem(w, http.StatusBadRequest, "space must be a space id from GET /api/v1/spaces.")
				return
			}
			sc, err = s.store.Access(r.Context(), u.ID, id)
		} else {
			sc, err = s.store.PersonalScope(r.Context(), u.ID)
		}
		if errors.Is(err, store.ErrNotFound) {
			problem(w, http.StatusNotFound, "No such space.")
			return
		}
		if err != nil {
			s.log.Error("api scope", "err", err)
			problem(w, http.StatusInternalServerError, "Something went wrong.")
			return
		}
		ctx := context.WithValue(r.Context(), apiCtxKey{}, apiAuth{user: u, token: t, scope: sc})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func requireScope(scope string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !apiCaller(r).token.Can(scope) {
				problem(w, http.StatusForbidden, "This token lacks the '"+scope+"' scope.")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// apiStoreError maps store errors to problems; it reports whether err was nil.
func (s *Server) apiStoreError(w http.ResponseWriter, err error) bool {
	var v store.ValidationError
	switch {
	case err == nil:
		return true
	case errors.Is(err, store.ErrNotFound):
		problem(w, http.StatusNotFound, "Not found.")
	case errors.Is(err, store.ErrForbidden):
		problem(w, http.StatusForbidden, "You can read this space's bookmarks but not change them.")
	case errors.Is(err, store.ErrConflict):
		problem(w, http.StatusConflict, "Another bookmark already has this URL.")
	case errors.As(err, &v):
		problem(w, http.StatusUnprocessableEntity, v.Error())
	default:
		s.log.Error("api", "err", err)
		problem(w, http.StatusInternalServerError, "Something went wrong.")
	}
	return false
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	return decodeBody(w, r, v, false)
}

// decodeOptionalJSON is decodeJSON for a body that may be left out. An empty
// body is only known by reading it: a chunked one has no Content-Length.
func decodeOptionalJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	return decodeBody(w, r, v, true)
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any, optional bool) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil && !(optional && errors.Is(err, io.EOF)) {
		problem(w, http.StatusBadRequest, "Invalid JSON body: "+err.Error())
		return false
	}
	return true
}

type apiBookmark struct {
	ID          int64     `json:"id"`
	URL         string    `json:"url"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Notes       string    `json:"notes"`
	Tags        []string  `json:"tags"`
	IsFavourite bool      `json:"is_favourite"`
	IsArchived  bool      `json:"is_archived"`
	ShareURL    *string   `json:"share_url"` // null unless shared
	Domain      string    `json:"domain"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func (s *Server) toAPI(b store.Bookmark) apiBookmark {
	tags := b.Tags
	if tags == nil {
		tags = []string{}
	}
	var share *string
	if b.IsShared {
		u := s.shareURL(b)
		share = &u
	}
	return apiBookmark{b.ID, b.URL, b.Title, b.Description, b.Notes, tags, b.IsFavourite, b.IsArchived, share, b.Domain, b.CreatedAt, b.UpdatedAt}
}

type apiSpace struct {
	ID       int64      `json:"id"`
	Name     string     `json:"name"`
	Personal bool       `json:"personal"`
	Role     store.Role `json:"role"`
}

// apiListSpaces lists the spaces the token's user can pass as ?space=:
// their personal space first, then the shared ones.
func (s *Server) apiListSpaces(w http.ResponseWriter, r *http.Request) {
	u := apiCaller(r).user
	own, err := s.store.PersonalScope(r.Context(), u.ID)
	if !s.apiStoreError(w, err) {
		return
	}
	shared, err := s.store.ListSpaces(r.Context(), u.ID)
	if !s.apiStoreError(w, err) {
		return
	}
	out := []apiSpace{{ID: own.SpaceID(), Name: "Personal", Personal: true, Role: own.Role()}}
	for _, sp := range shared {
		out = append(out, apiSpace{ID: sp.ID, Name: sp.Name, Role: sp.Role})
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": out})
}

func (s *Server) apiUser(w http.ResponseWriter, r *http.Request) {
	c := apiCaller(r)
	m, err := s.store.MFAStatus(r.Context(), c.user.ID)
	if !s.apiStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": c.user.ID, "username": c.user.Username, "is_admin": c.user.IsAdmin,
		"two_factor_enabled": m.Enabled(), "settings": c.user.Settings,
		"token": map[string]any{"name": c.token.Name, "scopes": c.token.Scopes},
	})
}

// Cursors are opaque to clients; today they encode an offset.
func encodeCursor(offset int) string {
	return base64.RawURLEncoding.EncodeToString([]byte("o:" + strconv.Itoa(offset)))
}

func decodeCursor(c string) (int, bool) {
	if c == "" {
		return 0, true
	}
	b, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil || !strings.HasPrefix(string(b), "o:") {
		return 0, false
	}
	n, err := strconv.Atoi(string(b[2:]))
	return n, err == nil && n >= 0
}

func (s *Server) apiListBookmarks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 50
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			problem(w, http.StatusBadRequest, "limit must be between 1 and 100.")
			return
		}
		limit = n
	}
	offset, ok := decodeCursor(q.Get("cursor"))
	if !ok {
		problem(w, http.StatusBadRequest, "Invalid cursor.")
		return
	}
	query := q.Get("q")
	for _, t := range q["tag"] {
		query += " " + search.TagQuery(strings.TrimPrefix(t, "#"))
	}
	if q.Get("archived") == "true" {
		query += " !archived"
	}
	if q.Get("favourite") == "true" {
		query += " !fav"
	}
	list, total, err := s.store.Search(r.Context(), apiCaller(r).scope, search.Parse(query), store.SortNewest, limit, offset)
	if !s.apiStoreError(w, err) {
		return
	}
	out := make([]apiBookmark, len(list))
	for i, b := range list {
		out[i] = s.toAPI(b)
	}
	var next any
	if offset+limit < total {
		next = encodeCursor(offset + limit)
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": out, "next": next})
}

func (s *Server) apiCheckBookmark(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("url")
	if raw == "" {
		problem(w, http.StatusBadRequest, "Pass the URL to check as ?url=.")
		return
	}
	b, err := s.store.FindBookmarkByURL(r.Context(), apiCaller(r).scope, raw)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusOK, map[string]any{"bookmark": nil})
		return
	}
	if !s.apiStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"bookmark": s.toAPI(b)})
}

func apiID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, ok := idParam(r)
	if !ok {
		problem(w, http.StatusNotFound, "Not found.")
	}
	return id, ok
}

func (s *Server) apiGetBookmark(w http.ResponseWriter, r *http.Request) {
	id, ok := apiID(w, r)
	if !ok {
		return
	}
	b, err := s.store.GetBookmark(r.Context(), apiCaller(r).scope, id)
	if !s.apiStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, s.toAPI(b))
}

type apiBookmarkInput struct {
	URL         *string   `json:"url"`
	Title       *string   `json:"title"`
	Description *string   `json:"description"`
	Notes       *string   `json:"notes"`
	Tags        *[]string `json:"tags"`
	IsFavourite *bool     `json:"is_favourite"`
	IsArchived  *bool     `json:"is_archived"`
}

// apply overlays the fields that were sent onto in.
func (p apiBookmarkInput) apply(in *store.BookmarkInput) {
	set := func(dst *string, src *string) {
		if src != nil {
			*dst = *src
		}
	}
	set(&in.URL, p.URL)
	set(&in.Title, p.Title)
	set(&in.Description, p.Description)
	set(&in.Notes, p.Notes)
	if p.Tags != nil {
		in.Tags = *p.Tags
	}
	if p.IsFavourite != nil {
		in.IsFavourite = *p.IsFavourite
	}
}

// apiCreateBookmark returns 201 with the new bookmark, or 200 with the
// existing one if the URL is already saved (linkding behaves the same).
func (s *Server) apiCreateBookmark(w http.ResponseWriter, r *http.Request) {
	var p apiBookmarkInput
	if !decodeJSON(w, r, &p) {
		return
	}
	if p.URL == nil {
		problem(w, http.StatusUnprocessableEntity, "url is required.")
		return
	}
	var in store.BookmarkInput
	p.apply(&in)
	sc := apiCaller(r).scope
	b, created, err := s.store.CreateBookmark(r.Context(), sc, in)
	if !s.apiStoreError(w, err) {
		return
	}
	if created && p.IsArchived != nil && *p.IsArchived {
		if !s.apiStoreError(w, s.store.SetFlag(r.Context(), sc, b.ID, store.FlagArchived, true)) {
			return
		}
		b.IsArchived = true
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
		s.fetcher.Notify()
	}
	writeJSON(w, status, s.toAPI(b))
}

func (s *Server) apiUpdateBookmark(w http.ResponseWriter, r *http.Request) {
	id, ok := apiID(w, r)
	if !ok {
		return
	}
	var p apiBookmarkInput
	if !decodeJSON(w, r, &p) {
		return
	}
	sc := apiCaller(r).scope
	cur, err := s.store.GetBookmark(r.Context(), sc, id)
	if !s.apiStoreError(w, err) {
		return
	}
	in := store.BookmarkInput{URL: cur.URL, Title: cur.Title, Description: cur.Description, Notes: cur.Notes,
		Tags: cur.Tags, IsFavourite: cur.IsFavourite}
	p.apply(&in)
	b, err := s.store.UpdateBookmark(r.Context(), sc, id, in)
	if !s.apiStoreError(w, err) {
		return
	}
	if p.IsArchived != nil && *p.IsArchived != b.IsArchived {
		if !s.apiStoreError(w, s.store.SetFlag(r.Context(), sc, id, store.FlagArchived, *p.IsArchived)) {
			return
		}
		b.IsArchived = *p.IsArchived
	}
	s.fetcher.Notify() // in case the URL changed
	writeJSON(w, http.StatusOK, s.toAPI(b))
}

// apiShareBookmark makes a public link (or with {"shared": false} removes it).
func (s *Server) apiShareBookmark(w http.ResponseWriter, r *http.Request) {
	id, ok := apiID(w, r)
	if !ok {
		return
	}
	body := struct {
		Shared *bool `json:"shared"`
	}{}
	if !decodeOptionalJSON(w, r, &body) {
		return
	}
	sc := apiCaller(r).scope
	if !s.apiStoreError(w, s.store.SetShared(r.Context(), sc, id, body.Shared == nil || *body.Shared)) {
		return
	}
	b, err := s.store.GetBookmark(r.Context(), sc, id)
	if !s.apiStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, s.toAPI(b))
}

func (s *Server) apiListSavedSearches(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListSavedSearches(r.Context(), apiCaller(r).scope)
	if !s.apiStoreError(w, err) {
		return
	}
	out := make([]map[string]any, len(list))
	for i, b := range list {
		out[i] = map[string]any{"id": b.ID, "name": b.Name, "query": b.Query}
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": out})
}

func (s *Server) apiDeleteBookmark(w http.ResponseWriter, r *http.Request) {
	id, ok := apiID(w, r)
	if !ok {
		return
	}
	if !s.apiStoreError(w, s.store.DeleteBookmark(r.Context(), apiCaller(r).scope, id)) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// apiArchiveBookmark archives (or with {"archived": false} unarchives).
func (s *Server) apiArchiveBookmark(w http.ResponseWriter, r *http.Request) {
	id, ok := apiID(w, r)
	if !ok {
		return
	}
	body := struct {
		Archived *bool `json:"archived"`
	}{}
	if !decodeOptionalJSON(w, r, &body) {
		return
	}
	archived := body.Archived == nil || *body.Archived
	sc := apiCaller(r).scope
	if !s.apiStoreError(w, s.store.SetFlag(r.Context(), sc, id, store.FlagArchived, archived)) {
		return
	}
	b, err := s.store.GetBookmark(r.Context(), sc, id)
	if !s.apiStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, s.toAPI(b))
}

// apiCreateTag creates a tag; bookmarks can only use tags that exist.
func (s *Server) apiCreateTag(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	t, err := s.store.CreateTag(r.Context(), apiCaller(r).scope, body.Name)
	if !s.apiStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": t.ID, "name": t.Name, "color": t.Color, "count": 0})
}

func (s *Server) apiListTags(w http.ResponseWriter, r *http.Request) {
	tags, err := s.store.ListTags(r.Context(), apiCaller(r).scope)
	if !s.apiStoreError(w, err) {
		return
	}
	out := make([]map[string]any, len(tags))
	for i, t := range tags {
		out[i] = map[string]any{"id": t.ID, "name": t.Name, "color": t.Color, "count": t.Count}
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": out})
}

// apiUpdateTag renames a tag ({"name": ...}) or merges it into another
// ({"merge_into": <tag id>}), and/or sets its colour ({"color": ...}).
func (s *Server) apiUpdateTag(w http.ResponseWriter, r *http.Request) {
	id, ok := apiID(w, r)
	if !ok {
		return
	}
	var body struct {
		Name      *string `json:"name"`
		MergeInto *int64  `json:"merge_into"`
		Color     *string `json:"color"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	sc := apiCaller(r).scope
	rename := body.Name != nil || body.MergeInto != nil
	var newName string
	switch {
	case body.Name != nil && body.MergeInto == nil:
		newName = *body.Name
	case body.MergeInto != nil && body.Name == nil && body.Color == nil:
		tags, err := s.store.ListTags(r.Context(), sc)
		if !s.apiStoreError(w, err) {
			return
		}
		for _, t := range tags {
			if t.ID == *body.MergeInto && t.ID != id {
				newName = t.Name
			}
		}
		if newName == "" {
			problem(w, http.StatusUnprocessableEntity, "merge_into must be the id of another of your tags.")
			return
		}
	case !rename && body.Color != nil:
	default:
		problem(w, http.StatusBadRequest, "Send name, color or both, or merge_into alone.")
		return
	}
	// Both checked first, so one doesn't happen without the other. The
	// colour goes first: a rename can merge this tag into another.
	if _, err := store.CleanTagName(newName); rename && !s.apiStoreError(w, err) {
		return
	}
	if body.Color != nil && !s.apiStoreError(w, s.store.SetTagColor(r.Context(), sc, id, *body.Color)) {
		return
	}
	if rename && !s.apiStoreError(w, s.store.RenameTag(r.Context(), sc, id, newName)) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) apiDeleteTag(w http.ResponseWriter, r *http.Request) {
	id, ok := apiID(w, r)
	if !ok {
		return
	}
	if !s.apiStoreError(w, s.store.DeleteTag(r.Context(), apiCaller(r).scope, id)) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// apiImport takes the file as the request body (or as a multipart "file"
// field) and queues it; poll the returned job.
func (s *Server) apiImport(w http.ResponseWriter, r *http.Request) {
	body := r.Body
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		f, _, err := r.FormFile("file")
		if err != nil {
			problem(w, http.StatusBadRequest, "Send the file as the body, or as a multipart field named 'file'.")
			return
		}
		defer f.Close()
		body = f
	}
	b, err := readImport(body)
	if err != nil {
		problem(w, http.StatusUnprocessableEntity, "Couldn't import that file: "+err.Error()+".")
		return
	}
	id, err := s.queueImport(r.Context(), apiCaller(r).scope, b)
	if !s.apiStoreError(w, err) {
		return
	}
	w.Header().Set("Location", fmt.Sprintf("/api/v1/jobs/%d", id))
	writeJSON(w, http.StatusAccepted, map[string]any{"job_id": id, "status_url": fmt.Sprintf("/api/v1/jobs/%d", id)})
}

func (s *Server) apiGetJob(w http.ResponseWriter, r *http.Request) {
	id, ok := apiID(w, r)
	if !ok {
		return
	}
	j, err := s.store.GetJob(r.Context(), apiCaller(r).user.ID, id)
	if !s.apiStoreError(w, err) {
		return
	}
	out := map[string]any{"id": j.ID, "kind": j.Kind, "status": j.Status, "progress": j.Progress, "total": j.Total}
	if j.Result != "" && j.Result != "null" {
		out["result"] = json.RawMessage(j.Result)
	}
	if j.LastError != "" {
		out["error"] = j.LastError
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) apiExport(w http.ResponseWriter, r *http.Request) {
	s.writeExport(w, r, apiCaller(r).scope, r.URL.Query().Get("format"))
}
