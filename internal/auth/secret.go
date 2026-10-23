package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
)

// Keys derives purpose-specific keys from ANCHOR_SECRET_KEY, so one leaked
// derived key (or its use in one place) says nothing about the others.
type Keys struct{ master []byte }

func NewKeys(master []byte) Keys { return Keys{master: master} }

// Derive returns a 32-byte key for purpose.
func (k Keys) Derive(purpose string) []byte {
	m := hmac.New(sha256.New, k.master)
	m.Write([]byte("anchor/" + purpose))
	return m.Sum(nil)
}

// Seal encrypts plaintext with AES-256-GCM under the key for purpose; the
// nonce is prepended to the ciphertext.
func (k Keys) Seal(purpose string, plaintext []byte) ([]byte, error) {
	gcm, err := k.gcm(purpose)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	rand.Read(nonce)
	return gcm.Seal(nonce, nonce, plaintext, []byte(purpose)), nil
}

func (k Keys) Open(purpose string, sealed []byte) ([]byte, error) {
	gcm, err := k.gcm(purpose)
	if err != nil {
		return nil, err
	}
	if len(sealed) < gcm.NonceSize() {
		return nil, errors.New("sealed value too short")
	}
	nonce, ct := sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():]
	return gcm.Open(nil, nonce, ct, []byte(purpose))
}

func (k Keys) gcm(purpose string) (cipher.AEAD, error) {
	block, err := aes.NewCipher(k.Derive(purpose))
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
