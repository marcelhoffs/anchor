package store

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/marcelhoffs/anchor/internal/importexport"
)

// Job kinds.
const JobImport = "import"

// Job is a queued background task.
type Job struct {
	ID         int64
	UserID     int64
	SpaceID    int64 // the space an import goes into
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

const jobCols = `id, coalesce(user_id, 0), coalesce(space_id, 0), kind, status, progress, total, result, coalesce(last_error, ''), created_at,
	coalesce(finished_at, 0)`

func scanJob(row interface{ Scan(...any) error }) (Job, error) {
	var j Job
	var created, finished int64
	err := row.Scan(&j.ID, &j.UserID, &j.SpaceID, &j.Kind, &j.Status, &j.Progress, &j.Total, &j.Result, &j.LastError, &created, &finished)
	j.CreatedAt = fromUnix(created)
	if finished > 0 {
		j.FinishedAt = fromUnix(finished)
	}
	return j, notFound(err)
}

// CreateJob queues a job for the scope's user. Jobs change their space (an
// import adds bookmarks), so they need write access. A user has one job of
// a kind in the queue at a time: an import keeps its whole file until it
// ran, so queueing them without end would fill the disk.
func (s *Store) CreateJob(ctx context.Context, sc Scope, kind, payload string) (int64, error) {
	if err := sc.mustWrite(); err != nil {
		return 0, err
	}
	now := unix(s.now())
	var id int64
	err := s.w.QueryRowContext(ctx,
		`INSERT INTO jobs (user_id, space_id, kind, payload, run_after, created_at)
		 SELECT ?, ?, ?, ?, ?, ? WHERE NOT EXISTS
		   (SELECT 1 FROM jobs WHERE user_id = ? AND kind = ? AND status IN ('queued', 'running'))
		 RETURNING id`,
		sc.userID, sc.spaceID, kind, payload, now, now, sc.userID, kind).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, invalid("Your previous import is still running. Try again when it's done.")
	}
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

// maxJobAttempts is how often a job may be interrupted before it is given
// up: one that crashes the process would otherwise crash it on every start.
const maxJobAttempts = 3

// RequeueRunning puts jobs a crash or restart interrupted back in the queue,
// or fails them once they have been tried maxJobAttempts times.
func (s *Store) RequeueRunning(ctx context.Context) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`UPDATE jobs SET status = 'failed', last_error = 'Stopped before finishing, too many times.', payload = '', finished_at = ?
			 WHERE status = 'running' AND attempts >= ?`, unix(s.now()), maxJobAttempts); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "UPDATE jobs SET status = 'queued' WHERE status = 'running'")
		return err
	})
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

// ImportBookmarks adds items to a space in one transaction. A URL the space
// already has is merged rather than duplicated: its tags gain the imported
// ones, empty fields are filled in, and the flags can only be turned on.
func (s *Store) ImportBookmarks(ctx context.Context, sc Scope, items []importexport.Item) (ImportResult, error) {
	var r ImportResult
	if err := sc.mustWrite(); err != nil {
		return r, err
	}
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
				`INSERT INTO bookmarks (space_id, created_by, url, url_hash, domain, title, description, notes, is_favourite,
				                        is_archived, created_at, updated_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
				 ON CONFLICT (space_id, url_hash) DO NOTHING RETURNING id`,
				sc.spaceID, sc.userID, normalized, urlHash(normalized), domain, in.Title, in.Description, in.Notes, it.Favourite, it.Archived,
				unix(created), unix(updated)).Scan(&id)
			switch {
			case err == sql.ErrNoRows:
				if err := tx.QueryRowContext(ctx, "SELECT id FROM bookmarks WHERE space_id = ? AND url_hash = ?",
					sc.spaceID, urlHash(normalized)).Scan(&id); err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx,
					`UPDATE bookmarks SET
					   title = CASE WHEN title = '' THEN ? ELSE title END,
					   description = CASE WHEN description = '' THEN ? ELSE description END,
					   notes = CASE WHEN notes = '' THEN ? ELSE notes END,
					   is_favourite = is_favourite OR ?, is_archived = is_archived OR ?
					 WHERE id = ? AND space_id = ?`,
					in.Title, in.Description, in.Notes, it.Favourite, it.Archived, id, sc.spaceID); err != nil {
					return err
				}
				r.Merged++
			case err != nil:
				return err
			default:
				r.Created++
			}
			if err := addTags(ctx, tx, sc.spaceID, id, in.Tags); err != nil {
				return err
			}
		}
		return nil
	})
	return r, err
}

// addTags adds tags to a bookmark, keeping the ones it has.
func addTags(ctx context.Context, tx *sql.Tx, spaceID, bookmarkID int64, tags []string) error {
	for _, name := range tags {
		if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO tags (space_id, name) VALUES (?, ?)", spaceID, name); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO bookmark_tags (bookmark_id, tag_id)
			 SELECT ?, id FROM tags WHERE space_id = ? AND name = ?`, bookmarkID, spaceID, name); err != nil {
			return err
		}
	}
	return nil
}

// ExportBookmarks calls fn for each of the space's bookmarks, oldest first,
// streaming rather than loading them all.
func (s *Store) ExportBookmarks(ctx context.Context, sc Scope, fn func(importexport.Item) error) error {
	rows, err := s.r.QueryContext(ctx,
		`SELECT b.url, b.title, b.description, b.notes, b.is_favourite, b.is_archived, b.created_at, b.updated_at,
		        coalesce((SELECT group_concat(t.name, char(31)) FROM bookmark_tags bt JOIN tags t ON t.id = bt.tag_id
		                  WHERE bt.bookmark_id = b.id), '')
		 FROM bookmarks b WHERE b.space_id = ? ORDER BY b.created_at, b.id`, sc.spaceID)
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
