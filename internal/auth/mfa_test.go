package auth

import (
	"strings"
	"testing"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/hotp"
)

func TestSealOpen(t *testing.T) {
	k := NewKeys([]byte("0123456789abcdef0123456789abcdef"))
	sealed, err := k.Seal("totp", []byte("JBSWY3DPEHPK3PXP"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := k.Open("totp", sealed); err != nil || string(got) != "JBSWY3DPEHPK3PXP" {
		t.Fatalf("Open = %q, %v", got, err)
	}
	if _, err := k.Open("other", sealed); err == nil {
		t.Fatal("opened with the wrong purpose")
	}
	if _, err := NewKeys([]byte("another key, also 32 bytes long!")).Open("totp", sealed); err == nil {
		t.Fatal("opened with the wrong master key")
	}
}

func code(t *testing.T, secret string, step int64) string {
	t.Helper()
	c, err := hotp.GenerateCodeCustom(secret, uint64(step), hotp.ValidateOpts{Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestVerifyTOTP(t *testing.T) {
	setup, err := NewTOTP("Anchor", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(setup.QR, "data:image/png;base64,") || !strings.Contains(setup.URL, "issuer=Anchor") {
		t.Fatalf("setup = %+v", setup)
	}
	now := time.Unix(1_800_000_000, 0)
	cur := now.Unix() / totpPeriod

	step, ok := VerifyTOTP(setup.Secret, code(t, setup.Secret, cur), 0, now)
	if !ok || step != cur {
		t.Fatalf("current code rejected: %v %v", step, ok)
	}
	if _, ok := VerifyTOTP(setup.Secret, code(t, setup.Secret, cur), cur, now); ok {
		t.Fatal("replayed code accepted")
	}
	if _, ok := VerifyTOTP(setup.Secret, code(t, setup.Secret, cur-1), 0, now); !ok {
		t.Fatal("previous step (clock drift) rejected")
	}
	if _, ok := VerifyTOTP(setup.Secret, code(t, setup.Secret, cur-2), 0, now); ok {
		t.Fatal("code two steps old accepted")
	}
	c := code(t, setup.Secret, cur)
	if _, ok := VerifyTOTP(setup.Secret, c[:3]+" "+c[3:], 0, now); !ok {
		t.Fatal("code with a space rejected")
	}
}

func TestRecoveryCodes(t *testing.T) {
	codes, hashes := NewRecoveryCodes()
	if len(codes) != RecoveryCodeCount || len(hashes) != RecoveryCodeCount {
		t.Fatalf("got %d codes", len(codes))
	}
	seen := map[string]bool{}
	for i, c := range codes {
		if len(c) != 11 || c[5] != '-' || strings.ContainsAny(c, "ilo") || seen[c] {
			t.Fatalf("bad code %q", c)
		}
		seen[c] = true
		if HashRecoveryCode(" "+strings.ToUpper(c)+" ") != hashes[i] {
			t.Fatalf("normalised %q does not match its hash", c)
		}
	}
	if HashRecoveryCode("ab0cd-efghj") != HashRecoveryCode("abocdefghj") {
		t.Fatal("o should be read as 0")
	}
}
