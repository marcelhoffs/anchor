package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/marcelhoffs/anchor/internal/config"
)

func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{
		"/":                  "/",
		"/tags?x=1#y":        "/tags?x=1#y",
		"":                   "/",
		"https://evil.com":   "/",
		"//evil.com":         "/",
		"/\\evil.com":        "/",
		"/\t/evil.com":       "/",
		"/\n/evil.com":       "/",
		"/x\\y":              "/",
		"javascript:alert()": "/",
		"/%2F%2Fevil.com":    "/%2F%2Fevil.com", // an encoded path, still on this site
	} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSecurityHeadersAndCookies(t *testing.T) {
	e := newEnv(t)
	res, err := http.Get(e.srv.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	for _, h := range []string{"Cross-Origin-Opener-Policy", "Cross-Origin-Resource-Policy", "Permissions-Policy"} {
		if res.Header.Get(h) == "" {
			t.Errorf("missing %s", h)
		}
	}
	if res.Header.Get("Strict-Transport-Security") != "" {
		t.Error("HSTS sent over plain http")
	}

	// Over https, cookies get the __Host- prefix and HSTS is sent.
	base, _ := url.Parse("https://anchor.example")
	s, err := New(&config.Config{BaseURL: base, SecretKey: make([]byte, 32)}, e.store,
		slog.New(slog.NewTextHandler(io.Discard, nil)), e.worker, e.fetcher)
	if err != nil {
		t.Fatal(err)
	}
	if s.sessionCookie != "__Host-anchor_session" || s.csrfCookie != "__Host-anchor_csrf" {
		t.Fatalf("cookies = %q, %q", s.sessionCookie, s.csrfCookie)
	}
	req, _ := http.NewRequest("GET", "https://anchor.example/healthz", nil)
	w := &headerRecorder{h: http.Header{}}
	s.Handler().ServeHTTP(w, req)
	if !strings.HasPrefix(w.h.Get("Strict-Transport-Security"), "max-age=") {
		t.Fatal("no HSTS over https")
	}
}

type headerRecorder struct{ h http.Header }

func (r *headerRecorder) Header() http.Header         { return r.h }
func (r *headerRecorder) Write(b []byte) (int, error) { return len(b), nil }
func (r *headerRecorder) WriteHeader(int)             {}

func TestChangePasswordIsRateLimited(t *testing.T) {
	e := newEnv(t)
	e.user("alice")
	c := e.login("alice")
	for range 10 {
		c.post("/settings/password", url.Values{"current": {"wrong"}, "password": {"new-password-1"}, "confirm": {"new-password-1"}})
	}
	code, body, _ := c.post("/settings/password", url.Values{"current": {"alice-password"}, "password": {"new-password-1"}, "confirm": {"new-password-1"}})
	if code != http.StatusTooManyRequests || !strings.Contains(body, "Too many failed attempts") {
		t.Fatalf("after 10 wrong passwords: %d", code)
	}
	// Validation messages read as sentences.
	if !strings.Contains(sentence(errString("password must be at least 10 characters")), "Password must be at least 10 characters.") {
		t.Fatal("sentence")
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func TestDestructiveActionsAreConfirmed(t *testing.T) {
	e := newEnv(t)
	root := e.admin()
	e.user("alice")
	ctx := context.Background()
	uid := e.userID("root")
	tag, _ := e.store.CreateTag(ctx, personal(e.store, uid), "go")
	bundle, _ := e.store.CreateBundle(ctx, personal(e.store, uid), "Go", "#go")
	tok, _ := e.store.CreateAPIToken(ctx, uid, "script", []byte("hash"), []string{"read"}, time.Time{})
	root.post("/admin/settings", url.Values{"signup": {"invite"}})
	root.post("/admin/invites", url.Values{"note": {"friend"}})
	totpUser := e.userID("alice")
	e.store.StartTOTP(ctx, totpUser, []byte("sealed"))
	e.store.EnableTOTP(ctx, totpUser, 1)
	alice := itoa(totpUser)
	space, _ := e.store.CreateSpace(ctx, uid, "Team")
	e.store.AddMember(ctx, space, "alice", "read")
	sp := spaceURL(space.SpaceID())

	// confirmed reports whether action is only posted from inside the dialog
	// with this id, which a button on the page opens.
	confirmed := func(body, id, action string) bool {
		start := strings.Index(body, `id="`+id+`"`)
		if !strings.Contains(body, `popovertarget="`+id+`"`) || start < 0 {
			return false
		}
		end := strings.Index(body[start:], "</form>")
		return end > 0 && strings.Contains(body[start:start+end], `action="`+action+`"`) &&
			strings.Count(body, `action="`+action+`"`) == 1
	}
	invites, _ := e.store.ListInvites(ctx)
	for _, c := range []struct{ page, id, action string }{
		{"/tags", "delete-tag-" + itoa(tag.ID), "/tags/" + itoa(tag.ID) + "/delete"},
		{"/bundles", "delete-bundle-" + itoa(bundle.ID), "/bundles/" + itoa(bundle.ID) + "/delete"},
		{"/settings/tokens", "revoke-token-" + itoa(tok.ID), "/settings/tokens/" + itoa(tok.ID) + "/delete"},
		{"/admin/invites", "revoke-invite-" + itoa(invites[0].ID), "/admin/invites/" + itoa(invites[0].ID) + "/delete"},
		{"/admin/", "disable-" + alice, "/admin/users/" + alice + "/disable"},
		{"/admin/", "reset-password-" + alice, "/admin/users/" + alice + "/reset-password"},
		{"/admin/", "reset-2fa-" + alice, "/admin/users/" + alice + "/reset-2fa"},
		{sp, "remove-" + alice, sp + "/members/" + alice + "/remove"},
		{sp, "owner-" + alice, sp + "/members/" + alice + "/owner"},
		{sp, "delete-space", sp + "/delete"},
		{"/admin/spaces", "space-delete-" + itoa(space.SpaceID()), "/admin/spaces/" + itoa(space.SpaceID()) + "/delete"},
	} {
		_, body, _ := root.get(c.page)
		if !confirmed(body, c.id, c.action) {
			t.Errorf("%s: %s isn't behind a confirmation", c.page, c.action)
		}
	}
	if _, body, _ := root.get("/tags"); !strings.Contains(body, "is removed from 0 bookmarks.") {
		t.Error("tag confirmation doesn't count bookmarks")
	}
}

func TestStaticFilesAreCompressed(t *testing.T) {
	e := newEnv(t)
	req, _ := http.NewRequest("GET", e.srv.URL+assetURL("style.css"), nil)
	req.Header.Set("Accept-Encoding", "gzip")
	res, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.Header.Get("Content-Encoding") != "gzip" || !strings.Contains(res.Header.Get("Cache-Control"), "immutable") {
		t.Fatalf("style.css: encoding %q, cache %q", res.Header.Get("Content-Encoding"), res.Header.Get("Cache-Control"))
	}
	// Pages are never compressed (BREACH).
	req, _ = http.NewRequest("GET", e.srv.URL+"/login", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	if res, err = http.DefaultTransport.RoundTrip(req); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.Header.Get("Content-Encoding") != "" {
		t.Fatal("HTML page compressed")
	}
}

// Confirmation dialogs live inside table cells that right-align and don't
// wrap; the dialog must undo both or its text is cut off.
func TestConfirmDialogResetsCellLayout(t *testing.T) {
	css, _ := assets.ReadFile("static/style.css")
	start := strings.Index(string(css), ".confirm {")
	end := strings.Index(string(css)[start:], "}")
	rule := string(css)[start : start+end]
	if !strings.Contains(rule, "text-align: left") || !strings.Contains(rule, "white-space: normal") {
		t.Fatalf(".confirm doesn't reset its table cell's layout: %s", rule)
	}
}
