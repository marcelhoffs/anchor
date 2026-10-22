package store

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/marcelhoffs/burrow/internal/db"
	"github.com/marcelhoffs/burrow/internal/search"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if err := d.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return New(d)
}

func newUser(t *testing.T, s *Store, name string) User {
	t.Helper()
	u, err := s.CreateUser(context.Background(), name, "x", false)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func titles(t *testing.T, s *Store, userID int64, q string) []string {
	t.Helper()
	list, _, err := s.Search(context.Background(), userID, search.Parse(q), 50, 0)
	if err != nil {
		t.Fatalf("search %q: %v", q, err)
	}
	var out []string
	for _, b := range list {
		out = append(out, b.Title)
	}
	slices.Sort(out)
	return out
}

func TestNormalizeURL(t *testing.T) {
	for in, want := range map[string]string{
		"example.com":                   "https://example.com/",
		"HTTPS://Example.COM:443/a?b#c": "https://example.com/a?b#c",
		"http://example.com:80":         "http://example.com/",
		"http://[::1]:8080/x":           "http://[::1]:8080/x",
	} {
		got, _, err := NormalizeURL(in)
		if err != nil || got != want {
			t.Errorf("NormalizeURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "javascript:alert(1)", "ftp://x.org", "https://"} {
		if _, _, err := NormalizeURL(bad); err == nil {
			t.Errorf("NormalizeURL(%q) accepted", bad)
		}
	}
}

func TestCreateDeduplicatesPerUser(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	a, b := newUser(t, s, "alice"), newUser(t, s, "bob")

	first, created, err := s.CreateBookmark(ctx, a.ID, BookmarkInput{URL: "https://Example.com", Title: "One", Tags: []string{"x"}})
	if err != nil || !created {
		t.Fatalf("create: %v %v", created, err)
	}
	again, created, err := s.CreateBookmark(ctx, a.ID, BookmarkInput{URL: "example.com/", Title: "Two"})
	if err != nil || created || again.ID != first.ID || again.Title != "One" {
		t.Fatalf("duplicate create = %+v, created=%v, %v", again, created, err)
	}
	if _, created, _ := s.CreateBookmark(ctx, b.ID, BookmarkInput{URL: "https://example.com"}); !created {
		t.Fatal("another user's identical URL should be a new bookmark")
	}
	other, _, _ := s.CreateBookmark(ctx, a.ID, BookmarkInput{URL: "https://other.org"})
	if _, err := s.UpdateBookmark(ctx, a.ID, other.ID, BookmarkInput{URL: "https://example.com"}); err != ErrConflict {
		t.Fatalf("update to an existing URL: %v, want ErrConflict", err)
	}
}

func TestSearch(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	u := newUser(t, s, "alice")
	add := func(in BookmarkInput) Bookmark {
		b, _, err := s.CreateBookmark(ctx, u.ID, in)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	add(BookmarkInput{URL: "https://kubernetes.io/docs/ingress", Title: "Kubernetes Ingress", Tags: []string{"homelab", "k8s"}})
	add(BookmarkInput{URL: "https://github.com/foo/bar", Title: "Bar repo", Description: "exact phrase here", IsFavourite: true})
	add(BookmarkInput{URL: "https://gist.github.com/x", Title: "Gist", Notes: "café notes", Tags: []string{"archive"}})
	arch := add(BookmarkInput{URL: "https://old.example.com", Title: "Old kubernetes post"})
	if err := s.SetFlag(ctx, u.ID, arch.ID, FlagArchived, true); err != nil {
		t.Fatal(err)
	}

	for q, want := range map[string][]string{
		"":                         {"Bar repo", "Gist", "Kubernetes Ingress"},
		"kube":                     {"Kubernetes Ingress"},
		"kubernetes !archived":     {"Old kubernetes post"},
		`"exact phrase"`:           {"Bar repo"},
		`"phrase exact"`:           nil,
		"cafe":                     {"Gist"},
		"#HomeLab":                 {"Kubernetes Ingress"},
		"-#archive":                {"Bar repo", "Kubernetes Ingress"},
		"site:github.com":          {"Bar repo", "Gist"},
		"site:hub.com":             nil,
		"!fav":                     {"Bar repo"},
		"k8s":                      {"Kubernetes Ingress"}, // tag names are full-text searchable
		"after:2000-01-01 ingress": {"Kubernetes Ingress"},
		"before:2000-01-01":        nil,
		`NEAR( OR "`:               nil,
	} {
		if got := titles(t, s, u.ID, q); !slices.Equal(got, want) {
			t.Errorf("search %q = %v, want %v", q, got, want)
		}
	}
}

func TestTagsRenameMergeAndFTS(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	u := newUser(t, s, "alice")
	b1, _, _ := s.CreateBookmark(ctx, u.ID, BookmarkInput{URL: "https://a.com", Title: "A", Tags: []string{"golang"}})
	s.CreateBookmark(ctx, u.ID, BookmarkInput{URL: "https://b.com", Title: "B", Tags: []string{"go", "golang"}})

	tags, _ := s.ListTags(ctx, u.ID)
	id := map[string]int64{}
	for _, t := range tags {
		id[t.Name] = t.ID
	}
	if err := s.RenameTag(ctx, u.ID, id["golang"], "programming"); err != nil {
		t.Fatal(err)
	}
	if got := titles(t, s, u.ID, "programming"); !slices.Equal(got, []string{"A", "B"}) {
		t.Fatalf("FTS after rename = %v", got)
	}
	if got := titles(t, s, u.ID, "golang"); got != nil {
		t.Fatalf("old tag name still indexed: %v", got)
	}
	if err := s.RenameTag(ctx, u.ID, id["go"], "Programming"); err != nil { // merge
		t.Fatal(err)
	}
	tags, _ = s.ListTags(ctx, u.ID)
	if len(tags) != 1 || tags[0].Name != "programming" || tags[0].Count != 2 {
		t.Fatalf("after merge: %+v", tags)
	}

	// Removing the last use of a tag deletes it.
	if _, err := s.UpdateBookmark(ctx, u.ID, b1.ID, BookmarkInput{URL: "https://a.com", Title: "A"}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteTag(ctx, u.ID, tags[0].ID); err != nil {
		t.Fatal(err)
	}
	if tags, _ := s.ListTags(ctx, u.ID); len(tags) != 0 {
		t.Fatalf("tags left: %+v", tags)
	}
	if got := titles(t, s, u.ID, "programming"); got != nil {
		t.Fatalf("deleted tag still indexed: %v", got)
	}

	if err := s.DeleteBookmark(ctx, u.ID, b1.ID); err != nil {
		t.Fatal(err)
	}
	if got := titles(t, s, u.ID, "A"); got != nil {
		t.Fatalf("deleted bookmark still indexed: %v", got)
	}
}

// Every user-scoped method must refuse another user's rows.
func TestTenantIsolation(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	a, b := newUser(t, s, "alice"), newUser(t, s, "bob")
	bm, _, _ := s.CreateBookmark(ctx, a.ID, BookmarkInput{URL: "https://secret.example", Title: "secret", Tags: []string{"private"}})
	tags, _ := s.ListTags(ctx, a.ID)
	tagID := tags[0].ID

	if _, err := s.GetBookmark(ctx, b.ID, bm.ID); err != ErrNotFound {
		t.Errorf("GetBookmark: %v", err)
	}
	if _, err := s.FindBookmarkByURL(ctx, b.ID, bm.URL); err != ErrNotFound {
		t.Errorf("FindBookmarkByURL: %v", err)
	}
	if _, err := s.UpdateBookmark(ctx, b.ID, bm.ID, BookmarkInput{URL: "https://x.org"}); err != ErrNotFound {
		t.Errorf("UpdateBookmark: %v", err)
	}
	if err := s.SetFlag(ctx, b.ID, bm.ID, FlagFavourite, true); err != ErrNotFound {
		t.Errorf("SetFlag: %v", err)
	}
	if err := s.DeleteBookmark(ctx, b.ID, bm.ID); err != ErrNotFound {
		t.Errorf("DeleteBookmark: %v", err)
	}
	if err := s.RenameTag(ctx, b.ID, tagID, "mine"); err != ErrNotFound {
		t.Errorf("RenameTag: %v", err)
	}
	if err := s.DeleteTag(ctx, b.ID, tagID); err != ErrNotFound {
		t.Errorf("DeleteTag: %v", err)
	}
	if got := titles(t, s, b.ID, "secret"); got != nil {
		t.Errorf("search leaked: %v", got)
	}
	if got := titles(t, s, b.ID, "#private"); got != nil {
		t.Errorf("tag search leaked: %v", got)
	}
	if tags, _ := s.ListTags(ctx, b.ID); len(tags) != 0 {
		t.Errorf("ListTags leaked: %v", tags)
	}

	// And alice's data is untouched.
	got, err := s.GetBookmark(ctx, a.ID, bm.ID)
	if err != nil || got.Title != "secret" || got.IsFavourite || !slices.Equal(got.Tags, []string{"private"}) {
		t.Errorf("alice's bookmark changed: %+v %v", got, err)
	}
}

func TestCreateFirstAdminOnlyOnce(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if _, err := s.CreateFirstAdmin(ctx, "root", "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateFirstAdmin(ctx, "root2", "x"); err != ErrConflict {
		t.Fatalf("second CreateFirstAdmin: %v", err)
	}
	if _, err := s.CreateUser(ctx, "ROOT", "x", false); err != ErrConflict {
		t.Fatalf("case-insensitive username clash: %v", err)
	}
}

func TestSettings(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	u := newUser(t, s, "alice")
	if u.Settings != (Settings{Accent: "indigo", Theme: "system", Sidebar: "left"}) {
		t.Fatalf("defaults = %+v", u.Settings)
	}
	if err := s.SaveSettings(ctx, u.ID, Settings{Accent: "terracotta", Theme: "dark", Sidebar: "right"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.UserByID(ctx, u.ID); got.Settings != (Settings{Accent: "terracotta", Theme: "dark", Sidebar: "right"}) {
		t.Fatalf("saved = %+v", got.Settings)
	}
	if err := s.SaveSettings(ctx, u.ID, Settings{Accent: "pink", Theme: "dark", Sidebar: "left"}); !isValidationErr(err) {
		t.Fatalf("unknown accent: %v", err)
	}
	if err := s.SaveSettings(ctx, u.ID, Settings{Accent: "indigo", Theme: "dark", Sidebar: "top"}); !isValidationErr(err) {
		t.Fatalf("unknown sidebar: %v", err)
	}
	// A corrupt value falls back to defaults rather than failing the login.
	s.w.Exec("UPDATE users SET settings = 'not json' WHERE id = ?", u.ID)
	if got, err := s.UserByID(ctx, u.ID); err != nil || got.Settings.Theme != "system" {
		t.Fatalf("corrupt settings: %+v %v", got.Settings, err)
	}
}

func TestCountBookmarks(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	u, other := newUser(t, s, "alice"), newUser(t, s, "bob")
	s.CreateBookmark(ctx, u.ID, BookmarkInput{URL: "https://a.com", IsFavourite: true})
	s.CreateBookmark(ctx, u.ID, BookmarkInput{URL: "https://b.com"})
	c, _, _ := s.CreateBookmark(ctx, u.ID, BookmarkInput{URL: "https://c.com", IsFavourite: true})
	s.SetFlag(ctx, u.ID, c.ID, FlagArchived, true)
	s.CreateBookmark(ctx, other.ID, BookmarkInput{URL: "https://d.com"})
	got, err := s.CountBookmarks(ctx, u.ID)
	if err != nil || got != (Counts{All: 2, Favourites: 1, Archived: 1}) {
		t.Fatalf("counts = %+v, %v", got, err)
	}
}

func isValidationErr(err error) bool {
	_, ok := err.(ValidationError)
	return ok
}
