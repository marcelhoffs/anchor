package auth

import "testing"

func TestPasswordRoundTrip(t *testing.T) {
	h := HashPassword("correct horse battery")
	if err := VerifyPassword(h, "correct horse battery"); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if err := VerifyPassword(h, "wrong"); err != ErrMismatch {
		t.Fatalf("wrong password: %v", err)
	}
	if HashPassword("x") == HashPassword("x") {
		t.Fatal("hashes must be salted")
	}
	if err := VerifyPassword("garbage", "x"); err == nil {
		t.Fatal("garbage hash accepted")
	}
}
