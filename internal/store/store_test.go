package store

import (
	"cmp"
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/marcelhoffs/anchor/internal/db"
	"github.com/marcelhoffs/anchor/internal/search"
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

// mustTags creates tags, as a user does on the Tags page before using them.
func mustTags(t *testing.T, s *Store, userID int64, names ...string) {
	t.Helper()
	for _, n := range names {
		if _, err := s.CreateTag(context.Background(), personal(s, userID), n); err != nil {
			t.Fatal(err)
		}
	}
}

func titles(t *testing.T, s *Store, userID int64, q string) []string {
	t.Helper()
	list, _, err := s.Search(context.Background(), personal(s, userID), search.Parse(q), SortNewest, 50, 0)
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
	mustTags(t, s, a.ID, "x")

	first, created, err := s.CreateBookmark(ctx, personal(s, a.ID), BookmarkInput{URL: "https://Example.com", Title: "One", Tags: []string{"x"}})
	if err != nil || !created {
		t.Fatalf("create: %v %v", created, err)
	}
	again, created, err := s.CreateBookmark(ctx, personal(s, a.ID), BookmarkInput{URL: "example.com/", Title: "Two"})
	if err != nil || created || again.ID != first.ID || again.Title != "One" {
		t.Fatalf("duplicate create = %+v, created=%v, %v", again, created, err)
	}
	if _, created, _ := s.CreateBookmark(ctx, personal(s, b.ID), BookmarkInput{URL: "https://example.com"}); !created {
		t.Fatal("another user's identical URL should be a new bookmark")
	}
	other, _, _ := s.CreateBookmark(ctx, personal(s, a.ID), BookmarkInput{URL: "https://other.org"})
	if _, err := s.UpdateBookmark(ctx, personal(s, a.ID), other.ID, BookmarkInput{URL: "https://example.com"}); err != ErrConflict {
		t.Fatalf("update to an existing URL: %v, want ErrConflict", err)
	}
}

func TestSearch(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	u := newUser(t, s, "alice")
	add := func(in BookmarkInput) Bookmark {
		b, _, err := s.CreateBookmark(ctx, personal(s, u.ID), in)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	mustTags(t, s, u.ID, "homelab", "k8s", "archive", "home lab")
	add(BookmarkInput{URL: "https://kubernetes.io/docs/ingress", Title: "Kubernetes Ingress", Tags: []string{"homelab", "k8s"}})
	add(BookmarkInput{URL: "https://github.com/foo/bar", Title: "Bar repo", Description: "exact phrase here", IsFavourite: true})
	add(BookmarkInput{URL: "https://gist.github.com/x", Title: "Gist", Notes: "café notes", Tags: []string{"archive"}})
	arch := add(BookmarkInput{URL: "https://old.example.com", Title: "Old kubernetes post"})
	if err := s.SetFlag(ctx, personal(s, u.ID), arch.ID, FlagArchived, true); err != nil {
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
		"!untagged":                {"Bar repo"}, // archived ones only with !archived
		"!untagged !archived":      {"Old kubernetes post"},
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
	mustTags(t, s, u.ID, "golang", "go")
	b1, _, _ := s.CreateBookmark(ctx, personal(s, u.ID), BookmarkInput{URL: "https://a.com", Title: "A", Tags: []string{"golang"}})
	b2, _, _ := s.CreateBookmark(ctx, personal(s, u.ID), BookmarkInput{URL: "https://b.com", Title: "B", Tags: []string{"go", "golang"}})

	tags, _ := s.ListTags(ctx, personal(s, u.ID))
	id := map[string]int64{}
	for _, t := range tags {
		id[t.Name] = t.ID
	}
	if err := s.RenameTag(ctx, personal(s, u.ID), id["golang"], "programming"); err != nil {
		t.Fatal(err)
	}
	if got := titles(t, s, u.ID, "programming"); !slices.Equal(got, []string{"A", "B"}) {
		t.Fatalf("FTS after rename = %v", got)
	}
	if got := titles(t, s, u.ID, "golang"); got != nil {
		t.Fatalf("old tag name still indexed: %v", got)
	}
	if err := s.RenameTag(ctx, personal(s, u.ID), id["go"], "Programming"); err != nil { // merge
		t.Fatal(err)
	}
	tags, _ = s.ListTags(ctx, personal(s, u.ID))
	if len(tags) != 1 || tags[0].Name != "programming" || tags[0].Count != 2 {
		t.Fatalf("after merge: %+v", tags)
	}

	// A tag outlives its last bookmark; only deleting it removes it.
	if _, err := s.UpdateBookmark(ctx, personal(s, u.ID), b1.ID, BookmarkInput{URL: "https://a.com", Title: "A"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateBookmark(ctx, personal(s, u.ID), b2.ID, BookmarkInput{URL: "https://b.com", Title: "B"}); err != nil {
		t.Fatal(err)
	}
	if tags, _ := s.ListTags(ctx, personal(s, u.ID)); len(tags) != 1 || tags[0].Count != 0 {
		t.Fatalf("unused tag removed: %+v", tags)
	}
	if err := s.DeleteTag(ctx, personal(s, u.ID), tags[0].ID); err != nil {
		t.Fatal(err)
	}
	if tags, _ := s.ListTags(ctx, personal(s, u.ID)); len(tags) != 0 {
		t.Fatalf("tags left: %+v", tags)
	}
	if got := titles(t, s, u.ID, "programming"); got != nil {
		t.Fatalf("deleted tag still indexed: %v", got)
	}

	if err := s.DeleteBookmark(ctx, personal(s, u.ID), b1.ID); err != nil {
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
	mustTags(t, s, a.ID, "private")
	bm, _, _ := s.CreateBookmark(ctx, personal(s, a.ID), BookmarkInput{URL: "https://secret.example", Title: "secret", Tags: []string{"private"}})
	tags, _ := s.ListTags(ctx, personal(s, a.ID))
	tagID := tags[0].ID

	if _, err := s.GetBookmark(ctx, personal(s, b.ID), bm.ID); err != ErrNotFound {
		t.Errorf("GetBookmark: %v", err)
	}
	if _, err := s.FindBookmarkByURL(ctx, personal(s, b.ID), bm.URL); err != ErrNotFound {
		t.Errorf("FindBookmarkByURL: %v", err)
	}
	if _, err := s.UpdateBookmark(ctx, personal(s, b.ID), bm.ID, BookmarkInput{URL: "https://x.org"}); err != ErrNotFound {
		t.Errorf("UpdateBookmark: %v", err)
	}
	if err := s.SetFlag(ctx, personal(s, b.ID), bm.ID, FlagFavourite, true); err != ErrNotFound {
		t.Errorf("SetFlag: %v", err)
	}
	if err := s.DeleteBookmark(ctx, personal(s, b.ID), bm.ID); err != ErrNotFound {
		t.Errorf("DeleteBookmark: %v", err)
	}
	if err := s.RenameTag(ctx, personal(s, b.ID), tagID, "mine"); err != ErrNotFound {
		t.Errorf("RenameTag: %v", err)
	}
	if err := s.DeleteTag(ctx, personal(s, b.ID), tagID); err != ErrNotFound {
		t.Errorf("DeleteTag: %v", err)
	}
	if got := titles(t, s, b.ID, "secret"); got != nil {
		t.Errorf("search leaked: %v", got)
	}
	if got := titles(t, s, b.ID, "#private"); got != nil {
		t.Errorf("tag search leaked: %v", got)
	}
	if tags, _ := s.ListTags(ctx, personal(s, b.ID)); len(tags) != 0 {
		t.Errorf("ListTags leaked: %v", tags)
	}

	// And alice's data is untouched.
	got, err := s.GetBookmark(ctx, personal(s, a.ID), bm.ID)
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
	if u.Settings != (Settings{Accent: "indigo", Theme: "system", Font: "inter", Language: "auto", Sidebar: "left", Sort: "newest", Descriptions: "show"}) {
		t.Fatalf("defaults = %+v", u.Settings)
	}
	if err := s.SaveSettings(ctx, u.ID, Settings{Accent: "terracotta", Theme: "dark", Font: "lato", Language: "nl", Sidebar: "right", Sort: "title", Descriptions: "hide"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.UserByID(ctx, u.ID); got.Settings != (Settings{Accent: "terracotta", Theme: "dark", Font: "lato", Language: "nl", Sidebar: "right", Sort: "title", Descriptions: "hide"}) {
		t.Fatalf("saved = %+v", got.Settings)
	}
	if err := s.SaveSettings(ctx, u.ID, Settings{Accent: "pink", Theme: "dark", Font: "inter", Language: "auto", Sidebar: "left", Sort: "newest", Descriptions: "show"}); !isValidationErr(err) {
		t.Fatalf("unknown accent: %v", err)
	}
	if err := s.SaveSettings(ctx, u.ID, Settings{Accent: "indigo", Theme: "dark", Font: "inter", Language: "auto", Sidebar: "top", Sort: "newest", Descriptions: "show"}); !isValidationErr(err) {
		t.Fatalf("unknown sidebar: %v", err)
	}
	if err := s.SaveSettings(ctx, u.ID, Settings{Accent: "indigo", Theme: "dark", Font: "inter", Language: "auto", Sidebar: "left", Sort: "random", Descriptions: "show"}); !isValidationErr(err) {
		t.Fatalf("unknown sort: %v", err)
	}
	if err := s.SaveSettings(ctx, u.ID, Settings{Accent: "indigo", Theme: "dark", Font: "papyrus", Language: "auto", Sidebar: "left", Sort: "newest", Descriptions: "show"}); !isValidationErr(err) {
		t.Fatalf("unknown font: %v", err)
	}
	if err := s.SaveSettings(ctx, u.ID, Settings{Accent: "indigo", Theme: "dark", Font: "inter", Language: "de", Sidebar: "left", Sort: "newest", Descriptions: "show"}); !isValidationErr(err) {
		t.Fatalf("unknown language: %v", err)
	}
	// A corrupt value falls back to defaults rather than failing the login.
	s.w.Exec("UPDATE users SET settings = 'not json' WHERE id = ?", u.ID)
	if got, err := s.UserByID(ctx, u.ID); err != nil || got.Settings.Theme != "system" {
		t.Fatalf("corrupt settings: %+v %v", got.Settings, err)
	}
}

func TestSearchSortAndTotal(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	u := newUser(t, s, "alice")
	s.CreateBookmark(ctx, personal(s, u.ID), BookmarkInput{URL: "https://b.com", Title: "banana"})
	s.CreateBookmark(ctx, personal(s, u.ID), BookmarkInput{URL: "https://c.com"}) // no title: sorts by URL
	s.CreateBookmark(ctx, personal(s, u.ID), BookmarkInput{URL: "https://a.com", Title: "Apple"})
	order := func(sort string, limit, offset int) ([]string, int) {
		t.Helper()
		list, total, err := s.Search(ctx, personal(s, u.ID), search.Parse(""), sort, limit, offset)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, b := range list {
			out = append(out, cmp.Or(b.Title, b.URL))
		}
		return out, total
	}
	if got, total := order(SortNewest, 10, 0); !slices.Equal(got, []string{"Apple", "https://c.com/", "banana"}) || total != 3 {
		t.Fatalf("newest = %v (%d)", got, total)
	}
	if got, _ := order(SortTitle, 10, 0); !slices.Equal(got, []string{"Apple", "banana", "https://c.com/"}) {
		t.Fatalf("title = %v", got)
	}
	if got, total := order(SortTitle, 2, 2); !slices.Equal(got, []string{"https://c.com/"}) || total != 3 {
		t.Fatalf("second page = %v (%d)", got, total)
	}
	if got, total := order(SortTitle, 2, 4); got != nil || total != 3 {
		t.Fatalf("past the end = %v (%d)", got, total)
	}
}

func TestCountBookmarks(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	u, other := newUser(t, s, "alice"), newUser(t, s, "bob")
	s.CreateBookmark(ctx, personal(s, u.ID), BookmarkInput{URL: "https://a.com", IsFavourite: true})
	s.CreateBookmark(ctx, personal(s, u.ID), BookmarkInput{URL: "https://b.com"})
	c, _, _ := s.CreateBookmark(ctx, personal(s, u.ID), BookmarkInput{URL: "https://c.com", IsFavourite: true})
	s.SetFlag(ctx, personal(s, u.ID), c.ID, FlagArchived, true)
	s.CreateBookmark(ctx, personal(s, other.ID), BookmarkInput{URL: "https://d.com"})
	got, err := s.CountBookmarks(ctx, personal(s, u.ID))
	if err != nil || got != (Counts{All: 2, Favourites: 1, Archived: 1}) {
		t.Fatalf("counts = %+v, %v", got, err)
	}
}

func isValidationErr(err error) bool {
	_, ok := err.(ValidationError)
	return ok
}

func TestTagsMustExist(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	u, other := newUser(t, s, "alice"), newUser(t, s, "bob")
	mustTags(t, s, other.ID, "home lab") // another user's tag doesn't count

	_, _, err := s.CreateBookmark(ctx, personal(s, u.ID), BookmarkInput{URL: "https://a.com", Tags: []string{"home lab, go"}})
	if !isValidationErr(err) || !strings.Contains(err.Error(), `"home lab", "go"`) {
		t.Fatalf("unknown tags: %v", err)
	}
	if _, err := s.FindBookmarkByURL(ctx, personal(s, u.ID), "https://a.com"); err != ErrNotFound {
		t.Fatal("bookmark saved despite unknown tags")
	}

	tag, err := s.CreateTag(ctx, personal(s, u.ID), "  #Home   Lab ")
	if err != nil || tag.Name != "Home Lab" {
		t.Fatalf("create = %+v, %v", tag, err)
	}
	for _, bad := range []string{"", "#", "a,b", `say "hi"`, strings.Repeat("x", 65)} {
		if _, err := s.CreateTag(ctx, personal(s, u.ID), bad); !isValidationErr(err) {
			t.Errorf("CreateTag(%q) = %v", bad, err)
		}
	}
	if _, err := s.CreateTag(ctx, personal(s, u.ID), "home lab"); !isValidationErr(err) {
		t.Fatalf("duplicate tag: %v", err)
	}
	mustTags(t, s, u.ID, "go")
	// Matching is case-insensitive; the tag keeps its own spelling.
	b, _, err := s.CreateBookmark(ctx, personal(s, u.ID), BookmarkInput{URL: "https://a.com", Tags: []string{"home lab, GO", "#go"}})
	if err != nil || !slices.Equal(b.Tags, []string{"go", "Home Lab"}) {
		t.Fatalf("tagged = %v, %v", b.Tags, err)
	}
	if got := titles(t, s, u.ID, `#"home lab"`); len(got) != 1 {
		t.Fatalf(`#"home lab" = %v`, got)
	}
	if got := titles(t, s, u.ID, `-#"Home Lab"`); got != nil {
		t.Fatalf(`-#"Home Lab" = %v`, got)
	}

	// Deleting the bookmark keeps its tags.
	s.DeleteBookmark(ctx, personal(s, u.ID), b.ID)
	if tags, _ := s.ListTags(ctx, personal(s, u.ID)); len(tags) != 2 {
		t.Fatalf("tags after delete = %+v", tags)
	}
	if err := s.RenameTag(ctx, personal(s, u.ID), tag.ID, "self hosted"); err != nil {
		t.Fatal(err)
	}
	if err := s.RenameTag(ctx, personal(s, u.ID), tag.ID, "a,b"); !isValidationErr(err) {
		t.Fatalf("rename to a,b: %v", err)
	}
}

// personal is a user's scope in their own space.
func personal(s *Store, userID int64) Scope {
	sc, err := s.PersonalScope(context.Background(), userID)
	if err != nil {
		panic(err)
	}
	return sc
}

func TestAddTag(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	u := newUser(t, s, "alice")
	sc := personal(s, u.ID)
	mustTags(t, s, u.ID, "go")
	tags, _ := s.ListTags(ctx, sc)
	b, _, err := s.CreateBookmark(ctx, sc, BookmarkInput{URL: "https://go.dev", Title: "Go"})
	if err != nil {
		t.Fatal(err)
	}
	if added, err := s.AddTag(ctx, sc, b.ID, tags[0].ID); err != nil || !added {
		t.Fatalf("add: %v %v", added, err)
	}
	if added, err := s.AddTag(ctx, sc, b.ID, tags[0].ID); err != nil || added {
		t.Fatalf("add again: %v %v", added, err)
	}
	if got := titles(t, s, u.ID, "#go"); !slices.Equal(got, []string{"Go"}) {
		t.Fatalf("#go after AddTag: %v", got)
	}
	if got := titles(t, s, u.ID, "go"); !slices.Equal(got, []string{"Go"}) {
		t.Fatalf("full text after AddTag: %v", got) // the tag is in the search index
	}
	// Another space's bookmark or tag is out of reach.
	bob := newUser(t, s, "bob")
	mustTags(t, s, bob.ID, "go")
	bobTags, _ := s.ListTags(ctx, personal(s, bob.ID))
	if _, err := s.AddTag(ctx, sc, b.ID, bobTags[0].ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other space's tag: %v", err)
	}
	if _, err := s.AddTag(ctx, personal(s, bob.ID), b.ID, bobTags[0].ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other space's bookmark: %v", err)
	}
}
