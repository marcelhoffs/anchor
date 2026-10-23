package store

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/marcelhoffs/burrow/internal/importexport"
)

// Job kinds.
const JobImport = "import"

// Job is a queued background task.
type Job struct {
	ID         int64
	UserID     int64
	Kind       string
	Status     string // queued | running | done | failed
	Progress   int
	Total      int
	Result     string // JSON summary when done
	LastError  string
	CreatedAt  time.Time
	FinishedAt time.Time // zero while unfinished
}

func (j Job) Finished() bool { return j.Status == "done" || j.Status == "failed" }

const jobCols = `id, coalesce(user_id, 0), kind, status, progress, total, result, coalesce(last_error, ''), created_at,
	coalesce(finished_at, 0)`

func scanJob(row interface{ Scan(...any) error }) (Job, error) {
	var j Job
	var created, finished int64
	err := row.Scan(&j.ID, &j.UserID, &j.Kind, &j.Status, &j.Progress, &j.Total, &j.Result, &j.LastError, &created, &finished)
	j.CreatedAt = fromUnix(created)
	if finished > 0 {
		j.FinishedAt = fromUnix(finished)
	}
	return j, notFound(err)
}

func (s *Store) CreateJob(ctx context.Context, userID int64, kind, payload string) (int64, error) {
	now := unix(s.now())
	var id int64
	err := s.w.QueryRowContext(ctx,
		"INSERT INTO jobs (user_id, kind, payload, run_after, created_at) VALUES (?, ?, ?, ?, ?) RETURNING id",
		userID, kind, payload, now, now).Scan(&id)
	return id, err
}

// ClaimJob marks the oldest due job as running and returns it with its
// payload; ErrNotFound when the queue is empty.
func (s *Store) ClaimJob(ctx context.Context) (Job, string, error) {
	var payload string
	var j Job
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var err error
		j, err = scanJob(tx.QueryRowContext(ctx,
			`UPDATE jobs SET status = 'running', attempts = attempts + 1
			 WHERE id = (SELECT id FROM jobs WHERE status = 'queued' AND run_after <= ? ORDER BY id LIMIT 1)
			 RETURNING `+jobCols, unix(s.now())))
		if err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, "SELECT payload FROM jobs WHERE id = ?", j.ID).Scan(&payload)
	})
	return j, payload, err
}

// RequeueRunning puts jobs a crash or restart interrupted back in the queue.
func (s *Store) RequeueRunning(ctx context.Context) error {
	_, err := s.w.ExecContext(ctx, "UPDATE jobs SET status = 'queued' WHERE status = 'running'")
	return err
}

func (s *Store) JobProgress(ctx context.Context, id int64, progress, total int) error {
	_, err := s.w.ExecContext(ctx, "UPDATE jobs SET progress = ?, total = ? WHERE id = ?", progress, total, id)
	return err
}

// FinishJob records the outcome and drops the payload, which for imports is
// the whole uploaded file.
func (s *Store) FinishJob(ctx context.Context, id int64, result string, jobErr error) error {
	status, lastErr := "done", ""
	if jobErr != nil {
		status, lastErr = "failed", jobErr.Error()
	}
	_, err := s.w.ExecContext(ctx,
		"UPDATE jobs SET status = ?, result = ?, last_error = ?, payload = '', finished_at = ? WHERE id = ?",
		status, result, lastErr, unix(s.now()), id)
	return err
}

func (s *Store) ListJobs(ctx context.Context, userID int64, limit int) ([]Job, error) {
	rows, err := s.r.QueryContext(ctx, "SELECT "+jobCols+" FROM jobs WHERE user_id = ? ORDER BY id DESC LIMIT ?", userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (s *Store) GetJob(ctx context.Context, userID, id int64) (Job, error) {
	return scanJob(s.r.QueryRowContext(ctx, "SELECT "+jobCols+" FROM jobs WHERE user_id = ? AND id = ?", userID, id))
}

// ImportResult counts what an import did.
type ImportResult struct {
	Created int `json:"created"`
	Merged  int `json:"merged"`
	Skipped int `json:"skipped"` // not http(s), or otherwise invalid
}

// ImportBookmarks adds items for a user in one transaction. A URL the user
// already has is merged rather than duplicated: its tags gain the imported
// ones, empty fields are filled in, and the flags can only be turned on.
func (s *Store) ImportBookmarks(ctx context.Context, userID int64, items []importexport.Item) (ImportResult, error) {
	var r ImportResult
	err := s.tx(ctx, func(tx *sql.Tx) error {
		now := s.now()
		for _, it := range items {
			// An import creates the tags it brings (addTags below); names
			// that aren't valid tags are dropped rather than failing the item.
			in := BookmarkInput{URL: it.URL, Title: it.Title, Description: it.Description, Notes: it.Notes,
				Tags: CleanTags(it.Tags), IsFavourite: it.Favourite}
			normalized, domain, err := in.clean()
			if err != nil {
				r.Skipped++
				continue
			}
			created, updated := it.CreatedAt, it.UpdatedAt
			if created.IsZero() || created.After(now) {
				created = now
			}
			if updated.Before(created) {
				updated = created
			}
			var id int64
			err = tx.QueryRowContext(ctx,
				`INSERT INTO bookmarks (user_id, url, url_hash, domain, title, description, notes, is_favourite, is_archived,
				                        created_at, updated_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
				 ON CONFLICT (user_id, url_hash) DO NOTHING RETURNING id`,
				userID, normalized, urlHash(normalized), domain, in.Title, in.Description, in.Notes, it.Favourite, it.Archived,
				unix(created), unix(updated)).Scan(&id)
			switch {
			case err == sql.ErrNoRows:
				if err := tx.QueryRowContext(ctx, "SELECT id FROM bookmarks WHERE user_id = ? AND url_hash = ?",
					userID, urlHash(normalized)).Scan(&id); err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx,
					`UPDATE bookmarks SET
					   title = CASE WHEN title = '' THEN ? ELSE title END,
					   description = CASE WHEN description = '' THEN ? ELSE description END,
					   notes = CASE WHEN notes = '' THEN ? ELSE notes END,
					   is_favourite = is_favourite OR ?, is_archived = is_archived OR ?
					 WHERE id = ? AND user_id = ?`,
					in.Title, in.Description, in.Notes, it.Favourite, it.Archived, id, userID); err != nil {
					return err
				}
				r.Merged++
			case err != nil:
				return err
			default:
				r.Created++
			}
			if err := addTags(ctx, tx, userID, id, in.Tags); err != nil {
				return err
			}
		}
		return nil
	})
	return r, err
}

// addTags adds tags to a bookmark, keeping the ones it has.
func addTags(ctx context.Context, tx *sql.Tx, userID, bookmarkID int64, tags []string) error {
	for _, name := range tags {
		if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO tags (user_id, name) VALUES (?, ?)", userID, name); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO bookmark_tags (bookmark_id, tag_id)
			 SELECT ?, id FROM tags WHERE user_id = ? AND name = ?`, bookmarkID, userID, name); err != nil {
			return err
		}
	}
	return nil
}

// ExportBookmarks calls fn for each of the user's bookmarks, oldest first,
// streaming rather than loading them all.
func (s *Store) ExportBookmarks(ctx context.Context, userID int64, fn func(importexport.Item) error) error {
	rows, err := s.r.QueryContext(ctx,
		`SELECT b.url, b.title, b.description, b.notes, b.is_favourite, b.is_archived, b.created_at, b.updated_at,
		        coalesce((SELECT group_concat(t.name, char(31)) FROM bookmark_tags bt JOIN tags t ON t.id = bt.tag_id
		                  WHERE bt.bookmark_id = b.id), '')
		 FROM bookmarks b WHERE b.user_id = ? ORDER BY b.created_at, b.id`, userID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var it importexport.Item
		var created, updated int64
		var tags string
		if err := rows.Scan(&it.URL, &it.Title, &it.Description, &it.Notes, &it.Favourite, &it.Archived,
			&created, &updated, &tags); err != nil {
			return err
		}
		it.CreatedAt, it.UpdatedAt = fromUnix(created), fromUnix(updated)
		if tags != "" {
			it.Tags = strings.Split(tags, "\x1f")
		}
		if err := fn(it); err != nil {
			return err
		}
	}
	return rows.Err()
}
