package store

import (
	"context"
	"testing"
	"time"

	"github.com/marcelhoffs/anchor/internal/search"
)

func TestSaveFetchFillsOnlyEmptyFields(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	u := newUser(t, s, "alice")
	kept, _, _ := s.CreateBookmark(ctx, personal(s, u.ID), BookmarkInput{URL: "https://a.com", Title: "Mine"})
	empty, _, _ := s.CreateBookmark(ctx, personal(s, u.ID), BookmarkInput{URL: "https://b.com"})

	items, err := s.PendingFetches(ctx, 10)
	if err != nil || len(items) != 2 || items[0].ID != empty.ID {
		t.Fatalf("pending = %+v, %v (want newest first)", items, err)
	}
	icon, err := s.SaveFavicon(ctx, "a.com", "image/png", []byte("png"))
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		s.SaveFetch(ctx, it, FetchResult{Status: FetchDone, Title: "Fetched", Description: "From the page", FaviconID: icon})
	}
	if b, _ := s.GetBookmark(ctx, personal(s, u.ID), kept.ID); b.Title != "Mine" || b.Description != "From the page" || b.FaviconID != icon || b.FetchStatus != FetchDone {
		t.Fatalf("kept = %+v", b)
	}
	if b, _ := s.GetBookmark(ctx, personal(s, u.ID), empty.ID); b.Title != "Fetched" {
		t.Fatalf("empty = %+v", b)
	}
	if items, _ := s.PendingFetches(ctx, 10); len(items) != 0 {
		t.Fatalf("still pending: %+v", items)
	}

	// A result for a URL the user has since changed is dropped.
	stale := FetchItem{ID: empty.ID, URL: empty.URL}
	s.UpdateBookmark(ctx, personal(s, u.ID), empty.ID, BookmarkInput{URL: "https://c.com"})
	s.SaveFetch(ctx, stale, FetchResult{Status: FetchFailed, Description: "stale"})
	if b, _ := s.GetBookmark(ctx, personal(s, u.ID), empty.ID); b.FetchStatus != FetchPending || b.Description != "" || b.FaviconID != 0 {
		t.Fatalf("after URL change = %+v", b)
	}
	// Editing without changing the URL keeps the icon and status.
	s.UpdateBookmark(ctx, personal(s, u.ID), kept.ID, BookmarkInput{URL: kept.URL, Title: "Renamed"})
	if b, _ := s.GetBookmark(ctx, personal(s, u.ID), kept.ID); b.FaviconID != icon || b.FetchStatus != FetchDone {
		t.Fatalf("after edit = %+v", b)
	}
	if err := s.Refetch(ctx, personal(s, u.ID), kept.ID); err != nil {
		t.Fatal(err)
	}
	if b, _ := s.GetBookmark(ctx, personal(s, u.ID), kept.ID); b.FetchStatus != FetchPending {
		t.Fatal("refetch didn't queue")
	}
}

func TestFavicons(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	alice, bob := newUser(t, s, "alice"), newUser(t, s, "bob")
	id, _ := s.SaveFavicon(ctx, "a.com", "image/png", []byte("old"))
	if again, _ := s.SaveFavicon(ctx, "a.com", "image/x-icon", []byte("new")); again != id {
		t.Fatal("a domain's icon should keep its id")
	}
	if got, err := s.FaviconForDomain(ctx, "a.com", time.Now().Add(-time.Hour)); err != nil || got != id {
		t.Fatalf("fresh icon = %d, %v", got, err)
	}
	if _, err := s.FaviconForDomain(ctx, "a.com", time.Now().Add(time.Hour)); err != ErrNotFound {
		t.Fatalf("stale icon: %v", err)
	}
	b, _, _ := s.CreateBookmark(ctx, personal(s, alice.ID), BookmarkInput{URL: "https://a.com"})
	s.SaveFetch(ctx, FetchItem{ID: b.ID, URL: b.URL}, FetchResult{Status: FetchDone, FaviconID: id})
	if mime, data, err := s.Favicon(ctx, alice.ID, id); err != nil || mime != "image/x-icon" || string(data) != "new" {
		t.Fatalf("alice's icon = %q %q %v", mime, data, err)
	}
	if _, _, err := s.Favicon(ctx, bob.ID, id); err != ErrNotFound {
		t.Fatalf("bob sees an icon he has no bookmark for: %v", err)
	}
}

func TestSharing(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	u, other := newUser(t, s, "alice"), newUser(t, s, "bob")
	b, _, _ := s.CreateBookmark(ctx, personal(s, u.ID), BookmarkInput{URL: "https://a.com", Title: "A"})
	if err := s.SetShared(ctx, personal(s, other.ID), b.ID, true); err != ErrNotFound {
		t.Fatalf("bob shared alice's bookmark: %v", err)
	}
	s.SetShared(ctx, personal(s, u.ID), b.ID, true)
	b, _ = s.GetBookmark(ctx, personal(s, u.ID), b.ID)
	if !b.IsShared || len(b.ShareToken) < 20 {
		t.Fatalf("shared = %+v", b)
	}
	token := b.ShareToken
	s.SetShared(ctx, personal(s, u.ID), b.ID, true)
	if again, _ := s.GetBookmark(ctx, personal(s, u.ID), b.ID); again.ShareToken != token {
		t.Fatal("sharing again changed the link")
	}
	if got, err := s.SharedBookmark(ctx, token); err != nil || got.ID != b.ID {
		t.Fatalf("shared lookup = %+v, %v", got, err)
	}
	if list, _, _ := s.Search(ctx, personal(s, u.ID), search.Parse("!shared"), SortNewest, 10, 0); len(list) != 1 {
		t.Fatal("!shared doesn't find it")
	}
	// A disabled owner's links stop working.
	s.SetActive(ctx, u.ID, false)
	if _, err := s.SharedBookmark(ctx, token); err != ErrNotFound {
		t.Fatalf("disabled owner's link works: %v", err)
	}
	s.SetActive(ctx, u.ID, true)
	s.SetShared(ctx, personal(s, u.ID), b.ID, false)
	if _, err := s.SharedBookmark(ctx, token); err != ErrNotFound {
		t.Fatalf("unshared link works: %v", err)
	}
	if _, err := s.SharedBookmark(ctx, ""); err != ErrNotFound {
		t.Fatal("empty token matched")
	}
}

func TestSavedSearches(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	u, other := newUser(t, s, "alice"), newUser(t, s, "bob")
	if _, err := s.CreateSavedSearch(ctx, personal(s, u.ID), " ", "#go"); !isValidationErr(err) {
		t.Fatalf("empty name: %v", err)
	}
	if _, err := s.CreateSavedSearch(ctx, personal(s, u.ID), "Go", " "); !isValidationErr(err) {
		t.Fatalf("empty query: %v", err)
	}
	b, err := s.CreateSavedSearch(ctx, personal(s, u.ID), " zebra ", " #go site:github.com ")
	if err != nil || b.Name != "zebra" || b.Query != "#go site:github.com" {
		t.Fatalf("create = %+v, %v", b, err)
	}
	s.CreateSavedSearch(ctx, personal(s, u.ID), "Apple", "!fav")
	list, _ := s.ListSavedSearches(ctx, personal(s, u.ID))
	if len(list) != 2 || list[0].Name != "Apple" {
		t.Fatalf("list = %+v", list)
	}
	if err := s.RenameSavedSearch(ctx, personal(s, other.ID), b.ID, "x"); err != ErrNotFound {
		t.Fatalf("bob renamed alice's saved search: %v", err)
	}
	if err := s.DeleteSavedSearch(ctx, personal(s, other.ID), b.ID); err != ErrNotFound {
		t.Fatalf("bob deleted alice's saved search: %v", err)
	}
	if err := s.RenameSavedSearch(ctx, personal(s, u.ID), b.ID, " "); !isValidationErr(err) {
		t.Fatalf("blank rename: %v", err)
	}
	if err := s.RenameSavedSearch(ctx, personal(s, u.ID), b.ID, "Go repos"); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.ListSavedSearches(ctx, personal(s, u.ID)); list[1].Name != "Go repos" || list[1].Query != "#go site:github.com" {
		t.Fatalf("after rename = %+v", list)
	}
	if err := s.DeleteSavedSearch(ctx, personal(s, u.ID), b.ID); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.ListSavedSearches(ctx, personal(s, u.ID)); len(list) != 1 {
		t.Fatalf("after delete = %+v", list)
	}
}

func TestBatchFlagsAndDelete(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	u, other := newUser(t, s, "alice"), newUser(t, s, "bob")
	sc := personal(s, u.ID)
	a, _, _ := s.CreateBookmark(ctx, sc, BookmarkInput{URL: "https://a.com"})
	b, _, _ := s.CreateBookmark(ctx, sc, BookmarkInput{URL: "https://b.com"})
	theirs, _, _ := s.CreateBookmark(ctx, personal(s, other.ID), BookmarkInput{URL: "https://c.com"})

	// Another space's bookmark is skipped, not changed.
	if n, err := s.SetFlags(ctx, sc, []int64{a.ID, b.ID, theirs.ID}, FlagFavourite, true); err != nil || n != 2 {
		t.Fatalf("SetFlags = %d, %v", n, err)
	}
	if got, _ := s.GetBookmark(ctx, personal(s, other.ID), theirs.ID); got.IsFavourite {
		t.Fatal("favourited another user's bookmark")
	}
	if got, _ := s.GetBookmark(ctx, sc, b.ID); !got.IsFavourite {
		t.Fatal("not favourited")
	}
	if _, err := s.SetFlags(ctx, sc, make([]int64, MaxBatch+1), FlagArchived, true); !isValidationErr(err) {
		t.Fatalf("too many ids: %v", err)
	}
	if n, err := s.DeleteBookmarks(ctx, sc, []int64{a.ID, theirs.ID}); err != nil || n != 1 {
		t.Fatalf("DeleteBookmarks = %d, %v", n, err)
	}
	if _, err := s.GetBookmark(ctx, personal(s, other.ID), theirs.ID); err != nil {
		t.Fatal("deleted another user's bookmark")
	}
	if _, err := s.GetBookmark(ctx, sc, a.ID); err != ErrNotFound {
		t.Fatal("not deleted")
	}
}

func TestCopyAndMoveBookmarks(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	u, reader := newUser(t, s, "alice"), newUser(t, s, "bob")
	from := personal(s, u.ID)
	space := newShared(t, s, map[int64]Role{u.ID: RoleOwner, reader.ID: RoleRead})
	to, _ := s.Access(ctx, u.ID, space)
	readOnly, _ := s.Access(ctx, reader.ID, space)

	mustTags(t, s, u.ID, "go", "docs")
	tags, _ := s.ListTags(ctx, from)
	s.SetTagColor(ctx, from, tags[1].ID, "rose") // go
	a, _, _ := s.CreateBookmark(ctx, from, BookmarkInput{URL: "https://a.com", Title: "A", Tags: []string{"go", "docs"}, IsFavourite: true})
	b, _, _ := s.CreateBookmark(ctx, from, BookmarkInput{URL: "https://b.com", Title: "B"})
	dup, _, _ := s.CreateBookmark(ctx, from, BookmarkInput{URL: "https://dup.com"})
	s.SetShared(ctx, from, b.ID, true)
	b, _ = s.GetBookmark(ctx, from, b.ID)
	s.CreateTag(ctx, to, "docs")
	s.CreateBookmark(ctx, to, BookmarkInput{URL: "https://dup.com", Title: "Already here"})

	if _, _, err := s.CopyBookmarks(ctx, from, from, []int64{a.ID}, false); !isValidationErr(err) {
		t.Fatalf("copy into the same space: %v", err)
	}
	if _, _, err := s.CopyBookmarks(ctx, from, readOnly, []int64{a.ID}, false); err != ErrForbidden {
		t.Fatalf("copy without write access to the target: %v", err)
	}

	// Copy: the source keeps it; tags come along, colour too; duplicates are skipped.
	done, skipped, err := s.CopyBookmarks(ctx, from, to, []int64{a.ID, dup.ID}, false)
	if err != nil || done != 1 || skipped != 1 {
		t.Fatalf("copy = %d, %d, %v", done, skipped, err)
	}
	if _, err := s.GetBookmark(ctx, from, a.ID); err != nil {
		t.Fatal("copy removed the original")
	}
	copied, err := s.FindBookmarkByURL(ctx, to, "https://a.com")
	if err != nil || copied.ID == a.ID || copied.Title != "A" || !copied.IsFavourite || len(copied.Tags) != 2 {
		t.Fatalf("copy = %+v, %v", copied, err)
	}
	toTags, _ := s.ListTags(ctx, to)
	if len(toTags) != 2 || toTags[1].Name != "go" || toTags[1].Color != "rose" {
		t.Fatalf("target tags = %+v", toTags)
	}
	if got, _ := s.FindBookmarkByURL(ctx, to, "https://dup.com"); got.Title != "Already here" {
		t.Fatal("a duplicate overwrote the target's bookmark")
	}

	// Move: gone from the source, share link still works; a skipped one stays.
	done, skipped, err = s.CopyBookmarks(ctx, from, to, []int64{b.ID, dup.ID}, true)
	if err != nil || done != 1 || skipped != 1 {
		t.Fatalf("move = %d, %d, %v", done, skipped, err)
	}
	if _, err := s.GetBookmark(ctx, from, b.ID); err != ErrNotFound {
		t.Fatal("moved bookmark still in the source")
	}
	if _, err := s.GetBookmark(ctx, from, dup.ID); err != nil {
		t.Fatal("skipped bookmark left the source")
	}
	if got, err := s.SharedBookmark(ctx, b.ShareToken); err != nil || got.ID != b.ID {
		t.Fatalf("share link after a move: %+v, %v", got, err)
	}
	// A copy of a shared bookmark is private.
	if _, _, err := s.CopyBookmarks(ctx, to, from, []int64{b.ID}, false); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.FindBookmarkByURL(ctx, from, "https://b.com"); got.IsShared {
		t.Fatal("copy is shared")
	}
}
