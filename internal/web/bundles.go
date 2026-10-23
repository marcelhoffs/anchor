package web

import (
	"net/http"
	"net/url"

	"github.com/marcelhoffs/anchor/internal/store"
)

func (s *Server) bundles(w http.ResponseWriter, r *http.Request) {
	s.renderBundles(w, r, http.StatusOK, "")
}

func (s *Server) renderBundles(w http.ResponseWriter, r *http.Request, status int, errMsg string) {
	list, err := s.store.ListBundles(r.Context(), currentScope(r))
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, status, "bundles", data{"BundleList": list, "Error": errMsg})
}

// createBundle saves the search on the list page and shows it.
func (s *Server) createBundle(w http.ResponseWriter, r *http.Request) {
	b, err := s.store.CreateBundle(r.Context(), currentScope(r), r.PostFormValue("name"), r.PostFormValue("query"))
	if isValidation(err) {
		s.renderBundles(w, r, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, bundleURL(b), http.StatusSeeOther)
}

func (s *Server) updateBundle(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r)
	if !ok {
		s.notFound(w, r)
		return
	}
	err := s.store.UpdateBundle(r.Context(), currentScope(r), id, r.PostFormValue("name"), r.PostFormValue("query"))
	if isValidation(err) {
		s.renderBundles(w, r, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if !s.storeError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/bundles", http.StatusSeeOther)
}

func (s *Server) deleteBundle(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r)
	if !ok {
		s.notFound(w, r)
		return
	}
	if !s.storeError(w, r, s.store.DeleteBundle(r.Context(), currentScope(r), id)) {
		return
	}
	http.Redirect(w, r, "/bundles", http.StatusSeeOther)
}

func bundleURL(b store.Bundle) string {
	return "/?q=" + url.QueryEscape(b.Query)
}
