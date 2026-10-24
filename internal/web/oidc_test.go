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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"

	"github.com/marcelhoffs/anchor/internal/config"
	"github.com/marcelhoffs/anchor/internal/store"
)

// fakeIdP is an OpenID Connect provider for tests: discovery, keys and a
// token endpoint that hands out the ID token prepared for each code.
type fakeIdP struct {
	t      *testing.T
	srv    *httptest.Server
	key    *rsa.PrivateKey
	mu     sync.Mutex
	codes  map[string]idpGrant
	lastAu url.Values // the last authorization request's query
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
	f := &fakeIdP{t: t, key: key, codes: map[string]idpGrant{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"issuer": f.srv.URL, "authorization_endpoint": f.srv.URL + "/authorize", "token_endpoint": f.srv.URL + "/token",
			"jwks_uri": f.srv.URL + "/jwks", "id_token_signing_alg_values_supported": []string{"RS256"},
			"response_types_supported": []string{"code"}, "subject_types_supported": []string{"public"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		f.mu.Lock()
		g, ok := f.codes[r.PostFormValue("code")]
		delete(f.codes, r.PostFormValue("code"))
		f.mu.Unlock()
		sum := sha256.Sum256([]byte(r.PostFormValue("code_verifier")))
		if !ok || base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "id_token": f.sign(g.claims)})
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

func TestSSOOnlyAndOff(t *testing.T) {
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
