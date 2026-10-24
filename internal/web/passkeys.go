package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/marcelhoffs/anchor/internal/store"
)

// waUser adapts an Anchor user to go-webauthn's User.
type waUser struct {
	id    []byte
	name  string
	creds []webauthn.Credential
}

func (u waUser) WebAuthnID() []byte                         { return u.id }
func (u waUser) WebAuthnName() string                       { return u.name }
func (u waUser) WebAuthnDisplayName() string                { return u.name }
func (u waUser) WebAuthnCredentials() []webauthn.Credential { return u.creds }

// webauthnUser loads u's passkeys. The WebAuthn user handle is an HMAC of the
// user id: stable, opaque, and not guessable from the id.
func (s *Server) webauthnUser(r *http.Request, u store.User) (waUser, error) {
	m := hmac.New(sha256.New, s.keys.Derive("webauthn-user"))
	m.Write([]byte(strconv.FormatInt(u.ID, 10)))
	keys, err := s.store.ListPasskeys(r.Context(), u.ID)
	if err != nil {
		return waUser{}, err
	}
	wu := waUser{id: m.Sum(nil), name: u.Username}
	for _, k := range keys {
		var c webauthn.Credential
		if err := json.Unmarshal([]byte(k.Credential), &c); err != nil {
			s.log.Error("bad stored passkey", "id", k.ID, "err", err)
			continue
		}
		wu.creds = append(wu.creds, c)
	}
	return wu, nil
}

// ceremonies holds the challenge of each WebAuthn ceremony in flight between
// its begin and finish calls: in memory, single use, five minutes.
type ceremonies struct {
	mu sync.Mutex
	m  map[string]ceremony
}

type ceremony struct {
	data    webauthn.SessionData
	expires time.Time
}

func ceremonyKey(purpose string, sessionHash []byte) string {
	return purpose + "|" + hex.EncodeToString(sessionHash)
}

func (c *ceremonies) put(key string, d webauthn.SessionData) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[key] = ceremony{d, time.Now().Add(5 * time.Minute)}
}

func (c *ceremonies) take(key string) (webauthn.SessionData, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cer, ok := c.m[key]
	delete(c.m, key)
	return cer.data, ok && time.Now().Before(cer.expires)
}

func (c *ceremonies) sweep() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, cer := range c.m {
		if time.Now().After(cer.expires) {
			delete(c.m, k)
		}
	}
}

// passkeysOff answers a passkey call when passkeys are disabled; it reports
// whether it did.
func (s *Server) passkeysOff(w http.ResponseWriter, r *http.Request) bool {
	if s.webauthn != nil {
		return false
	}
	jsonError(w, http.StatusConflict, langOf(r).T("Passkeys aren't available: Anchor must be reached by a domain name, not an IP address."))
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func jsonError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func jsonMarshal(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}
