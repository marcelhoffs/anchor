package store

import (
	"context"
	"database/sql"
	"strings"
	"unicode"
	"unicode/utf8"
)

type Tag struct {
	ID    int64
	Name  string
	Count int
}

const maxTagLen = 64

// CleanTags splits raw tag input on commas and whitespace, strips a leading
// '#', drops empties and over-long names, and removes case-insensitive
// duplicates while keeping first-seen order and spelling.
func CleanTags(raw []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range raw {
		for _, t := range strings.FieldsFunc(r, func(c rune) bool { return c == ',' || unicode.IsSpace(c) }) {
			t = strings.TrimLeft(t, "#")
			key := strings.ToLower(t)
			if t == "" || utf8.RuneCountInString(t) > maxTagLen || seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, t)
		}
	}
	return out
}

// setTags replaces a bookmark's tags. The caller has already confirmed the
// bookmark belongs to userID.
func setTags(ctx context.Context, tx *sql.Tx, userID, bookmarkID int64, tags []string) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM bookmark_tags WHERE bookmark_id = ?", bookmarkID); err != nil {
		return err
	}
	for _, name := range tags {
		// Not an upsert: a no-op UPDATE of name would fire tags_fts_rename and
		// re-index every bookmark carrying the tag.
		if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO tags (user_id, name) VALUES (?, ?)", userID, name); err != nil {
			return err
		}
		var tagID int64
		if err := tx.QueryRowContext(ctx, "SELECT id FROM tags WHERE user_id = ? AND name = ?", userID, name).Scan(&tagID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO bookmark_tags (bookmark_id, tag_id) VALUES (?, ?)", bookmarkID, tagID); err != nil {
			return err
		}
	}
	return deleteOrphanTags(ctx, tx, userID)
}

func deleteOrphanTags(ctx context.Context, tx *sql.Tx, userID int64) error {
	_, err := tx.ExecContext(ctx,
		"DELETE FROM tags WHERE user_id = ? AND NOT EXISTS (SELECT 1 FROM bookmark_tags bt WHERE bt.tag_id = tags.id)", userID)
	return err
}

// ListTags returns the user's tags with bookmark counts, by name.
func (s *Store) ListTags(ctx context.Context, userID int64) ([]Tag, error) {
	rows, err := s.r.QueryContext(ctx,
		`SELECT t.id, t.name, count(bt.bookmark_id) FROM tags t
		 LEFT JOIN bookmark_tags bt ON bt.tag_id = t.id
		 WHERE t.user_id = ? GROUP BY t.id ORDER BY t.name`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Tag
	for rows.Next() {
		var t Tag
		if err := rows.Scan(&t.ID, &t.Name, &t.Count); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RenameTag renames a tag. If the user already has a tag with the new name,
// the two are merged into that one.
func (s *Store) RenameTag(ctx context.Context, userID, id int64, newName string) error {
	clean := CleanTags([]string{newName})
	if len(clean) != 1 {
		return invalid("Enter a single tag name without spaces or commas.")
	}
	newName = clean[0]
	return s.tx(ctx, func(tx *sql.Tx) error {
		var exists bool
		if err := tx.QueryRowContext(ctx, "SELECT 1 FROM tags WHERE user_id = ? AND id = ?", userID, id).Scan(&exists); err != nil {
			return notFound(err)
		}
		var targetID int64
		err := tx.QueryRowContext(ctx, "SELECT id FROM tags WHERE user_id = ? AND name = ? AND id != ?", userID, newName, id).Scan(&targetID)
		switch {
		case err == sql.ErrNoRows:
			_, err = tx.ExecContext(ctx, "UPDATE tags SET name = ? WHERE user_id = ? AND id = ?", newName, userID, id)
			return err
		case err != nil:
			return err
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT OR IGNORE INTO bookmark_tags (bookmark_id, tag_id) SELECT bookmark_id, ? FROM bookmark_tags WHERE tag_id = ?",
			targetID, id); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "DELETE FROM tags WHERE user_id = ? AND id = ?", userID, id)
		return err
	})
}

// DeleteTag removes a tag from all of the user's bookmarks.
func (s *Store) DeleteTag(ctx context.Context, userID, id int64) error {
	res, err := s.w.ExecContext(ctx, "DELETE FROM tags WHERE user_id = ? AND id = ?", userID, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
