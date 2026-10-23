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

	f := NewFetcher(st, fetch.New(true), slog.New(slog.NewTextHandler(io.Discard, nil)))
	f.serviceURL = func(_, domain string) string { return service.URL + "/" + domain + ".ico" }
	b, _, _ := st.CreateBookmark(ctx, u.ID, store.BookmarkInput{URL: blocked.URL + "/"})

	// Off by default: the service is never asked.
	for f.RunOnce(ctx) {
	}
	if got, _ := st.GetBookmark(ctx, u.ID, b.ID); got.FaviconID != 0 || len(asked) != 0 {
		t.Fatalf("service used while off: %+v %v", got, asked)
	}

	// Switched on, iconless bookmarks are fetched again once.
	f.IconService = "test"
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
	if fetch.ServiceIcon("duckduckgo", "www.klm.be") != "https://icons.duckduckgo.com/ip3/www.klm.be.ico" || fetch.ServiceIcon("nope", "x") != "" {
		t.Fatal("ServiceIcon")
	}
}
