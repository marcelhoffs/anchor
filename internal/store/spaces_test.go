package store

import (
	"context"
	"errors"
	"testing"

	"github.com/marcelhoffs/anchor/internal/importexport"
	"github.com/marcelhoffs/anchor/internal/search"
)

// newOrg makes an organization space with the given members. Organizations
// get their own store methods later; the access rules don't depend on them.
func newOrg(t *testing.T, s *Store, members map[int64]Role) int64 {
	t.Helper()
	ctx := context.Background()
	var id int64
	if err := s.w.QueryRowContext(ctx, "INSERT INTO spaces (name, created_at) VALUES ('Team', 0) RETURNING id").Scan(&id); err != nil {
		t.Fatal(err)
	}
	for uid, role := range members {
		if _, err := s.w.ExecContext(ctx, "INSERT INTO space_members (space_id, user_id, role, created_at) VALUES (?, ?, ?, 0)",
			id, uid, role); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func TestSpaceAccess(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner, writer, reader, outsider := newUser(t, s, "owner"), newUser(t, s, "writer"), newUser(t, s, "reader"), newUser(t, s, "outsider")
	org := newOrg(t, s, map[int64]Role{owner.ID: RoleOwner, writer.ID: RoleWrite, reader.ID: RoleRead})

	scope := func(u User) Scope {
		t.Helper()
		sc, err := s.Access(ctx, u.ID, org)
		if err != nil {
			t.Fatalf("%s: %v", u.Username, err)
		}
		return sc
	}
	if _, err := s.Access(ctx, outsider.ID, org); !errors.Is(err, ErrNotFound) {
		t.Fatalf("outsider got access: %v", err)
	}
	if _, err := s.Access(ctx, owner.ID, personal(s, writer.ID).SpaceID()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("access to someone's personal space: %v", err)
	}

	// Writers add to the organization; everyone in it sees the same bookmarks and tags.
	w := scope(writer)
	if _, err := s.CreateTag(ctx, w, "team"); err != nil {
		t.Fatal(err)
	}
	b, _, err := s.CreateBookmark(ctx, w, BookmarkInput{URL: "https://go.dev", Tags: []string{"team"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []User{owner, writer, reader} {
		list, total, err := s.Search(ctx, scope(u), search.Parse("#team"), SortNewest, 10, 0)
		if err != nil || total != 1 || list[0].ID != b.ID {
			t.Fatalf("%s sees %v (%d), %v", u.Username, list, total, err)
		}
		if sc, err := s.BookmarkScope(ctx, u.ID, b.ID); err != nil || sc.SpaceID() != org || sc.Role() != scope(u).Role() {
			t.Fatalf("%s: bookmark scope %+v, %v", u.Username, sc, err)
		}
	}
	// The same URL can be saved again in a personal space: it's another space.
	if _, created, err := s.CreateBookmark(ctx, personal(s, writer.ID), BookmarkInput{URL: "https://go.dev"}); err != nil || !created {
		t.Fatalf("same URL in a personal space: created=%v, %v", created, err)
	}

	// Readers can read but not change anything.
	r := scope(reader)
	if _, err := s.GetBookmark(ctx, r, b.ID); err != nil {
		t.Fatal(err)
	}
	for name, err := range map[string]error{
		"create": func() error { _, _, err := s.CreateBookmark(ctx, r, BookmarkInput{URL: "https://x.org"}); return err }(),
		"update": func() error {
			_, err := s.UpdateBookmark(ctx, r, b.ID, BookmarkInput{URL: "https://x.org"})
			return err
		}(),
		"flag":    s.SetFlag(ctx, r, b.ID, FlagFavourite, true),
		"share":   s.SetShared(ctx, r, b.ID, true),
		"refetch": s.Refetch(ctx, r, b.ID),
		"delete":  s.DeleteBookmark(ctx, r, b.ID),
		"new tag": func() error { _, err := s.CreateTag(ctx, r, "x"); return err }(),
		"rename":  s.RenameTag(ctx, r, 1, "x"),
		"del tag": s.DeleteTag(ctx, r, 1),
		"import": func() error {
			_, err := s.ImportBookmarks(ctx, r, []importexport.Item{{URL: "https://x.org"}})
			return err
		}(),
		"queue job": func() error { _, err := s.CreateJob(ctx, r, JobImport, ""); return err }(),
	} {
		if !errors.Is(err, ErrForbidden) {
			t.Errorf("reader %s: %v, want ErrForbidden", name, err)
		}
	}
	// Bundles are the reader's own: keeping them changes nothing in the space.
	if _, err := s.CreateBundle(ctx, r, "Team", "#team"); err != nil {
		t.Fatalf("reader can't save a search: %v", err)
	}
	if list, _ := s.ListBundles(ctx, w); len(list) != 0 {
		t.Fatal("bundles are shared between members")
	}

	// Outsiders reach nothing, not even through a bookmark id.
	if _, err := s.BookmarkScope(ctx, outsider.ID, b.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("outsider's bookmark scope: %v", err)
	}
	if _, err := s.GetBookmark(ctx, personal(s, outsider.ID), b.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bookmark read through another space: %v", err)
	}
	if err := s.DeleteBookmark(ctx, personal(s, outsider.ID), b.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bookmark deleted through another space: %v", err)
	}
	if list, total, err := s.Search(ctx, Scope{}, search.Query{}, SortNewest, 10, 0); err != nil || total != 0 || len(list) != 0 {
		t.Fatalf("the zero Scope matched %d bookmarks, %v", total, err)
	}

	// Icons follow the bookmarks: members see them, outsiders don't.
	fav, err := s.SaveFavicon(ctx, "go.dev", "image/png", []byte("png"))
	if err != nil {
		t.Fatal(err)
	}
	s.SaveFetch(ctx, FetchItem{ID: b.ID, URL: b.URL}, FetchResult{Status: FetchDone, FaviconID: fav})
	if _, _, err := s.Favicon(ctx, reader.ID, fav); err != nil {
		t.Fatalf("member can't load the icon: %v", err)
	}
	if _, _, err := s.Favicon(ctx, outsider.ID, fav); !errors.Is(err, ErrNotFound) {
		t.Fatalf("outsider loaded the icon: %v", err)
	}

	// An organization's share link doesn't depend on any one account.
	if err := s.SetShared(ctx, w, b.ID, true); err != nil {
		t.Fatal(err)
	}
	shared, _ := s.GetBookmark(ctx, w, b.ID)
	s.SetActive(ctx, writer.ID, false)
	if _, err := s.SharedBookmark(ctx, shared.ShareToken); err != nil {
		t.Fatalf("org share link stopped with its creator's account: %v", err)
	}
}
