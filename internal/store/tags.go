package store

import (
	"context"
	"database/sql"
	"hash/fnv"
	"slices"
	"strings"
	"unicode/utf8"
)

type Tag struct {
	ID    int64
	Name  string
	Color string // one of TagColors
	Count int
}

const maxTagLen = 64

// TagColors are the colours a tag can have (see [data-color] in style.css).
var TagColors = []string{"blue", "teal", "green", "amber", "orange", "rose", "purple", "slate"}

// TagColor is a tag's colour: the one chosen for it, or else one picked
// from its name, so every tag has one without anyone choosing.
func TagColor(name, chosen string) string {
	if slices.Contains(TagColors, chosen) {
		return chosen
	}
	h := fnv.New32a()
	h.Write([]byte(strings.ToLower(name)))
	return TagColors[h.Sum32()%uint32(len(TagColors))]
}

// CleanTagName tidies one tag name: it trims it, drops a leading '#' and
// collapses runs of whitespace to one space. Names may contain spaces but
// not commas (which separate tags) or double quotes (which delimit a tag
// with spaces in a search, as in #"home lab").
func CleanTagName(s string) (string, error) {
	s = strings.Join(strings.Fields(strings.TrimLeft(strings.TrimSpace(s), "#")), " ")
	switch {
	case s == "":
		return "", invalid("Enter a tag name.")
	case strings.ContainsAny(s, `,"`):
		return "", invalid("Tag names can't contain commas or double quotes.")
	case utf8.RuneCountInString(s) > maxTagLen:
		return "", invalid("Tag names are at most %d characters.", maxTagLen)
	}
	return s, nil
}

// ParseTags splits raw tag input on commas, tidies each name and removes
// case-insensitive duplicates, keeping first-seen order and spelling. An
// invalid name is an error; empty entries are skipped.
func ParseTags(raw []string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, r := range raw {
		for _, part := range strings.Split(r, ",") {
			if strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(part), "#")) == "" {
				continue
			}
			name, err := CleanTagName(part)
			if err != nil {
				return nil, err
			}
			if key := strings.ToLower(name); !seen[key] {
				seen[key] = true
				out = append(out, name)
			}
		}
	}
	return out, nil
}

// CleanTags is ParseTags for input that can't be corrected, such as an
// import: invalid names are dropped instead of failing.
func CleanTags(raw []string) []string {
	var valid []string
	for _, r := range raw {
		for _, part := range strings.Split(r, ",") {
			if name, err := CleanTagName(part); err == nil {
				valid = append(valid, name)
			}
		}
	}
	tags, _ := ParseTags(valid) // can't fail now; removes duplicates
	return tags
}

// setTags replaces a bookmark's tags. Tags must already exist: they are
// created on the Tags page (or by an import), never by tagging a bookmark.
// The caller has already confirmed the bookmark belongs to spaceID.
func setTags(ctx context.Context, tx *sql.Tx, spaceID, bookmarkID int64, tags []string) error {
	ids := make([]int64, 0, len(tags))
	var missing []string
	for _, name := range tags {
		var id int64
		err := tx.QueryRowContext(ctx, "SELECT id FROM tags WHERE space_id = ? AND name = ?", spaceID, name).Scan(&id)
		switch {
		case err == sql.ErrNoRows:
			missing = append(missing, `"`+name+`"`)
		case err != nil:
			return err
		default:
			ids = append(ids, id)
		}
	}
	switch len(missing) {
	case 0:
	case 1:
		return invalid("There's no tag %s yet. Create it under Tags first.", missing[0])
	default:
		return invalid("There are no tags %s yet. Create them under Tags first.", strings.Join(missing, ", "))
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM bookmark_tags WHERE bookmark_id = ?", bookmarkID); err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO bookmark_tags (bookmark_id, tag_id) VALUES (?, ?)", bookmarkID, id); err != nil {
			return err
		}
	}
	return nil
}

// AddTag puts an existing tag on a bookmark, both in sc's space. It reports
// whether the bookmark didn't have the tag yet.
func (s *Store) AddTag(ctx context.Context, sc Scope, bookmarkID, tagID int64) (bool, error) {
	if err := sc.mustWrite(); err != nil {
		return false, err
	}
	var added bool
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var ok bool
		if err := tx.QueryRowContext(ctx, "SELECT 1 FROM bookmarks WHERE space_id = ? AND id = ?", sc.spaceID, bookmarkID).Scan(&ok); err != nil {
			return notFound(err)
		}
		if err := tx.QueryRowContext(ctx, "SELECT 1 FROM tags WHERE space_id = ? AND id = ?", sc.spaceID, tagID).Scan(&ok); err != nil {
			return notFound(err)
		}
		res, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO bookmark_tags (bookmark_id, tag_id) VALUES (?, ?)", bookmarkID, tagID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			added = true
			_, err = tx.ExecContext(ctx, "UPDATE bookmarks SET updated_at = ? WHERE id = ?", unix(s.now()), bookmarkID)
		}
		return err
	})
	return added, err
}

// CreateTag adds a tag, ready to be put on bookmarks.
func (s *Store) CreateTag(ctx context.Context, sc Scope, name string) (Tag, error) {
	if err := sc.mustWrite(); err != nil {
		return Tag{}, err
	}
	name, err := CleanTagName(name)
	if err != nil {
		return Tag{}, err
	}
	t := Tag{Name: name, Color: TagColor(name, "")}
	err = s.w.QueryRowContext(ctx, "INSERT INTO tags (space_id, name) VALUES (?, ?) RETURNING id", sc.spaceID, name).Scan(&t.ID)
	if isUniqueViolation(err) {
		return Tag{}, invalid("There's already a tag called %q.", name)
	}
	return t, err
}

// ListTags returns the space's tags with bookmark counts, by name.
func (s *Store) ListTags(ctx context.Context, sc Scope) ([]Tag, error) {
	rows, err := s.r.QueryContext(ctx,
		`SELECT t.id, t.name, t.color, count(bt.bookmark_id) FROM tags t
		 LEFT JOIN bookmark_tags bt ON bt.tag_id = t.id
		 WHERE t.space_id = ? GROUP BY t.id ORDER BY t.name`, sc.spaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Tag
	for rows.Next() {
		var t Tag
		if err := rows.Scan(&t.ID, &t.Name, &t.Color, &t.Count); err != nil {
			return nil, err
		}
		t.Color = TagColor(t.Name, t.Color)
		out = append(out, t)
	}
	return out, rows.Err()
}

// SetTagColor gives a tag one of TagColors.
func (s *Store) SetTagColor(ctx context.Context, sc Scope, id int64, color string) error {
	if err := sc.mustWrite(); err != nil {
		return err
	}
	if !slices.Contains(TagColors, color) {
		return invalid("Pick one of the tag colours.")
	}
	return affected(s.w.ExecContext(ctx, "UPDATE tags SET color = ? WHERE space_id = ? AND id = ?", color, sc.spaceID, id))
}

// RenameTag renames a tag. If the space already has a tag with the new name,
// the two are merged into that one.
func (s *Store) RenameTag(ctx context.Context, sc Scope, id int64, newName string) error {
	if err := sc.mustWrite(); err != nil {
		return err
	}
	newName, err := CleanTagName(newName)
	if err != nil {
		return err
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		var exists bool
		if err := tx.QueryRowContext(ctx, "SELECT 1 FROM tags WHERE space_id = ? AND id = ?", sc.spaceID, id).Scan(&exists); err != nil {
			return notFound(err)
		}
		var targetID int64
		err := tx.QueryRowContext(ctx, "SELECT id FROM tags WHERE space_id = ? AND name = ? AND id != ?", sc.spaceID, newName, id).Scan(&targetID)
		switch {
		case err == sql.ErrNoRows:
			_, err = tx.ExecContext(ctx, "UPDATE tags SET name = ? WHERE space_id = ? AND id = ?", newName, sc.spaceID, id)
			return err
		case err != nil:
			return err
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT OR IGNORE INTO bookmark_tags (bookmark_id, tag_id) SELECT bookmark_id, ? FROM bookmark_tags WHERE tag_id = ?",
			targetID, id); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "DELETE FROM tags WHERE space_id = ? AND id = ?", sc.spaceID, id)
		return err
	})
}

// DeleteTag removes a tag from all of the space's bookmarks. Tags are only
// ever removed this way: a tag no bookmark uses yet is kept.
func (s *Store) DeleteTag(ctx context.Context, sc Scope, id int64) error {
	if err := sc.mustWrite(); err != nil {
		return err
	}
	return affected(s.w.ExecContext(ctx, "DELETE FROM tags WHERE space_id = ? AND id = ?", sc.spaceID, id))
}
