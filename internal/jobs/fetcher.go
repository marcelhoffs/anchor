package jobs

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/marcelhoffs/anchor/internal/fetch"
	"github.com/marcelhoffs/anchor/internal/store"
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

	// IconServices ("duckduckgo", "google") are asked in order for the icon
	// of a site that gives none itself. Set before Run.
	IconServices []string
	serviceURL   func(service, domain string) string // fetch.ServiceIcon; tests swap it
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
	if m.FinalURL != "" && !fetch.SameHost(it.URL, m.FinalURL) {
		// Redirected to another host, typically a sign-in page or a
		// cookie-consent wall: its title and icon aren't this page's. The
		// icon comes from it.URL's host, unless the redirect went to the
		// site's home page, whose icon is the site's.
		f.log.Debug("fetch page: redirected to another host", "bookmark", it.ID, "to", m.FinalURL)
		if fetch.ToSiteHome(it.URL, m.FinalURL) {
			m = fetch.Meta{Icons: m.Icons, FinalURL: m.FinalURL}
		} else {
			m = fetch.Meta{}
		}
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
const iconBackfillVersion = "3"

// backfillIcons queues bookmarks without an icon to be fetched again once,
// when an icon service is switched on (or its lookup improved).
func (f *Fetcher) backfillIcons(ctx context.Context) {
	if len(f.IconServices) == 0 {
		return
	}
	marker := strings.Join(f.IconServices, ",") + "/" + iconBackfillVersion
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
	f.log.Info("fetching icons again for bookmarks without one", "services", f.IconServices, "bookmarks", n)
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
	for _, u := range append(f.siteIcons(ctx, page, m), fetch.DefaultIcon(page)) {
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
	return f.serviceIcon(ctx, it.Domain)
}

// siteIcons are the icons page's host declares. The stored icon is its
// whole host's, for every user, so they come from the host's home page:
// one user of a shared host (a wiki, a paste site) can't edit that, but can
// point their own page at any image.
func (f *Fetcher) siteIcons(ctx context.Context, page string, m fetch.Meta) []string {
	u, err := url.Parse(page)
	if err != nil {
		return nil
	}
	if (u.Path == "" || u.Path == "/") && u.RawQuery == "" {
		return m.Icons
	}
	home, err := f.client.Page(ctx, u.Scheme+"://"+u.Host+"/")
	if err != nil || (home.FinalURL != "" && !fetch.SameHost(page, home.FinalURL)) {
		return nil
	}
	return home.Icons
}

// serviceIcon asks the icon services, in order, for domain's icon and
// stores the first one found.
func (f *Fetcher) serviceIcon(ctx context.Context, domain string) int64 {
	for _, service := range f.IconServices {
		mime, data, ok := f.serviceLookup(ctx, service, domain)
		if !ok {
			continue
		}
		id, err := f.store.SaveFavicon(ctx, domain, mime, data)
		if err != nil {
			f.log.Error("save icon", "domain", domain, "err", err)
			return 0
		}
		return id
	}
	return 0
}

// serviceLookup asks one service for domain's icon. The host and its main
// domain (www.nu.nl, nu.nl) are one site: the host's icon, else the main
// domain's. A separate host is different: services answer for a product
// such as keep.google.com with its parent's brand (Google's "G"), so a known
// product host gets no service icon and shows its letter. A host they don't
// know is usually a customer space on a platform (cronos.sharepoint.com),
// where the platform's icon fits.
func (f *Fetcher) serviceLookup(ctx context.Context, service, domain string) (string, []byte, bool) {
	get := func(d string) (string, []byte, error) {
		u := f.serviceURL(service, d)
		if u == "" {
			return "", nil, errors.New("unknown icon service")
		}
		return f.client.Icon(ctx, u)
	}
	mime, data, err := get(domain)
	main := fetch.RegistrableDomain(domain)
	switch {
	case main == domain:
	case strings.TrimPrefix(domain, "www.") != main && err == nil:
		f.log.Debug("icon service: product host, parent's brand", "service", service, "domain", domain)
		return "", nil, false
	case err != nil:
		mime, data, err = get(main)
	}
	if err != nil {
		f.log.Debug("icon service", "service", service, "domain", domain, "err", err)
		return "", nil, false
	}
	return mime, data, true
}
