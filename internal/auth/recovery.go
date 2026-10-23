package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"strings"
)

const RecoveryCodeCount = 10

// 32 symbols with no i, l or o, so codes are easy to read back; a typed 'o'
// is taken as '0'.
var recoveryEncoding = base32.NewEncoding("abcdefghjkmnpqrstuvwxyz023456789").WithPadding(base32.NoPadding)

// NewRecoveryCodes returns RecoveryCodeCount codes like "k7m2p-x9qta", each
// with 50 random bits, and their hashes for storage.
func NewRecoveryCodes() (codes, hashes []string) {
	for range RecoveryCodeCount {
		b := make([]byte, 7)
		rand.Read(b)
		raw := recoveryEncoding.EncodeToString(b)[:10]
		code := raw[:5] + "-" + raw[5:]
		codes = append(codes, code)
		hashes = append(hashes, HashRecoveryCode(code))
	}
	return codes, hashes
}

// HashRecoveryCode normalises what a user typed (case, spaces, the dash, o
// for 0) and hashes it. The codes carry 50 random bits, so a fast hash is
// enough.
func HashRecoveryCode(code string) string {
	code = strings.ToLower(strings.TrimSpace(code))
	code = strings.NewReplacer(" ", "", "-", "", "o", "0").Replace(code)
	h := sha256.Sum256([]byte("anchor-recovery:" + code))
	return hex.EncodeToString(h[:])
}
