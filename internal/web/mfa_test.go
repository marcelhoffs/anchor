package web

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/hotp"

	"github.com/marcelhoffs/anchor/internal/store"
)

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
	if !strings.Contains(body, `data-copy="recovery-codes"`) || !strings.Contains(body, "copy.js") {
		t.Fatal("recovery codes have no copy button")
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
}

// Sensitive actions never ask to confirm it's you again, however long ago
// the sign-in was.
func TestSensitiveActionsDontAskAgain(t *testing.T) {
	e := newEnv(t)
	e.user("alice")
	c := e.login("alice")
	if _, err := e.db.W.Exec("UPDATE sessions SET created_at = ?", time.Now().Add(-24*time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	if code, body, _ := c.post("/settings/tokens", url.Values{"name": {"cli"}, "scope": {"write"}, "expires": {"90"}}); code != http.StatusOK || !strings.Contains(body, apiTokenPrefix) {
		t.Fatalf("create token: %d", code)
	}
	if code, _, _ := c.get("/reauth"); code != http.StatusNotFound {
		t.Fatalf("/reauth: %d", code)
	}
	// Whoever made that token can't keep using it once the password changes,
	// or an admin resets two-factor.
	ctx := context.Background()
	c.post("/settings/password", url.Values{"current": {"alice-password"}, "password": {"new-alice-password"}, "confirm": {"new-alice-password"}})
	if list, _ := e.store.ListAPITokens(ctx, e.userID("alice")); len(list) != 0 {
		t.Fatal("API token survived a password change")
	}
	e.store.CreateAPIToken(ctx, e.userID("alice"), "cli", []byte("h"), []string{"read"}, time.Time{})
	e.store.ResetMFA(ctx, e.userID("alice"))
	if list, _ := e.store.ListAPITokens(ctx, e.userID("alice")); len(list) != 0 {
		t.Fatal("API token survived a two-factor reset")
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
}

// Reached by IP address, Anchor must still start, just without passkeys.
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
