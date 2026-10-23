package store

import (
	"context"
	"strings"
	"unicode/utf8"
)

// Bundle is a saved search, listed in the sidebar.
type Bundle struct {
	ID    int64
	Name  string
	Query string
}

const (
	maxBundleName  = 100
	maxBundleQuery = 1000
)

func cleanBundle(name, query string) (string, string, error) {
	name, query = strings.TrimSpace(name), strings.TrimSpace(query)
	switch {
	case name == "":
		return "", "", invalid("Give the bundle a name.")
	case query == "":
		return "", "", invalid("A bundle needs a search.")
	case utf8.RuneCountInString(name) > maxBundleName:
		return "", "", invalid("Bundle names are at most %d characters.", maxBundleName)
	case utf8.RuneCountInString(query) > maxBundleQuery:
		return "", "", invalid("The search is longer than %d characters.", maxBundleQuery)
	}
	return name, query, nil
}

// Bundles are one person's saved searches in one space. Reading the space is
// enough to keep them: they change nothing in it.

func (s *Store) ListBundles(ctx context.Context, sc Scope) ([]Bundle, error) {
	rows, err := s.r.QueryContext(ctx,
		"SELECT id, name, query FROM bundles WHERE space_id = ? AND user_id = ? ORDER BY position, name COLLATE NOCASE, id",
		sc.spaceID, sc.userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Bundle
	for rows.Next() {
		var b Bundle
		if err := rows.Scan(&b.ID, &b.Name, &b.Query); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *Store) CreateBundle(ctx context.Context, sc Scope, name, query string) (Bundle, error) {
	name, query, err := cleanBundle(name, query)
	if err != nil {
		return Bundle{}, err
	}
	b := Bundle{Name: name, Query: query}
	if sc.spaceID == 0 {
		return Bundle{}, ErrNotFound
	}
	err = s.w.QueryRowContext(ctx, "INSERT INTO bundles (space_id, user_id, name, query) VALUES (?, ?, ?, ?) RETURNING id",
		sc.spaceID, sc.userID, name, query).Scan(&b.ID)
	return b, err
}

func (s *Store) UpdateBundle(ctx context.Context, sc Scope, id int64, name, query string) error {
	name, query, err := cleanBundle(name, query)
	if err != nil {
		return err
	}
	res, err := s.w.ExecContext(ctx, "UPDATE bundles SET name = ?, query = ? WHERE space_id = ? AND user_id = ? AND id = ?",
		name, query, sc.spaceID, sc.userID, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteBundle(ctx context.Context, sc Scope, id int64) error {
	res, err := s.w.ExecContext(ctx, "DELETE FROM bundles WHERE space_id = ? AND user_id = ? AND id = ?",
		sc.spaceID, sc.userID, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
