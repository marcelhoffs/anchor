package store

import (
	"context"
	"strings"
	"time"
)

// APIToken is a personal access token. Only its hash is stored.
type APIToken struct {
	ID         int64
	UserID     int64
	Name       string
	Scopes     []string // "read", "write"
	CreatedAt  time.Time
	LastUsedAt time.Time // zero if never
	ExpiresAt  time.Time // zero = never
}

func (t APIToken) Can(scope string) bool {
	for _, s := range t.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

const tokenCols = "id, user_id, name, scopes, created_at, coalesce(last_used_at, 0), coalesce(expires_at, 0)"

func scanToken(row interface{ Scan(...any) error }) (APIToken, error) {
	var t APIToken
	var scopes string
	var created, used, expires int64
	err := row.Scan(&t.ID, &t.UserID, &t.Name, &scopes, &created, &used, &expires)
	t.Scopes = strings.Split(scopes, ",")
	t.CreatedAt = fromUnix(created)
	if used > 0 {
		t.LastUsedAt = fromUnix(used)
	}
	if expires > 0 {
		t.ExpiresAt = fromUnix(expires)
	}
	return t, notFound(err)
}

func (s *Store) CreateAPIToken(ctx context.Context, userID int64, name string, hash []byte, scopes []string, expires time.Time) (APIToken, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 100 {
		return APIToken{}, invalid("Give the token a name (up to 100 characters).")
	}
	var exp any
	if !expires.IsZero() {
		exp = unix(expires)
	}
	return scanToken(s.w.QueryRowContext(ctx,
		`INSERT INTO api_tokens (user_id, name, token_hash, scopes, expires_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?) RETURNING `+tokenCols,
		userID, name, hash, strings.Join(scopes, ","), exp, unix(s.now())))
}

func (s *Store) ListAPITokens(ctx context.Context, userID int64) ([]APIToken, error) {
	rows, err := s.r.QueryContext(ctx, "SELECT "+tokenCols+" FROM api_tokens WHERE user_id = ? ORDER BY created_at DESC", userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIToken
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) DeleteAPIToken(ctx context.Context, userID, id int64) error {
	res, err := s.w.ExecContext(ctx, "DELETE FROM api_tokens WHERE user_id = ? AND id = ?", userID, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// TokenByHash returns an unexpired token and its active owner.
func (s *Store) TokenByHash(ctx context.Context, hash []byte) (APIToken, User, error) {
	t, err := scanToken(s.r.QueryRowContext(ctx,
		"SELECT "+tokenCols+" FROM api_tokens WHERE token_hash = ? AND (expires_at IS NULL OR expires_at > ?)",
		hash, unix(s.now())))
	if err != nil {
		return APIToken{}, User{}, err
	}
	u, err := s.UserByID(ctx, t.UserID)
	if err != nil {
		return APIToken{}, User{}, err
	}
	if !u.IsActive {
		return APIToken{}, User{}, ErrNotFound
	}
	return t, u, nil
}

// TouchAPIToken records use, at most once a minute per token.
func (s *Store) TouchAPIToken(ctx context.Context, id int64) error {
	now := unix(s.now())
	_, err := s.w.ExecContext(ctx,
		"UPDATE api_tokens SET last_used_at = ? WHERE id = ? AND coalesce(last_used_at, 0) < ?", now, id, now-60)
	return err
}
