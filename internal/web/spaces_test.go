package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/marcelhoffs/anchor/internal/search"
)

func TestSpacePages(t *testing.T) {
	e := newEnv(t)
	root := e.admin()
	e.user("alice")
	e.user("bob")
	alice, bob := e.login("alice"), e.login("bob")

	if _, body, _ := alice.get("/spaces"); !strings.Contains(body, "No shared spaces yet") || !strings.Contains(body, "Create space") {
		t.Fatal("empty spaces page")
	}
	code, _, loc := alice.post("/spaces", url.Values{"name": {"Family"}})
	if code != http.StatusSeeOther || !strings.HasSuffix(loc, "?done=created") {
		t.Fatalf("create: %d %q", code, loc)
	}
	page := strings.TrimSuffix(loc, "?done=created")
	if _, body, _ := alice.get(loc); !strings.Contains(body, "<h1>Family</h1>") || !strings.Contains(body, "Add member") {
		t.Fatal("owner's space page")
	}

	// Outsiders get nothing, not even that the space exists.
	if code, _, _ := bob.get(page); code != http.StatusNotFound {
		t.Fatalf("outsider sees the space: %d", code)
	}
	if code, _, _ := bob.post(page+"/members", url.Values{"username": {"bob"}, "role": {"write"}}); code != http.StatusNotFound {
		t.Fatalf("outsider added himself: %d", code)
	}
	personalID := personal(e.store, e.userID("alice")).SpaceID()
	if code, _, _ := alice.get(spaceURL(personalID)); code != http.StatusNotFound {
		t.Fatalf("personal space shown as a shared one: %d", code)
	}

	if code, body, _ := alice.post(page+"/members", url.Values{"username": {"nobody"}, "role": {"read"}}); code != http.StatusUnprocessableEntity || !strings.Contains(body, "no user called") {
		t.Fatalf("unknown member: %d", code)
	}
	if code, _, _ := alice.post(page+"/members", url.Values{"username": {"bob"}, "role": {"read"}}); code != http.StatusSeeOther {
		t.Fatalf("add member: %d", code)
	}

	// A reader sees the space and its members, but manages nothing.
	if _, body, _ := bob.get("/spaces"); !strings.Contains(body, "Family") || !strings.Contains(body, "Read only") {
		t.Fatal("member's spaces list")
	}
	if _, body, _ := bob.get(page); strings.Contains(body, "Add member") || strings.Contains(body, "member-menu-") || !strings.Contains(body, "Leave space") {
		t.Fatal("reader's space page offers management")
	}
	aliceID := itoa(e.userID("alice"))
	for path, form := range map[string]url.Values{
		page + "/rename":                         {"name": {"Mine"}},
		page + "/members":                        {"username": {"root"}, "role": {"write"}},
		page + "/members/" + aliceID + "/remove": nil,
	} {
		if code, _, _ := bob.post(path, form); code != http.StatusForbidden {
			t.Errorf("reader POST %s: %d", path, code)
		}
	}

	bobID := itoa(e.userID("bob"))
	if code, _, _ := alice.post(page+"/members/"+bobID+"/write", nil); code != http.StatusSeeOther {
		t.Fatalf("change role: %d", code)
	}
	if _, body, _ := bob.get("/spaces"); !strings.Contains(body, "Can edit") {
		t.Fatal("role change not shown")
	}

	// Handing over and deleting need a recent sign-in check.
	e.expireReauth()
	if code, _, loc := alice.post(page+"/members/"+bobID+"/owner", nil); code != http.StatusSeeOther || !strings.HasPrefix(loc, "/reauth") {
		t.Fatalf("stale hand-over: %d %q", code, loc)
	}
	alice.post("/reauth/password", url.Values{"password": {"alice-password"}})
	if code, _, _ := alice.post(page+"/members/"+bobID+"/owner", nil); code != http.StatusSeeOther {
		t.Fatalf("hand-over: %d", code)
	}
	if code, _, loc := alice.post(page+"/leave", nil); code != http.StatusSeeOther || loc != "/spaces?done=left" {
		t.Fatalf("leave: %d %q", code, loc)
	}
	if code, _, _ := alice.get(page); code != http.StatusNotFound {
		t.Fatal("still in the space after leaving")
	}
	bob.post("/reauth/password", url.Values{"password": {"bob-password"}})
	if code, _, loc := bob.post(page+"/delete", nil); code != http.StatusSeeOther || loc != "/spaces?done=deleted" {
		t.Fatalf("delete: %d %q", code, loc)
	}
	if code, _, _ := bob.get(page); code != http.StatusNotFound {
		t.Fatal("deleted space still there")
	}

	// Admins can keep space creation to themselves.
	root.post("/reauth/password", url.Values{"password": {"root-password"}})
	root.post("/admin/settings", url.Values{"signup": {"closed"}, "space_creation": {"admins"}})
	if _, body, _ := bob.get("/spaces"); strings.Contains(body, "Create space") {
		t.Fatal("create form shown to a non-admin")
	}
	if code, _, _ := bob.post("/spaces", url.Values{"name": {"Mine"}}); code != http.StatusForbidden {
		t.Fatalf("non-admin created a space: %d", code)
	}
	if code, _, _ := root.post("/spaces", url.Values{"name": {"Team"}}); code != http.StatusSeeOther {
		t.Fatalf("admin can't create a space: %d", code)
	}
	if list, _ := e.store.ListSpaces(context.Background(), e.userID("root")); len(list) != 1 {
		t.Fatalf("admin's spaces: %+v", list)
	}
}

func TestSpaceSwitcher(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.user("alice")
	e.user("bob")
	alice, bob := e.login("alice"), e.login("bob")

	if _, body, _ := alice.get("/"); strings.Contains(body, "space-menu") {
		t.Fatal("switcher shown without shared spaces")
	}
	team, _ := e.store.CreateSpace(ctx, e.userID("alice"), "Team")
	e.store.AddMember(ctx, team, "bob", "read")
	teamID := itoa(team.SpaceID())
	if _, body, _ := alice.get("/"); !strings.Contains(body, "space-menu") || !strings.Contains(body, ">Team<") {
		t.Fatal("no switcher for a member of a shared space")
	}

	// Switching shows the space; new bookmarks go into it.
	if code, _, loc := alice.post("/space", url.Values{"space": {teamID}}); code != http.StatusSeeOther || loc != "/" {
		t.Fatalf("switch: %d %q", code, loc)
	}
	if _, body, _ := alice.get("/bookmarks/new"); !strings.Contains(body, "Saving to <strong>Team</strong>") {
		t.Fatal("add form doesn't say where it saves")
	}
	if code, _, _ := alice.post("/bookmarks", url.Values{"url": {"https://go.dev"}, "space": {teamID}}); code != http.StatusSeeOther {
		t.Fatalf("add to the space: %d", code)
	}
	if _, body, _ := alice.get("/"); !strings.Contains(body, "go.dev") {
		t.Fatal("space's bookmark not listed")
	}
	alice.post("/space", url.Values{"space": {""}})
	if _, body, _ := alice.get("/"); strings.Contains(body, "go.dev") {
		t.Fatal("space's bookmark listed in the personal space")
	}
	b, _ := e.store.FindBookmarkByURL(ctx, team, "https://go.dev")
	bid := itoa(b.ID)

	// A reader sees the space's bookmarks without the controls to change them.
	bob.post("/space", url.Values{"space": {teamID}})
	_, body, _ := bob.get("/")
	if !strings.Contains(body, "go.dev") {
		t.Fatal("reader doesn't see the space's bookmarks")
	}
	for _, control := range []string{`href="/bookmarks/new"`, `action="/bookmarks/` + bid + `/flag"`, `/bookmarks/` + bid + `/edit`, `delete-` + bid} {
		if strings.Contains(body, control) {
			t.Errorf("reader's list has %s", control)
		}
	}
	if code, _, _ := bob.post("/bookmarks/"+bid+"/flag", url.Values{"flag": {"favourite"}, "on": {"1"}}); code != http.StatusForbidden {
		t.Fatalf("reader starred a bookmark: %d", code)
	}
	if code, _, _ := bob.get("/bookmarks/" + bid + "/edit"); code != http.StatusForbidden {
		t.Fatalf("reader opened the edit form: %d", code)
	}
	if _, body, _ := bob.get("/tags"); strings.Contains(body, `action="/tags"`) {
		t.Fatal("reader can create tags")
	}
	if _, body, _ := bob.get("/settings/data"); strings.Contains(body, `action="/settings/import"`) {
		t.Fatal("reader can import")
	}
	// Adding while reading a space saves to the personal one, never the space.
	if _, body, _ := bob.get("/bookmarks/new"); !strings.Contains(body, "Saving to <strong>Personal</strong>") {
		t.Fatal("reader's add form doesn't fall back to the personal space")
	}
	if code, _, _ := bob.post("/bookmarks", url.Values{"url": {"https://x.org"}, "space": {teamID}}); code != http.StatusForbidden {
		t.Fatalf("reader added to the space: %d", code)
	}
	if code, _, _ := bob.post("/bookmarks", url.Values{"url": {"https://x.org"}}); code != http.StatusSeeOther {
		t.Fatalf("reader's personal add: %d", code)
	}
	if list, _, _ := e.store.Search(ctx, personal(e.store, e.userID("bob")), search.Query{}, "", 10, 0); len(list) != 1 {
		t.Fatalf("bob's personal bookmarks: %+v", list)
	}

	// Removed from the space, the session falls back to the personal one.
	e.store.RemoveMember(ctx, team, e.userID("bob"))
	if _, body, _ := bob.get("/"); strings.Contains(body, "go.dev") || strings.Contains(body, "space-menu") || !strings.Contains(body, "x.org") {
		t.Fatal("removed member still sees the space")
	}
	if code, _, _ := bob.post("/space", url.Values{"space": {teamID}}); code != http.StatusNotFound {
		t.Fatalf("switched to a space after removal: %d", code)
	}
}

func TestMemberSuggestions(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for _, n := range []string{"alice", "bob", "albert", "malia", "alfred"} {
		e.user(n)
	}
	alice, bob := e.login("alice"), e.login("bob")
	sp, _ := e.store.CreateSpace(ctx, e.userID("alice"), "Team")
	e.store.AddMember(ctx, sp, "alfred", "read")
	path := spaceURL(sp.SpaceID()) + "/members/suggest?q="

	// Names containing the text, those starting with it first; members left out.
	if code, body, _ := alice.get(path + "AL"); code != http.StatusOK || strings.TrimSpace(body) != `["albert","malia"]` {
		t.Fatalf("suggestions: %d %s", code, body)
	}
	if _, body, _ := alice.get(path); strings.TrimSpace(body) != `[]` {
		t.Fatalf("empty query: %s", body)
	}
	// Only the owner gets them; outsiders don't learn the space exists.
	e.store.AddMember(ctx, sp, "bob", "write")
	if code, _, _ := bob.get(path + "al"); code != http.StatusForbidden {
		t.Fatalf("editor got suggestions: %d", code)
	}
	if code, _, _ := e.login("albert").get(path + "al"); code != http.StatusNotFound {
		t.Fatalf("outsider got suggestions: %d", code)
	}
}
