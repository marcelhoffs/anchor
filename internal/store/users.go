package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

type User struct {
	ID           int64
	Username     string
	PasswordHash string
	IsAdmin      bool
	IsActive     bool
	Settings     Settings
	CreatedAt    time.Time
}

// Settings are a user's preferences, stored as JSON in users.settings.
type Settings struct {
	Accent string `json:"accent,omitempty"` // one of Accents; "" = the first
	Theme  string `json:"theme,omitempty"`  // one of Themes; "" = the first
}

// Accents and Themes list the allowed values, the default first.
var (
	Accents = []string{"indigo", "terracotta"}
	Themes  = []string{"system", "light", "dark"}
)

// Normalized fills in defaults and replaces unknown values with them.
func (st Settings) Normalized() Settings {
	pick := func(v string, allowed []string) string {
		for _, a := range allowed {
			if v == a {
				return v
			}
		}
		return allowed[0]
	}
	return Settings{Accent: pick(st.Accent, Accents), Theme: pick(st.Theme, Themes)}
}

const userCols = "id, username, password_hash, is_admin, is_active, settings, created_at"

func scanUser(row interface{ Scan(...any) error }) (User, error) {
	var u User
	var settings string
	var created int64
	err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.IsAdmin, &u.IsActive, &settings, &created)
	if err == nil {
		// A malformed value just means defaults; it must not lock the user out.
		json.Unmarshal([]byte(settings), &u.Settings)
	}
	u.Settings = u.Settings.Normalized()
	u.CreatedAt = fromUnix(created)
	return u, notFound(err)
}

// SaveSettings stores a user's preferences. Unknown values are rejected.
func (s *Store) SaveSettings(ctx context.Context, userID int64, st Settings) error {
	if st.Normalized() != st {
		return invalid("Unknown appearance setting.")
	}
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	res, err := s.w.ExecContext(ctx, "UPDATE users SET settings = ?, updated_at = ? WHERE id = ?", string(b), unix(s.now()), userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.r.QueryRowContext(ctx, "SELECT count(*) FROM users").Scan(&n)
	return n, err
}

func (s *Store) CreateUser(ctx context.Context, username, passwordHash string, isAdmin bool) (User, error) {
	now := unix(s.now())
	row := s.w.QueryRowContext(ctx,
		"INSERT INTO users (username, password_hash, is_admin, created_at, updated_at) VALUES (?, ?, ?, ?, ?) RETURNING "+userCols,
		username, passwordHash, isAdmin, now, now)
	u, err := scanUser(row)
	if isUniqueViolation(err) {
		return User{}, ErrConflict
	}
	return u, err
}

// CreateFirstAdmin creates an admin only while the users table is empty, so
// two concurrent /setup submissions cannot both succeed. It returns
// ErrConflict once any user exists.
func (s *Store) CreateFirstAdmin(ctx context.Context, username, passwordHash string) (User, error) {
	now := unix(s.now())
	row := s.w.QueryRowContext(ctx,
		`INSERT INTO users (username, password_hash, is_admin, created_at, updated_at)
		 SELECT ?, ?, 1, ?, ? WHERE NOT EXISTS (SELECT 1 FROM users) RETURNING `+userCols,
		username, passwordHash, now, now)
	u, err := scanUser(row)
	if err == ErrNotFound {
		return User{}, ErrConflict
	}
	return u, err
}

func (s *Store) UserByUsername(ctx context.Context, username string) (User, error) {
	return scanUser(s.r.QueryRowContext(ctx, "SELECT "+userCols+" FROM users WHERE username = ?", username))
}

func (s *Store) UserByID(ctx context.Context, id int64) (User, error) {
	return scanUser(s.r.QueryRowContext(ctx, "SELECT "+userCols+" FROM users WHERE id = ?", id))
}

// SetPassword changes a user's password and revokes all of their sessions
// except keepSession (pass nil to revoke every session).
func (s *Store) SetPassword(ctx context.Context, userID int64, passwordHash string, keepSession []byte) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, "UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ?",
			passwordHash, unix(s.now()), userID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		_, err = tx.ExecContext(ctx, "DELETE FROM sessions WHERE user_id = ? AND token_hash IS NOT ?", userID, keepSession)
		return err
	})
}
