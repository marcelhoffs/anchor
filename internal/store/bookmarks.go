package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/marcelhoffs/burrow/internal/search"
)

type Bookmark struct {
	ID          int64
	URL         string
	Domain      string
	Title       string
	Description string
	Notes       string
	Tags        []string
	IsUnread    bool
	IsArchived  bool
	IsFavourite bool
	IsShared    bool
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
	IsUnread    bool
	IsFavourite bool
}

// Flag names a boolean bookmark column that can be toggled on its own.
type Flag string

const (
	FlagUnread    Flag = "unread"
	FlagFavourite Flag = "favourite"
	FlagArchived  Flag = "archived"
)

var flagColumns = map[Flag]string{
	FlagUnread:    "is_unread",
	FlagFavourite: "is_favourite",
	FlagArchived:  "is_archived",
}

const bookmarkCols = `b.id, b.url, b.domain, b.title, b.description, b.notes, b.is_unread, b.is_archived,
	b.is_favourite, b.share_token IS NOT NULL, b.fetch_status, b.created_at, b.updated_at`

func scanBookmark(row interface{ Scan(...any) error }) (Bookmark, error) {
	var b Bookmark
	var created, updated int64
	err := row.Scan(&b.ID, &b.URL, &b.Domain, &b.Title, &b.Description, &b.Notes, &b.IsUnread, &b.IsArchived,
		&b.IsFavourite, &b.IsShared, &b.FetchStatus, &created, &updated)
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
	in.Tags = CleanTags(in.Tags)
	return normalized, domain, nil
}

// CreateBookmark saves a new bookmark. If the user already saved this URL it
// returns the existing bookmark unchanged and created=false.
func (s *Store) CreateBookmark(ctx context.Context, userID int64, in BookmarkInput) (b Bookmark, created bool, err error) {
	normalized, domain, err := in.clean()
	if err != nil {
		return Bookmark{}, false, err
	}
	hash := urlHash(normalized)
	var id int64
	err = s.tx(ctx, func(tx *sql.Tx) error {
		now := unix(s.now())
		err := tx.QueryRowContext(ctx,
			`INSERT INTO bookmarks (user_id, url, url_hash, domain, title, description, notes, is_unread, is_favourite, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT (user_id, url_hash) DO NOTHING RETURNING id`,
			userID, normalized, hash, domain, in.Title, in.Description, in.Notes, in.IsUnread, in.IsFavourite, now, now,
		).Scan(&id)
		if err == sql.ErrNoRows {
			return tx.QueryRowContext(ctx, "SELECT id FROM bookmarks WHERE user_id = ? AND url_hash = ?", userID, hash).Scan(&id)
		}
		if err != nil {
			return err
		}
		created = true
		return setTags(ctx, tx, userID, id, in.Tags)
	})
	if err != nil {
		return Bookmark{}, false, err
	}
	b, err = s.GetBookmark(ctx, userID, id)
	return b, created, err
}

// FindBookmarkByURL returns the user's bookmark for url, if any.
func (s *Store) FindBookmarkByURL(ctx context.Context, userID int64, rawURL string) (Bookmark, error) {
	normalized, _, err := NormalizeURL(rawURL)
	if err != nil {
		return Bookmark{}, ErrNotFound
	}
	b, err := scanBookmark(s.r.QueryRowContext(ctx,
		"SELECT "+bookmarkCols+" FROM bookmarks b WHERE b.user_id = ? AND b.url_hash = ?", userID, urlHash(normalized)))
	if err != nil {
		return Bookmark{}, err
	}
	return b, s.loadTags(ctx, []*Bookmark{&b})
}

func (s *Store) GetBookmark(ctx context.Context, userID, id int64) (Bookmark, error) {
	b, err := scanBookmark(s.r.QueryRowContext(ctx,
		"SELECT "+bookmarkCols+" FROM bookmarks b WHERE b.user_id = ? AND b.id = ?", userID, id))
	if err != nil {
		return Bookmark{}, err
	}
	return b, s.loadTags(ctx, []*Bookmark{&b})
}

// UpdateBookmark replaces a bookmark's editable fields. It returns
// ErrConflict if the new URL is already saved as another bookmark.
func (s *Store) UpdateBookmark(ctx context.Context, userID, id int64, in BookmarkInput) (Bookmark, error) {
	normalized, domain, err := in.clean()
	if err != nil {
		return Bookmark{}, err
	}
	err = s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE bookmarks SET url = ?, url_hash = ?, domain = ?, title = ?, description = ?, notes = ?,
			 is_unread = ?, is_favourite = ?, updated_at = ? WHERE user_id = ? AND id = ?`,
			normalized, urlHash(normalized), domain, in.Title, in.Description, in.Notes,
			in.IsUnread, in.IsFavourite, unix(s.now()), userID, id)
		if isUniqueViolation(err) {
			return ErrConflict
		}
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return setTags(ctx, tx, userID, id, in.Tags)
	})
	if err != nil {
		return Bookmark{}, err
	}
	return s.GetBookmark(ctx, userID, id)
}

func (s *Store) SetFlag(ctx context.Context, userID, id int64, flag Flag, on bool) error {
	col, ok := flagColumns[flag]
	if !ok {
		return fmt.Errorf("unknown flag %q", flag)
	}
	res, err := s.w.ExecContext(ctx, "UPDATE bookmarks SET "+col+" = ?, updated_at = ? WHERE user_id = ? AND id = ?",
		on, unix(s.now()), userID, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteBookmark(ctx context.Context, userID, id int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, "DELETE FROM bookmarks WHERE user_id = ? AND id = ?", userID, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return deleteOrphanTags(ctx, tx, userID)
	})
}

// Search returns one page of the user's bookmarks matching q, best match
// first for free-text queries and newest first otherwise. more reports
// whether another page follows.
func (s *Store) Search(ctx context.Context, userID int64, q search.Query, limit, offset int) (list []Bookmark, more bool, err error) {
	var (
		from  = "bookmarks b"
		where = []string{"b.user_id = ?"}
		args  []any
		order = "b.created_at DESC, b.id DESC"
	)
	if fts := q.FTS(); fts != "" {
		// Title weighted ×3 over description, notes, URL and tags.
		from = "bookmarks b JOIN bookmarks_fts ON bookmarks_fts.rowid = b.id"
		where = append(where, "bookmarks_fts MATCH ?")
		args = append(args, userID, fts)
		order = "bm25(bookmarks_fts, 3.0, 1.0, 1.0, 1.0, 1.0), " + order
	} else {
		args = append(args, userID)
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
	if q.Unread {
		where = append(where, "b.is_unread = 1")
	}
	if q.Fav {
		where = append(where, "b.is_favourite = 1")
	}
	if q.Shared {
		where = append(where, "b.share_token IS NOT NULL")
	}
	if !q.After.IsZero() {
		where = append(where, "b.created_at >= ?")
		args = append(args, unix(q.After))
	}
	if !q.Before.IsZero() {
		where = append(where, "b.created_at < ?")
		args = append(args, unix(q.Before))
	}
	args = append(args, limit+1, offset)

	rows, err := s.r.QueryContext(ctx,
		"SELECT "+bookmarkCols+" FROM "+from+" WHERE "+strings.Join(where, " AND ")+" ORDER BY "+order+" LIMIT ? OFFSET ?",
		args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	for rows.Next() {
		b, err := scanBookmark(rows)
		if err != nil {
			return nil, false, err
		}
		list = append(list, b)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(list) > limit {
		list, more = list[:limit], true
	}
	ptrs := make([]*Bookmark, len(list))
	for i := range list {
		ptrs[i] = &list[i]
	}
	return list, more, s.loadTags(ctx, ptrs)
}

// loadTags fills in Tags for bookmarks that were already loaded for one user.
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
