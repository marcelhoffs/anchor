package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestAdmin(t *testing.T) {
	e := newEnv(t)
	e.user("alice")
	alice := e.login("alice")
	if code, _, _ := alice.get("/admin/"); code != http.StatusNotFound {
		t.Fatalf("non-admin reached /admin/: %d", code)
	}

	root := e.admin()
	alice.post("/bookmarks", url.Values{"url": {"https://secret.example"}, "title": {"Alice private"}})
	code, body, _ := root.get("/admin/users")
	if code != http.StatusOK || !strings.Contains(body, "alice") || strings.Contains(body, "Alice private") {
		t.Fatalf("admin users page: %d", code)
	}
	if code, body, _ := root.get("/admin/"); code != http.StatusOK || !strings.Contains(body, `name="signup"`) {
		t.Fatalf("admin landing page isn't security: %d", code)
	}

	// Users join through invites only.
	if strings.Contains(body, `action="/admin/users"`) {
		t.Fatal("add-user form still there")
	}
	if code, _, _ := root.post("/admin/users", url.Values{"username": {"carol"}}); code != http.StatusNotFound && code != http.StatusMethodNotAllowed {
		t.Fatalf("admin created a user directly: %d", code)
	}

	aliceID := itoa(e.userID("alice"))
	if code, _, _ := root.post("/admin/users/"+aliceID+"/disable", nil); code != http.StatusSeeOther {
		t.Fatalf("disable: %d", code)
	}
	if code, _, _ := alice.get("/"); code != http.StatusSeeOther {
		t.Fatalf("disabled user still logged in: %d", code)
	}
	if code, body, _ := root.post("/admin/users/"+itoa(e.userID("root"))+"/disable", nil); code != http.StatusUnprocessableEntity || !strings.Contains(body, "own account") {
		t.Fatalf("admin disabled themselves: %d", code)
	}

	// The audit log is gone.
	if code, _, _ := root.get("/admin/audit"); code != http.StatusNotFound {
		t.Fatalf("audit log page: %d, want 404", code)
	}
}

func TestInvitesAndSignupModes(t *testing.T) {
	e := newEnv(t)
	root := e.admin()
	anon := e.client()
	if code, _, _ := anon.get("/signup"); code != http.StatusNotFound {
		t.Fatalf("closed signup page: %d", code)
	}

	// While signup is closed there is nothing to invite anyone to.
	if _, body, _ := root.get("/admin/invites"); strings.Contains(body, "Create invite link") || !strings.Contains(body, "Sign-up is closed") {
		t.Fatal("invite form shown while signup is closed")
	}
	if code, _, loc := root.post("/admin/invites", url.Values{"note": {"stale tab"}}); code != http.StatusSeeOther || loc != "/admin/invites" {
		t.Fatalf("invite created while closed: %d %q", code, loc)
	}
	if invites, _ := e.store.ListInvites(context.Background()); len(invites) != 0 {
		t.Fatalf("invites while closed: %+v", invites)
	}

	root.post("/admin/settings", url.Values{"signup": {"invite"}})
	_, body, _ := root.post("/admin/invites", url.Values{"note": {"for dana"}})
	link := find(t, body, `value="(http[^"]+/signup\?invite=[^"]+)"`)
	if !strings.Contains(body, `data-copy="secret"`) || !strings.Contains(body, "Copy link") {
		t.Fatal("invite link has no copy button")
	}
	invite := link[strings.Index(link, "invite=")+7:]
	// Closing signup again stops the link working.
	root.post("/admin/settings", url.Values{"signup": {"closed"}})
	if code, _, _ := anon.get("/signup?invite=" + invite); code != http.StatusNotFound {
		t.Fatalf("invite worked while closed: %d", code)
	}

	root.post("/admin/settings", url.Values{"signup": {"invite"}})
	if code, _, _ := anon.get("/signup"); code != http.StatusNotFound {
		t.Fatalf("invite mode without invite: %d", code)
	}
	form := url.Values{"invite": {invite}, "username": {"dana"}, "password": {"dana-password"}, "confirm": {"dana-password"}}
	if code, _, loc := anon.post("/signup", form); code != http.StatusSeeOther || loc != "/" {
		t.Fatalf("signup with invite: %d %q", code, loc)
	}
	form.Set("username", "erin")
	form.Del("csrf") // post() filled in the first client's token
	if code, _, _ := e.client().post("/signup", form); code != http.StatusNotFound {
		t.Fatalf("invite reused: %d", code)
	}

	root.post("/admin/settings", url.Values{"signup": {"open"}})
	if _, body, _ := e.client().get("/login"); !strings.Contains(body, "Create an account") {
		t.Fatal("login page has no signup link in open mode")
	}
	open := url.Values{"username": {"frank"}, "password": {"frank-password"}, "confirm": {"frank-password"}}
	if code, _, _ := e.client().post("/signup", open); code != http.StatusSeeOther {
		t.Fatalf("open signup: %d", code)
	}
}

func TestRequire2FAPolicy(t *testing.T) {
	e := newEnv(t)
	e.user("alice")
	root := e.admin()
	root.post("/admin/settings", url.Values{"signup": {"closed"}, "require_2fa": {"1"}})

	alice := e.login("alice")
	if code, _, loc := alice.get("/"); code != http.StatusSeeOther || loc != "/settings/security?required=1" {
		t.Fatalf("user without 2FA not sent to set it up: %d %q", code, loc)
	}
	if code, body, _ := alice.get("/settings/security?required=1"); code != http.StatusOK || !strings.Contains(body, "requires two-factor") {
		t.Fatalf("security page: %d", code)
	}
	enableTOTP(t, alice)
	if code, _, _ := alice.get("/"); code != http.StatusOK {
		t.Fatalf("still blocked after enrolling: %d", code)
	}
	// The only factor can't be removed while the policy is on.
	if code, _, _ := alice.post("/settings/totp/disable", nil); code != http.StatusConflict {
		t.Fatalf("removed the only factor under the policy: %d", code)
	}
}

func TestAdminDeleteUser(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.user("alice")
	alice := e.login("alice")
	root := e.admin()
	sc, err := e.store.CreateSpace(ctx, e.userID("alice"), "Team")
	if err != nil {
		t.Fatal(err)
	}
	aliceID := itoa(e.userID("alice"))
	if _, body, _ := root.get("/admin/users"); !strings.Contains(body, `action="/admin/users/`+aliceID+`/delete"`) {
		t.Fatal("no delete confirmation on the users page")
	}
	if code, _, _ := alice.post("/admin/users/"+aliceID+"/delete", nil); code != http.StatusNotFound {
		t.Fatalf("non-admin deleted a user: %d", code)
	}
	if code, body, _ := root.post("/admin/users/"+itoa(e.userID("root"))+"/delete", nil); code != http.StatusUnprocessableEntity || !strings.Contains(body, "own account") {
		t.Fatalf("admin deleted themselves: %d", code)
	}
	code, _, loc := root.post("/admin/users/"+aliceID+"/delete", nil)
	if code != http.StatusSeeOther || loc != "/admin/users?done=delete-spaces" {
		t.Fatalf("delete: %d %q", code, loc)
	}
	if _, body, _ := root.get(loc); !strings.Contains(body, "now yours") || strings.Contains(body, ">alice<") {
		t.Fatal("users page after delete")
	}
	if code, _, _ := alice.get("/"); code != http.StatusSeeOther {
		t.Fatalf("deleted user still signed in: %d", code)
	}
	if got, err := e.store.Access(ctx, e.userID("root"), sc.SpaceID()); err != nil || !got.CanWrite() {
		t.Fatalf("admin didn't get the space: %v", err)
	}
}
