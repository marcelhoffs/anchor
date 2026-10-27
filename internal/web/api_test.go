package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/marcelhoffs/anchor/internal/ratelimit"
)

// apiToken creates a token through the web UI and returns it.
func apiToken(t *testing.T, c *client, scope string) string {
	t.Helper()
	code, body, _ := c.post("/settings/tokens", url.Values{"name": {"test " + scope}, "scope": {scope}, "expires": {"30"}})
	if code != http.StatusOK {
		t.Fatalf("create token: %d", code)
	}
	if !strings.Contains(body, `data-copy="new-token-value"`) || !strings.Contains(body, "copy.js") {
		t.Fatal("new token has no copy button")
	}
	return find(t, body, `value="(`+apiTokenPrefix+`[A-Za-z0-9_-]+)"`)
}

type apiClient struct {
	t     *testing.T
	base  string
	token string
}

func (a apiClient) do(method, path string, body any) (int, map[string]any) {
	a.t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, a.base+"/api/v1"+path, r)
	if a.token != "" {
		req.Header.Set("Authorization", "Bearer "+a.token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func TestAPI(t *testing.T) {
	e := newEnv(t)
	e.user("alice")
	e.user("bob")
	e.tags("alice", "go")
	alice := apiClient{t, e.srv.URL, apiToken(t, e.login("alice"), "write")}
	reader := apiClient{t, e.srv.URL, apiToken(t, e.login("alice"), "read")}
	bob := apiClient{t, e.srv.URL, apiToken(t, e.login("bob"), "write")}

	if code, out := (apiClient{t, e.srv.URL, ""}).do("GET", "/bookmarks", nil); code != 401 || out["status"] != float64(401) {
		t.Fatalf("no token: %d %v", code, out)
	}
	if code, _ := (apiClient{t, e.srv.URL, "brw_nope"}).do("GET", "/bookmarks", nil); code != 401 {
		t.Fatalf("bad token: %d", code)
	}

	code, b := alice.do("POST", "/bookmarks", map[string]any{"url": "https://go.dev", "title": "Go", "tags": []string{"go"}})
	if code != 201 || b["title"] != "Go" {
		t.Fatalf("create: %d %v", code, b)
	}
	id := int(b["id"].(float64))
	if code, _ := alice.do("POST", "/bookmarks", map[string]any{"url": "go.dev/"}); code != 200 {
		t.Fatalf("duplicate create: %d, want 200", code)
	}
	if code, _ := reader.do("POST", "/bookmarks", map[string]any{"url": "https://x.org"}); code != 403 {
		t.Fatalf("read-only token wrote: %d", code)
	}
	if code, _ := alice.do("POST", "/bookmarks", map[string]any{"url": "javascript:alert(1)"}); code != 422 {
		t.Fatalf("bad URL: %d", code)
	}
	if code, _ := alice.do("POST", "/bookmarks", map[string]any{"url": "https://x.org", "bogus": 1}); code != 400 {
		t.Fatalf("unknown field: %d", code)
	}

	if code, out := alice.do("GET", "/bookmarks/check?url=https%3A%2F%2Fgo.dev%2F", nil); code != 200 || out["bookmark"] == nil {
		t.Fatalf("check: %d %v", code, out)
	}
	if code, out := bob.do("GET", "/bookmarks/check?url=https%3A%2F%2Fgo.dev%2F", nil); code != 200 || out["bookmark"] != nil {
		t.Fatalf("bob sees alice's bookmark via check: %v", out)
	}
	path := "/bookmarks/" + itoa(int64(id))
	for _, m := range []string{"GET", "PATCH", "DELETE"} {
		if code, _ := bob.do(m, path, map[string]any{}); code != 404 {
			t.Errorf("bob %s alice's bookmark: %d", m, code)
		}
	}

	code, b = alice.do("PATCH", path, map[string]any{"title": "Go docs", "is_favourite": true})
	if code != 200 || b["title"] != "Go docs" || b["is_favourite"] != true || b["url"] != "https://go.dev/" {
		t.Fatalf("patch: %d %v", code, b)
	}
	if code, b := alice.do("POST", path+"/archive", nil); code != 200 || b["is_archived"] != true {
		t.Fatalf("archive: %d %v", code, b)
	}
	if _, out := alice.do("GET", "/bookmarks?q=docs", nil); len(out["results"].([]any)) != 0 {
		t.Fatal("archived bookmark in the default list")
	}
	if _, out := alice.do("GET", "/bookmarks?q=docs&archived=true", nil); len(out["results"].([]any)) != 1 {
		t.Fatal("archived bookmark missing with archived=true")
	}

	// Pagination.
	for i := range 3 {
		alice.do("POST", "/bookmarks", map[string]any{"url": "https://page.example/" + itoa(int64(i))})
	}
	_, page1 := alice.do("GET", "/bookmarks?limit=2", nil)
	next, _ := page1["next"].(string)
	if len(page1["results"].([]any)) != 2 || next == "" {
		t.Fatalf("page 1: %v", page1)
	}
	_, page2 := alice.do("GET", "/bookmarks?limit=2&cursor="+next, nil)
	if len(page2["results"].([]any)) != 1 || page2["next"] != nil {
		t.Fatalf("page 2: %v", page2)
	}

	_, tags := alice.do("GET", "/tags", nil)
	tagID := int64(tags["results"].([]any)[0].(map[string]any)["id"].(float64))
	if code, _ := bob.do("DELETE", "/tags/"+itoa(tagID), nil); code != 404 {
		t.Fatalf("bob deleted alice's tag: %d", code)
	}
	if code, _ := alice.do("PATCH", "/tags/"+itoa(tagID), map[string]any{"name": "golang"}); code != 204 {
		t.Fatalf("rename tag: %d", code)
	}

	if code, out := alice.do("GET", "/user", nil); code != 200 || out["username"] != "alice" {
		t.Fatalf("user: %d %v", code, out)
	}
	if code, _ := alice.do("DELETE", path, nil); code != 204 {
		t.Fatalf("delete: %d", code)
	}

	// Disabling the account kills its tokens.
	e.store.SetActive(context.Background(), e.userID("alice"), false)
	if code, _ := alice.do("GET", "/bookmarks", nil); code != 401 {
		t.Fatalf("disabled user's token: %d", code)
	}
}

func TestAPIRateLimit(t *testing.T) {
	e := newEnv(t)
	e.user("alice")
	e.server.apiToken = ratelimit.New(3, time.Minute)
	a := apiClient{t, e.srv.URL, apiToken(t, e.login("alice"), "read")}
	for range 3 {
		a.do("GET", "/user", nil)
	}
	if code, _ := a.do("GET", "/user", nil); code != 429 {
		t.Fatalf("4th request: %d, want 429", code)
	}
}

func TestAPITagWithSpace(t *testing.T) {
	e := newEnv(t)
	e.user("alice")
	e.tags("alice", "home lab", "home")
	alice := apiClient{t, e.srv.URL, apiToken(t, e.login("alice"), "write")}
	alice.do("POST", "/bookmarks", map[string]any{"url": "https://a.org", "title": "A", "tags": []string{"home lab"}})
	alice.do("POST", "/bookmarks", map[string]any{"url": "https://b.org", "title": "B lab", "tags": []string{"home"}})
	code, out := alice.do("GET", "/bookmarks?tag="+url.QueryEscape("home lab"), nil)
	res, _ := out["results"].([]any)
	if code != 200 || len(res) != 1 || res[0].(map[string]any)["title"] != "A" {
		t.Fatalf("tag with a space: %d %v", code, out)
	}
}

func TestAPIArchiveWithEmptyChunkedBody(t *testing.T) {
	e := newEnv(t)
	e.user("alice")
	alice := apiClient{t, e.srv.URL, apiToken(t, e.login("alice"), "write")}
	_, b := alice.do("POST", "/bookmarks", map[string]any{"url": "https://a.org"})
	// No Content-Length: the body is sent chunked, and is empty.
	req, _ := http.NewRequest("POST", fmt.Sprintf("%s/api/v1/bookmarks/%v/archive", e.srv.URL, b["id"]), io.NopCloser(strings.NewReader("")))
	req.ContentLength = -1
	req.Header.Set("Authorization", "Bearer "+alice.token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("archive with an empty chunked body: %d", res.StatusCode)
	}
}
