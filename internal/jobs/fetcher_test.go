package jobs

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marcelhoffs/burrow/internal/db"
	"github.com/marcelhoffs/burrow/internal/fetch"
	"github.com/marcelhoffs/burrow/internal/store"
)

func TestIconServiceFallback(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	st := store.New(d)
	u, _ := st.CreateUser(ctx, "alice", "x", false)

	// A site that blocks everything, and an icon service that knows it.
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no bots", http.StatusForbidden)
	}))
	t.Cleanup(blocked.Close)
	icon := "\x89PNG\r\n\x1a\n" + strings.Repeat("\x04", 40)
	var asked []string
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Path)
		w.Write([]byte(icon))
	}))
	t.Cleanup(service.Close)
	service2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(icon)) }))
	t.Cleanup(service2.Close)

	// A short timeout: the made-up .invalid hosts below never answer.
	f := NewFetcher(st, fetch.NewWithTimeout(true, time.Second), slog.New(slog.NewTextHandler(io.Discard, nil)))
	f.serviceURL = func(_, domain string) string { return service.URL + "/" + domain + ".ico" }
	b, _, _ := st.CreateBookmark(ctx, u.ID, store.BookmarkInput{URL: blocked.URL + "/"})

	// Off by default: the service is never asked.
	for f.RunOnce(ctx) {
	}
	if got, _ := st.GetBookmark(ctx, u.ID, b.ID); got.FaviconID != 0 || len(asked) != 0 {
		t.Fatalf("service used while off: %+v %v", got, asked)
	}

	// Switched on, iconless bookmarks are fetched again once.
	f.IconServices = []string{"test"}
	f.backfillIcons(ctx)
	for f.RunOnce(ctx) {
	}
	got, _ := st.GetBookmark(ctx, u.ID, b.ID)
	if got.FaviconID == 0 || len(asked) != 1 || asked[0] != "/127.0.0.1.ico" {
		t.Fatalf("after backfill: %+v, asked %v", got, asked)
	}
	if _, data, _ := st.Favicon(ctx, u.ID, got.FaviconID); string(data) != icon {
		t.Fatal("stored the wrong icon")
	}
	// The backfill runs once per service, not on every start.
	st.SetFlag(ctx, u.ID, b.ID, store.FlagFavourite, true) // any write; the bookmark keeps its icon
	if f.backfillIcons(ctx); len(asked) != 1 {
		t.Fatal("backfill ran again")
	}
	if items, _ := st.PendingFetches(ctx, 10); len(items) != 0 {
		t.Fatalf("requeued again: %+v", items)
	}
	// A tenant host the service doesn't know falls back to its site's icon.
	tenant, _, _ := st.CreateBookmark(ctx, u.ID, store.BookmarkInput{URL: "https://cronos.sharepoint.invalid/"})
	f.serviceURL = func(_, domain string) string {
		if domain != "sharepoint.invalid" {
			return service.URL + "/unknown"
		}
		return service.URL + "/" + domain + ".ico"
	}
	unknown := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Path)
		if r.URL.Path == "/unknown" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(icon))
	})
	service.Config.Handler = unknown
	for f.RunOnce(ctx) {
	}
	if got, _ := st.GetBookmark(ctx, u.ID, tenant.ID); got.FaviconID == 0 || asked[len(asked)-1] != "/sharepoint.invalid.ico" {
		t.Fatalf("tenant icon: %+v, asked %v", got, asked)
	}
	// When the service answers for a host with exactly its main domain's
	// icon (Keep gets Google's "G"), that's no icon of its own.
	product, _, _ := st.CreateBookmark(ctx, u.ID, store.BookmarkInput{URL: "https://keep.google.invalid/"})
	f.serviceURL = func(_, domain string) string { return service.URL + "/" + domain + ".ico" }
	for f.RunOnce(ctx) {
	}
	if got, _ := st.GetBookmark(ctx, u.ID, product.ID); got.FaviconID != 0 {
		t.Fatalf("product got its parent's generic icon: %+v", got)
	}
	// www.nu.nl and nu.nl are one site: the same icon for both is its own.
	www, _, _ := st.CreateBookmark(ctx, u.ID, store.BookmarkInput{URL: "https://www.nu.invalid/"})
	for f.RunOnce(ctx) {
	}
	if got, _ := st.GetBookmark(ctx, u.ID, www.ID); got.FaviconID == 0 {
		t.Fatal("www host lost its icon")
	}

	// Services are asked in order; the second fills in for the first.
	f.IconServices = []string{"none", "test"}
	f.serviceURL = func(service, domain string) string {
		if service == "none" {
			return service + "://" // fails: not an http URL
		}
		return service2.URL + "/" + domain + ".ico"
	}
	second, _, _ := st.CreateBookmark(ctx, u.ID, store.BookmarkInput{URL: "https://telegraaf.invalid/"})
	for f.RunOnce(ctx) {
	}
	if got, _ := st.GetBookmark(ctx, u.ID, second.ID); got.FaviconID == 0 {
		t.Fatal("second service not asked")
	}

	if fetch.ServiceIcon("duckduckgo", "www.klm.be") != "https://icons.duckduckgo.com/ip3/www.klm.be.ico" ||
		fetch.ServiceIcon("google", "telegraaf.nl") != "https://www.google.com/s2/favicons?sz=64&domain=telegraaf.nl" || fetch.ServiceIcon("nope", "x") != "" {
		t.Fatal("ServiceIcon")
	}
}
