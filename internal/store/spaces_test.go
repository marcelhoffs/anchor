package store

import (
	"context"
	"errors"
	"testing"

	"github.com/marcelhoffs/anchor/internal/importexport"
	"github.com/marcelhoffs/anchor/internal/search"
)

// newShared makes a shared space with the given members straight in the
// database, so the access rules are tested apart from the space methods.
func newShared(t *testing.T, s *Store, members map[int64]Role) int64 {
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
	space := newShared(t, s, map[int64]Role{owner.ID: RoleOwner, writer.ID: RoleWrite, reader.ID: RoleRead})

	scope := func(u User) Scope {
		t.Helper()
		sc, err := s.Access(ctx, u.ID, space)
		if err != nil {
			t.Fatalf("%s: %v", u.Username, err)
		}
		return sc
	}
	if _, err := s.Access(ctx, outsider.ID, space); !errors.Is(err, ErrNotFound) {
		t.Fatalf("outsider got access: %v", err)
	}
	if _, err := s.Access(ctx, owner.ID, personal(s, writer.ID).SpaceID()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("access to someone's personal space: %v", err)
	}

	// Writers add to the shared space; everyone in it sees the same bookmarks and tags.
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
		if sc, err := s.BookmarkScope(ctx, u.ID, b.ID); err != nil || sc.SpaceID() != space || sc.Role() != scope(u).Role() {
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
	// Saved searches are the reader's own: keeping them changes nothing in the space.
	if _, err := s.CreateSavedSearch(ctx, r, "Team", "#team"); err != nil {
		t.Fatalf("reader can't save a search: %v", err)
	}
	if list, _ := s.ListSavedSearches(ctx, w); len(list) != 0 {
		t.Fatal("saved searches are shared between members")
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

	// A shared space's share link doesn't depend on any one account.
	if err := s.SetShared(ctx, w, b.ID, true); err != nil {
		t.Fatal(err)
	}
	shared, _ := s.GetBookmark(ctx, w, b.ID)
	s.SetActive(ctx, writer.ID, false)
	if _, err := s.SharedBookmark(ctx, shared.ShareToken); err != nil {
		t.Fatalf("shared space's link stopped with its creator's account: %v", err)
	}
}

func TestSharedSpaces(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	alice, bob, carol := newUser(t, s, "alice"), newUser(t, s, "bob"), newUser(t, s, "carol")

	if _, err := s.CreateSpace(ctx, alice.ID, "   "); !isValidationErr(err) {
		t.Fatalf("empty name: %v", err)
	}
	owner, err := s.CreateSpace(ctx, alice.ID, "  Family   home ")
	if err != nil || owner.Role() != RoleOwner || owner.IsPersonal() {
		t.Fatalf("create: %+v, %v", owner, err)
	}
	if list, _ := s.ListSpaces(ctx, alice.ID); len(list) != 1 || list[0].Name != "Family home" || list[0].Owner != "alice" || list[0].Members != 1 {
		t.Fatalf("list: %+v", list)
	}
	if list, _ := s.ListSpaces(ctx, bob.ID); len(list) != 0 {
		t.Fatalf("bob sees spaces he isn't in: %+v", list)
	}

	// Adding members: existing users only, once, as editor or reader.
	for name, err := range map[string]error{
		"unknown user": s.AddMember(ctx, owner, "nobody", RoleRead),
		"owner role":   s.AddMember(ctx, owner, "bob", RoleOwner),
	} {
		if !isValidationErr(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := s.AddMember(ctx, owner, "BOB", RoleRead); err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, owner, "bob", RoleWrite); !isValidationErr(err) {
		t.Fatalf("added twice: %v", err)
	}
	s.AddMember(ctx, owner, "carol", RoleWrite)
	members, _ := s.Members(ctx, owner)
	if len(members) != 3 || members[0].Username != "alice" || members[0].Role != RoleOwner {
		t.Fatalf("members: %+v", members)
	}

	// Only the owner manages the space, not its other members.
	for who, id := range map[string]int64{"bob": bob.ID, "carol": carol.ID} {
		sc, _ := s.Access(ctx, id, owner.SpaceID())
		other := carol.ID
		if id == carol.ID {
			other = bob.ID
		}
		for name, err := range map[string]error{
			"add":      s.AddMember(ctx, sc, "alice", RoleRead),
			"role":     s.SetRole(ctx, sc, other, RoleWrite),
			"remove":   s.RemoveMember(ctx, sc, other),
			"rename":   s.RenameSpace(ctx, sc, "Mine"),
			"icon":     s.SetSpaceIcon(ctx, sc, "car"),
			"transfer": s.TransferSpace(ctx, sc, id),
			"delete":   s.DeleteSpace(ctx, sc),
		} {
			if !errors.Is(err, ErrForbidden) {
				t.Errorf("%s (%s) can %s: %v", who, sc.Role(), name, err)
			}
		}
	}
	// A personal space has nothing to manage.
	if err := s.AddMember(ctx, personal(s, alice.ID), "bob", RoleRead); !errors.Is(err, ErrNotFound) {
		t.Fatalf("member added to a personal space: %v", err)
	}
	if _, err := s.GetSpace(ctx, personal(s, alice.ID)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("personal space listed as shared: %v", err)
	}

	// Roles change; the owner's only by handing the space over.
	if err := s.SetRole(ctx, owner, bob.ID, RoleWrite); err != nil {
		t.Fatal(err)
	}
	bobScope, _ := s.Access(ctx, bob.ID, owner.SpaceID())
	if !bobScope.CanWrite() {
		t.Fatal("bob can't edit after being made an editor")
	}
	if err := s.SetRole(ctx, owner, alice.ID, RoleRead); !errors.Is(err, ErrNotFound) {
		t.Fatalf("owner demoted: %v", err)
	}
	if err := s.RemoveMember(ctx, owner, alice.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("owner removed: %v", err)
	}
	if err := s.LeaveSpace(ctx, owner); !isValidationErr(err) {
		t.Fatalf("owner left: %v", err)
	}

	// Handing over: bob owns it, alice stays as an editor.
	if err := s.TransferSpace(ctx, owner, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("handed to a non-member: %v", err)
	}
	if sc, _ := s.Access(ctx, alice.ID, owner.SpaceID()); sc.Role() != RoleOwner {
		t.Fatal("failed hand-over changed the owner")
	}
	if err := s.TransferSpace(ctx, owner, bob.ID); err != nil {
		t.Fatal(err)
	}
	aliceScope, _ := s.Access(ctx, alice.ID, owner.SpaceID())
	bobScope, _ = s.Access(ctx, bob.ID, owner.SpaceID())
	if aliceScope.Role() != RoleWrite || bobScope.Role() != RoleOwner {
		t.Fatalf("after hand-over: alice %s, bob %s", aliceScope.Role(), bobScope.Role())
	}

	// Members leave; removed members lose access at once, and their saved
	// searches in the space go with them.
	carolScope, _ := s.Access(ctx, carol.ID, owner.SpaceID())
	for _, sc := range []Scope{aliceScope, carolScope} {
		if _, err := s.CreateSavedSearch(ctx, sc, "Go", "#go"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.LeaveSpace(ctx, aliceScope); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveMember(ctx, bobScope, carol.ID); err != nil {
		t.Fatal(err)
	}
	for _, u := range []User{alice, carol} {
		if _, err := s.Access(ctx, u.ID, owner.SpaceID()); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s still has access: %v", u.Username, err)
		}
	}
	var stale int
	s.r.QueryRowContext(ctx, "SELECT count(*) FROM saved_searches WHERE space_id = ?", owner.SpaceID()).Scan(&stale)
	if stale != 0 {
		t.Fatalf("%d saved searches left behind", stale)
	}

	// Deleting takes everything in it along.
	s.CreateTag(ctx, bobScope, "family")
	b, _, err := s.CreateBookmark(ctx, bobScope, BookmarkInput{URL: "https://go.dev", Tags: []string{"family"}})
	if err != nil {
		t.Fatal(err)
	}
	s.CreateSavedSearch(ctx, bobScope, "Fam", "#family")
	if err := s.RenameSpace(ctx, bobScope, "Gone soon"); err != nil {
		t.Fatal(err)
	}
	// The owner picks an icon from the list, or the default again.
	for _, icon := range []string{"truck", ""} {
		if err := s.SetSpaceIcon(ctx, bobScope, icon); err != nil {
			t.Fatal(err)
		}
		if sp, _ := s.GetSpace(ctx, bobScope); sp.Icon != icon {
			t.Fatalf("icon %q, want %q", sp.Icon, icon)
		}
	}
	if err := s.SetSpaceIcon(ctx, bobScope, "../x"); !isValidationErr(err) {
		t.Fatalf("unknown icon: %v", err)
	}
	if err := s.SetSpaceIcon(ctx, personal(s, bob.ID), "car"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("icon on a personal space: %v", err)
	}
	if err := s.DeleteSpace(ctx, bobScope); err != nil {
		t.Fatal(err)
	}
	var left int
	s.r.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM bookmarks) + (SELECT count(*) FROM tags) + (SELECT count(*) FROM saved_searches)
		+ (SELECT count(*) FROM space_members WHERE space_id = ?) + (SELECT count(*) FROM bookmarks_fts WHERE rowid = ?)`,
		bobScope.SpaceID(), b.ID).Scan(&left)
	if left != 0 {
		t.Fatalf("%d rows left behind by the deleted space", left)
	}
	if _, err := s.Access(ctx, bob.ID, bobScope.SpaceID()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("access to a deleted space: %v", err)
	}
}

func TestDeleteUser(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	root, err := s.CreateUser(ctx, "root", "x", true)
	if err != nil {
		t.Fatal(err)
	}
	gone, member := newUser(t, s, "gone"), newUser(t, s, "member")
	team := newShared(t, s, map[int64]Role{gone.ID: RoleOwner, member.ID: RoleWrite})
	shared := newShared(t, s, map[int64]Role{gone.ID: RoleOwner, root.ID: RoleRead})
	if _, _, err := s.CreateBookmark(ctx, personal(s, gone.ID), BookmarkInput{URL: "https://private.example", Title: "private"}); err != nil {
		t.Fatal(err)
	}
	sc, _ := s.Access(ctx, gone.ID, team)
	if _, _, err := s.CreateBookmark(ctx, sc, BookmarkInput{URL: "https://team.example", Title: "team"}); err != nil {
		t.Fatal(err)
	}

	// The last active admin can't be deleted.
	if _, err := s.DeleteUser(ctx, root.ID, 0); !isValidationErr(err) {
		t.Fatalf("deleted the last admin: %v", err)
	}
	moved, err := s.DeleteUser(ctx, gone.ID, root.ID)
	if err != nil || moved != 2 {
		t.Fatalf("DeleteUser: %d %v", moved, err)
	}
	if _, err := s.UserByID(ctx, gone.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("user still there: %v", err)
	}
	// The admin owns both shared spaces now, joining the one they weren't in.
	for _, id := range []int64{team, shared} {
		if sc, err := s.Access(ctx, root.ID, id); err != nil || sc.Role() != RoleOwner {
			t.Fatalf("space %d: %v %v", id, sc.Role(), err)
		}
	}
	// The shared bookmark stays; the personal one is gone, from search too.
	sc, _ = s.Access(ctx, member.ID, team)
	if n, err := s.CountBookmarks(ctx, sc); err != nil || n.All != 1 {
		t.Fatalf("shared bookmarks: %d %v", n, err)
	}
	var left int
	s.r.QueryRowContext(ctx, "SELECT (SELECT count(*) FROM bookmarks) + (SELECT count(*) FROM bookmarks_fts WHERE bookmarks_fts MATCH 'private')").Scan(&left)
	if left != 1 {
		t.Fatalf("rows left: %d, want only the shared bookmark", left)
	}
	if _, err := s.DeleteUser(ctx, gone.ID, root.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted twice: %v", err)
	}
}
