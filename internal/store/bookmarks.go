package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/marcelhoffs/anchor/internal/search"
)

type Bookmark struct {
	ID          int64
	URL         string
	Domain      string
	Title       string
	Description string
	Notes       string
	Tags        []string
	IsArchived  bool
	IsFavourite bool
	IsShared    bool
	ShareToken  string // "" unless shared
	FaviconID   int64  // 0 = no icon
	FetchStatus string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// BookmarkInput is what a user can set when creating or editing a bookmark.
type BookmarkInput struct {
	URL         string
	Title       string
	Description string
	Notes       string
	Tags        []string
	IsFavourite bool
}

// Flag names a boolean bookmark column that can be toggled on its own.
type Flag string

const (
	FlagFavourite Flag = "favourite"
	FlagArchived  Flag = "archived"
)

var flagColumns = map[Flag]string{
	FlagFavourite: "is_favourite",
	FlagArchived:  "is_archived",
}

const bookmarkCols = `b.id, b.url, b.domain, b.title, b.description, b.notes, b.is_archived,
	b.is_favourite, coalesce(b.share_token, ''), coalesce(b.favicon_id, 0), b.fetch_status, b.created_at, b.updated_at`

func scanBookmark(row interface{ Scan(...any) error }) (Bookmark, error) {
	var b Bookmark
	var created, updated int64
	err := row.Scan(&b.ID, &b.URL, &b.Domain, &b.Title, &b.Description, &b.Notes, &b.IsArchived,
		&b.IsFavourite, &b.ShareToken, &b.FaviconID, &b.FetchStatus, &created, &updated)
	b.IsShared = b.ShareToken != ""
	b.CreatedAt, b.UpdatedAt = fromUnix(created), fromUnix(updated)
	return b, notFound(err)
}

const (
	maxTitle       = 1000
	maxDescription = 10_000
	maxNotes       = 100_000
)

func (in *BookmarkInput) clean() (normalized, domain string, err error) {
	normalized, domain, err = NormalizeURL(in.URL)
	if err != nil {
		return "", "", err
	}
	in.Title = strings.TrimSpace(in.Title)
	in.Description = strings.TrimSpace(in.Description)
	switch {
	case utf8.RuneCountInString(in.Title) > maxTitle:
		return "", "", invalid("Title is longer than %d characters.", maxTitle)
	case utf8.RuneCountInString(in.Description) > maxDescription:
		return "", "", invalid("Description is longer than %d characters.", maxDescription)
	case utf8.RuneCountInString(in.Notes) > maxNotes:
		return "", "", invalid("Notes are longer than %d characters.", maxNotes)
	}
	if in.Tags, err = ParseTags(in.Tags); err != nil {
		return "", "", err
	}
	return normalized, domain, nil
}

// CreateBookmark saves a new bookmark. If the space already has this URL it
// returns the existing bookmark unchanged and created=false.
func (s *Store) CreateBookmark(ctx context.Context, sc Scope, in BookmarkInput) (b Bookmark, created bool, err error) {
	if err := sc.mustWrite(); err != nil {
		return Bookmark{}, false, err
	}
	normalized, domain, err := in.clean()
	if err != nil {
		return Bookmark{}, false, err
	}
	hash := urlHash(normalized)
	var id int64
	err = s.tx(ctx, func(tx *sql.Tx) error {
		now := unix(s.now())
		err := tx.QueryRowContext(ctx,
			`INSERT INTO bookmarks (space_id, created_by, url, url_hash, domain, title, description, notes, is_favourite,
			                        created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT (space_id, url_hash) DO NOTHING RETURNING id`,
			sc.spaceID, sc.userID, normalized, hash, domain, in.Title, in.Description, in.Notes, in.IsFavourite, now, now,
		).Scan(&id)
		if err == sql.ErrNoRows {
			return tx.QueryRowContext(ctx, "SELECT id FROM bookmarks WHERE space_id = ? AND url_hash = ?", sc.spaceID, hash).Scan(&id)
		}
		if err != nil {
			return err
		}
		created = true
		return setTags(ctx, tx, sc.spaceID, id, in.Tags)
	})
	if err != nil {
		return Bookmark{}, false, err
	}
	b, err = s.GetBookmark(ctx, sc, id)
	return b, created, err
}

// FindBookmarkByURL returns the space's bookmark for url, if any.
func (s *Store) FindBookmarkByURL(ctx context.Context, sc Scope, rawURL string) (Bookmark, error) {
	normalized, _, err := NormalizeURL(rawURL)
	if err != nil {
		return Bookmark{}, ErrNotFound
	}
	b, err := scanBookmark(s.r.QueryRowContext(ctx,
		"SELECT "+bookmarkCols+" FROM bookmarks b WHERE b.space_id = ? AND b.url_hash = ?", sc.spaceID, urlHash(normalized)))
	if err != nil {
		return Bookmark{}, err
	}
	return b, s.loadTags(ctx, []*Bookmark{&b})
}

func (s *Store) GetBookmark(ctx context.Context, sc Scope, id int64) (Bookmark, error) {
	b, err := scanBookmark(s.r.QueryRowContext(ctx,
		"SELECT "+bookmarkCols+" FROM bookmarks b WHERE b.space_id = ? AND b.id = ?", sc.spaceID, id))
	if err != nil {
		return Bookmark{}, err
	}
	return b, s.loadTags(ctx, []*Bookmark{&b})
}

// UpdateBookmark replaces a bookmark's editable fields. It returns
// ErrConflict if the new URL is already saved as another bookmark. A new URL
// drops the old icon and queues the page details to be fetched again.
func (s *Store) UpdateBookmark(ctx context.Context, sc Scope, id int64, in BookmarkInput) (Bookmark, error) {
	if err := sc.mustWrite(); err != nil {
		return Bookmark{}, err
	}
	normalized, domain, err := in.clean()
	if err != nil {
		return Bookmark{}, err
	}
	err = s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE bookmarks SET url = ?, url_hash = ?, domain = ?, title = ?, description = ?, notes = ?,
			 is_favourite = ?, updated_at = ?,
			 favicon_id = CASE WHEN url = ? THEN favicon_id END,
			 fetch_status = CASE WHEN url = ? THEN fetch_status ELSE 'pending' END
			 WHERE space_id = ? AND id = ?`,
			normalized, urlHash(normalized), domain, in.Title, in.Description, in.Notes,
			in.IsFavourite, unix(s.now()), normalized, normalized, sc.spaceID, id)
		if isUniqueViolation(err) {
			return ErrConflict
		}
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return setTags(ctx, tx, sc.spaceID, id, in.Tags)
	})
	if err != nil {
		return Bookmark{}, err
	}
	return s.GetBookmark(ctx, sc, id)
}

func (s *Store) SetFlag(ctx context.Context, sc Scope, id int64, flag Flag, on bool) error {
	if err := sc.mustWrite(); err != nil {
		return err
	}
	col, ok := flagColumns[flag]
	if !ok {
		return fmt.Errorf("unknown flag %q", flag)
	}
	res, err := s.w.ExecContext(ctx, "UPDATE bookmarks SET "+col+" = ?, updated_at = ? WHERE space_id = ? AND id = ?",
		on, unix(s.now()), sc.spaceID, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteBookmark(ctx context.Context, sc Scope, id int64) error {
	if err := sc.mustWrite(); err != nil {
		return err
	}
	res, err := s.w.ExecContext(ctx, "DELETE FROM bookmarks WHERE space_id = ? AND id = ?", sc.spaceID, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// List orders.
const (
	SortNewest = "newest"
	SortTitle  = "title" // A–Z by title, or by URL when there is no title
)

// Search returns one page of the space's bookmarks matching q, best match
// first for free-text queries and then in sort order (one of Sorts; anything
// else means newest first). total is how many match in all.
func (s *Store) Search(ctx context.Context, sc Scope, q search.Query, sort string, limit, offset int) (list []Bookmark, total int, err error) {
	var (
		from  = "bookmarks b"
		where = []string{"b.space_id = ?"}
		args  []any
		order = "b.created_at DESC, b.id DESC"
	)
	if sort == SortTitle {
		order = "CASE b.title WHEN '' THEN b.url ELSE b.title END COLLATE NOCASE, b.id"
	}
	if fts := q.FTS(); fts != "" {
		// Title weighted ×3 over description, notes, URL and tags.
		from = "bookmarks b JOIN bookmarks_fts ON bookmarks_fts.rowid = b.id"
		where = append(where, "bookmarks_fts MATCH ?")
		args = append(args, sc.spaceID, fts)
		order = "bm25(bookmarks_fts, 3.0, 1.0, 1.0, 1.0, 1.0), " + order
	} else {
		args = append(args, sc.spaceID)
	}
	const hasTag = "EXISTS (SELECT 1 FROM bookmark_tags bt JOIN tags t ON t.id = bt.tag_id WHERE bt.bookmark_id = b.id AND t.name = ?)"
	for _, t := range q.Tags {
		where = append(where, hasTag)
		args = append(args, t)
	}
	for _, t := range q.NotTags {
		where = append(where, "NOT "+hasTag)
		args = append(args, t)
	}
	for _, site := range q.Sites {
		where = append(where, "(b.domain = ? OR substr(b.domain, -length(?) - 1) = '.' || ?)")
		args = append(args, site, site, site)
	}
	if q.Archived {
		where = append(where, "b.is_archived = 1")
	} else {
		where = append(where, "b.is_archived = 0")
	}
	if q.Fav {
		where = append(where, "b.is_favourite = 1")
	}
	if q.Shared {
		where = append(where, "b.share_token IS NOT NULL")
	}
	if q.Untagged {
		where = append(where, "NOT EXISTS (SELECT 1 FROM bookmark_tags bt WHERE bt.bookmark_id = b.id)")
	}
	if !q.After.IsZero() {
		where = append(where, "b.created_at >= ?")
		args = append(args, unix(q.After))
	}
	if !q.Before.IsZero() {
		where = append(where, "b.created_at < ?")
		args = append(args, unix(q.Before))
	}
	cond := " FROM " + from + " WHERE " + strings.Join(where, " AND ")
	if err := s.r.QueryRowContext(ctx, "SELECT count(*)"+cond, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if offset >= total {
		return nil, total, nil
	}

	rows, err := s.r.QueryContext(ctx,
		"SELECT "+bookmarkCols+cond+" ORDER BY "+order+" LIMIT ? OFFSET ?", append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		b, err := scanBookmark(rows)
		if err != nil {
			return nil, 0, err
		}
		list = append(list, b)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	ptrs := make([]*Bookmark, len(list))
	for i := range list {
		ptrs[i] = &list[i]
	}
	return list, total, s.loadTags(ctx, ptrs)
}

// loadTags fills in Tags for bookmarks that were already loaded for one space.
func (s *Store) loadTags(ctx context.Context, bs []*Bookmark) error {
	if len(bs) == 0 {
		return nil
	}
	byID := make(map[int64]*Bookmark, len(bs))
	args := make([]any, len(bs))
	for i, b := range bs {
		byID[b.ID] = b
		args[i] = b.ID
	}
	rows, err := s.r.QueryContext(ctx,
		`SELECT bt.bookmark_id, t.name FROM bookmark_tags bt JOIN tags t ON t.id = bt.tag_id
		 WHERE bt.bookmark_id IN (?`+strings.Repeat(", ?", len(bs)-1)+`) ORDER BY t.name`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return err
		}
		byID[id].Tags = append(byID[id].Tags, name)
	}
	return rows.Err()
}

// Counts holds the size of each sidebar view.
type Counts struct {
	All        int // not archived
	Favourites int // favourites, not archived
	Archived   int
}

func (s *Store) CountBookmarks(ctx context.Context, sc Scope) (Counts, error) {
	var c Counts
	err := s.r.QueryRowContext(ctx,
		`SELECT coalesce(sum(is_archived = 0), 0), coalesce(sum(is_archived = 0 AND is_favourite = 1), 0),
		        coalesce(sum(is_archived = 1), 0)
		 FROM bookmarks WHERE space_id = ?`, sc.spaceID).Scan(&c.All, &c.Favourites, &c.Archived)
	return c, err
}
