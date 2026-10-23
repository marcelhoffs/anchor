package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Invite is a single-use signup link. Only its token hash is stored.
type Invite struct {
	ID        int64
	Note      string
	CreatedBy string
	CreatedAt time.Time
	ExpiresAt time.Time
	UsedAt    time.Time // zero if unused
	UsedBy    string
}

var ErrInvalidInvite = errors.New("invite is invalid, expired or already used")

func (s *Store) CreateInvite(ctx context.Context, createdBy int64, note string, hash []byte, expires time.Time) error {
	_, err := s.w.ExecContext(ctx,
		"INSERT INTO invites (token_hash, note, created_by, created_at, expires_at) VALUES (?, ?, ?, ?, ?)",
		hash, strings.TrimSpace(note), createdBy, unix(s.now()), unix(expires))
	return err
}

// ListInvites returns unused invites and those used in the last 30 days.
func (s *Store) ListInvites(ctx context.Context) ([]Invite, error) {
	rows, err := s.r.QueryContext(ctx,
		`SELECT i.id, i.note, coalesce(c.username, ''), i.created_at, i.expires_at, coalesce(i.used_at, 0), coalesce(u.username, '')
		 FROM invites i LEFT JOIN users c ON c.id = i.created_by LEFT JOIN users u ON u.id = i.used_by
		 WHERE i.used_at IS NULL OR i.used_at > ? ORDER BY i.created_at DESC`, unix(s.now().Add(-30*24*time.Hour)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Invite
	for rows.Next() {
		var in Invite
		var created, expires, used int64
		if err := rows.Scan(&in.ID, &in.Note, &in.CreatedBy, &created, &expires, &used, &in.UsedBy); err != nil {
			return nil, err
		}
		in.CreatedAt, in.ExpiresAt = fromUnix(created), fromUnix(expires)
		if used > 0 {
			in.UsedAt = fromUnix(used)
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

func (s *Store) DeleteInvite(ctx context.Context, id int64) error {
	res, err := s.w.ExecContext(ctx, "DELETE FROM invites WHERE id = ? AND used_at IS NULL", id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// InviteValid reports whether hash is an unused, unexpired invite.
func (s *Store) InviteValid(ctx context.Context, hash []byte) bool {
	var one int
	err := s.r.QueryRowContext(ctx,
		"SELECT 1 FROM invites WHERE token_hash = ? AND used_at IS NULL AND expires_at > ?", hash, unix(s.now())).Scan(&one)
	return err == nil
}

// SignUp creates a user. With an invite hash it consumes that invite in the
// same transaction, so a link can never create two accounts.
func (s *Store) SignUp(ctx context.Context, username, passwordHash string, inviteHash []byte) (User, error) {
	var u User
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var inviteID int64
		if inviteHash != nil {
			err := tx.QueryRowContext(ctx,
				"SELECT id FROM invites WHERE token_hash = ? AND used_at IS NULL AND expires_at > ?",
				inviteHash, unix(s.now())).Scan(&inviteID)
			if err == sql.ErrNoRows {
				return ErrInvalidInvite
			}
			if err != nil {
				return err
			}
		}
		now := unix(s.now())
		var err error
		u, err = scanUser(tx.QueryRowContext(ctx,
			"INSERT INTO users (username, password_hash, created_at, updated_at) VALUES (?, ?, ?, ?) RETURNING "+userCols,
			username, passwordHash, now, now))
		if isUniqueViolation(err) {
			return ErrConflict
		}
		if err != nil {
			return err
		}
		if inviteID != 0 {
			_, err = tx.ExecContext(ctx, "UPDATE invites SET used_at = ?, used_by = ? WHERE id = ?", now, u.ID, inviteID)
		}
		return err
	})
	return u, err
}

// Instance settings an admin can change.
const (
	SettingSignup     = "signup"      // closed | invite | open
	SettingRequire2FA = "require_2fa" // "true" | "false"
)

var SignupModes = []string{"closed", "invite", "open"}

func (s *Store) InstanceSetting(ctx context.Context, key, def string) (string, error) {
	var v string
	err := s.r.QueryRowContext(ctx, "SELECT value FROM instance_settings WHERE key = ?", key).Scan(&v)
	if err == sql.ErrNoRows {
		return def, nil
	}
	return v, err
}

func (s *Store) SetInstanceSetting(ctx context.Context, key, value string) error {
	_, err := s.w.ExecContext(ctx,
		"INSERT INTO instance_settings (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value",
		key, value)
	return err
}

// Stats are instance-wide totals for the admin page.
type Stats struct {
	Users, ActiveUsers, Bookmarks, Tags int
	DBBytes                             int64
}

func (s *Store) Stats(ctx context.Context) (Stats, error) {
	var st Stats
	err := s.r.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM users), (SELECT count(*) FROM users WHERE is_active = 1),
		(SELECT count(*) FROM bookmarks), (SELECT count(*) FROM tags),
		(SELECT page_count * page_size FROM pragma_page_count(), pragma_page_size())`).
		Scan(&st.Users, &st.ActiveUsers, &st.Bookmarks, &st.Tags, &st.DBBytes)
	return st, err
}
