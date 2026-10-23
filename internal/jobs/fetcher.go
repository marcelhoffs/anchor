package jobs

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/marcelhoffs/burrow/internal/fetch"
	"github.com/marcelhoffs/burrow/internal/store"
)

const (
	// fetchBatch bookmarks are fetched at once, in parallel.
	fetchBatch = 4
	// A domain's stored icon is reused for this long before it's fetched again.
	iconMaxAge = 30 * 24 * time.Hour
)

// Fetcher fills in the title, description and icon of bookmarks whose
// fetch_status is pending. The status column is the queue, so the work
// survives restarts and needs no job rows.
type Fetcher struct {
	store  *store.Store
	client *fetch.Client
	log    *slog.Logger
	wake   chan struct{}

	// IconService, when set ("duckduckgo"), is asked for the icon of a site
	// that gives none itself. Set it before Run.
	IconService string
	serviceURL  func(service, domain string) string // fetch.ServiceIcon; tests swap it
}

func NewFetcher(st *store.Store, client *fetch.Client, log *slog.Logger) *Fetcher {
	return &Fetcher{store: st, client: client, log: log, wake: make(chan struct{}, 1), serviceURL: fetch.ServiceIcon}
}

// Notify tells the fetcher a bookmark was saved.
func (f *Fetcher) Notify() {
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

// Run fetches until ctx is cancelled.
func (f *Fetcher) Run(ctx context.Context) {
	f.backfillIcons(ctx)
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		for f.RunOnce(ctx) {
		}
		select {
		case <-ctx.Done():
			return
		case <-f.wake:
		case <-t.C:
		}
	}
}

// RunOnce fetches one batch; it reports whether there was any work.
func (f *Fetcher) RunOnce(ctx context.Context) bool {
	items, err := f.store.PendingFetches(ctx, fetchBatch)
	if err != nil {
		if ctx.Err() == nil {
			f.log.Error("pending fetches", "err", err)
		}
		return false
	}
	var wg sync.WaitGroup
	var saved atomic.Int32
	for _, it := range items {
		wg.Go(func() {
			r := f.fetchOne(ctx, it)
			if err := f.store.SaveFetch(ctx, it, r); err != nil {
				if ctx.Err() == nil {
					f.log.Error("save fetch", "bookmark", it.ID, "err", err)
				}
				return
			}
			saved.Add(1)
		})
	}
	wg.Wait()
	// If nothing could be saved, wait for the next tick rather than spin on
	// the same rows.
	return saved.Load() > 0 && ctx.Err() == nil
}

func (f *Fetcher) fetchOne(ctx context.Context, it store.FetchItem) store.FetchResult {
	r := store.FetchResult{Status: store.FetchDone}
	m, err := f.client.Page(ctx, it.URL)
	if m.FinalURL != "" && !fetch.SameSite(it.URL, m.FinalURL) {
		// Redirected to another site, typically a cookie-consent wall: its
		// title and icon aren't this site's. The icon comes from it.URL.
		f.log.Debug("fetch page: redirected to another site", "bookmark", it.ID, "to", m.FinalURL)
		m = fetch.Meta{}
	}
	if err != nil {
		r.Status = store.FetchFailed
		f.log.Debug("fetch page", "bookmark", it.ID, "err", err)
		if errors.Is(err, fetch.ErrBlocked) {
			return r // no point asking the same host for an icon
		}
	}
	r.Title, r.Description = m.Title, m.Description
	r.FaviconID = f.icon(ctx, it, m)
	return r
}

// iconBackfillVersion goes up when the icon service lookup improves, so
// bookmarks still without an icon get one more try.
const iconBackfillVersion = "2"

// backfillIcons queues bookmarks without an icon to be fetched again once,
// when an icon service is switched on (or its lookup improved).
func (f *Fetcher) backfillIcons(ctx context.Context) {
	if f.IconService == "" {
		return
	}
	marker := f.IconService + "/" + iconBackfillVersion
	done, err := f.store.InstanceSetting(ctx, store.SettingIconBackfill, "")
	if err != nil || done == marker {
		return
	}
	n, err := f.store.RequeueIconless(ctx)
	if err == nil {
		err = f.store.SetInstanceSetting(ctx, store.SettingIconBackfill, marker)
	}
	if err != nil {
		f.log.Error("icon backfill", "err", err)
		return
	}
	f.log.Info("fetching icons again for bookmarks without one", "service", f.IconService, "bookmarks", n)
}

// icon returns the domain's stored icon, fetching it when it is missing or
// old. When fetching fails, an old (or meanwhile stored) icon is better than
// none; after that the icon service, if one is set, is asked. 0 means there
// is none.
func (f *Fetcher) icon(ctx context.Context, it store.FetchItem, m fetch.Meta) int64 {
	if id, err := f.store.FaviconForDomain(ctx, it.Domain, time.Now().Add(-iconMaxAge)); err == nil {
		return id
	}
	page := m.FinalURL
	if page == "" {
		page = it.URL
	}
	for _, u := range append(m.Icons, fetch.DefaultIcon(page)) {
		mime, data, err := f.client.Icon(ctx, u)
		if err != nil {
			f.log.Debug("fetch icon", "url", u, "err", err)
			continue
		}
		id, err := f.store.SaveFavicon(ctx, it.Domain, mime, data)
		if err != nil {
			f.log.Error("save icon", "domain", it.Domain, "err", err)
			return 0
		}
		return id
	}
	if id, err := f.store.FaviconForDomain(ctx, it.Domain, time.Time{}); err == nil {
		return id
	}
	if f.IconService == "" {
		return 0
	}
	// The host first, then its site: a tenant like cronos.sharepoint.com is
	// unknown to the service, but sharepoint.com's icon fits.
	for _, d := range slices.Compact([]string{it.Domain, fetch.RegistrableDomain(it.Domain)}) {
		u := f.serviceURL(f.IconService, d)
		if u == "" {
			break
		}
		mime, data, err := f.client.Icon(ctx, u)
		if err != nil {
			f.log.Debug("icon service", "domain", d, "err", err)
			continue
		}
		if id, err := f.store.SaveFavicon(ctx, it.Domain, mime, data); err == nil {
			return id
		}
		break
	}
	return 0
}
