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
