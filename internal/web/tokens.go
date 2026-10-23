package web

import (
	"net/http"
	"time"

	"github.com/marcelhoffs/anchor/internal/auth"
)

// apiTokenPrefix marks Anchor tokens, so secret scanners can recognise them.
const apiTokenPrefix = "brw_"

func (s *Server) tokens(w http.ResponseWriter, r *http.Request) {
	s.renderTokens(w, r, http.StatusOK, data{"Notice": settingsNotices[r.URL.Query().Get("done")]})
}

func (s *Server) renderTokens(w http.ResponseWriter, r *http.Request, status int, d data) {
	list, err := s.store.ListAPITokens(r.Context(), currentUser(r).ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	d["Tab"] = "tokens"
	d["Tokens"] = list
	d["APIBase"] = s.cfg.BaseURL.String() + "/api/v1"
	s.render(w, r, status, "tokens", d)
}

var tokenExpiries = map[string]time.Duration{"30": 30 * 24 * time.Hour, "90": 90 * 24 * time.Hour, "365": 365 * 24 * time.Hour}

func (s *Server) createToken(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	scopes := []string{"read"}
	if r.PostFormValue("scope") == "write" {
		scopes = append(scopes, "write")
	}
	var expires time.Time
	if d, ok := tokenExpiries[r.PostFormValue("expires")]; ok {
		expires = time.Now().Add(d)
	}
	raw, _ := auth.NewToken(32)
	token := apiTokenPrefix + raw
	t, err := s.store.CreateAPIToken(r.Context(), u.ID, r.PostFormValue("name"), auth.HashToken(token), scopes, expires)
	if isValidation(err) {
		s.renderTokens(w, r, http.StatusUnprocessableEntity, data{"Error": err.Error()})
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.renderTokens(w, r, http.StatusOK, data{"NewToken": token, "NewTokenName": t.Name})
}

func (s *Server) deleteToken(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	id, ok := idParam(r)
	if !ok {
		s.notFound(w, r)
		return
	}
	if !s.storeError(w, r, s.store.DeleteAPIToken(r.Context(), u.ID, id)) {
		return
	}
	http.Redirect(w, r, "/settings/tokens?done=token-off", http.StatusSeeOther)
}
