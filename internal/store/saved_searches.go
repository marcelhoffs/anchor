package store

import (
	"context"
	"strings"
	"unicode/utf8"
)

// SavedSearch is a search kept in the sidebar.
type SavedSearch struct {
	ID    int64
	Name  string
	Query string
}

const (
	maxSavedSearchName  = 100
	maxSavedSearchQuery = 1000
)

func cleanSavedSearchName(name string) (string, error) {
	name = strings.TrimSpace(name)
	switch {
	case name == "":
		return "", invalid("Give the saved search a name.")
	case utf8.RuneCountInString(name) > maxSavedSearchName:
		return "", invalid("Saved search names are at most %d characters.", maxSavedSearchName)
	}
	return name, nil
}

func cleanSavedSearch(name, query string) (string, string, error) {
	name, err := cleanSavedSearchName(name)
	if err != nil {
		return "", "", err
	}
	query = strings.TrimSpace(query)
	switch {
	case query == "":
		return "", "", invalid("A saved search needs a query.")
	case utf8.RuneCountInString(query) > maxSavedSearchQuery:
		return "", "", invalid("The search is longer than %d characters.", maxSavedSearchQuery)
	}
	return name, query, nil
}

// Saved searches belong to one person in one space. Reading the space is
// enough to keep them: they change nothing in it.

func (s *Store) ListSavedSearches(ctx context.Context, sc Scope) ([]SavedSearch, error) {
	rows, err := s.r.QueryContext(ctx,
		"SELECT id, name, query FROM saved_searches WHERE space_id = ? AND user_id = ? ORDER BY name COLLATE NOCASE, id",
		sc.spaceID, sc.userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SavedSearch
	for rows.Next() {
		var b SavedSearch
		if err := rows.Scan(&b.ID, &b.Name, &b.Query); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *Store) CreateSavedSearch(ctx context.Context, sc Scope, name, query string) (SavedSearch, error) {
	name, query, err := cleanSavedSearch(name, query)
	if err != nil {
		return SavedSearch{}, err
	}
	b := SavedSearch{Name: name, Query: query}
	if sc.spaceID == 0 {
		return SavedSearch{}, ErrNotFound
	}
	err = s.w.QueryRowContext(ctx, "INSERT INTO saved_searches (space_id, user_id, name, query) VALUES (?, ?, ?, ?) RETURNING id",
		sc.spaceID, sc.userID, name, query).Scan(&b.ID)
	return b, err
}

func (s *Store) RenameSavedSearch(ctx context.Context, sc Scope, id int64, name string) error {
	name, err := cleanSavedSearchName(name)
	if err != nil {
		return err
	}
	res, err := s.w.ExecContext(ctx, "UPDATE saved_searches SET name = ? WHERE space_id = ? AND user_id = ? AND id = ?",
		name, sc.spaceID, sc.userID, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteSavedSearch(ctx context.Context, sc Scope, id int64) error {
	res, err := s.w.ExecContext(ctx, "DELETE FROM saved_searches WHERE space_id = ? AND user_id = ? AND id = ?",
		sc.spaceID, sc.userID, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
