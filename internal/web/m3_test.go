package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/marcelhoffs/burrow/internal/fetch"
	"github.com/marcelhoffs/burrow/internal/store"
)

// site serves a page with a title, description and icon, like a real site.
func site(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/article":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write([]byte(`<html><head><title>A Fine Article</title><meta name="description" content="All about burrows.">
				<link rel="icon" href="/fav.png"></head><body>…</body></html>`))
		case "/fav.png":
			w.Write([]byte("\x89PNG\r\n\x1a\n" + strings.Repeat("\x01", 40)))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchFillsInDetails(t *testing.T) {
	e := newEnv(t)
	e.user("alice")
	e.user("bob")
	c := e.login("alice")
	srv := site(t)
	c.post("/bookmarks", url.Values{"url": {srv.URL + "/article"}})
	if _, body, _ := c.get("/bookmarks/1/edit"); !strings.Contains(body, "Fetching the page") {
		t.Fatal("edit page doesn't say the fetch is pending")
	}
	for e.fetcher.RunOnce(context.Background()) {
	}
	c.post("/bookmarks", url.Values{"url": {srv.URL + "/gone"}, "title": {"Kept title"}})
	for e.fetcher.RunOnce(context.Background()) {
	}

	_, body, _ := c.get("/")
	if !strings.Contains(body, "A Fine Article") || !strings.Contains(body, "All about burrows.") || !strings.Contains(body, "Kept title") {
		t.Fatal("details not filled in")
	}
	b, _ := e.store.GetBookmark(context.Background(), e.userID("alice"), 1)
	if b.FaviconID == 0 || !strings.Contains(body, `src="/favicons/`) {
		t.Fatalf("no icon: %+v", b)
	}
	// Both bookmarks are on the same host, so they share its icon.
	if b2, _ := e.store.GetBookmark(context.Background(), e.userID("alice"), 2); b2.FaviconID != b.FaviconID || b2.FetchStatus != store.FetchFailed {
		t.Fatalf("second bookmark = %+v", b2)
	}
	res, err := c.http.Get(e.srv.URL + "/favicons/" + itoa(b.FaviconID))
	if err != nil || res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "image/png" {
		t.Fatalf("favicon: %v %v", res, err)
	}
	res.Body.Close()
	if code, _, _ := e.login("bob").get("/favicons/" + itoa(b.FaviconID)); code != http.StatusNotFound {
		t.Fatalf("bob got alice's icon: %d", code)
	}

	// Fetch again from the edit page.
	if _, body, _ := c.get("/bookmarks/2/edit"); !strings.Contains(body, "be fetched, so empty fields stay empty") {
		t.Fatal("failed fetch not shown")
	}
	if code, _, loc := c.post("/bookmarks/2/refetch", url.Values{"next": {"/"}}); code != http.StatusSeeOther || !strings.Contains(loc, "done=refetched") {
		t.Fatalf("refetch: %d %q", code, loc)
	}
	if b2, _ := e.store.GetBookmark(context.Background(), e.userID("alice"), 2); b2.FetchStatus != store.FetchPending {
		t.Fatal("refetch didn't queue")
	}
}

func TestSharingFlow(t *testing.T) {
	e := newEnv(t)
	e.user("alice")
	e.user("bob")
	c := e.login("alice")
	e.tags("alice", "private-tag")
	c.post("/bookmarks", url.Values{"url": {"https://go.dev"}, "title": {"Go"}, "description": {"The Go site"},
		"notes": {"secret note"}, "tags": {"private-tag"}})
	if code, _, _ := e.login("bob").post("/bookmarks/1/share", url.Values{"on": {"1"}}); code != http.StatusNotFound {
		t.Fatalf("bob shared alice's bookmark: %d", code)
	}
	if code, _, loc := c.post("/bookmarks/1/share", url.Values{"on": {"1"}, "next": {"/"}}); code != http.StatusSeeOther || !strings.Contains(loc, "done=shared") {
		t.Fatalf("share: %d %q", code, loc)
	}
	b, _ := e.store.GetBookmark(context.Background(), e.userID("alice"), 1)
	link := "/s/" + b.ShareToken
	if _, body, _ := c.get("/bookmarks/1/edit"); !strings.Contains(body, e.origin+link) {
		t.Fatal("edit page doesn't show the public link")
	}
	if _, body, _ := c.get("/"); !strings.Contains(body, "Shared</a>") {
		t.Fatal("list doesn't mark it shared")
	}

	// Anyone can open the link; it shows no notes or tags.
	res, err := http.Get(e.srv.URL + link)
	if err != nil || res.StatusCode != http.StatusOK || res.Header.Get("X-Robots-Tag") != "noindex" {
		t.Fatalf("public page: %v %v", res, err)
	}
	res.Body.Close()
	_, body, _ := e.client().get(link)
	if !strings.Contains(body, "The Go site") || strings.Contains(body, "secret note") || strings.Contains(body, "private-tag") {
		t.Fatal("public page shows the wrong things")
	}

	// The API reports the link and can remove it.
	api := apiClient{t, e.srv.URL, apiToken(t, c, "write")}
	if _, out := api.do("GET", "/bookmarks/1", nil); out["share_url"] != e.origin+link {
		t.Fatalf("api share_url = %v", out["share_url"])
	}
	if code, out := api.do("POST", "/bookmarks/1/share", map[string]bool{"shared": false}); code != http.StatusOK || out["share_url"] != nil {
		t.Fatalf("api unshare: %d %v", code, out)
	}
	if code, _, _ := e.client().get(link); code != http.StatusNotFound {
		t.Fatalf("unshared link: %d", code)
	}
}

func TestBundlesFlow(t *testing.T) {
	e := newEnv(t)
	e.user("alice")
	e.user("bob")
	c := e.login("alice")
	e.tags("alice", "go")
	c.post("/bookmarks", url.Values{"url": {"https://go.dev"}, "title": {"Go"}, "tags": {"go"}})
	c.post("/bookmarks", url.Values{"url": {"https://rust-lang.org"}, "title": {"Rust"}})

	if _, body, _ := c.get("/?q=" + url.QueryEscape("#go")); strings.Contains(body, `popovertarget="save-bundle"`) {
		t.Fatal("a tag view offers to save itself")
	}
	q := "site:go.dev #go"
	if _, body, _ := c.get("/?q=" + url.QueryEscape(q)); !strings.Contains(body, `popovertarget="save-bundle"`) {
		t.Fatal("no Save search on a search")
	}
	code, _, loc := c.post("/bundles", url.Values{"name": {"Go sites"}, "query": {q}})
	if code != http.StatusSeeOther || loc != "/?q="+url.QueryEscape(q) {
		t.Fatalf("save: %d %q", code, loc)
	}
	_, body, _ := c.get(loc)
	if !strings.Contains(body, "<h1>Go sites</h1>") || strings.Contains(body, `popovertarget="save-bundle"`) {
		t.Fatal("bundle view not shown as the bundle")
	}
	if !strings.Contains(body, `aria-label="Bundles"`) || strings.Count(body, `aria-current="page"`) != 1 {
		t.Fatal("bundle not in the sidebar, or not the only current entry")
	}
	if code, body, _ := c.post("/bundles", url.Values{"name": {""}, "query": {"x"}}); code != http.StatusUnprocessableEntity || !strings.Contains(body, "Give the bundle a name") {
		t.Fatalf("empty name: %d", code)
	}

	list, _ := e.store.ListBundles(context.Background(), e.userID("alice"))
	id := itoa(list[0].ID)
	bob := e.login("bob")
	if _, body, _ := bob.get("/"); strings.Contains(body, "Go sites") {
		t.Fatal("bob sees alice's bundle")
	}
	if code, _, _ := bob.post("/bundles/"+id+"/delete", nil); code != http.StatusNotFound {
		t.Fatalf("bob deleted alice's bundle: %d", code)
	}
	if code, _, _ := c.post("/bundles/"+id, url.Values{"name": {"Go"}, "query": {"#go"}}); code != http.StatusSeeOther {
		t.Fatalf("rename: %d", code)
	}
	if _, body, _ := c.get("/bundles"); !strings.Contains(body, `value="Go"`) {
		t.Fatal("rename not shown")
	}
	api := apiClient{t, e.srv.URL, apiToken(t, c, "read")}
	if code, out := api.do("GET", "/bundles", nil); code != http.StatusOK || len(out["results"].([]any)) != 1 {
		t.Fatalf("api bundles: %d %v", code, out)
	}
	if code, _, _ := c.post("/bundles/"+id+"/delete", nil); code != http.StatusSeeOther {
		t.Fatalf("delete: %d", code)
	}
	if _, body, _ := c.get("/"); strings.Contains(body, `aria-label="Bundles"`) {
		t.Fatal("empty bundles section shown")
	}
}

// A page that redirects to another host (a cookie-consent wall, a sign-in
// page) must not lend the bookmark that page's title or icon.
func TestFetchIgnoresCrossSiteRedirect(t *testing.T) {
	e := newEnv(t)
	e.user("alice")
	c := e.login("alice")
	consentIcon := "\x89PNG\r\n\x1a\n" + strings.Repeat("\x02", 40)
	siteIcon := "\x89PNG\r\n\x1a\n" + strings.Repeat("\x03", 40)
	consent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/favicon.ico" {
			w.Write([]byte(consentIcon))
			return
		}
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<title>Privacy Gate</title>"))
	}))
	t.Cleanup(consent.Close)
	// The consent server is reached as "localhost", the site as 127.0.0.1:
	// two different sites.
	consentURL := strings.Replace(consent.URL, "127.0.0.1", "localhost", 1)
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/favicon.ico":
			w.Write([]byte(siteIcon))
		case "/old-icon.ico":
			http.Redirect(w, r, consentURL+"/favicon.ico", http.StatusFound)
		default:
			http.Redirect(w, r, consentURL+"/", http.StatusFound)
		}
	}))
	t.Cleanup(site.Close)

	c.post("/bookmarks", url.Values{"url": {site.URL + "/news"}})
	for e.fetcher.RunOnce(context.Background()) {
	}
	b, _ := e.store.GetBookmark(context.Background(), e.userID("alice"), 1)
	if b.Title != "" {
		t.Fatalf("took the consent page's title: %q", b.Title)
	}
	_, img, err := e.store.Favicon(context.Background(), e.userID("alice"), b.FaviconID)
	if err != nil || string(img) != siteIcon {
		t.Fatalf("icon = %q, %v; want the site's own", img, err)
	}

	// Icon redirects are followed anywhere (Gmail's icon is on gstatic.com);
	// what comes back must still be an image.
	if _, img, err := fetch.New(true).Icon(context.Background(), site.URL+"/old-icon.ico"); err != nil || string(img) != consentIcon {
		t.Fatalf("icon redirect: %v", err)
	}
}
