package web

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/hotp"

	"github.com/marcelhoffs/burrow/internal/auth"
	"github.com/marcelhoffs/burrow/internal/ratelimit"
	"github.com/marcelhoffs/burrow/internal/store"
)

// --- helpers ---

func (c *client) postJSON(path string, body []byte) (int, map[string]any) {
	c.e.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, c.e.srv.URL+path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", c.csrf())
	res, err := c.http.Do(req)
	if err != nil {
		c.e.t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func find(t *testing.T, body, pattern string) string {
	t.Helper()
	m := regexp.MustCompile(pattern).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("pattern %q not found in page", pattern)
	}
	return m[1]
}

// expireReauth makes every session look like it last proved itself long ago.
func (e *env) expireReauth() {
	if _, err := e.db.W.Exec("UPDATE sessions SET verified_at = 0"); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) userID(name string) int64 {
	u, err := e.store.UserByUsername(context.Background(), name)
	if err != nil {
		e.t.Fatal(err)
	}
	return u.ID
}

func totpCode(t *testing.T, secret string, offset int64) string {
	t.Helper()
	return stepCode(t, secret, time.Now().Unix()/30+offset)
}

func stepCode(t *testing.T, secret string, step int64) string {
	t.Helper()
	c, err := hotp.GenerateCodeCustom(secret, uint64(step), hotp.ValidateOpts{Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// totpAfter returns the code for the step after the user's last used one:
// never a replay, and within the allowed drift whenever the test runs.
func (e *env) totpAfter(name, secret string) string {
	e.t.Helper()
	u, err := e.store.UserByUsername(context.Background(), name)
	if err != nil {
		e.t.Fatal(err)
	}
	return stepCode(e.t, secret, u.TOTPLastStep+1)
}

func (e *env) totpReplay(name, secret string) string {
	e.t.Helper()
	u, _ := e.store.UserByUsername(context.Background(), name)
	return stepCode(e.t, secret, u.TOTPLastStep)
}

// enableTOTP runs the setup flow and returns the secret and recovery codes.
func enableTOTP(t *testing.T, c *client) (string, []string) {
	t.Helper()
	code, body, _ := c.post("/settings/totp/start", nil)
	if code != http.StatusOK {
		t.Fatalf("totp start: %d", code)
	}
	secret := find(t, body, `<code class="secret">([A-Z2-7]+)</code>`)
	code, body, _ = c.post("/settings/totp/enable", url.Values{"code": {totpCode(t, secret, 0)}})
	if code != http.StatusOK || !strings.Contains(body, "Save your recovery codes") {
		t.Fatalf("totp enable: %d", code)
	}
	codes := regexp.MustCompile(`<li><code>([a-z0-9]{5}-[a-z0-9]{5})</code></li>`).FindAllStringSubmatch(body, -1)
	var out []string
	for _, m := range codes {
		out = append(out, m[1])
	}
	if len(out) != 10 {
		t.Fatalf("got %d recovery codes", len(out))
	}
	return secret, out
}

// passwordStep logs in with a password only and expects the 2FA page.
func (e *env) passwordStep(name string) *client {
	e.t.Helper()
	c := e.client()
	code, _, loc := c.post("/login", url.Values{"username": {name}, "password": {name + "-password"}})
	if code != http.StatusSeeOther || !strings.HasPrefix(loc, "/login/2fa") {
		e.t.Fatalf("password step: %d %q", code, loc)
	}
	return c
}

// --- TOTP and recovery codes ---

func TestTOTPLoginFlow(t *testing.T) {
	e := newEnv(t)
	e.user("alice")
	secret, recovery := enableTOTP(t, e.login("alice"))

	c := e.passwordStep("alice")
	// The half-logged-in session can't reach anything.
	if code, _, loc := c.get("/"); code != http.StatusSeeOther || !strings.HasPrefix(loc, "/login") {
		t.Fatalf("pending session reached /: %d %q", code, loc)
	}
	if code, body, _ := c.get("/login/2fa"); code != http.StatusOK || !strings.Contains(body, "Signing in as <strong>alice</strong>") {
		t.Fatalf("2fa page: %d", code)
	}
	if code, _, _ := c.post("/login/2fa/totp", url.Values{"code": {"000000"}}); code != http.StatusUnauthorized {
		t.Fatalf("wrong code: %d", code)
	}
	// Enrolment used the current step, so its code can't be replayed; the
	// next step's code is accepted (clock drift allowance).
	if code, _, _ := c.post("/login/2fa/totp", url.Values{"code": {e.totpReplay("alice", secret)}}); code != http.StatusUnauthorized {
		t.Fatalf("replayed enrolment code accepted: %d", code)
	}
	if code, _, loc := c.post("/login/2fa/totp", url.Values{"code": {e.totpAfter("alice", secret)}}); code != http.StatusSeeOther || loc != "/" {
		t.Fatalf("valid code: %d %q", code, loc)
	}
	if code, _, _ := c.get("/"); code != http.StatusOK {
		t.Fatalf("not logged in after 2FA: %d", code)
	}

	// A recovery code works once.
	c2 := e.passwordStep("alice")
	if code, _, _ := c2.post("/login/2fa/recovery", url.Values{"code": {strings.ToUpper(recovery[0])}}); code != http.StatusSeeOther {
		t.Fatalf("recovery code: %d", code)
	}
	c3 := e.passwordStep("alice")
	if code, _, _ := c3.post("/login/2fa/recovery", url.Values{"code": {recovery[0]}}); code != http.StatusUnauthorized {
		t.Fatalf("recovery code reused: %d", code)
	}
}

func TestSecondFactorRateLimit(t *testing.T) {
	e := newEnv(t)
	e.user("alice")
	secret, _ := enableTOTP(t, e.login("alice"))
	c := e.passwordStep("alice")
	for range 10 {
		c.post("/login/2fa/totp", url.Values{"code": {"000000"}})
	}
	if code, _, _ := c.post("/login/2fa/totp", url.Values{"code": {e.totpAfter("alice", secret)}}); code != http.StatusTooManyRequests {
		t.Fatalf("11th attempt: %d, want 429", code)
	}
}

func TestLoginRateLimit(t *testing.T) {
	e := newEnv(t)
	e.user("alice")
	c := e.client()
	for range 10 {
		c.post("/login", url.Values{"username": {"alice"}, "password": {"wrong"}})
	}
	code, body, _ := c.post("/login", url.Values{"username": {"alice"}, "password": {"alice-password"}})
	if code != http.StatusTooManyRequests || !strings.Contains(body, "Too many failed attempts") {
		t.Fatalf("after 10 failures: %d", code)
	}
	// Another username from the same IP is still allowed.
	e.user("bob")
	if code, _, _ := c.post("/login", url.Values{"username": {"bob"}, "password": {"bob-password"}}); code != http.StatusSeeOther {
		t.Fatalf("bob blocked by alice's failures: %d", code)
	}
	rows, _, _ := e.store.ListAudit(context.Background(), 50, 0)
	fails := 0
	for _, r := range rows {
		if r.Event == store.EventLoginFail {
			fails++
		}
	}
	if fails != 10 {
		t.Fatalf("audit has %d login failures, want 10", fails)
	}
}

func TestSensitiveActionsNeedRecentAuth(t *testing.T) {
	e := newEnv(t)
	e.user("alice")
	c := e.login("alice")
	e.expireReauth()

	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/settings/tokens",
		strings.NewReader(url.Values{"csrf": {c.csrf()}, "name": {"cli"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Referer", e.srv.URL+"/settings/tokens")
	res, err := c.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if loc := res.Header.Get("Location"); res.StatusCode != http.StatusSeeOther || loc != "/reauth?next=%2Fsettings%2Ftokens" {
		t.Fatalf("stale session creating a token: %d %q", res.StatusCode, loc)
	}
	if list, _ := e.store.ListAPITokens(context.Background(), e.userID("alice")); len(list) != 0 {
		t.Fatal("token created without re-authentication")
	}

	// Without 2FA, the password confirms.
	if code, _, _ := c.post("/reauth/password", url.Values{"password": {"wrong"}, "next": {"/settings/tokens"}}); code != http.StatusUnauthorized {
		t.Fatalf("wrong password: %d", code)
	}
	if code, _, loc := c.post("/reauth/password", url.Values{"password": {"alice-password"}, "next": {"/settings/tokens"}}); code != http.StatusSeeOther || loc != "/settings/tokens?done=confirmed" {
		t.Fatalf("reauth: %d %q", code, loc)
	}
	if code, body, _ := c.post("/settings/tokens", url.Values{"name": {"cli"}, "scope": {"write"}, "expires": {"90"}}); code != http.StatusOK || !strings.Contains(body, apiTokenPrefix) {
		t.Fatalf("create token after reauth: %d", code)
	}

	// With 2FA on, the password alone no longer confirms.
	secret, _ := enableTOTP(t, c)
	e.expireReauth()
	if code, _, _ := c.post("/reauth/password", url.Values{"password": {"alice-password"}}); code != http.StatusUnauthorized {
		t.Fatalf("password reauth with 2FA on: %d", code)
	}
	if code, _, _ := c.post("/reauth/totp", url.Values{"code": {e.totpAfter("alice", secret)}}); code != http.StatusSeeOther {
		t.Fatalf("totp reauth: %d", code)
	}
}

func TestRemovingFactorSignsOutOtherSessions(t *testing.T) {
	e := newEnv(t)
	e.user("alice")
	c := e.login("alice")
	enableTOTP(t, c)
	// A second signed-in device.
	e.store.CreateSession(context.Background(), store.Session{TokenHash: []byte("other-device"), UserID: e.userID("alice"),
		MFAPassed: true, CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour), LastSeenAt: time.Now()})
	if code, _, loc := c.post("/settings/totp/disable", nil); code != http.StatusSeeOther || loc != "/settings/security?done=totp-off" {
		t.Fatalf("disable totp: %d %q", code, loc)
	}
	list, _ := e.store.ListSessions(context.Background(), e.userID("alice"))
	if len(list) != 1 {
		t.Fatalf("%d sessions left, want only the current one", len(list))
	}
	if m, _ := e.store.MFAStatus(context.Background(), e.userID("alice")); m.Enabled() || m.RecoveryLeft != 0 {
		t.Fatalf("after removing the only factor: %+v", m)
	}
}

// --- Passkeys, with a software authenticator ---

type softKey struct {
	t      *testing.T
	priv   *ecdsa.PrivateKey
	id     []byte
	rpID   string
	origin string
	count  uint32
}

func newSoftKey(t *testing.T, e *env) *softKey {
	priv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	id := make([]byte, 16)
	rand.Read(id)
	u, _ := url.Parse(e.origin)
	return &softKey{t: t, priv: priv, id: id, rpID: u.Hostname(), origin: e.origin}
}

var b64u = base64.RawURLEncoding

func (k *softKey) clientData(typ string, opts map[string]any) []byte {
	challenge := opts["publicKey"].(map[string]any)["challenge"].(string)
	b, _ := json.Marshal(map[string]any{"type": typ, "challenge": challenge, "origin": k.origin, "crossOrigin": false})
	return b
}

func (k *softKey) authData(flags byte, attested []byte) []byte {
	rp := sha256.Sum256([]byte(k.rpID))
	out := append(rp[:], flags)
	out = binary.BigEndian.AppendUint32(out, k.count)
	return append(out, attested...)
}

func (k *softKey) create(opts map[string]any) []byte {
	x, y := k.priv.PublicKey.X.FillBytes(make([]byte, 32)), k.priv.PublicKey.Y.FillBytes(make([]byte, 32))
	cose, _ := cbor.Marshal(map[int]any{1: 2, 3: -7, -1: 1, -2: x, -3: y})
	attested := make([]byte, 16) // AAGUID: zeros
	attested = binary.BigEndian.AppendUint16(attested, uint16(len(k.id)))
	attested = append(append(attested, k.id...), cose...)
	ad := k.authData(0x45, attested) // UP | UV | AT
	att, _ := cbor.Marshal(map[string]any{"fmt": "none", "attStmt": map[string]any{}, "authData": ad})
	b, _ := json.Marshal(map[string]any{
		"id": b64u.EncodeToString(k.id), "rawId": b64u.EncodeToString(k.id), "type": "public-key",
		"response": map[string]any{
			"attestationObject": b64u.EncodeToString(att),
			"clientDataJSON":    b64u.EncodeToString(k.clientData("webauthn.create", opts)),
			"transports":        []string{"internal"},
		},
		"clientExtensionResults": map[string]any{},
	})
	return b
}

func (k *softKey) get(opts map[string]any, tamper bool) []byte {
	k.count++
	ad := k.authData(0x05, nil) // UP | UV
	cd := k.clientData("webauthn.get", opts)
	h := sha256.Sum256(cd)
	digest := sha256.Sum256(append(append([]byte{}, ad...), h[:]...))
	sig, _ := ecdsa.SignASN1(rand.Reader, k.priv, digest[:])
	if tamper {
		sig[len(sig)-1] ^= 0xff
	}
	b, _ := json.Marshal(map[string]any{
		"id": b64u.EncodeToString(k.id), "rawId": b64u.EncodeToString(k.id), "type": "public-key",
		"response": map[string]any{
			"authenticatorData": b64u.EncodeToString(ad),
			"clientDataJSON":    b64u.EncodeToString(cd),
			"signature":         b64u.EncodeToString(sig),
		},
		"clientExtensionResults": map[string]any{},
	})
	return b
}

func TestPasskeyRegistrationAndLogin(t *testing.T) {
	e := newEnv(t)
	e.user("alice")
	c := e.login("alice")
	key := newSoftKey(t, e)

	code, opts := c.postJSON("/settings/passkeys/begin", nil)
	if code != http.StatusOK {
		t.Fatalf("register begin: %d %v", code, opts)
	}
	code, out := c.postJSON("/settings/passkeys/finish?name=Test+key", key.create(opts))
	if code != http.StatusOK || out["redirect"] != "/settings/security?done=passkey" {
		t.Fatalf("register finish: %d %v", code, out)
	}
	keys, _ := e.store.ListPasskeys(context.Background(), e.userID("alice"))
	if len(keys) != 1 || keys[0].Name != "Test key" {
		t.Fatalf("passkeys = %+v", keys)
	}
	// The security page now asks for recovery codes.
	if _, body, _ := c.get("/settings/security"); !strings.Contains(body, "None left. Generate a set now") {
		t.Fatal("security page doesn't prompt for recovery codes")
	}

	// Sign in: password, then the passkey.
	c2 := e.passwordStep("alice")
	code, opts = c2.postJSON("/login/2fa/passkey/begin", nil)
	if code != http.StatusOK {
		t.Fatalf("login begin: %d %v", code, opts)
	}
	if code, _ := c2.postJSON("/login/2fa/passkey/finish", key.get(opts, true)); code != http.StatusUnauthorized {
		t.Fatalf("bad signature accepted: %d", code)
	}
	_, opts = c2.postJSON("/login/2fa/passkey/begin", nil)
	code, out = c2.postJSON("/login/2fa/passkey/finish?next=%2Ftags", key.get(opts, false))
	if code != http.StatusOK || out["redirect"] != "/tags" {
		t.Fatalf("login finish: %d %v", code, out)
	}
	if code, _, _ := c2.get("/tags"); code != http.StatusOK {
		t.Fatalf("not logged in after passkey: %d", code)
	}
	// The challenge is single use.
	if code, _ := c2.postJSON("/login/2fa/passkey/finish", key.get(opts, false)); code == http.StatusOK {
		t.Fatal("challenge reused")
	}

	// A stale session can't start adding another passkey; it's told where to confirm.
	e.expireReauth()
	if code, out := c2.postJSON("/settings/passkeys/begin", nil); code != http.StatusUnauthorized || !strings.HasPrefix(out["redirect"].(string), "/reauth") {
		t.Fatalf("stale register begin: %d %v", code, out)
	}
}

// --- API ---

// apiToken creates a token through the web UI and returns it.
func apiToken(t *testing.T, c *client, scope string) string {
	t.Helper()
	code, body, _ := c.post("/settings/tokens", url.Values{"name": {"test " + scope}, "scope": {scope}, "expires": {"30"}})
	if code != http.StatusOK {
		t.Fatalf("create token: %d", code)
	}
	return find(t, body, `value="(`+apiTokenPrefix+`[A-Za-z0-9_-]+)"`)
}

type apiClient struct {
	t     *testing.T
	base  string
	token string
}

func (a apiClient) do(method, path string, body any) (int, map[string]any) {
	a.t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, a.base+"/api/v1"+path, r)
	if a.token != "" {
		req.Header.Set("Authorization", "Bearer "+a.token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func TestAPI(t *testing.T) {
	e := newEnv(t)
	e.user("alice")
	e.user("bob")
	alice := apiClient{t, e.srv.URL, apiToken(t, e.login("alice"), "write")}
	reader := apiClient{t, e.srv.URL, apiToken(t, e.login("alice"), "read")}
	bob := apiClient{t, e.srv.URL, apiToken(t, e.login("bob"), "write")}

	if code, out := (apiClient{t, e.srv.URL, ""}).do("GET", "/bookmarks", nil); code != 401 || out["status"] != float64(401) {
		t.Fatalf("no token: %d %v", code, out)
	}
	if code, _ := (apiClient{t, e.srv.URL, "brw_nope"}).do("GET", "/bookmarks", nil); code != 401 {
		t.Fatalf("bad token: %d", code)
	}

	code, b := alice.do("POST", "/bookmarks", map[string]any{"url": "https://go.dev", "title": "Go", "tags": []string{"go"}})
	if code != 201 || b["title"] != "Go" {
		t.Fatalf("create: %d %v", code, b)
	}
	id := int(b["id"].(float64))
	if code, _ := alice.do("POST", "/bookmarks", map[string]any{"url": "go.dev/"}); code != 200 {
		t.Fatalf("duplicate create: %d, want 200", code)
	}
	if code, _ := reader.do("POST", "/bookmarks", map[string]any{"url": "https://x.org"}); code != 403 {
		t.Fatalf("read-only token wrote: %d", code)
	}
	if code, _ := alice.do("POST", "/bookmarks", map[string]any{"url": "javascript:alert(1)"}); code != 422 {
		t.Fatalf("bad URL: %d", code)
	}
	if code, _ := alice.do("POST", "/bookmarks", map[string]any{"url": "https://x.org", "bogus": 1}); code != 400 {
		t.Fatalf("unknown field: %d", code)
	}

	if code, out := alice.do("GET", "/bookmarks/check?url=https%3A%2F%2Fgo.dev%2F", nil); code != 200 || out["bookmark"] == nil {
		t.Fatalf("check: %d %v", code, out)
	}
	if code, out := bob.do("GET", "/bookmarks/check?url=https%3A%2F%2Fgo.dev%2F", nil); code != 200 || out["bookmark"] != nil {
		t.Fatalf("bob sees alice's bookmark via check: %v", out)
	}
	path := "/bookmarks/" + itoa(int64(id))
	for _, m := range []string{"GET", "PATCH", "DELETE"} {
		if code, _ := bob.do(m, path, map[string]any{}); code != 404 {
			t.Errorf("bob %s alice's bookmark: %d", m, code)
		}
	}

	code, b = alice.do("PATCH", path, map[string]any{"title": "Go docs", "is_favourite": true})
	if code != 200 || b["title"] != "Go docs" || b["is_favourite"] != true || b["url"] != "https://go.dev/" {
		t.Fatalf("patch: %d %v", code, b)
	}
	if code, b := alice.do("POST", path+"/archive", nil); code != 200 || b["is_archived"] != true {
		t.Fatalf("archive: %d %v", code, b)
	}
	if _, out := alice.do("GET", "/bookmarks?q=docs", nil); len(out["results"].([]any)) != 0 {
		t.Fatal("archived bookmark in the default list")
	}
	if _, out := alice.do("GET", "/bookmarks?q=docs&archived=true", nil); len(out["results"].([]any)) != 1 {
		t.Fatal("archived bookmark missing with archived=true")
	}

	// Pagination.
	for i := range 3 {
		alice.do("POST", "/bookmarks", map[string]any{"url": "https://page.example/" + itoa(int64(i))})
	}
	_, page1 := alice.do("GET", "/bookmarks?limit=2", nil)
	next, _ := page1["next"].(string)
	if len(page1["results"].([]any)) != 2 || next == "" {
		t.Fatalf("page 1: %v", page1)
	}
	_, page2 := alice.do("GET", "/bookmarks?limit=2&cursor="+next, nil)
	if len(page2["results"].([]any)) != 1 || page2["next"] != nil {
		t.Fatalf("page 2: %v", page2)
	}

	_, tags := alice.do("GET", "/tags", nil)
	tagID := int64(tags["results"].([]any)[0].(map[string]any)["id"].(float64))
	if code, _ := bob.do("DELETE", "/tags/"+itoa(tagID), nil); code != 404 {
		t.Fatalf("bob deleted alice's tag: %d", code)
	}
	if code, _ := alice.do("PATCH", "/tags/"+itoa(tagID), map[string]any{"name": "golang"}); code != 204 {
		t.Fatalf("rename tag: %d", code)
	}

	if code, out := alice.do("GET", "/user", nil); code != 200 || out["username"] != "alice" {
		t.Fatalf("user: %d %v", code, out)
	}
	if code, _ := alice.do("DELETE", path, nil); code != 204 {
		t.Fatalf("delete: %d", code)
	}

	// Disabling the account kills its tokens.
	e.store.SetActive(context.Background(), e.userID("alice"), false)
	if code, _ := alice.do("GET", "/bookmarks", nil); code != 401 {
		t.Fatalf("disabled user's token: %d", code)
	}
}

func TestAPIRateLimit(t *testing.T) {
	e := newEnv(t)
	e.user("alice")
	e.server.apiToken = ratelimit.New(3, time.Minute)
	a := apiClient{t, e.srv.URL, apiToken(t, e.login("alice"), "read")}
	for range 3 {
		a.do("GET", "/user", nil)
	}
	if code, _ := a.do("GET", "/user", nil); code != 429 {
		t.Fatalf("4th request: %d, want 429", code)
	}
}

// --- Admin, invites, signup, 2FA policy ---

func (e *env) admin() *client {
	e.t.Helper()
	if _, err := e.store.CreateUser(context.Background(), "root", auth.HashPassword("root-password"), true); err != nil {
		e.t.Fatal(err)
	}
	return e.login("root")
}

func TestAdmin(t *testing.T) {
	e := newEnv(t)
	e.user("alice")
	alice := e.login("alice")
	if code, _, _ := alice.get("/admin/"); code != http.StatusNotFound {
		t.Fatalf("non-admin reached /admin/: %d", code)
	}
	if code, _, _ := alice.post("/admin/users", url.Values{"username": {"evil"}}); code != http.StatusNotFound {
		t.Fatalf("non-admin created a user: %d", code)
	}

	root := e.admin()
	alice.post("/bookmarks", url.Values{"url": {"https://secret.example"}, "title": {"Alice private"}})
	code, body, _ := root.get("/admin/")
	if code != http.StatusOK || !strings.Contains(body, "alice") || strings.Contains(body, "Alice private") {
		t.Fatalf("admin users page: %d", code)
	}

	code, body, _ = root.post("/admin/users", url.Values{"username": {"carol"}})
	if code != http.StatusOK {
		t.Fatalf("create user: %d", code)
	}
	pw := find(t, body, `class="secret-field" readonly value="([^"]+)"`)
	carol := e.client()
	if code, _, _ := carol.post("/login", url.Values{"username": {"carol"}, "password": {pw}}); code != http.StatusSeeOther {
		t.Fatalf("carol can't log in with the generated password: %d", code)
	}

	aliceID := itoa(e.userID("alice"))
	if code, _, _ := root.post("/admin/users/"+aliceID+"/disable", nil); code != http.StatusSeeOther {
		t.Fatalf("disable: %d", code)
	}
	if code, _, _ := alice.get("/"); code != http.StatusSeeOther {
		t.Fatalf("disabled user still logged in: %d", code)
	}
	if code, body, _ := root.post("/admin/users/"+itoa(e.userID("root"))+"/disable", nil); code != http.StatusUnprocessableEntity || !strings.Contains(body, "own account") {
		t.Fatalf("admin disabled themselves: %d", code)
	}

	// Admin actions need a recent check too.
	e.expireReauth()
	if code, _, loc := root.post("/admin/users/"+aliceID+"/enable", nil); code != http.StatusSeeOther || !strings.HasPrefix(loc, "/reauth") {
		t.Fatalf("stale admin action: %d %q", code, loc)
	}

	_, body, _ = root.get("/admin/audit")
	for _, ev := range []string{store.EventUserCreated, store.EventUserDisabled, store.EventLoginOK} {
		if !strings.Contains(body, ev) {
			t.Errorf("audit log missing %s", ev)
		}
	}
}

func TestInvitesAndSignupModes(t *testing.T) {
	e := newEnv(t)
	root := e.admin()
	anon := e.client()
	if code, _, _ := anon.get("/signup"); code != http.StatusNotFound {
		t.Fatalf("closed signup page: %d", code)
	}

	_, body, _ := root.post("/admin/invites", url.Values{"note": {"for dana"}})
	link := find(t, body, `value="(http[^"]+/signup\?invite=[^"]+)"`)
	invite := link[strings.Index(link, "invite=")+7:]
	if !strings.Contains(body, "signup is closed") {
		t.Fatal("no warning that the invite won't work while signup is closed")
	}
	if code, _, _ := anon.get("/signup?invite=" + invite); code != http.StatusNotFound {
		t.Fatalf("invite worked while closed: %d", code)
	}

	root.post("/admin/settings", url.Values{"signup": {"invite"}})
	if code, _, _ := anon.get("/signup"); code != http.StatusNotFound {
		t.Fatalf("invite mode without invite: %d", code)
	}
	form := url.Values{"invite": {invite}, "username": {"dana"}, "password": {"dana-password"}, "confirm": {"dana-password"}}
	if code, _, loc := anon.post("/signup", form); code != http.StatusSeeOther || loc != "/" {
		t.Fatalf("signup with invite: %d %q", code, loc)
	}
	form.Set("username", "erin")
	form.Del("csrf") // post() filled in the first client's token
	if code, _, _ := e.client().post("/signup", form); code != http.StatusNotFound {
		t.Fatalf("invite reused: %d", code)
	}

	root.post("/admin/settings", url.Values{"signup": {"open"}})
	if _, body, _ := e.client().get("/login"); !strings.Contains(body, "Create an account") {
		t.Fatal("login page has no signup link in open mode")
	}
	open := url.Values{"username": {"frank"}, "password": {"frank-password"}, "confirm": {"frank-password"}}
	if code, _, _ := e.client().post("/signup", open); code != http.StatusSeeOther {
		t.Fatalf("open signup: %d", code)
	}
}

func TestRequire2FAPolicy(t *testing.T) {
	e := newEnv(t)
	e.user("alice")
	root := e.admin()
	root.post("/admin/settings", url.Values{"signup": {"closed"}, "require_2fa": {"1"}})

	alice := e.login("alice")
	if code, _, loc := alice.get("/"); code != http.StatusSeeOther || loc != "/settings/security?required=1" {
		t.Fatalf("user without 2FA not sent to set it up: %d %q", code, loc)
	}
	if code, body, _ := alice.get("/settings/security?required=1"); code != http.StatusOK || !strings.Contains(body, "requires two-factor") {
		t.Fatalf("security page: %d", code)
	}
	enableTOTP(t, alice)
	if code, _, _ := alice.get("/"); code != http.StatusOK {
		t.Fatalf("still blocked after enrolling: %d", code)
	}
	// The only factor can't be removed while the policy is on.
	if code, _, _ := alice.post("/settings/totp/disable", nil); code != http.StatusConflict {
		t.Fatalf("removed the only factor under the policy: %d", code)
	}
}

// --- Import and export ---

const importHTML = `<!DOCTYPE NETSCAPE-Bookmark-file-1>
<DL><p>
<DT><H3>Reading</H3>
<DL><p>
<DT><A HREF="https://go.dev/doc/effective_go" ADD_DATE="1700000000">Effective Go</A>
<DD>Tips for writing clear Go
<DT><A HREF="https://sqlite.org/fts5.html" TAGS="sqlite">FTS5</A>
</DL><p>
<DT><A HREF="javascript:void(0)">Bookmarklet</A>
</DL><p>
`

func TestImportExport(t *testing.T) {
	e := newEnv(t)
	e.user("alice")
	c := e.login("alice")

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("csrf", c.csrf())
	fw, _ := mw.CreateFormFile("file", "bookmarks.html")
	fw.Write([]byte(importHTML))
	mw.Close()
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/settings/import", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	res, err := c.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("upload: %d", res.StatusCode)
	}
	if _, body, _ := c.get("/settings/data"); !strings.Contains(body, `http-equiv="refresh"`) {
		t.Fatal("data page doesn't refresh while the import is queued")
	}
	if !e.worker.RunOnce(context.Background()) {
		t.Fatal("no job to run")
	}
	_, body, _ := c.get("/settings/data")
	if !strings.Contains(body, "2 added, 0 merged") || !strings.Contains(body, "1 skipped") {
		t.Fatalf("import summary missing:\n%s", body)
	}
	_, body, _ = c.get("/?q=%23Reading")
	if !strings.Contains(body, "Effective Go") || !strings.Contains(body, "FTS5") {
		t.Fatal("imported bookmarks not tagged with their folder")
	}

	res, err = c.http.Get(e.srv.URL + "/settings/export?format=json")
	if err != nil {
		t.Fatal(err)
	}
	exported, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if !strings.Contains(res.Header.Get("Content-Disposition"), ".json") || !strings.Contains(string(exported), `"Tips for writing clear Go"`) {
		t.Fatalf("export: %q\n%s", res.Header.Get("Content-Disposition"), exported)
	}

	// Bad files fail at upload, not later.
	buf.Reset()
	mw = multipart.NewWriter(&buf)
	mw.WriteField("csrf", c.csrf())
	fw, _ = mw.CreateFormFile("file", "notes.txt")
	fw.Write([]byte("just some text"))
	mw.Close()
	req, _ = http.NewRequest(http.MethodPost, e.srv.URL+"/settings/import", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	res, _ = c.http.Do(req)
	res.Body.Close()
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("bad file: %d", res.StatusCode)
	}

	// The API imports too, and reports the job.
	a := apiClient{t, e.srv.URL, apiToken(t, c, "write")}
	req, _ = http.NewRequest(http.MethodPost, e.srv.URL+"/api/v1/import", strings.NewReader(importHTML))
	req.Header.Set("Authorization", "Bearer "+a.token)
	res, _ = http.DefaultClient.Do(req)
	var job map[string]any
	json.NewDecoder(res.Body).Decode(&job)
	res.Body.Close()
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("api import: %d", res.StatusCode)
	}
	e.worker.RunOnce(context.Background())
	code, status := a.do("GET", "/jobs/"+itoa(int64(job["job_id"].(float64))), nil)
	result, _ := status["result"].(map[string]any)
	if code != 200 || status["status"] != "done" || result["merged"] != float64(2) {
		t.Fatalf("api job: %d %v", code, status)
	}
}

// Reached by IP address, Burrow must still start, just without passkeys.
func TestPasskeysDisabledOnIPBaseURL(t *testing.T) {
	e := newEnvHost(t, "127.0.0.1")
	e.user("alice")
	c := e.login("alice")
	if code, body, _ := c.get("/settings/security"); code != http.StatusOK || !strings.Contains(body, "Not available: passkeys need") ||
		strings.Contains(body, "passkeys.js") {
		t.Fatalf("security page: %d", code)
	}
	if code, out := c.postJSON("/settings/passkeys/begin", nil); code != http.StatusConflict || !strings.Contains(out["error"].(string), "domain name") {
		t.Fatalf("passkey begin: %d %v", code, out)
	}
	enableTOTP(t, c) // TOTP still works
}
