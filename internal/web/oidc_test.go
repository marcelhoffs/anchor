package web

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"

	"github.com/marcelhoffs/anchor/internal/config"
	"github.com/marcelhoffs/anchor/internal/store"
)

// fakeIdP is an OpenID Connect provider for tests: discovery, keys, a
// token endpoint that hands out the ID token prepared for each code and
// renews tokens, and userinfo with each subject's groups.
type fakeIdP struct {
	t      *testing.T
	srv    *httptest.Server
	key    *rsa.PrivateKey
	mu     sync.Mutex
	codes  map[string]idpGrant
	lastAu url.Values // the last authorization request's query

	access, refresh map[string]string // valid tokens, to their subject
	groups          map[string]any    // each subject's groups now
	n               int               // tokens issued
	userinfos       int               // userinfo calls
	refreshes       int               // tokens renewed
	down            bool              // userinfo and renewing fail
	noGroupInfo     bool              // userinfo leaves the groups out, like Entra ID
}

type idpGrant struct {
	claims    map[string]any
	challenge string // PKCE S256 challenge the code was issued for
}

func newFakeIdP(t *testing.T) *fakeIdP {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIdP{t: t, key: key, codes: map[string]idpGrant{},
		access: map[string]string{}, refresh: map[string]string{}, groups: map[string]any{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"issuer": f.srv.URL, "authorization_endpoint": f.srv.URL + "/authorize", "token_endpoint": f.srv.URL + "/token",
			"jwks_uri": f.srv.URL + "/jwks", "userinfo_endpoint": f.srv.URL + "/userinfo",
			"id_token_signing_alg_values_supported": []string{"RS256"}, "response_types_supported": []string{"code"},
			"subject_types_supported": []string{"public"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		f.mu.Lock()
		defer f.mu.Unlock()
		var claims map[string]any
		if r.PostFormValue("grant_type") == "refresh_token" {
			sub, ok := f.refresh[r.PostFormValue("refresh_token")]
			if f.down {
				http.Error(w, "down", http.StatusBadGateway)
				return
			}
			delete(f.refresh, r.PostFormValue("refresh_token")) // each works once
			if !ok {
				http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
				return
			}
			f.refreshes++
			claims = map[string]any{"iss": f.srv.URL, "aud": "anchor", "sub": sub, "groups": f.groups[sub],
				"iat": time.Now().Unix(), "exp": time.Now().Add(5 * time.Minute).Unix()}
		} else {
			g, ok := f.codes[r.PostFormValue("code")]
			delete(f.codes, r.PostFormValue("code"))
			sum := sha256.Sum256([]byte(r.PostFormValue("code_verifier")))
			if !ok || base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge {
				http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
				return
			}
			claims = g.claims
			if gr, ok := claims["groups"]; ok {
				f.groups[claims["sub"].(string)] = gr
			}
		}
		f.n++
		sub := claims["sub"].(string)
		at, rt := "at-"+strconv.Itoa(f.n), "rt-"+strconv.Itoa(f.n)
		f.access[at], f.refresh[rt] = sub, sub
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"access_token": at, "refresh_token": rt, "token_type": "Bearer",
			"expires_in": 300, "id_token": f.sign(claims)})
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.userinfos++
		sub, ok := f.access[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
		switch {
		case f.down:
			http.Error(w, "down", http.StatusBadGateway)
		case !ok:
			http.Error(w, "invalid token", http.StatusUnauthorized)
		case f.noGroupInfo:
			json.NewEncoder(w).Encode(map[string]any{"sub": sub})
		default:
			json.NewEncoder(w).Encode(map[string]any{"sub": sub, "groups": f.groups[sub]})
		}
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeIdP) sign(claims map[string]any) string {
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: f.key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "k1"))
	if err != nil {
		f.t.Fatal(err)
	}
	payload, _ := json.Marshal(claims)
	obj, err := signer.Sign(payload)
	if err != nil {
		f.t.Fatal(err)
	}
	s, _ := obj.CompactSerialize()
	return s
}

// oidcEnv is Anchor configured for the fake provider.
func oidcEnv(t *testing.T, change func(*config.OIDC)) (*env, *fakeIdP) {
	idp := newFakeIdP(t)
	e := newEnvWith(t, "localhost", func(c *config.Config) {
		c.OIDC = &config.OIDC{Issuer: idp.srv.URL, ClientID: "anchor", ClientSecret: "s3cret", Name: "Authentik",
			GroupsClaim: "groups", Scopes: []string{"openid", "profile", "email"}}
		if change != nil {
			change(c.OIDC)
		}
	})
	return e, idp
}

// ssoFlow walks c through a sign-in at the provider, as subject with the
// given extra claims. start is the request that sends the browser there
// (a GET path, or a POST with a form). It returns the callback's answer.
func (e *env) ssoFlow(idp *fakeIdP, c *client, start func() (int, string, string), subject string, extra map[string]any) (int, string, string) {
	e.t.Helper()
	code, body, loc := start()
	if code != http.StatusSeeOther || !strings.HasPrefix(loc, idp.srv.URL+"/authorize") {
		e.t.Fatalf("start: %d %q %s", code, loc, body)
	}
	au, _ := url.Parse(loc)
	q := au.Query()
	claims := map[string]any{
		"iss": idp.srv.URL, "aud": "anchor", "sub": subject, "nonce": q.Get("nonce"),
		"iat": time.Now().Unix(), "exp": time.Now().Add(5 * time.Minute).Unix(),
		"auth_time": time.Now().Unix(), // the user signed in at the provider just now
	}
	for k, v := range extra {
		claims[k] = v
	}
	idp.mu.Lock()
	idp.codes["code-"+subject] = idpGrant{claims, q.Get("code_challenge")}
	idp.lastAu = q
	idp.mu.Unlock()
	return c.get("/login/oidc/callback?state=" + url.QueryEscape(q.Get("state")) + "&code=code-" + subject)
}

func getStart(c *client, path string) func() (int, string, string) {
	return func() (int, string, string) { return c.get(path) }
}

func TestSSOFirstUserAndLogin(t *testing.T) {
	e, idp := oidcEnv(t, nil)
	c := e.client()
	// No users yet: the first sign-in through the provider sets Anchor up.
	code, _, loc := e.ssoFlow(idp, c, getStart(c, "/login/oidc"), "u-1", map[string]any{"preferred_username": "Marcel H."})
	if code != http.StatusSeeOther || loc != "/" {
		t.Fatalf("first sign-in: %d %q", code, loc)
	}
	u, err := e.store.UserByUsername(context.Background(), "Marcel-H")
	if err != nil || !u.IsAdmin || u.HasPassword() {
		t.Fatalf("first user: %+v %v", u, err)
	}
	if code, body, _ := c.get("/"); code != http.StatusOK || !strings.Contains(body, "Marcel-H") {
		t.Fatalf("signed in: %d", code)
	}
	// Signing in again finds the same account.
	c2 := e.client()
	if code, _, loc := e.ssoFlow(idp, c2, getStart(c2, "/login/oidc?next=%2Ftags"), "u-1", nil); code != http.StatusSeeOther || loc != "/tags" {
		t.Fatalf("second sign-in: %d %q", code, loc)
	}
	if n, _ := e.store.CountUsers(context.Background()); n != 1 {
		t.Fatalf("users: %d", n)
	}
	// Without a password, the password form can't sign in.
	if code, _, _ := c.post("/login", url.Values{"username": {"Marcel-H"}, "password": {""}}); code == http.StatusSeeOther {
		t.Fatal("password sign-in without a password")
	}
}

func TestSSOSignupModes(t *testing.T) {
	e, idp := oidcEnv(t, nil)
	root := e.admin()
	try := func(sub, start string) (int, string) {
		c := e.client()
		code, body, _ := e.ssoFlow(idp, c, getStart(c, start), sub, map[string]any{"preferred_username": sub})
		return code, body
	}
	// Closed (the default): no account for an unknown identity.
	if code, body := try("closed-1", "/login/oidc"); code != http.StatusUnprocessableEntity || !strings.Contains(body, "No Anchor account is linked") {
		t.Fatalf("closed: %d", code)
	}
	// Invite only: with an invite, once.
	root.post("/admin/settings", url.Values{"signup": {"invite"}})
	_, body, _ := root.post("/admin/invites", url.Values{"note": {"x"}})
	token := strings.Split(strings.Split(body, "invite=")[1], `"`)[0]
	if code, _ := try("invite-1", "/login/oidc"); code != http.StatusUnprocessableEntity {
		t.Fatalf("invite mode without invite: %d", code)
	}
	if code, _ := try("invite-2", "/login/oidc?invite="+token); code != http.StatusSeeOther {
		t.Fatalf("with invite: %d", code)
	}
	if code, _ := try("invite-3", "/login/oidc?invite="+token); code != http.StatusUnprocessableEntity {
		t.Fatalf("invite used twice: %d", code)
	}
	// Open: anyone the provider lets in; a taken name gets a suffix.
	root.post("/admin/settings", url.Values{"signup": {"open"}})
	if code, _ := try("root", "/login/oidc"); code != http.StatusSeeOther {
		t.Fatalf("open: %d", code)
	}
	if _, err := e.store.UserByUsername(context.Background(), "root-2"); err != nil {
		t.Fatalf("suffixed username: %v", err)
	}
}

func TestSSORejectsBadFlows(t *testing.T) {
	e, idp := oidcEnv(t, nil)
	e.admin()
	c := e.client()
	code, _, loc := c.get("/login/oidc")
	if code != http.StatusSeeOther {
		t.Fatal(code)
	}
	q, _ := url.Parse(loc)
	state := q.Query().Get("state")
	// Another browser can't finish it.
	if code, _, _ := e.client().get("/login/oidc/callback?state=" + state + "&code=x"); code != http.StatusUnprocessableEntity {
		t.Fatalf("foreign browser: %d", code)
	}
	// A wrong nonce, audience or an expired token is refused.
	for name, extra := range map[string]map[string]any{
		"nonce":    {"nonce": "other"},
		"audience": {"aud": "someone-else"},
		"expired":  {"exp": time.Now().Add(-time.Minute).Unix()},
	} {
		c := e.client()
		if code, _, _ := e.ssoFlow(idp, c, getStart(c, "/login/oidc"), "bad-"+name, extra); code != http.StatusUnprocessableEntity {
			t.Errorf("%s: %d", name, code)
		}
	}
	// A state works once.
	c = e.client()
	code, _, _ = e.ssoFlow(idp, c, getStart(c, "/login/oidc"), "once", nil)
	if code != http.StatusUnprocessableEntity { // closed sign-up, but the flow itself got through
		t.Fatalf("once: %d", code)
	}
	if code, _, _ := c.get("/login/oidc/callback?state=" + url.QueryEscape(idp.lastAu.Get("state")) + "&code=code-once"); code != http.StatusUnprocessableEntity {
		t.Fatalf("replayed state: %d", code)
	}
}

func TestSSOLinkReauthUnlink(t *testing.T) {
	e, idp := oidcEnv(t, nil)
	e.user("alice")
	a := e.login("alice")
	e.expireReauth()
	// Linking is a sensitive action: it asks to confirm first.
	if code, _, loc := a.post("/settings/sso/link", nil); code != http.StatusSeeOther || !strings.HasPrefix(loc, "/reauth") {
		t.Fatalf("link without recent auth: %d %q", code, loc)
	}
	a.post("/reauth/password", url.Values{"password": {"alice-password"}, "next": {"/settings/security"}})
	code, _, loc := e.ssoFlow(idp, a, func() (int, string, string) { return a.post("/settings/sso/link", nil) }, "alice-sub", nil)
	if code != http.StatusSeeOther || loc != "/settings/security?done=sso-linked" {
		t.Fatalf("link: %d %q", code, loc)
	}
	if _, body, _ := a.get("/settings/security"); !strings.Contains(body, "Your account is linked to Authentik") {
		t.Fatal("security page doesn't show the link")
	}
	// The identity now signs in as alice, without her password.
	c := e.client()
	if code, _, _ := e.ssoFlow(idp, c, getStart(c, "/login/oidc"), "alice-sub", nil); code != http.StatusSeeOther {
		t.Fatalf("sign in as alice: %d", code)
	}
	// Another user can't link the same identity.
	e.user("bob")
	b := e.login("bob")
	if code, body, _ := e.ssoFlow(idp, b, func() (int, string, string) { return b.post("/settings/sso/link", nil) }, "alice-sub", nil); code != http.StatusConflict || !strings.Contains(body, "already linked") {
		t.Fatalf("link taken identity: %d", code)
	}

	// Reauth through the provider: only alice's own identity counts.
	e.expireReauth()
	if _, body, _ := a.get("/reauth?next=%2Fsettings%2Ftokens"); !strings.Contains(body, "Confirm with Authentik") {
		t.Fatal("reauth page lacks the provider")
	}
	reauth := func() (int, string, string) { return a.post("/reauth/sso", url.Values{"next": {"/settings/tokens"}}) }
	if code, _, _ := e.ssoFlow(idp, a, reauth, "someone-else", nil); code != http.StatusForbidden {
		t.Fatalf("reauth as someone else: %d", code)
	}
	if !strings.Contains(idp.lastAu.Get("prompt"), "login") {
		t.Fatal("reauth doesn't ask the provider to sign in again")
	}
	if code, _, loc := e.ssoFlow(idp, a, reauth, "alice-sub", nil); code != http.StatusSeeOther || loc != "/settings/tokens?done=confirmed" {
		t.Fatalf("reauth: %d %q", code, loc)
	}

	// Unlink: fine with a password; refused for an account without one.
	if code, _, loc := a.post("/settings/sso/unlink", nil); code != http.StatusSeeOther || loc != "/settings/security?done=sso-unlinked" {
		t.Fatalf("unlink: %d %q", code, loc)
	}
	e.store.SetInstanceSetting(context.Background(), store.SettingSignup, "open")
	n := e.client()
	e.ssoFlow(idp, n, getStart(n, "/login/oidc"), "new-sub", map[string]any{"preferred_username": "nopass"})
	if code, body, _ := n.post("/settings/sso/unlink", nil); code != http.StatusConflict || !strings.Contains(body, "Set a password first") {
		t.Fatalf("unlink without password: %d", code)
	}
	// It sets a first password without a current one (the sign-in was just now).
	if code, _, loc := n.post("/settings/password", url.Values{"password": {"brand-new-pass"}, "confirm": {"brand-new-pass"}}); code != http.StatusSeeOther || loc != "/settings/security?done=password" {
		t.Fatalf("set first password: %d %q", code, loc)
	}
}

func TestSSOAdminGroupAndPolicy(t *testing.T) {
	e, idp := oidcEnv(t, func(o *config.OIDC) { o.AdminGroup = "anchor-admins" })
	e.admin()
	e.store.SetInstanceSetting(context.Background(), store.SettingSignup, "open")
	e.store.SetInstanceSetting(context.Background(), store.SettingRequire2FA, "true")
	c := e.client()
	e.ssoFlow(idp, c, getStart(c, "/login/oidc"), "g-1", map[string]any{"preferred_username": "gina", "groups": []string{"staff", "anchor-admins"}})
	u, _ := e.store.UserByUsername(context.Background(), "gina")
	if !u.IsAdmin {
		t.Fatal("admin group not applied")
	}
	if idp.lastAu.Get("scope") != "openid profile email" {
		t.Fatalf("scopes: %q", idp.lastAu.Get("scope"))
	}
	// The provider did the second factor: the 2FA policy doesn't stop her.
	if code, _, loc := c.get("/"); code != http.StatusOK {
		t.Fatalf("SSO session sent to 2FA setup: %d %q", code, loc)
	}
	// Leaving the group takes admin away at the next sign-in.
	c = e.client()
	e.ssoFlow(idp, c, getStart(c, "/login/oidc"), "g-1", map[string]any{"groups": []string{"staff"}})
	if u, _ := e.store.UserByUsername(context.Background(), "gina"); u.IsAdmin {
		t.Fatal("admin kept after leaving the group")
	}
	// A password session still meets the policy.
	e.user("pat")
	if code, _, loc := e.login("pat").get("/"); code != http.StatusSeeOther || !strings.HasPrefix(loc, "/settings/security") {
		t.Fatalf("password session skipped 2FA policy: %d %q", code, loc)
	}
	// A disabled account can't sign in through the provider either.
	e.store.SetActive(context.Background(), u.ID, false)
	c = e.client()
	if code, body, _ := e.ssoFlow(idp, c, getStart(c, "/login/oidc"), "g-1", nil); code != http.StatusUnprocessableEntity || !strings.Contains(body, "disabled") {
		t.Fatalf("disabled: %d", code)
	}
}

// A sign-in through the provider confirms it's you only when the provider
// really asked (auth_time), not when it reused its session; a reauth must.
func TestSSOFreshAuth(t *testing.T) {
	e, idp := oidcEnv(t, nil)
	e.admin()
	e.store.SetInstanceSetting(context.Background(), store.SettingSignup, "open")
	hourAgo := time.Now().Add(-time.Hour).Unix()
	needsReauth := func(c *client) bool {
		_, _, loc := c.post("/settings/tokens", url.Values{"name": {"t"}})
		return strings.HasPrefix(loc, "/reauth")
	}
	silent := e.client()
	e.ssoFlow(idp, silent, getStart(silent, "/login/oidc"), "sam-sub", map[string]any{"preferred_username": "sam", "auth_time": hourAgo})
	if !needsReauth(silent) {
		t.Fatal("a sign-in on the provider's old session counts as confirmed")
	}
	reauth := func() (int, string, string) {
		return silent.post("/reauth/sso", url.Values{"next": {"/settings/tokens"}})
	}
	for name, at := range map[string]any{"stale": hourAgo, "missing": nil, "ahead": time.Now().Add(time.Hour).Unix()} {
		if code, body, _ := e.ssoFlow(idp, silent, reauth, "sam-sub", map[string]any{"auth_time": at}); code != http.StatusForbidden ||
			!strings.Contains(body, "ask you to sign in again") {
			t.Errorf("reauth with %s auth_time: %d", name, code)
		}
	}
	if !needsReauth(silent) {
		t.Fatal("refused reauths confirmed the session")
	}
	if code, _, loc := e.ssoFlow(idp, silent, reauth, "sam-sub", nil); code != http.StatusSeeOther || loc != "/settings/tokens?done=confirmed" {
		t.Fatalf("fresh reauth: %d %q", code, loc)
	}
	if needsReauth(silent) {
		t.Fatal("fresh reauth didn't confirm")
	}
	// A sign-in where the provider just asked counts.
	fresh := e.client()
	e.ssoFlow(idp, fresh, getStart(fresh, "/login/oidc"), "sam-sub", nil)
	if needsReauth(fresh) {
		t.Fatal("fresh sign-in not confirmed")
	}
}

// Starting a sign-in is open to anyone: limited per IP, with what it keeps
// cut to size and a ceiling on how many are in flight.
func TestSSOStartLimits(t *testing.T) {
	e, _ := oidcEnv(t, nil)
	e.admin()
	c := e.client()
	if code, _, _ := c.get("/login/oidc?invite=" + strings.Repeat("x", 5000) + "&next=%2F" + strings.Repeat("y", 5000)); code != http.StatusSeeOther {
		t.Fatalf("start: %d", code)
	}
	o := e.server.oidc
	o.mu.Lock()
	for _, f := range o.flows {
		if f.invite != "" || f.next != "/" {
			t.Errorf("kept invite %d and next %d bytes", len(f.invite), len(f.next))
		}
	}
	o.mu.Unlock()
	for i := 1; i < 30; i++ {
		c.get("/login/oidc")
	}
	if code, _, _ := c.get("/login/oidc"); code != http.StatusTooManyRequests {
		t.Fatalf("31st start from one IP: %d", code)
	}
	e.server.ssoIP.Reset("127.0.0.1")
	o.mu.Lock()
	for i := len(o.flows); i < maxOIDCFlows; i++ {
		o.flows[strconv.Itoa(i)] = oidcFlow{expires: time.Now().Add(time.Minute)}
	}
	o.mu.Unlock()
	if code, body, _ := c.get("/login/oidc"); code != http.StatusUnprocessableEntity || !strings.Contains(body, "unavailable") {
		t.Fatalf("start beyond the ceiling: %d", code)
	}
}

// A provider that's down is asked again only after discoveryRetry, and
// doesn't hold up the sign-ins in flight meanwhile.
func TestSSODiscoveryRetry(t *testing.T) {
	var hits atomic.Int32
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "down", http.StatusBadGateway)
	}))
	defer down.Close()
	e := newEnvWith(t, "localhost", func(c *config.Config) {
		c.OIDC = &config.OIDC{Issuer: down.URL, ClientID: "anchor", Name: "Down", Scopes: []string{"openid"}}
	})
	e.admin()
	c := e.client()
	for range 3 {
		if code, _, _ := c.get("/login/oidc"); code != http.StatusUnprocessableEntity {
			t.Fatalf("start while down: %d", code)
		}
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("asked the provider %d times", n)
	}
}

// With an admin group, the group alone decides who is admin, and admin rights
// rest on a recent check of it.
func TestSSOAdminGroupIsTheSourceOfTruth(t *testing.T) {
	e, idp := oidcEnv(t, func(o *config.OIDC) { o.AdminGroup = "anchor-admins" })
	ctx := context.Background()
	root := e.admin() // an admin from before the group, with a password
	e.store.SetInstanceSetting(ctx, store.SettingSignup, "open")
	inGroup := map[string]any{"preferred_username": "gina", "groups": []string{"anchor-admins"}}
	g := e.client()
	e.ssoFlow(idp, g, getStart(g, "/login/oidc"), "g-1", inGroup)

	// Only a session through the provider has admin rights.
	if code, _, _ := root.get("/admin/users"); code != http.StatusNotFound {
		t.Fatalf("password admin: %d", code)
	}
	if _, body, _ := root.get("/"); strings.Contains(body, `href="/admin/"`) {
		t.Fatal("password admin offered the admin pages")
	}
	code, body, _ := g.get("/admin/users")
	if code != http.StatusOK || strings.Contains(body, "/make-admin") || strings.Contains(body, "/remove-admin") || !strings.Contains(body, "anchor-admins") {
		t.Fatalf("admin pages: %d", code)
	}
	// root still has the flag from before the group, but isn't an admin.
	if strings.Contains(body, `<strong>root</strong> <span class="badge on">`) || !strings.Contains(body, `<strong>gina</strong> <span class="badge on">`) {
		t.Fatal("admin badges don't follow the group")
	}
	// Anchor doesn't hand out or take away the role.
	if code, body, _ := g.post("/admin/users/"+strconv.FormatInt(e.userID("root"), 10)+"/remove-admin", nil); code != http.StatusConflict ||
		!strings.Contains(body, "Admin rights come from the anchor-admins group") {
		t.Fatalf("remove-admin: %d", code)
	}

	// Later on, the provider confirms the group without asking her anything;
	// confirming it's her goes through the provider only.
	if _, err := e.db.W.Exec("UPDATE sessions SET created_at = ?, verified_at = 0", time.Now().Add(-2*time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := g.get("/admin/users"); code != http.StatusOK {
		t.Fatalf("admin pages after a while: %d", code)
	}
	if _, body, _ := g.get("/reauth?next=%2Fadmin%2Fusers"); strings.Contains(body, `action="/reauth/password"`) || !strings.Contains(body, "Confirm with Authentik") {
		t.Fatal("reauth offers more than the provider")
	}
	if code, _, _ := g.post("/reauth/recovery", url.Values{"code": {"x"}, "next": {"/admin/users"}}); code != http.StatusForbidden {
		t.Fatalf("reauth with a recovery code: %d", code)
	}
	// She left the group: the reauth takes the role away, even from the last admin.
	e.store.SyncAdmin(ctx, e.userID("root"), false)
	reauth := func() (int, string, string) { return g.post("/reauth/sso", url.Values{"next": {"/admin/users"}}) }
	e.ssoFlow(idp, g, reauth, "g-1", map[string]any{"groups": []string{"staff"}})
	if code, _, _ := g.get("/admin/users"); code != http.StatusNotFound {
		t.Fatalf("admin pages after leaving the group: %d", code)
	}
	// Back in the group, her next sign-in makes her admin again.
	g = e.client()
	e.ssoFlow(idp, g, getStart(g, "/login/oidc"), "g-1", inGroup)
	if code, _, _ := g.get("/admin/users"); code != http.StatusOK {
		t.Fatalf("admin pages after rejoining: %d", code)
	}
	// Unlinked, the group can't vouch for her any more.
	e.store.UnlinkIdentity(ctx, e.userID("gina"), true)
	if code, _, _ := g.get("/admin/users"); code != http.StatusNotFound {
		t.Fatalf("admin pages after unlinking: %d", code)
	}
}

// With an admin group, the provider confirms the group at every page load
// and every adminGroupCheck otherwise, renewing tokens as needed; a session
// it no longer accepts ends.
func TestSSOAdminGroupRecheck(t *testing.T) {
	e, idp := oidcEnv(t, func(o *config.OIDC) { o.AdminGroup = "anchor-admins" })
	ctx := context.Background()
	e.admin()
	e.store.SetInstanceSetting(ctx, store.SettingSignup, "open")
	g := e.client()
	e.ssoFlow(idp, g, getStart(g, "/login/oidc"), "g-1", map[string]any{"preferred_username": "gina", "groups": []string{"anchor-admins"}})
	setGroups := func(groups ...string) {
		idp.mu.Lock()
		idp.groups["g-1"] = groups
		idp.mu.Unlock()
	}
	isAdmin := func() bool {
		u, _ := e.store.UserByUsername(ctx, "gina")
		return u.IsAdmin
	}
	stale := func() {
		old := time.Now().Add(-10 * time.Minute).Unix()
		e.db.W.Exec("UPDATE sessions SET created_at = ?, groups_checked_at = ?", old, old)
	}

	// Removed from the group: the next page load takes admin away.
	setGroups("staff")
	if _, body, _ := g.get("/"); strings.Contains(body, `href="/admin/"`) || isAdmin() {
		t.Fatal("admin kept after leaving the group")
	}
	if code, _, _ := g.get("/admin/users"); code != http.StatusNotFound {
		t.Fatalf("admin pages after leaving the group: %d", code)
	}
	setGroups("anchor-admins")
	if code, _, _ := g.get("/admin/users"); code != http.StatusOK || !isAdmin() {
		t.Fatalf("admin pages after rejoining: %d", code)
	}

	// A script's call checks only once the last check is old.
	calls := func() int {
		idp.mu.Lock()
		defer idp.mu.Unlock()
		return idp.userinfos
	}
	before := calls()
	g.post("/settings/appearance", url.Values{"theme": {"dark"}})
	if calls() != before {
		t.Fatal("a recent check was repeated")
	}
	stale()
	g.post("/settings/appearance", url.Values{"theme": {"dark"}})
	if calls() != before+1 {
		t.Fatal("an old check wasn't repeated")
	}

	// An access token that's refused is renewed with the refresh token.
	idp.mu.Lock()
	clear(idp.access)
	idp.mu.Unlock()
	if code, _, _ := g.get("/admin/users"); code != http.StatusOK || idp.refreshes != 1 {
		t.Fatalf("renewing the access token: %d, %d renewals", code, idp.refreshes)
	}

	// Groups left out of userinfo come from a new ID token.
	idp.noGroupInfo = true
	setGroups("staff")
	if g.get("/"); isAdmin() || idp.refreshes != 2 {
		t.Fatalf("groups from the ID token: admin %v, %d renewals", isAdmin(), idp.refreshes)
	}
	idp.noGroupInfo = false
	setGroups("anchor-admins")
	g.get("/")

	// The provider can't be reached: pages still work, admin pages only
	// while the last check is recent.
	idp.down = true
	if code, _, _ := g.get("/admin/users"); code != http.StatusOK {
		t.Fatalf("admin pages, provider down, recent check: %d", code)
	}
	stale()
	if code, _, _ := g.get("/"); code != http.StatusOK {
		t.Fatalf("pages while the provider is down: %d", code)
	}
	if code, _, _ := g.get("/admin/users"); code != http.StatusServiceUnavailable {
		t.Fatalf("admin pages, provider down, old check: %d", code)
	}
	idp.down = false

	// The provider refuses the refresh token too: the session ends.
	idp.mu.Lock()
	clear(idp.access)
	clear(idp.refresh)
	idp.mu.Unlock()
	if code, _, loc := g.get("/tags"); code != http.StatusSeeOther || loc != "/login?next=%2Ftags" {
		t.Fatalf("session the provider ended: %d %q", code, loc)
	}
	if code, _, _ := g.get("/"); code != http.StatusSeeOther {
		t.Fatalf("still signed in: %d", code)
	}
}

func TestSSOOnlyAndOff(t *testing.T) {
	// A fresh instance sets up through the provider only.
	fresh, _ := oidcEnv(t, func(o *config.OIDC) { o.Only = true })
	fc := fresh.client()
	if _, body, _ := fc.get("/setup"); strings.Contains(body, `action="/setup"`) || !strings.Contains(body, "Set up with Authentik") {
		t.Fatal("setup page offers a password while SSO-only")
	}
	if code, _, _ := fc.post("/setup", url.Values{"username": {"x"}, "password": {"long-enough-pw"}, "confirm": {"long-enough-pw"}}); code != http.StatusNotFound {
		t.Fatalf("password setup while SSO-only: %d", code)
	}

	e, _ := oidcEnv(t, func(o *config.OIDC) { o.Only = true })
	e.user("alice")
	c := e.client()
	_, body, _ := c.get("/login")
	if strings.Contains(body, `action="/login"`) || !strings.Contains(body, "Sign in with Authentik") {
		t.Fatal("login page still offers passwords")
	}
	if code, _, _ := c.post("/login", url.Values{"username": {"alice"}, "password": {"alice-password"}}); code != http.StatusNotFound {
		t.Fatalf("password login while SSO-only: %d", code)
	}
	if code, _, _ := c.post("/signup", url.Values{"username": {"x"}}); code != http.StatusNotFound {
		t.Fatalf("password sign-up while SSO-only: %d", code)
	}

	// Without OIDC configured, nothing of it shows or answers.
	off := newEnv(t)
	off.user("alice")
	oc := off.client()
	if _, body, _ := oc.get("/login"); strings.Contains(body, "/login/oidc") {
		t.Fatal("SSO button without SSO")
	}
	if code, _, _ := oc.get("/login/oidc"); code != http.StatusNotFound {
		t.Fatalf("/login/oidc while off: %d", code)
	}
	if code, _, _ := off.login("alice").post("/settings/sso/link", nil); code != http.StatusNotFound {
		t.Fatalf("link while off: %d", code)
	}
}

func TestUsernameFrom(t *testing.T) {
	for want, c := range map[string]oidcClaims{
		"marcel":    {PreferredUsername: "marcel"},
		"Jan-Peter": {PreferredUsername: "Jan Peter"},
		"ana.b":     {Email: "ana.b@example.com"},
		"jan":       {PreferredUsername: "jan@contoso.com"},
		"Zoe":       {Name: "Zoë"},
		"user":      {},
	} {
		if got := usernameFrom(c); got != want {
			t.Errorf("usernameFrom(%+v) = %q, want %q", c, got, want)
		}
	}
}
