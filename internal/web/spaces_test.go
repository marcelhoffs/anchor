package web

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/marcelhoffs/anchor/internal/search"
	"github.com/marcelhoffs/anchor/internal/store"
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
	if _, body, _ := alice.get(loc); !strings.Contains(body, "<h1>Family</h1>") || !strings.Contains(body, "Add member") ||
		!strings.Contains(body, `name="icon" value="" aria-label="Default" checked`) {
		t.Fatal("owner's space page")
	}

	// The owner picks the space's icon; it shows in the space menu and the list.
	if code, _, loc := alice.post(page+"/icon", url.Values{"icon": {"truck"}}); code != http.StatusSeeOther || !strings.HasSuffix(loc, "?done=icon") {
		t.Fatalf("set icon: %d %q", code, loc)
	}
	if _, body, _ := alice.get(page); !strings.Contains(body, `value="truck" aria-label="Lorry" checked`) {
		t.Fatal("icon not picked on the space page")
	}
	_, list, _ := alice.get("/spaces")
	_, home, _ := alice.get("/") // the space menu isn't on the spaces pages
	if !strings.Contains(list, `<a href="`+page+`"><svg class="icon sm" aria-hidden="true" focusable="false"><use href="#i-truck">`) ||
		!strings.Contains(home, `<use href="#i-truck"></use></svg><span class="space-option">Family`) {
		t.Fatal("icon not shown in the spaces list or the space menu")
	}
	if code, _, _ := alice.post(page+"/icon", url.Values{"icon": {"pizza"}}); code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown icon: %d", code)
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
		page + "/icon":                           {"icon": {"car"}},
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

	if code, _, _ := alice.post(page+"/members/"+bobID+"/owner", nil); code != http.StatusSeeOther {
		t.Fatalf("hand-over: %d", code)
	}
	if code, _, loc := alice.post(page+"/leave", nil); code != http.StatusSeeOther || loc != "/spaces?done=left" {
		t.Fatalf("leave: %d %q", code, loc)
	}
	if code, _, _ := alice.get(page); code != http.StatusNotFound {
		t.Fatal("still in the space after leaving")
	}
	if code, _, loc := bob.post(page+"/delete", nil); code != http.StatusSeeOther || loc != "/spaces?done=deleted" {
		t.Fatalf("delete: %d %q", code, loc)
	}
	if code, _, _ := bob.get(page); code != http.StatusNotFound {
		t.Fatal("deleted space still there")
	}

	// Admins can keep space creation to themselves.
	root.post("/admin/spaces", url.Values{"space_creation": {"admins"}})
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
	for _, control := range []string{`/bookmarks/bulk`, `class="bm-check"`, `class="bulk-bar"`, `/bookmarks/` + bid + `/edit`} {
		if strings.Contains(body, control) {
			t.Errorf("reader's list has %s", control)
		}
	}
	if code, _, _ := bob.post("/bookmarks/bulk", url.Values{"from": {teamID}, "action": {"favourite"}, "id": {bid}}); code != http.StatusForbidden {
		t.Fatalf("reader starred a bookmark from the toolbar: %d", code)
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

func TestAPISpaces(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.user("alice")
	e.user("bob")
	e.user("carol")
	alice := apiClient{t, e.srv.URL, apiToken(t, e.login("alice"), "write")}
	bob := apiClient{t, e.srv.URL, apiToken(t, e.login("bob"), "write")}
	carol := apiClient{t, e.srv.URL, apiToken(t, e.login("carol"), "write")}
	team, _ := e.store.CreateSpace(ctx, e.userID("alice"), "Team")
	e.store.AddMember(ctx, team, "bob", "read")
	q := "?space=" + itoa(team.SpaceID())

	code, out := bob.do("GET", "/spaces", nil)
	list, _ := out["results"].([]any)
	if code != 200 || len(list) != 2 {
		t.Fatalf("spaces: %d %v", code, out)
	}
	if first := list[0].(map[string]any); first["personal"] != true || first["name"] != "Personal" {
		t.Fatalf("personal space not first: %v", first)
	}
	if second := list[1].(map[string]any); second["name"] != "Team" || second["role"] != "read" || second["icon"] != "" {
		t.Fatalf("shared space: %v", second)
	}

	// Writes and reads go to the space named in ?space=, nowhere else.
	if code, out := alice.do("POST", "/tags"+q, map[string]any{"name": "team"}); code != 201 {
		t.Fatalf("tag in space: %d %v", code, out)
	}
	code, out = alice.do("POST", "/bookmarks"+q, map[string]any{"url": "https://go.dev", "tags": []string{"team"}})
	if code != 201 {
		t.Fatalf("bookmark in space: %d %v", code, out)
	}
	id := strconv.FormatInt(int64(out["id"].(float64)), 10)
	if code, out := alice.do("GET", "/bookmarks", nil); code != 200 || len(out["results"].([]any)) != 0 {
		t.Fatalf("space's bookmark in the personal list: %v", out)
	}
	if code, _ := alice.do("GET", "/bookmarks/"+id, nil); code != 404 {
		t.Fatalf("space's bookmark found in the personal space: %d", code)
	}
	if code, out := bob.do("GET", "/bookmarks"+q, nil); code != 200 || len(out["results"].([]any)) != 1 {
		t.Fatalf("reader's list: %d %v", code, out)
	}

	// A write token can't do more than the member's role allows.
	if code, out := bob.do("PATCH", "/bookmarks/"+id+q, map[string]any{"title": "x"}); code != 403 || out["status"] != float64(403) {
		t.Fatalf("reader changed a bookmark: %d %v", code, out)
	}
	if code, _ := bob.do("POST", "/bookmarks"+q, map[string]any{"url": "https://x.org"}); code != 403 {
		t.Fatalf("reader added a bookmark: %d", code)
	}
	// Outsiders and bad ids get nothing.
	if code, _ := carol.do("GET", "/bookmarks"+q, nil); code != 404 {
		t.Fatalf("outsider listed the space: %d", code)
	}
	if code, _ := alice.do("GET", "/bookmarks?space=abc", nil); code != 400 {
		t.Fatalf("bad space id: %d", code)
	}
}

func TestAdminSpaces(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	root := e.admin()
	e.user("alice")
	e.user("bob")
	alice := e.login("alice")
	team, _ := e.store.CreateSpace(ctx, e.userID("alice"), "Team")
	e.store.AddMember(ctx, team, "bob", "read")
	e.store.CreateBookmark(ctx, team, store.BookmarkInput{URL: "https://secret.example/plans", Title: "Secret plans"})
	page := "/admin/spaces"
	sp := itoa(team.SpaceID())

	// Admins see who's in a space and its size, never the bookmarks.
	if code, _, _ := alice.get(page); code != http.StatusNotFound {
		t.Fatalf("non-admin opened the admin spaces page: %d", code)
	}
	_, body, _ := root.get(page)
	if !strings.Contains(body, "<strong>Team</strong>") || !strings.Contains(body, "alice") {
		t.Fatal("space not listed for the admin")
	}
	if strings.Contains(body, "Secret plans") || strings.Contains(body, "secret.example") {
		t.Fatal("the admin page shows a space's bookmarks")
	}

	// The owner is disabled: an admin hands the space to bob.
	e.store.SetActive(ctx, e.userID("alice"), false)
	if _, body, _ := root.get(page); !strings.Contains(body, "Disabled") {
		t.Fatal("disabled owner not flagged")
	}
	if code, _, _ := root.post(page+"/"+sp+"/owner", url.Values{"user_id": {itoa(e.userID("root"))}}); code != http.StatusNotFound {
		t.Fatalf("space handed to a non-member: %d", code)
	}
	if code, _, loc := root.post(page+"/"+sp+"/owner", url.Values{"user_id": {itoa(e.userID("bob"))}}); code != http.StatusSeeOther || !strings.Contains(loc, "done=space-owner") {
		t.Fatalf("hand-over: %d %q", code, loc)
	}
	if sc, _ := e.store.Access(ctx, e.userID("bob"), team.SpaceID()); sc.Role() != store.RoleOwner {
		t.Fatalf("bob's role: %s", sc.Role())
	}
	if sc, _ := e.store.Access(ctx, e.userID("alice"), team.SpaceID()); sc.Role() != store.RoleWrite {
		t.Fatalf("old owner's role: %s", sc.Role())
	}
	// Personal spaces aren't the admin's to delete.
	if code, _, _ := root.post(page+"/"+itoa(personal(e.store, e.userID("bob")).SpaceID())+"/delete", nil); code != http.StatusNotFound {
		t.Fatalf("admin deleted a personal space: %d", code)
	}
	if code, _, _ := root.post(page+"/"+sp+"/delete", nil); code != http.StatusSeeOther {
		t.Fatalf("delete: %d", code)
	}
	if list, _ := e.store.AdminListSpaces(ctx); len(list) != 0 {
		t.Fatalf("space still there: %+v", list)
	}
}

// Every space icon can be drawn and has a name for screen readers.
func TestSpaceIcons(t *testing.T) {
	for _, name := range store.SpaceIcons {
		if _, ok := spaceIconLabels[name]; !ok {
			t.Errorf("no label for %q", name)
		}
		if !strings.Contains(string(iconSprite), `id="i-`+name+`"`) {
			t.Errorf("no symbol for %q", name)
		}
	}
}

// Settings, admin and spaces pages have a way back instead of the switcher
// and Add bookmark; import & export picks its space on the page.
func TestManagePagesTopBar(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.user("alice")
	alice := e.login("alice")
	team, _ := e.store.CreateSpace(ctx, e.userID("alice"), "Team")
	teamID := itoa(team.SpaceID())

	for _, path := range []string{"/settings", "/settings/data", "/spaces", "/spaces/" + teamID} {
		_, body, _ := alice.get(path)
		top := body[strings.Index(body, `<header class="top">`):strings.Index(body, "</header>")]
		if !strings.Contains(top, "top-back") || strings.Contains(top, "space-menu") || strings.Contains(top, `href="/bookmarks/new"`) {
			t.Fatalf("%s: top bar isn't the manage one", path)
		}
	}
	if _, body, _ := alice.get("/"); strings.Contains(body, "top-back") || !strings.Contains(body, "space-menu") {
		t.Fatal("the list has the manage top bar")
	}

	_, body, _ := alice.get("/settings/data")
	if !strings.Contains(body, `<option value="`+teamID+`">Team</option>`) {
		t.Fatal("import & export has no space picker")
	}
	if code, _, loc := alice.post("/space", url.Values{"space": {teamID}, "next": {"/settings/data"}}); code != http.StatusSeeOther || loc != "/settings/data" {
		t.Fatalf("switch from import & export: %d %q", code, loc)
	}
	if _, body, _ := alice.get("/settings/data"); !strings.Contains(body, `<option value="`+teamID+`" selected>Team</option>`) {
		t.Fatal("picker doesn't show the space switched to")
	}
	// Only that page: anything else goes back to the list.
	if _, _, loc := alice.post("/space", url.Values{"next": {"https://evil.example/"}}); loc != "/" {
		t.Fatalf("next followed to %q", loc)
	}
}
