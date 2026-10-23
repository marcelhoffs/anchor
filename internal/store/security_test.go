package store

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/marcelhoffs/anchor/internal/importexport"
)

func TestTOTPAndRecoveryCodes(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	u := newUser(t, s, "alice")

	if err := s.EnableTOTP(ctx, u.ID, 5); err == nil {
		t.Fatal("enabled TOTP without a pending secret")
	}
	if err := s.StartTOTP(ctx, u.ID, []byte("sealed")); err != nil {
		t.Fatal(err)
	}
	if err := s.EnableTOTP(ctx, u.ID, 100); err != nil {
		t.Fatal(err)
	}
	if err := s.StartTOTP(ctx, u.ID, []byte("other")); err == nil {
		t.Fatal("restarted setup while TOTP is enabled")
	}
	if ok, _ := s.UseTOTPStep(ctx, u.ID, 100); ok {
		t.Fatal("step used at enrolment accepted again")
	}
	if ok, _ := s.UseTOTPStep(ctx, u.ID, 101); !ok {
		t.Fatal("next step rejected")
	}
	if ok, _ := s.UseTOTPStep(ctx, u.ID, 101); ok {
		t.Fatal("same step accepted twice")
	}

	if err := s.ReplaceRecoveryCodes(ctx, u.ID, []string{"h1", "h2"}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.UseRecoveryCode(ctx, u.ID, "h1"); !ok {
		t.Fatal("valid recovery code rejected")
	}
	if ok, _ := s.UseRecoveryCode(ctx, u.ID, "h1"); ok {
		t.Fatal("recovery code used twice")
	}
	other := newUser(t, s, "bob")
	if ok, _ := s.UseRecoveryCode(ctx, other.ID, "h2"); ok {
		t.Fatal("alice's recovery code worked for bob")
	}
	if m, _ := s.MFAStatus(ctx, u.ID); !m.TOTP || m.RecoveryLeft != 1 || !m.Enabled() {
		t.Fatalf("status = %+v", m)
	}

	s.AddPasskey(ctx, u.ID, "Laptop", []byte("cred-1"), []byte("pk"), 0, "{}")
	if err := s.AddPasskey(ctx, other.ID, "Copy", []byte("cred-1"), []byte("pk"), 0, "{}"); err == nil {
		t.Fatal("same credential registered twice")
	}
	keys, _ := s.ListPasskeys(ctx, u.ID)
	if err := s.DeletePasskey(ctx, other.ID, keys[0].ID); err != ErrNotFound {
		t.Fatalf("bob deleted alice's passkey: %v", err)
	}

	if err := s.ResetMFA(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if m, _ := s.MFAStatus(ctx, u.ID); m.Enabled() || m.RecoveryLeft != 0 {
		t.Fatalf("after reset: %+v", m)
	}
}

func TestAPITokens(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	admin, _ := s.CreateUser(ctx, "root", "x", true)
	u := newUser(t, s, "alice")
	tok, err := s.CreateAPIToken(ctx, u.ID, "cli", []byte("hash-1"), []string{"read"}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	got, owner, err := s.TokenByHash(ctx, []byte("hash-1"))
	if err != nil || owner.ID != u.ID || !got.Can("read") || got.Can("write") {
		t.Fatalf("lookup: %+v %+v %v", got, owner, err)
	}
	s.CreateAPIToken(ctx, u.ID, "old", []byte("hash-2"), []string{"read"}, time.Now().Add(-time.Minute))
	if _, _, err := s.TokenByHash(ctx, []byte("hash-2")); err != ErrNotFound {
		t.Fatalf("expired token accepted: %v", err)
	}
	if err := s.SetActive(ctx, u.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.TokenByHash(ctx, []byte("hash-1")); err != ErrNotFound {
		t.Fatalf("disabled user's token accepted: %v", err)
	}
	if err := s.DeleteAPIToken(ctx, admin.ID, tok.ID); err != ErrNotFound {
		t.Fatalf("other user revoked the token: %v", err)
	}
}

func TestLastAdminGuard(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	root, _ := s.CreateUser(ctx, "root", "x", true)
	if err := s.SetActive(ctx, root.ID, false); !isValidationErr(err) {
		t.Fatalf("disabled the only admin: %v", err)
	}
	if err := s.SetAdmin(ctx, root.ID, false); !isValidationErr(err) {
		t.Fatalf("demoted the only admin: %v", err)
	}
	second, _ := s.CreateUser(ctx, "second", "x", true)
	if err := s.SetAdmin(ctx, root.ID, false); err != nil {
		t.Fatalf("demote with another admin: %v", err)
	}
	if err := s.SetActive(ctx, second.ID, false); !isValidationErr(err) {
		t.Fatalf("disabled the last remaining admin: %v", err)
	}
}

func TestInvites(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	root, _ := s.CreateUser(ctx, "root", "x", true)
	s.CreateInvite(ctx, root.ID, "for alice", []byte("inv-1"), time.Now().Add(time.Hour))
	s.CreateInvite(ctx, root.ID, "expired", []byte("inv-2"), time.Now().Add(-time.Hour))

	if !s.InviteValid(ctx, []byte("inv-1")) || s.InviteValid(ctx, []byte("inv-2")) {
		t.Fatal("InviteValid wrong")
	}
	if _, err := s.SignUp(ctx, "alice", "x", []byte("inv-1")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SignUp(ctx, "mallory", "x", []byte("inv-1")); err != ErrInvalidInvite {
		t.Fatalf("invite used twice: %v", err)
	}
	if _, err := s.SignUp(ctx, "eve", "x", []byte("inv-2")); err != ErrInvalidInvite {
		t.Fatalf("expired invite used: %v", err)
	}
	// A username clash must not burn the invite.
	s.CreateInvite(ctx, root.ID, "", []byte("inv-3"), time.Now().Add(time.Hour))
	if _, err := s.SignUp(ctx, "alice", "x", []byte("inv-3")); err != ErrConflict {
		t.Fatalf("duplicate username: %v", err)
	}
	if !s.InviteValid(ctx, []byte("inv-3")) {
		t.Fatal("failed signup consumed the invite")
	}
	list, _ := s.ListInvites(ctx)
	if len(list) != 3 { // inv-1 (used recently), inv-2 (expired, unused), inv-3
		t.Fatalf("ListInvites = %d", len(list))
	}
}

func TestJobsAndImport(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	u := newUser(t, s, "alice")
	mustTags(t, s, u.ID, "old")
	s.CreateBookmark(ctx, personal(s, u.ID), BookmarkInput{URL: "https://go.dev", Tags: []string{"old"}})

	if _, _, err := s.ClaimJob(ctx); err != ErrNotFound {
		t.Fatalf("empty queue: %v", err)
	}
	id, _ := s.CreateJob(ctx, personal(s, u.ID), JobImport, "payload")
	j, payload, err := s.ClaimJob(ctx)
	if err != nil || j.ID != id || payload != "payload" || j.Status != "running" {
		t.Fatalf("claim: %+v %q %v", j, payload, err)
	}
	if _, _, err := s.ClaimJob(ctx); err != ErrNotFound {
		t.Fatal("running job claimed twice")
	}

	r, err := s.ImportBookmarks(ctx, personal(s, u.ID), []importexport.Item{
		{URL: "https://go.dev/", Title: "Go", Tags: []string{"new", `bad"name`}, Favourite: true}, // an import creates its tags
		{URL: "https://sqlite.org", Title: "SQLite", CreatedAt: time.Unix(1_600_000_000, 0)},
		{URL: "place:sort=8", Title: "Firefox smart folder"},
	})
	if err != nil || r != (ImportResult{Created: 1, Merged: 1, Skipped: 1}) {
		t.Fatalf("import = %+v, %v", r, err)
	}
	b, _ := s.FindBookmarkByURL(ctx, personal(s, u.ID), "https://go.dev")
	if b.Title != "Go" || !b.IsFavourite || !slices.Equal(b.Tags, []string{"new", "old"}) {
		t.Fatalf("merged bookmark: %+v", b)
	}
	sq, _ := s.FindBookmarkByURL(ctx, personal(s, u.ID), "https://sqlite.org")
	if sq.CreatedAt.Unix() != 1_600_000_000 {
		t.Fatalf("import lost the saved date: %v", sq.CreatedAt)
	}
	if got := titles(t, s, u.ID, "sqlite"); !slices.Equal(got, []string{"SQLite"}) {
		t.Fatalf("imported bookmark not searchable: %v", got)
	}

	var exported []string
	s.ExportBookmarks(ctx, personal(s, u.ID), func(it importexport.Item) error { exported = append(exported, it.URL); return nil })
	if !slices.Equal(exported, []string{"https://sqlite.org/", "https://go.dev/"}) {
		t.Fatalf("export order = %v", exported)
	}

	s.FinishJob(ctx, id, `{"created":1}`, nil)
	j, _ = s.GetJob(ctx, u.ID, id)
	if !j.Finished() || j.Result != `{"created":1}` {
		t.Fatalf("finished job: %+v", j)
	}
	other := newUser(t, s, "bob")
	if _, err := s.GetJob(ctx, other.ID, id); err != ErrNotFound {
		t.Fatal("bob can see alice's job")
	}
}
