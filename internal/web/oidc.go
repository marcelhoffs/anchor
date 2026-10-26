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
	"encoding/json"
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
	// Sign-ins in flight are kept in memory: at most this many, so starting
	// them can't be a way to fill it.
	maxOIDCFlows = 10000
	// After failing, discovery waits this long before asking again.
	discoveryRetry = 30 * time.Second
	// With an admin group, requests other than page loads ask the provider
	// again once its last answer is this old; see recheckGroup.
	adminGroupCheck = 5 * time.Minute
	// How long a page waits for the provider to confirm the group.
	groupCheckTimeout = 5 * time.Second
	// What a session's provider tokens are sealed for.
	ssoTokensKey = "sso-tokens"
)

// oidcFlow is a sign-in at the provider in flight, kept until it comes back.
type oidcFlow struct {
	purpose  string // "login" or "link"
	nonce    string
	verifier string // PKCE
	next     string
	invite   string // an invite token, for a first sign-in under invite-only
	userID   int64  // link: who started it
	expires  time.Time
}

// oidcClient talks to the provider. Discovery happens on first use and is
// kept, so Anchor starts even while the provider is down.
type oidcClient struct {
	cfg      *config.OIDC
	redirect string
	http     *http.Client

	// dmu guards discovery, apart from mu: a slow provider mustn't hold up
	// the flows.
	dmu      sync.Mutex
	provider *oidc.Provider
	failedAt time.Time
	failErr  error

	mu    sync.Mutex
	flows map[string]oidcFlow

	// One session's group checks run one at a time (see checkGroup); a
	// session's lock is picked by its hash.
	checks [64]sync.Mutex
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
	o.dmu.Lock()
	defer o.dmu.Unlock()
	if o.provider == nil {
		if time.Since(o.failedAt) < discoveryRetry {
			return nil, nil, o.failErr
		}
		// Background, not the request's context: the provider keeps it to
		// fetch signing keys later.
		p, err := oidc.NewProvider(oidc.ClientContext(context.Background(), o.http), o.cfg.Issuer)
		if err != nil {
			o.failedAt, o.failErr = time.Now(), err
			return nil, nil, err
		}
		o.provider = p
	}
	return o.provider, &oauth2.Config{
		ClientID: o.cfg.ClientID, ClientSecret: o.cfg.ClientSecret,
		Endpoint: o.provider.Endpoint(), RedirectURL: o.redirect, Scopes: o.cfg.Scopes,
	}, nil
}

// put keeps a flow; false when too many are in flight.
func (o *oidcClient) put(state string, f oidcFlow) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.flows) >= maxOIDCFlows {
		return false
	}
	o.flows[state] = f
	return true
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
	if !s.oidc.put(state, f) {
		s.log.Warn("oidc: too many sign-ins in flight", "max", maxOIDCFlows)
		s.oidcFailed(w, r, f.purpose, i18n.M("Single sign-on is unavailable right now. Try again later."))
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: s.oidcCookieName(), Value: state, Path: "/", MaxAge: int(oidcFlowTTL.Seconds()), HttpOnly: true,
		Secure: s.cfg.SecureCookies(), SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, oc.AuthCodeURL(state, oauth2.S256ChallengeOption(f.verifier), oidc.Nonce(f.nonce)), http.StatusSeeOther)
}

// oidcLogin starts signing in (or signing up, with ?invite=) at the provider.
// Anyone can, so it's limited per IP, and what it keeps of the request is
// cut to size.
func (s *Server) oidcLogin(w http.ResponseWriter, r *http.Request) {
	if ok, retry := s.ssoIP.Allow(s.clientIP(r)); !ok {
		w.Header().Set("Retry-After", fmt.Sprint(int(retry.Seconds())+1))
		s.renderLogin(w, r, http.StatusTooManyRequests, data{
			"Error": i18n.M("Too many sign-ins from your network. Try again in %s.", retryText(langOf(r), retry))})
		return
	}
	q := r.URL.Query()
	next, invite := safeNext(q.Get("next")), q.Get("invite")
	if len(next) > 2048 {
		next = "/"
	}
	if len(invite) > 64 { // invite tokens are 32 characters
		invite = ""
	}
	s.oidcRedirect(w, r, oidcFlow{purpose: "login", next: next, invite: invite})
}

// oidcLink starts linking the signed-in user's account to the provider.
func (s *Server) oidcLink(w http.ResponseWriter, r *http.Request) {
	s.oidcRedirect(w, r, oidcFlow{purpose: "link", next: "/settings/security", userID: currentUser(r).ID})
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
		s.oidcFinishLink(w, r, f, idt.Issuer, idt.Subject, s.inAdminGroup(all))
	default:
		s.oidcFinishLogin(w, r, f, idt.Issuer, idt.Subject, claims, s.inAdminGroup(all), tok)
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

func (s *Server) oidcFinishLogin(w http.ResponseWriter, r *http.Request, f oidcFlow, issuer, subject string, claims oidcClaims, admin *bool, tok *oauth2.Token) {
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
	if err := s.syncAdmin(r, u, admin); err != nil {
		s.serverError(w, r, err)
		return
	}
	if s.groupAdmins() && tok.RefreshToken == "" {
		s.log.Warn("oidc: no refresh token, so the session ends when its access token does; allow offline_access at the provider", "user", u.Username)
	}
	sealed, err := s.sealTokens(tok)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.store.TouchIdentity(ctx, issuer, subject)
	s.store.RecordLogin(ctx, u.ID)
	if err := s.newSession(w, r, u, true, sessionTTL, true, sealed); err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, safeNext(f.next), http.StatusSeeOther)
}

// oidcNewUser makes an account for a first sign-in, as far as the sign-up
// mode allows: the first user ever (setup), open sign-up, or a valid invite.
func (s *Server) oidcNewUser(w http.ResponseWriter, r *http.Request, f oidcFlow, issuer, subject string, claims oidcClaims, admin *bool) (store.User, bool) {
	n := store.NewOIDCUser{Issuer: issuer, Subject: subject, First: !s.hasUsers.Load()}
	// The first user is the admin, unless the provider's group decides.
	n.Admin = n.First
	if admin != nil {
		n.Admin = *admin
	}
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

func (s *Server) oidcFinishLink(w http.ResponseWriter, r *http.Request, f oidcFlow, issuer, subject string, admin *bool) {
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
	if err := s.syncAdmin(r, *u, admin); err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/settings/security?done=sso-linked", http.StatusSeeOther)
}

// syncAdmin makes u an admin or not as the provider's group says; admin is
// nil when no group is configured. The group is the source of truth, so it
// can take the role from the last admin too.
func (s *Server) syncAdmin(r *http.Request, u store.User, admin *bool) error {
	if admin == nil || *admin == u.IsAdmin {
		return nil
	}
	s.log.Info("admin role from the provider's group", "user", u.Username, "admin", *admin)
	return s.store.SyncAdmin(r.Context(), u.ID, *admin)
}

// groupAdmins reports whether the provider's group decides who is admin.
func (s *Server) groupAdmins() bool { return s.cfg.OIDC != nil && s.cfg.OIDC.AdminGroup != "" }

// isAdmin reports whether the request has admin rights. With an admin group
// only a session signed in through the provider has them, while the account
// is still linked: the group was checked there, and is again as the session
// is used (see recheckGroup).
func (s *Server) isAdmin(r *http.Request) bool {
	u, ss := currentUser(r), currentSession(r)
	return s.accountAdmin(r.Context(), u) && (!s.groupAdmins() || ss.SSO)
}

// accountAdmin reports whether u's account is an admin's, whatever the
// session: with an admin group, only a linked account can be (an unlinked
// one may still carry the flag from before the group).
func (s *Server) accountAdmin(ctx context.Context, u *store.User) bool {
	return u != nil && u.IsAdmin && (!s.groupAdmins() || s.ssoLinked(ctx, u))
}

// groupStale reports whether the session's admin rights rest on a group
// check older than adminGroupCheck: its sign-in through the provider, or
// the provider's last answer since.
func (s *Server) groupStale(r *http.Request) bool {
	ss := currentSession(r)
	return s.groupAdmins() && time.Since(later(ss.CreatedAt, ss.GroupsCheckedAt)) > adminGroupCheck
}

// errSSOEnded: the provider no longer accepts the session's tokens.
var errSSOEnded = errors.New("the provider ended the session")

// recheckGroup asks the provider again whether a session signed in there is
// in the admin group: at every page load, and for other requests once the
// last check is over adminGroupCheck old. The admin pages ask on every
// request themselves (see requireAdmin). When the provider has ended the
// session, Anchor ends it too; when it can't be reached, the request goes on.
func (s *Server) recheckGroup(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.groupAdmins() || !currentSession(r).SSO || adminPath(r) || !(pageLoad(r) || s.groupStale(r)) {
			next.ServeHTTP(w, r)
			return
		}
		err := s.checkGroup(r)
		switch {
		case errors.Is(err, errSSOEnded):
			s.endSSOSession(w, r)
			return
		case err != nil:
			s.log.Warn("oidc: checking the admin group", "user", currentUser(r).Username, "err", err)
		}
		next.ServeHTTP(w, r)
	})
}

// endSSOSession signs out a session the provider no longer accepts.
func (s *Server) endSSOSession(w http.ResponseWriter, r *http.Request) {
	ss := currentSession(r)
	s.log.Info("signed out: the provider ended the session", "user", currentUser(r).Username)
	if err := s.store.DeleteSession(r.Context(), ss.UserID, ss.TokenHash); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.clearSessionCookie(w)
	if r.Method != http.MethodGet {
		http.Error(w, langOf(r).T("Not logged in"), http.StatusUnauthorized)
		return
	}
	http.Redirect(w, r, "/login?next="+urlQueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
}

// pageLoad reports whether r is the browser loading a page, not a script's
// call: it asks for HTML. (Sec-Fetch-Mode won't do: through the service
// worker, a page load isn't "navigate".) Without any Sec-Fetch headers, it's
// not a browser's script either.
func pageLoad(r *http.Request) bool {
	return r.Method == http.MethodGet &&
		(strings.Contains(r.Header.Get("Accept"), "text/html") || r.Header.Get("Sec-Fetch-Mode") == "")
}

func adminPath(r *http.Request) bool {
	return r.URL.Path == "/admin" || strings.HasPrefix(r.URL.Path, "/admin/")
}

// checkGroup asks the provider's userinfo endpoint for the user's groups and
// applies the admin group. The access token is renewed when it has expired
// or is refused; errSSOEnded when the provider refuses the refresh token, or
// there is none.
func (s *Server) checkGroup(r *http.Request) error {
	ss, u := currentSession(r), currentUser(r)
	subject, err := s.store.IdentityOf(r.Context(), u.ID, s.cfg.OIDC.Issuer)
	if errors.Is(err, store.ErrNotFound) {
		return nil // unlinked: no admin rights to check (see accountAdmin)
	} else if err != nil {
		return err
	}
	p, oc, err := s.oidc.discover()
	if err != nil {
		return err
	}
	// Two requests mustn't both spend the refresh token: a provider may take
	// each only once. The second one finds the tokens the first one got.
	lock := &s.oidc.checks[int(ss.TokenHash[0])%len(s.oidc.checks)]
	lock.Lock()
	defer lock.Unlock()
	sealed, err := s.store.SessionTokens(r.Context(), ss.TokenHash)
	if err != nil {
		return err
	}
	var tok oauth2.Token
	if b, err := s.keys.Open(ssoTokensKey, sealed); err != nil || json.Unmarshal(b, &tok) != nil {
		return errSSOEnded // a session from before Anchor kept them
	}

	ctx, cancel := context.WithTimeout(oidc.ClientContext(r.Context(), s.oidc.http), groupCheckTimeout)
	defer cancel()
	renewed := false
	renew := func() error {
		if tok.RefreshToken == "" {
			return errSSOEnded
		}
		old := tok
		old.AccessToken = "" // makes the token source refresh it
		t, err := oc.TokenSource(ctx, &old).Token()
		var re *oauth2.RetrieveError
		if errors.As(err, &re) && re.Response != nil && re.Response.StatusCode < 500 {
			return errSSOEnded
		} else if err != nil {
			return err
		}
		tok, renewed = *t, true
		return nil
	}
	if !tok.Valid() {
		if err := renew(); err != nil {
			return err
		}
	}
	info, err := p.UserInfo(ctx, oauth2.StaticTokenSource(&tok))
	if err != nil && !renewed {
		// Perhaps revoked before it expired: once more with new tokens.
		if err := renew(); err != nil {
			return err
		}
		info, err = p.UserInfo(ctx, oauth2.StaticTokenSource(&tok))
	}
	if err != nil {
		return err
	}
	if info.Subject != subject {
		return errSSOEnded
	}
	var claims map[string]any
	if err := info.Claims(&claims); err != nil {
		return err
	}
	if _, ok := claims[s.cfg.OIDC.GroupsClaim]; !ok {
		// Some providers (Entra ID) leave the groups out of userinfo; a new
		// ID token has them.
		if !renewed {
			if err := renew(); err != nil {
				return err
			}
		}
		raw, _ := tok.Extra("id_token").(string)
		idt, err := p.Verifier(&oidc.Config{ClientID: s.cfg.OIDC.ClientID}).Verify(ctx, raw)
		if err != nil {
			return err
		}
		if idt.Subject != subject {
			return errSSOEnded
		}
		if err := idt.Claims(&claims); err != nil {
			return err
		}
	}

	admin := s.inAdminGroup(claims)
	if err := s.syncAdmin(r, *u, admin); err != nil {
		return err
	}
	u.IsAdmin = *admin // for the rest of this request
	if sealed, err = s.sealTokens(&tok); err != nil {
		return err
	}
	if err := s.store.GroupsChecked(r.Context(), ss.TokenHash, sealed); err != nil {
		return err
	}
	ss.GroupsCheckedAt = time.Now()
	return nil
}

// sealTokens encrypts the provider's tokens for keeping with a session.
func (s *Server) sealTokens(tok *oauth2.Token) ([]byte, error) {
	b, err := json.Marshal(tok)
	if err != nil {
		return nil, err
	}
	return s.keys.Seal(ssoTokensKey, b)
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// ssoLinked reports whether u's account is linked to the provider.
func (s *Server) ssoLinked(ctx context.Context, u *store.User) bool {
	if s.oidc == nil || u == nil {
		return false
	}
	_, err := s.store.IdentityOf(ctx, u.ID, s.cfg.OIDC.Issuer)
	return err == nil
}
