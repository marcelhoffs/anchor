package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/marcelhoffs/anchor/internal/i18n"
)

type User struct {
	ID           int64
	Username     string
	PasswordHash string
	IsAdmin      bool
	IsActive     bool
	Settings     Settings
	CreatedAt    time.Time
	LastLoginAt  time.Time // zero if never

	// TOTPSecret is sealed (see auth.Keys); it is set as soon as enrolment
	// starts, and only counts once TOTPEnabled.
	TOTPSecret   []byte
	TOTPEnabled  bool
	TOTPLastStep int64
}

// Settings are a user's preferences, stored as JSON in users.settings.
type Settings struct {
	Accent string `json:"accent,omitempty"` // one of Accents; "" = the first
	Theme  string `json:"theme,omitempty"`  // one of Themes; "" = the first
	Font   string `json:"font,omitempty"`   // one of Fonts; "" = the first
	// Language is the interface language; "auto" follows the browser.
	Language string `json:"language,omitempty"` // one of Languages; "" = the first
	// Sidebar is where the views and tags sit next to the bookmark list.
	Sidebar string `json:"sidebar,omitempty"` // one of Sidebars; "" = the first
	// Sort is the bookmark list order: newest first or by title.
	Sort string `json:"sort,omitempty"` // one of Sorts; "" = the first
	// Descriptions shows or hides bookmark descriptions in the list.
	Descriptions string `json:"descriptions,omitempty"` // one of DescriptionModes; "" = the first
}

// Accents, Themes, Fonts, Languages, Sidebars, Sorts and DescriptionModes list the allowed
// values, the default first.
var (
	Accents          = []string{"indigo", "terracotta", "yellow", "emerald", "forest", "ocean", "rose", "graphite"}
	Themes           = []string{"system", "light", "dark"}
	Fonts            = []string{"inter", "system", "roboto", "open-sans", "lato"}
	Languages        = languages()
	Sidebars         = []string{"left", "right"}
	Sorts            = []string{SortNewest, SortTitle}
	DescriptionModes = []string{"show", "hide"}
)

// languages are "auto" (follow the browser) and every language the
// interface speaks.
func languages() []string {
	out := []string{"auto"}
	for _, l := range i18n.Langs {
		out = append(out, string(l))
	}
	return out
}

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
	return Settings{
		Accent: pick(st.Accent, Accents), Theme: pick(st.Theme, Themes), Font: pick(st.Font, Fonts),
		Language: pick(st.Language, Languages),
		Sidebar:  pick(st.Sidebar, Sidebars), Sort: pick(st.Sort, Sorts), Descriptions: pick(st.Descriptions, DescriptionModes),
	}
}

const userCols = `id, username, password_hash, is_admin, is_active, settings, created_at, coalesce(last_login_at, 0),
	totp_secret, totp_enabled, totp_last_step`

func scanUser(row interface{ Scan(...any) error }) (User, error) {
	var u User
	var settings string
	var created, lastLogin int64
	err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.IsAdmin, &u.IsActive, &settings, &created, &lastLogin,
		&u.TOTPSecret, &u.TOTPEnabled, &u.TOTPLastStep)
	if err == nil {
		// A malformed value just means defaults; it must not lock the user out.
		json.Unmarshal([]byte(settings), &u.Settings)
	}
	u.Settings = u.Settings.Normalized()
	u.CreatedAt = fromUnix(created)
	if lastLogin > 0 {
		u.LastLoginAt = fromUnix(lastLogin)
	}
	return u, notFound(err)
}

// SaveSettings stores a user's preferences. Unknown values are rejected.
func (s *Store) SaveSettings(ctx context.Context, userID int64, st Settings) error {
	if st.Normalized() != st {
		return invalid("Unknown setting.")
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

// RecordLogin stamps a successful login.
func (s *Store) RecordLogin(ctx context.Context, userID int64) error {
	_, err := s.w.ExecContext(ctx, "UPDATE users SET last_login_at = ? WHERE id = ?", unix(s.now()), userID)
	return err
}

// UserSummary is a row of the admin's user list: account facts only, never
// the user's bookmarks.
type UserSummary struct {
	User
	Passkeys int
}

func (s *Store) ListUsers(ctx context.Context) ([]UserSummary, error) {
	rows, err := s.r.QueryContext(ctx, "SELECT "+userCols+`,
		(SELECT count(*) FROM webauthn_credentials w WHERE w.user_id = users.id)
		FROM users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UserSummary
	for rows.Next() {
		var us UserSummary
		var n int
		u, err := scanUser(scanTail{rows, &n})
		if err != nil {
			return nil, err
		}
		us.User, us.Passkeys = u, n
		out = append(out, us)
	}
	return out, rows.Err()
}

// scanTail scans extra trailing columns after the ones a scan function reads.
type scanTail struct {
	row  interface{ Scan(...any) error }
	tail *int
}

func (st scanTail) Scan(dest ...any) error { return st.row.Scan(append(dest, st.tail)...) }

// SetActive enables or disables an account. Disabling signs it out
// everywhere; its API tokens stop working because lookups check is_active.
func (s *Store) SetActive(ctx context.Context, userID int64, active bool) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if !active {
			if err := lastAdminGuard(ctx, tx, userID); err != nil {
				return err
			}
		}
		res, err := tx.ExecContext(ctx, "UPDATE users SET is_active = ?, updated_at = ? WHERE id = ?", active, unix(s.now()), userID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		if !active {
			_, err = tx.ExecContext(ctx, "DELETE FROM sessions WHERE user_id = ?", userID)
		}
		return err
	})
}

func (s *Store) SetAdmin(ctx context.Context, userID int64, admin bool) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if !admin {
			if err := lastAdminGuard(ctx, tx, userID); err != nil {
				return err
			}
		}
		res, err := tx.ExecContext(ctx, "UPDATE users SET is_admin = ?, updated_at = ? WHERE id = ?", admin, unix(s.now()), userID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// DeleteUser deletes an account for good, with its personal space and
// everything in it. Shared spaces it owned go to heirID, an active admin
// (0 = the longest-standing one); bookmarks it added to shared spaces stay.
// It returns how many shared spaces changed owner.
func (s *Store) DeleteUser(ctx context.Context, userID, heirID int64) (int, error) {
	var moved int
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if err := lastAdminGuard(ctx, tx, userID); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx,
			`SELECT m.space_id FROM space_members m JOIN spaces sp ON sp.id = m.space_id
			 WHERE m.user_id = ? AND m.role = 'owner' AND sp.personal_of IS NULL`, userID)
		if err != nil {
			return err
		}
		var owned []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			owned = append(owned, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(owned) > 0 {
			err := tx.QueryRowContext(ctx,
				`SELECT id FROM users WHERE is_admin = 1 AND is_active = 1 AND id != ?
				 ORDER BY id != ? , id LIMIT 1`, userID, heirID).Scan(&heirID)
			if errors.Is(err, sql.ErrNoRows) {
				return invalid("Anchor needs an active admin to take over this user's shared spaces.")
			}
			if err != nil {
				return err
			}
		}
		now := unix(s.now())
		for _, id := range owned {
			// One owner per space: the old owner steps out before the heir
			// steps up, joining if they weren't a member.
			if _, err := tx.ExecContext(ctx, "DELETE FROM space_members WHERE space_id = ? AND user_id = ?", id, userID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO space_members (space_id, user_id, role, created_at) VALUES (?, ?, 'owner', ?)
				 ON CONFLICT (space_id, user_id) DO UPDATE SET role = 'owner'`, id, heirID, now); err != nil {
				return err
			}
		}
		moved = len(owned)
		return affected(tx.ExecContext(ctx, "DELETE FROM users WHERE id = ?", userID))
	})
	return moved, err
}

// lastAdminGuard refuses to take away the last active admin.
func lastAdminGuard(ctx context.Context, tx *sql.Tx, userID int64) error {
	var others int
	err := tx.QueryRowContext(ctx,
		"SELECT count(*) FROM users WHERE is_admin = 1 AND is_active = 1 AND id != ?", userID).Scan(&others)
	if err != nil {
		return err
	}
	var isAdmin bool
	if err := tx.QueryRowContext(ctx, "SELECT is_admin FROM users WHERE id = ?", userID).Scan(&isAdmin); err != nil {
		return notFound(err)
	}
	if isAdmin && others == 0 {
		return invalid("Anchor needs at least one active admin.")
	}
	return nil
}
