package web

// Single sign-on through an OpenID Connect provider (Authentik, Keycloak,
// Entra ID…), configured by ANCHOR_OIDC_*. An identity at the provider is
// the pair (issuer, subject); it's never matched to an account by email or
// name. A sign-in through the provider counts as complete: the provider
// does its own second factor.

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
	"golang.org/x/text/unicode/norm"

	"github.com/marcelhoffs/anchor/internal/auth"
	"github.com/marcelhoffs/anchor/internal/config"
	"github.com/marcelhoffs/anchor/internal/i18n"
	"github.com/marcelhoffs/anchor/internal/store"
)

const (
	oidcFlowTTL = 10 * time.Minute
	oidcCookie  = "anchor_oidc" // the state of the sign-in in flight
)

// oidcFlow is a sign-in at the provider in flight, kept until it comes back.
type oidcFlow struct {
	purpose  string // "login", "link" or "reauth"
	nonce    string
	verifier string // PKCE
	next     string
	invite   string // an invite token, for a first sign-in under invite-only
	userID   int64  // link and reauth: who started it
	expires  time.Time
}

// oidcClient talks to the provider. Discovery happens on first use and is
// kept, so Anchor starts even while the provider is down.
type oidcClient struct {
	cfg      *config.OIDC
	redirect string
	http     *http.Client

	mu       sync.Mutex
	provider *oidc.Provider
	flows    map[string]oidcFlow
}

func newOIDCClient(cfg *config.OIDC, baseURL string) *oidcClient {
	return &oidcClient{
		cfg: cfg, redirect: baseURL + "/login/oidc/callback",
		http:  &http.Client{Timeout: 15 * time.Second},
		flows: map[string]oidcFlow{},
	}
}

// discover returns the provider and the OAuth2 settings for it.
func (o *oidcClient) discover() (*oidc.Provider, *oauth2.Config, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.provider == nil {
		// Background, not the request's context: the provider keeps it to
		// fetch signing keys later.
		p, err := oidc.NewProvider(oidc.ClientContext(context.Background(), o.http), o.cfg.Issuer)
		if err != nil {
			return nil, nil, err
		}
		o.provider = p
	}
	return o.provider, &oauth2.Config{
		ClientID: o.cfg.ClientID, ClientSecret: o.cfg.ClientSecret,
		Endpoint: o.provider.Endpoint(), RedirectURL: o.redirect, Scopes: o.cfg.Scopes,
	}, nil
}

func (o *oidcClient) put(state string, f oidcFlow) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.flows[state] = f
}

// take returns a flow once: a state can't be replayed.
func (o *oidcClient) take(state string) (oidcFlow, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	f, ok := o.flows[state]
	delete(o.flows, state)
	return f, ok && time.Now().Before(f.expires)
}

func (o *oidcClient) sweep() {
	o.mu.Lock()
	defer o.mu.Unlock()
	for k, f := range o.flows {
		if time.Now().After(f.expires) {
			delete(o.flows, k)
		}
	}
}

// requireSSO answers 404 while single sign-on is off.
func (s *Server) requireSSO(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.oidc == nil {
			s.notFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func randomString() string {
	b := make([]byte, 24)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func (s *Server) oidcCookieName() string {
	if s.cfg.SecureCookies() {
		return "__Host-" + oidcCookie
	}
	return oidcCookie
}

// oidcRedirect starts a flow and sends the browser to the provider.
func (s *Server) oidcRedirect(w http.ResponseWriter, r *http.Request, f oidcFlow) {
	_, oc, err := s.oidc.discover()
	if err != nil {
		s.log.Error("oidc discovery", "issuer", s.cfg.OIDC.Issuer, "err", err)
		s.oidcFailed(w, r, f.purpose, i18n.M("Single sign-on is unavailable right now. Try again later."))
		return
	}
	state := randomString()
	f.nonce, f.verifier, f.expires = randomString(), oauth2.GenerateVerifier(), time.Now().Add(oidcFlowTTL)
	s.oidc.put(state, f)
	http.SetCookie(w, &http.Cookie{
		Name: s.oidcCookieName(), Value: state, Path: "/", MaxAge: int(oidcFlowTTL.Seconds()), HttpOnly: true,
		Secure: s.cfg.SecureCookies(), SameSite: http.SameSiteLaxMode,
	})
	opts := []oauth2.AuthCodeOption{oauth2.S256ChallengeOption(f.verifier), oidc.Nonce(f.nonce)}
	if f.purpose == "reauth" {
		// Ask the provider to check it's really them, not reuse its session.
		opts = append(opts, oauth2.SetAuthURLParam("prompt", "login"), oauth2.SetAuthURLParam("max_age", "0"))
	}
	http.Redirect(w, r, oc.AuthCodeURL(state, opts...), http.StatusSeeOther)
}

// oidcLogin starts signing in (or signing up, with ?invite=) at the provider.
func (s *Server) oidcLogin(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	s.oidcRedirect(w, r, oidcFlow{purpose: "login", next: safeNext(q.Get("next")), invite: q.Get("invite")})
}

// oidcLink starts linking the signed-in user's account to the provider.
func (s *Server) oidcLink(w http.ResponseWriter, r *http.Request) {
	s.oidcRedirect(w, r, oidcFlow{purpose: "link", next: "/settings/security", userID: currentUser(r).ID})
}

// oidcReauth confirms it's the signed-in user with a fresh sign-in at the
// provider, before a sensitive action.
func (s *Server) oidcReauth(w http.ResponseWriter, r *http.Request) {
	s.oidcRedirect(w, r, oidcFlow{purpose: "reauth", next: safeNext(r.PostFormValue("next")), userID: currentUser(r).ID})
}

func (s *Server) oidcUnlink(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if s.cfg.OIDC.Only {
		s.renderSecurity(w, r, http.StatusConflict, data{"Error": i18n.M("Single sign-on is the only way to sign in here, so it can't be unlinked.")})
		return
	}
	err := s.store.UnlinkIdentity(r.Context(), u.ID, false)
	if isValidation(err) {
		s.renderSecurity(w, r, http.StatusConflict, data{"Error": errMsg(err)})
		return
	}
	if !s.storeError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/settings/security?done=sso-unlinked", http.StatusSeeOther)
}

// oidcFailed tells someone their sign-in at the provider didn't work, on the
// page they started from.
func (s *Server) oidcFailed(w http.ResponseWriter, r *http.Request, purpose string, msg i18n.Msg) {
	if purpose != "login" && currentUser(r) != nil {
		s.renderSecurity(w, r, http.StatusUnprocessableEntity, data{"Error": msg})
		return
	}
	s.renderLogin(w, r, http.StatusUnprocessableEntity, data{"Error": msg})
}

// oidcClaims are the ID token claims Anchor reads besides the subject.
type oidcClaims struct {
	PreferredUsername string `json:"preferred_username"`
	Email             string `json:"email"`
	Name              string `json:"name"`
}

// oidcCallback is where the provider sends the browser back.
func (s *Server) oidcCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	c, err := r.Cookie(s.oidcCookieName())
	state := q.Get("state")
	http.SetCookie(w, &http.Cookie{Name: s.oidcCookieName(), Value: "", Path: "/", MaxAge: -1, HttpOnly: true,
		Secure: s.cfg.SecureCookies(), SameSite: http.SameSiteLaxMode})
	// The state must be this browser's and still in flight: a sign-in can't
	// be finished in someone else's browser, or twice.
	if err != nil || state == "" || c.Value != state {
		s.oidcFailed(w, r, "login", i18n.M("That sign-in expired or was started elsewhere. Start again."))
		return
	}
	f, ok := s.oidc.take(state)
	if !ok {
		s.oidcFailed(w, r, "login", i18n.M("That sign-in expired or was started elsewhere. Start again."))
		return
	}
	if e := q.Get("error"); e != "" {
		s.log.Info("oidc sign-in refused by the provider", "error", e, "description", q.Get("error_description"))
		s.oidcFailed(w, r, f.purpose, i18n.M("Single sign-on was cancelled or refused."))
		return
	}
	p, oc, err := s.oidc.discover()
	if err != nil {
		s.log.Error("oidc discovery", "issuer", s.cfg.OIDC.Issuer, "err", err)
		s.oidcFailed(w, r, f.purpose, i18n.M("Single sign-on is unavailable right now. Try again later."))
		return
	}
	ctx := oidc.ClientContext(r.Context(), s.oidc.http)
	tok, err := oc.Exchange(ctx, q.Get("code"), oauth2.VerifierOption(f.verifier))
	if err != nil {
		s.log.Warn("oidc code exchange", "err", err)
		s.oidcFailed(w, r, f.purpose, i18n.M("Single sign-on didn't work. Try again."))
		return
	}
	raw, _ := tok.Extra("id_token").(string)
	idt, err := p.Verifier(&oidc.Config{ClientID: s.cfg.OIDC.ClientID}).Verify(ctx, raw)
	if err != nil || idt.Nonce != f.nonce || idt.Subject == "" {
		s.log.Warn("oidc id token rejected", "err", err)
		s.oidcFailed(w, r, f.purpose, i18n.M("Single sign-on didn't work. Try again."))
		return
	}
	var claims oidcClaims
	var all map[string]any
	if err := idt.Claims(&claims); err != nil || idt.Claims(&all) != nil {
		s.oidcFailed(w, r, f.purpose, i18n.M("Single sign-on didn't work. Try again."))
		return
	}
	switch f.purpose {
	case "link":
		s.oidcFinishLink(w, r, f, idt.Issuer, idt.Subject)
	case "reauth":
		s.oidcFinishReauth(w, r, f, idt.Issuer, idt.Subject)
	default:
		s.oidcFinishLogin(w, r, f, idt.Issuer, idt.Subject, claims, s.inAdminGroup(all))
	}
}

// inAdminGroup reports whether the claims put the user in the admin group;
// nil when no admin group is configured.
func (s *Server) inAdminGroup(claims map[string]any) *bool {
	group := s.cfg.OIDC.AdminGroup
	if group == "" {
		return nil
	}
	in := false
	switch v := claims[s.cfg.OIDC.GroupsClaim].(type) {
	case []any:
		for _, g := range v {
			in = in || fmt.Sprint(g) == group
		}
	case string:
		in = v == group
	}
	return &in
}

func (s *Server) oidcFinishLogin(w http.ResponseWriter, r *http.Request, f oidcFlow, issuer, subject string, claims oidcClaims, admin *bool) {
	ctx := r.Context()
	u, err := s.store.UserByIdentity(ctx, issuer, subject)
	switch {
	case errors.Is(err, store.ErrNotFound):
		var ok bool
		if u, ok = s.oidcNewUser(w, r, f, issuer, subject, claims, admin); !ok {
			return
		}
	case err != nil:
		s.serverError(w, r, err)
		return
	}
	if !u.IsActive {
		s.oidcFailed(w, r, "login", i18n.M("Your Anchor account is disabled. Ask an admin."))
		return
	}
	if admin != nil && *admin != u.IsAdmin {
		if err := s.store.SetAdmin(ctx, u.ID, *admin); err != nil {
			// The last admin keeps the role, whatever the group says.
			s.log.Warn("oidc admin group", "user", u.Username, "admin", *admin, "err", err)
		}
	}
	s.store.TouchIdentity(ctx, issuer, subject)
	s.store.RecordLogin(ctx, u.ID)
	if err := s.newSession(w, r, u, true, sessionTTL, true); err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, safeNext(f.next), http.StatusSeeOther)
}

// oidcNewUser makes an account for a first sign-in, as far as the sign-up
// mode allows: the first user ever (setup), open sign-up, or a valid invite.
func (s *Server) oidcNewUser(w http.ResponseWriter, r *http.Request, f oidcFlow, issuer, subject string, claims oidcClaims, admin *bool) (store.User, bool) {
	n := store.NewOIDCUser{Issuer: issuer, Subject: subject, First: !s.hasUsers.Load(), Admin: admin != nil && *admin}
	if !n.First {
		inviteHash, ok := s.signupAllowed(r, f.invite)
		if !ok {
			s.oidcFailed(w, r, "login", i18n.M("No Anchor account is linked to this sign-in. Ask an admin for an invite, or link it under Settings → Security."))
			return store.User{}, false
		}
		n.InviteHash = inviteHash
	}
	base := usernameFrom(claims)
	for i := 1; i <= 50; i++ {
		n.Username = base
		if i > 1 {
			n.Username = fmt.Sprintf("%s-%d", truncate(base, 60), i)
		}
		u, err := s.store.CreateOIDCUser(r.Context(), n)
		switch {
		case err == nil:
			s.hasUsers.Store(true)
			s.log.Info("account created by single sign-on", "user", u.Username)
			return u, true
		case errors.Is(err, store.ErrInvalidInvite):
			s.oidcFailed(w, r, "login", i18n.M("This invite can't be used"))
			return store.User{}, false
		case !errors.Is(err, store.ErrConflict):
			s.serverError(w, r, err)
			return store.User{}, false
		}
		// A conflict: the identity was linked meanwhile, someone else set up
		// Anchor at the same moment, or the name is taken (try the next).
		if u, err := s.store.UserByIdentity(r.Context(), issuer, subject); err == nil {
			return u, true
		}
		if n.First {
			s.hasUsers.Store(true)
			s.oidcFailed(w, r, "login", i18n.M("Someone else just set up Anchor. Sign in again."))
			return store.User{}, false
		}
	}
	s.serverError(w, r, fmt.Errorf("no free username for %q", base))
	return store.User{}, false
}

var notUsername = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// usernameFrom makes a valid Anchor username from the provider's claims:
// the part before any "@" (Entra's preferred_username is an email address),
// accents dropped (Zoë → Zoe), anything else not allowed becomes "-".
func usernameFrom(c oidcClaims) string {
	for _, v := range []string{c.PreferredUsername, c.Email, c.Name} {
		v, _, _ = strings.Cut(v, "@")
		v = strings.Map(func(r rune) rune {
			if unicode.Is(unicode.Mn, r) {
				return -1
			}
			return r
		}, norm.NFD.String(strings.TrimSpace(v)))
		name := strings.Trim(notUsername.ReplaceAllString(v, "-"), "-._")
		if name = truncate(name, 60); auth.ValidateUsername(name) == nil {
			return name
		}
	}
	return "user"
}

func (s *Server) oidcFinishLink(w http.ResponseWriter, r *http.Request, f oidcFlow, issuer, subject string) {
	u := currentUser(r)
	if u == nil || u.ID != f.userID {
		s.oidcFailed(w, r, "login", i18n.M("That sign-in expired or was started elsewhere. Start again."))
		return
	}
	if err := s.store.LinkIdentity(r.Context(), u.ID, issuer, subject); errors.Is(err, store.ErrConflict) {
		s.renderSecurity(w, r, http.StatusConflict, data{"Error": i18n.M("That account at the provider is already linked to another Anchor user.")})
		return
	} else if err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/settings/security?done=sso-linked", http.StatusSeeOther)
}

func (s *Server) oidcFinishReauth(w http.ResponseWriter, r *http.Request, f oidcFlow, issuer, subject string) {
	u, cur := currentUser(r), currentSession(r)
	if u == nil || u.ID != f.userID {
		s.oidcFailed(w, r, "login", i18n.M("That sign-in expired or was started elsewhere. Start again."))
		return
	}
	if mine, err := s.store.IdentityOf(r.Context(), u.ID, issuer); err != nil || mine != subject {
		s.renderSecurity(w, r, http.StatusForbidden, data{"Error": i18n.M("That's a different account at the provider than the one linked to yours.")})
		return
	}
	if err := s.store.MarkVerified(r.Context(), cur.TokenHash); err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, withNotice(safeNext(f.next), "confirmed"), http.StatusSeeOther)
}

// ssoLinked reports whether u's account is linked to the provider.
func (s *Server) ssoLinked(ctx context.Context, u *store.User) bool {
	if s.oidc == nil || u == nil {
		return false
	}
	_, err := s.store.IdentityOf(ctx, u.ID, s.cfg.OIDC.Issuer)
	return err == nil
}
