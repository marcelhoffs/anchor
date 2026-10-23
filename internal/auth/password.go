// Package auth holds password hashing and token helpers.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters: OWASP's minimum recommended profile (19 MiB, t=2,
// p=1), which keeps memory use low enough for a Raspberry Pi.
const (
	argonMemory  = 19 * 1024
	argonTime    = 2
	argonThreads = 1
	argonKeyLen  = 32
	saltLen      = 16

	MinPasswordLen = 10
	maxPasswordLen = 256
)

var ErrMismatch = errors.New("password mismatch")

// argonSlots caps concurrent hashing: each takes argonMemory, so a burst of
// logins would otherwise be a way to exhaust the server's memory.
var argonSlots = make(chan struct{}, max(2, runtime.NumCPU()))

func idKey(password string, salt []byte, time, memory uint32, threads uint8, keyLen uint32) []byte {
	argonSlots <- struct{}{}
	defer func() { <-argonSlots }()
	return argon2.IDKey([]byte(password), salt, time, memory, threads, keyLen)
}

// dummyHash is verified against when a username does not exist, so a failed
// login takes the same time either way.
var dummyHash = HashPassword("burrow-dummy-password")

func ValidatePassword(p string) error {
	switch n := utf8.RuneCountInString(p); {
	case n < MinPasswordLen:
		return fmt.Errorf("password must be at least %d characters", MinPasswordLen)
	case n > maxPasswordLen:
		return fmt.Errorf("password must be at most %d characters", maxPasswordLen)
	}
	return nil
}

// HashPassword returns a PHC-format argon2id hash.
func HashPassword(password string) string {
	salt := make([]byte, saltLen)
	rand.Read(salt)
	key := idKey(password, salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads, b64.EncodeToString(salt), b64.EncodeToString(key))
}

// VerifyPassword checks password against encoded in constant time.
func VerifyPassword(encoded, password string) error {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return errors.New("unsupported password hash")
	}
	var version int
	var memory, iterations uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return errors.New("unsupported argon2 version")
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &iterations, &threads); err != nil {
		return err
	}
	b64 := base64.RawStdEncoding
	salt, err := b64.DecodeString(parts[4])
	if err != nil {
		return err
	}
	want, err := b64.DecodeString(parts[5])
	if err != nil {
		return err
	}
	got := idKey(password, salt, iterations, memory, threads, uint32(len(want)))
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return ErrMismatch
	}
	return nil
}

// VerifyDummy burns the same time as a real verification.
func VerifyDummy(password string) {
	VerifyPassword(dummyHash, password)
}

// NewToken returns a random URL-safe token of n bytes and its SHA-256 hash.
func NewToken(n int) (token string, hash []byte) {
	b := make([]byte, n)
	rand.Read(b)
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, HashToken(token)
}

func HashToken(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// RandomPassword returns a random password for CLI-created accounts.
func RandomPassword() string {
	t, _ := NewToken(15)
	return t
}
