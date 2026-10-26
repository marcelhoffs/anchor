package store

import (
	"context"
	"database/sql"
)

// UserByIdentity returns the user an identity at the OpenID Connect
// provider, (issuer, subject), signs in as.
func (s *Store) UserByIdentity(ctx context.Context, issuer, subject string) (User, error) {
	return scanUser(s.r.QueryRowContext(ctx,
		"SELECT "+userCols+" FROM users WHERE id = (SELECT user_id FROM oidc_identities WHERE issuer = ? AND subject = ?)",
		issuer, subject))
}

// IdentityOf returns the subject linked to a user at issuer, or ErrNotFound.
func (s *Store) IdentityOf(ctx context.Context, userID int64, issuer string) (string, error) {
	var subject string
	err := s.r.QueryRowContext(ctx, "SELECT subject FROM oidc_identities WHERE user_id = ? AND issuer = ?", userID, issuer).
		Scan(&subject)
	return subject, notFound(err)
}

// LinkedUsers returns the IDs of the users with a linked identity.
func (s *Store) LinkedUsers(ctx context.Context) (map[int64]bool, error) {
	rows, err := s.r.QueryContext(ctx, "SELECT user_id FROM oidc_identities")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// LinkIdentity links an identity to a user. It returns ErrConflict when the
// identity belongs to another user or the user already has one.
func (s *Store) LinkIdentity(ctx context.Context, userID int64, issuer, subject string) error {
	_, err := s.w.ExecContext(ctx,
		"INSERT INTO oidc_identities (issuer, subject, user_id, created_at, last_used_at) VALUES (?, ?, ?, ?, ?)",
		issuer, subject, userID, unix(s.now()), unix(s.now()))
	if isUniqueViolation(err) {
		return ErrConflict
	}
	return err
}

// UnlinkIdentity removes a user's identity. Unless force is set, a user
// without a password keeps it: it's their only way in. dropAdmin also takes
// the admin role away, when it came from the provider's group.
func (s *Store) UnlinkIdentity(ctx context.Context, userID int64, force, dropAdmin bool) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var hash string
		if err := tx.QueryRowContext(ctx, "SELECT password_hash FROM users WHERE id = ?", userID).Scan(&hash); err != nil {
			return notFound(err)
		}
		if hash == "" && !force {
			return invalid("Set a password first: without single sign-on it's your only way in.")
		}
		res, err := tx.ExecContext(ctx, "DELETE FROM oidc_identities WHERE user_id = ?", userID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		if dropAdmin {
			_, err = tx.ExecContext(ctx, "UPDATE users SET is_admin = 0, updated_at = ? WHERE id = ?", unix(s.now()), userID)
		}
		return err
	})
}

// TouchIdentity records a sign-in with an identity.
func (s *Store) TouchIdentity(ctx context.Context, issuer, subject string) error {
	_, err := s.w.ExecContext(ctx, "UPDATE oidc_identities SET last_used_at = ? WHERE issuer = ? AND subject = ?",
		unix(s.now()), issuer, subject)
	return err
}

// NewOIDCUser is an account made by a first sign-in through the provider.
type NewOIDCUser struct {
	Username        string
	Issuer, Subject string
	// First makes the account the first user, and fails with ErrConflict
	// once any user exists: setup through the provider.
	First bool
	Admin bool
	// InviteHash, when set, is consumed with the account, as in SignUp.
	InviteHash []byte
}

// CreateOIDCUser creates a user without a password and links the identity,
// in one transaction. ErrConflict: the username or identity is taken, or
// (First) a user exists already; ErrInvalidInvite: the invite can't be used.
func (s *Store) CreateOIDCUser(ctx context.Context, n NewOIDCUser) (User, error) {
	var u User
	err := s.tx(ctx, func(tx *sql.Tx) error {
		now := unix(s.now())
		if n.First {
			var users int
			if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM users").Scan(&users); err != nil {
				return err
			}
			if users > 0 {
				return ErrConflict
			}
		}
		var inviteID int64
		if n.InviteHash != nil {
			err := tx.QueryRowContext(ctx,
				"SELECT id FROM invites WHERE token_hash = ? AND used_at IS NULL AND expires_at > ?",
				n.InviteHash, now).Scan(&inviteID)
			if err == sql.ErrNoRows {
				return ErrInvalidInvite
			}
			if err != nil {
				return err
			}
		}
		var err error
		u, err = scanUser(tx.QueryRowContext(ctx,
			"INSERT INTO users (username, password_hash, is_admin, created_at, updated_at) VALUES (?, '', ?, ?, ?) RETURNING "+userCols,
			n.Username, n.Admin, now, now))
		if isUniqueViolation(err) {
			return ErrConflict
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO oidc_identities (issuer, subject, user_id, created_at, last_used_at) VALUES (?, ?, ?, ?, ?)",
			n.Issuer, n.Subject, u.ID, now, now); err != nil {
			if isUniqueViolation(err) {
				return ErrConflict
			}
			return err
		}
		if inviteID != 0 {
			_, err = tx.ExecContext(ctx, "UPDATE invites SET used_at = ?, used_by = ? WHERE id = ?", now, u.ID, inviteID)
		}
		return err
	})
	return u, err
}
