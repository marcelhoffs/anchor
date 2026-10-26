package web

import (
	"net/http"
	"net/url"

	"github.com/marcelhoffs/anchor/internal/i18n"
	"github.com/marcelhoffs/anchor/internal/store"
)

func (s *Server) savedSearches(w http.ResponseWriter, r *http.Request) {
	s.renderSavedSearches(w, r, http.StatusOK, i18n.Msg{})
}

func (s *Server) renderSavedSearches(w http.ResponseWriter, r *http.Request, status int, problem i18n.Msg) {
	list, err := s.store.ListSavedSearches(r.Context(), currentScope(r))
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, status, "saved_searches", data{"SavedSearchList": list, "Error": problem})
}

// createSavedSearch saves the search on the list page and shows it.
func (s *Server) createSavedSearch(w http.ResponseWriter, r *http.Request) {
	b, err := s.store.CreateSavedSearch(r.Context(), currentScope(r), r.PostFormValue("name"), r.PostFormValue("query"))
	if isValidation(err) {
		s.renderSavedSearches(w, r, http.StatusUnprocessableEntity, errMsg(err))
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, savedSearchURL(b), http.StatusSeeOther)
}

func (s *Server) renameSavedSearch(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r)
	if !ok {
		s.notFound(w, r)
		return
	}
	err := s.store.RenameSavedSearch(r.Context(), currentScope(r), id, r.PostFormValue("name"))
	if isValidation(err) {
		s.renderSavedSearches(w, r, http.StatusUnprocessableEntity, errMsg(err))
		return
	}
	if !s.storeError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/saved-searches", http.StatusSeeOther)
}

func (s *Server) deleteSavedSearch(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r)
	if !ok {
		s.notFound(w, r)
		return
	}
	if !s.storeError(w, r, s.store.DeleteSavedSearch(r.Context(), currentScope(r), id)) {
		return
	}
	http.Redirect(w, r, "/saved-searches", http.StatusSeeOther)
}

func savedSearchURL(b store.SavedSearch) string {
	return "/?q=" + url.QueryEscape(b.Query)
}
