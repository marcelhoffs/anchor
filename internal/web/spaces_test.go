package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
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
	if _, body, _ := bob.get(page); strings.Contains(body, "Add member") || strings.Contains(body, "Manage") || !strings.Contains(body, "Leave space") {
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
