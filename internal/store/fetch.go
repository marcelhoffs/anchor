package store

import (
	"context"
	"time"
)

// Fetch statuses. New and imported bookmarks start pending; the fetcher
// moves them to done or failed.
const (
	FetchPending = "pending"
	FetchDone    = "done"
	FetchFailed  = "failed"
)

// FetchItem is a bookmark waiting for its page details.
type FetchItem struct {
	ID     int64
	URL    string
	Domain string
}

// PendingFetches returns up to limit bookmarks to fetch, newest first so
// what a user just saved isn't stuck behind a large import.
func (s *Store) PendingFetches(ctx context.Context, limit int) ([]FetchItem, error) {
	rows, err := s.r.QueryContext(ctx,
		"SELECT id, url, domain FROM bookmarks WHERE fetch_status = ? ORDER BY id DESC LIMIT ?", FetchPending, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FetchItem
	for rows.Next() {
		var it FetchItem
		if err := rows.Scan(&it.ID, &it.URL, &it.Domain); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// FetchResult is what the fetcher learned about a bookmark's page.
type FetchResult struct {
	Status      string
	Title       string
	Description string
	FaviconID   int64 // 0 = none found
}

// SaveFetch records a fetch. Title and description only fill fields the
// user left empty; a bookmark whose URL changed meanwhile is left pending.
func (s *Store) SaveFetch(ctx context.Context, it FetchItem, r FetchResult) error {
	_, err := s.w.ExecContext(ctx,
		`UPDATE bookmarks SET
		   title = CASE WHEN title = '' THEN ? ELSE title END,
		   description = CASE WHEN description = '' THEN ? ELSE description END,
		   favicon_id = coalesce(?, favicon_id),
		   fetch_status = ?
		 WHERE id = ? AND url = ?`,
		r.Title, r.Description, nullID(r.FaviconID), r.Status, it.ID, it.URL)
	return err
}

// Refetch queues a bookmark's page details to be fetched again.
func (s *Store) Refetch(ctx context.Context, sc Scope, id int64) error {
	if err := sc.mustWrite(); err != nil {
		return err
	}
	return affected(s.w.ExecContext(ctx, "UPDATE bookmarks SET fetch_status = ? WHERE space_id = ? AND id = ?",
		FetchPending, sc.spaceID, id))
}

// RequeueIconless queues every bookmark that has no icon to be fetched again.
func (s *Store) RequeueIconless(ctx context.Context) (int64, error) {
	res, err := s.w.ExecContext(ctx, "UPDATE bookmarks SET fetch_status = ? WHERE favicon_id IS NULL AND fetch_status != ?",
		FetchPending, FetchPending)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// FaviconForDomain returns the stored icon for domain if it was fetched
// after since; ErrNotFound otherwise.
func (s *Store) FaviconForDomain(ctx context.Context, domain string, since time.Time) (int64, error) {
	var id int64
	err := s.r.QueryRowContext(ctx, "SELECT id FROM favicons WHERE domain = ? AND fetched_at >= ?",
		domain, unix(since)).Scan(&id)
	return id, notFound(err)
}

// SaveFavicon stores (or replaces) a domain's icon. Icons are shared by
// every bookmark on that domain, in every space.
func (s *Store) SaveFavicon(ctx context.Context, domain, mime string, data []byte) (int64, error) {
	var id int64
	err := s.w.QueryRowContext(ctx,
		`INSERT INTO favicons (domain, mime, data, fetched_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT (domain) DO UPDATE SET mime = excluded.mime, data = excluded.data, fetched_at = excluded.fetched_at
		 RETURNING id`, domain, mime, data, unix(s.now())).Scan(&id)
	return id, err
}

// Favicon returns an icon, but only to a user who can see a bookmark that
// uses it, in any of their spaces.
func (s *Store) Favicon(ctx context.Context, userID, id int64) (mime string, data []byte, err error) {
	err = s.r.QueryRowContext(ctx,
		`SELECT mime, data FROM favicons f
		 WHERE f.id = ? AND EXISTS (SELECT 1 FROM bookmarks b JOIN space_members m ON m.space_id = b.space_id
		                            WHERE m.user_id = ? AND b.favicon_id = f.id)`,
		id, userID).Scan(&mime, &data)
	return mime, data, notFound(err)
}

func nullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}
