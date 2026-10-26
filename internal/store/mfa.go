package store

import (
	"context"
	"database/sql"
	"time"
)

// MFAStatus summarises a user's second factors.
type MFAStatus struct {
	TOTP         bool
	Passkeys     int
	RecoveryLeft int
}

// Enabled reports whether the user has any second factor.
func (m MFAStatus) Enabled() bool { return m.TOTP || m.Passkeys > 0 }

func (s *Store) MFAStatus(ctx context.Context, userID int64) (MFAStatus, error) {
	var m MFAStatus
	err := s.r.QueryRowContext(ctx, `SELECT
		(SELECT totp_enabled FROM users WHERE id = ?),
		(SELECT count(*) FROM webauthn_credentials WHERE user_id = ?),
		(SELECT count(*) FROM recovery_codes WHERE user_id = ? AND used_at IS NULL)`,
		userID, userID, userID).Scan(&m.TOTP, &m.Passkeys, &m.RecoveryLeft)
	return m, notFound(err)
}

// StartTOTP stores a new, not yet enabled, sealed secret. It refuses while
// TOTP is enabled: the user must turn it off first.
func (s *Store) StartTOTP(ctx context.Context, userID int64, sealed []byte) error {
	res, err := s.w.ExecContext(ctx,
		"UPDATE users SET totp_secret = ?, totp_last_step = 0, updated_at = ? WHERE id = ? AND totp_enabled = 0",
		sealed, unix(s.now()), userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return invalid("Authenticator app is already set up.")
	}
	return nil
}

// EnableTOTP turns on the pending secret, consuming the step of the code
// that confirmed it.
func (s *Store) EnableTOTP(ctx context.Context, userID, step int64) error {
	res, err := s.w.ExecContext(ctx,
		`UPDATE users SET totp_enabled = 1, totp_last_step = ?, updated_at = ?
		 WHERE id = ? AND totp_enabled = 0 AND totp_secret IS NOT NULL`, step, unix(s.now()), userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return invalid("Start the authenticator setup again.")
	}
	return nil
}

func (s *Store) DisableTOTP(ctx context.Context, userID int64) error {
	_, err := s.w.ExecContext(ctx,
		"UPDATE users SET totp_enabled = 0, totp_secret = NULL, totp_last_step = 0, updated_at = ? WHERE id = ?",
		unix(s.now()), userID)
	return err
}

// UseTOTPStep records that a code for step was used. It fails if that step
// (or a later one) was already used, so a code works once even when two
// requests race.
func (s *Store) UseTOTPStep(ctx context.Context, userID, step int64) (bool, error) {
	res, err := s.w.ExecContext(ctx,
		"UPDATE users SET totp_last_step = ? WHERE id = ? AND totp_enabled = 1 AND totp_last_step < ?", step, userID, step)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// ReplaceRecoveryCodes swaps the user's recovery codes for new hashes.
func (s *Store) ReplaceRecoveryCodes(ctx context.Context, userID int64, hashes []string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM recovery_codes WHERE user_id = ?", userID); err != nil {
			return err
		}
		for _, h := range hashes {
			if _, err := tx.ExecContext(ctx, "INSERT INTO recovery_codes (user_id, code_hash) VALUES (?, ?)", userID, h); err != nil {
				return err
			}
		}
		return nil
	})
}

// UseRecoveryCode marks a matching unused code as used; it reports whether
// one matched.
func (s *Store) UseRecoveryCode(ctx context.Context, userID int64, hash string) (bool, error) {
	res, err := s.w.ExecContext(ctx,
		`UPDATE recovery_codes SET used_at = ? WHERE id = (
		   SELECT id FROM recovery_codes WHERE user_id = ? AND code_hash = ? AND used_at IS NULL LIMIT 1)`,
		unix(s.now()), userID, hash)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// Passkey is a registered WebAuthn credential.
type Passkey struct {
	ID           int64
	Name         string
	CredentialID []byte
	Credential   string // go-webauthn Credential as JSON
	CreatedAt    time.Time
	LastUsedAt   time.Time // zero if never
}

func (s *Store) AddPasskey(ctx context.Context, userID int64, name string, credentialID []byte, credJSON string) error {
	_, err := s.w.ExecContext(ctx,
		`INSERT INTO webauthn_credentials (user_id, credential_id, name, credential, created_at)
		 VALUES (?, ?, ?, ?, ?)`, userID, credentialID, name, credJSON, unix(s.now()))
	if isUniqueViolation(err) {
		return invalid("This passkey is already registered.")
	}
	return err
}

func (s *Store) ListPasskeys(ctx context.Context, userID int64) ([]Passkey, error) {
	rows, err := s.r.QueryContext(ctx,
		`SELECT id, name, credential_id, credential, created_at, coalesce(last_used_at, 0)
		 FROM webauthn_credentials WHERE user_id = ? ORDER BY created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Passkey
	for rows.Next() {
		var p Passkey
		var created, used int64
		if err := rows.Scan(&p.ID, &p.Name, &p.CredentialID, &p.Credential, &created, &used); err != nil {
			return nil, err
		}
		p.CreatedAt = fromUnix(created)
		if used > 0 {
			p.LastUsedAt = fromUnix(used)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// UpdatePasskeyUse stores the credential's new state (sign count, flags)
// after a successful login with it.
func (s *Store) UpdatePasskeyUse(ctx context.Context, userID int64, credentialID []byte, credJSON string) error {
	_, err := s.w.ExecContext(ctx,
		`UPDATE webauthn_credentials SET credential = ?, last_used_at = ?
		 WHERE user_id = ? AND credential_id = ?`, credJSON, unix(s.now()), userID, credentialID)
	return err
}

func (s *Store) DeletePasskey(ctx context.Context, userID, id int64) error {
	res, err := s.w.ExecContext(ctx, "DELETE FROM webauthn_credentials WHERE user_id = ? AND id = ?", userID, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ResetMFA removes every second factor and API token and signs the user
// out everywhere (admin recovery for a lost device).
func (s *Store) ResetMFA(ctx context.Context, userID int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			"UPDATE users SET totp_enabled = 0, totp_secret = NULL, totp_last_step = 0, updated_at = ? WHERE id = ?",
			unix(s.now()), userID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		for _, q := range []string{
			"DELETE FROM webauthn_credentials WHERE user_id = ?",
			"DELETE FROM recovery_codes WHERE user_id = ?",
			"DELETE FROM api_tokens WHERE user_id = ?",
			"DELETE FROM sessions WHERE user_id = ?",
		} {
			if _, err := tx.ExecContext(ctx, q, userID); err != nil {
				return err
			}
		}
		return nil
	})
}
