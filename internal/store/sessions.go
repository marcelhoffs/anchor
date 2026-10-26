package store

import (
	"context"
	"time"
)

type Session struct {
	TokenHash  []byte
	UserID     int64
	MFAPassed  bool
	UserAgent  string
	IP         string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	LastSeenAt time.Time
	VerifiedAt time.Time // last password, 2FA or re-authentication check
	SpaceID    int64     // the space being looked at; 0 = the personal one
	SSO        bool      // signed in through the OpenID Connect provider
	// SSO only: the provider's tokens, sealed, and when it last confirmed
	// the user's groups (before the first check: the sign-in did).
	SSOTokens       []byte
	GroupsCheckedAt time.Time
}

const sessionCols = `token_hash, user_id, mfa_passed, coalesce(user_agent, ''), coalesce(ip, ''), created_at, expires_at,
	last_seen_at, verified_at, coalesce(space_id, 0), sso, sso_tokens, groups_checked_at`

func scanSession(row interface{ Scan(...any) error }) (Session, error) {
	var ss Session
	var created, expires, seen, verified, checked int64
	err := row.Scan(&ss.TokenHash, &ss.UserID, &ss.MFAPassed, &ss.UserAgent, &ss.IP, &created, &expires, &seen, &verified,
		&ss.SpaceID, &ss.SSO, &ss.SSOTokens, &checked)
	ss.CreatedAt, ss.ExpiresAt, ss.LastSeenAt = fromUnix(created), fromUnix(expires), fromUnix(seen)
	ss.VerifiedAt, ss.GroupsCheckedAt = fromUnix(verified), fromUnix(checked)
	return ss, notFound(err)
}

func (s *Store) CreateSession(ctx context.Context, ss Session) error {
	_, err := s.w.ExecContext(ctx,
		`INSERT INTO sessions (token_hash, user_id, mfa_passed, user_agent, ip, created_at, expires_at, last_seen_at, verified_at, sso,
		 sso_tokens) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ss.TokenHash, ss.UserID, ss.MFAPassed, ss.UserAgent, ss.IP, unix(ss.CreatedAt), unix(ss.ExpiresAt), unix(ss.LastSeenAt),
		unix(ss.VerifiedAt), ss.SSO, ss.SSOTokens)
	return err
}

// SessionTokens returns a session's sealed provider tokens.
func (s *Store) SessionTokens(ctx context.Context, hash []byte) ([]byte, error) {
	var tokens []byte
	err := s.r.QueryRowContext(ctx, "SELECT sso_tokens FROM sessions WHERE token_hash = ?", hash).Scan(&tokens)
	return tokens, notFound(err)
}

// GroupsChecked records that the provider just confirmed the session's
// groups, with tokens (sealed) as its tokens now.
func (s *Store) GroupsChecked(ctx context.Context, hash, tokens []byte) error {
	_, err := s.w.ExecContext(ctx, "UPDATE sessions SET sso_tokens = ?, groups_checked_at = ? WHERE token_hash = ?",
		tokens, unix(s.now()), hash)
	return err
}

// MarkVerified records that the session just proved its identity again.
func (s *Store) MarkVerified(ctx context.Context, hash []byte) error {
	_, err := s.w.ExecContext(ctx, "UPDATE sessions SET verified_at = ? WHERE token_hash = ?", unix(s.now()), hash)
	return err
}

// SelectSpace makes sc's space the one the session looks at. Taking a Scope
// means the user's membership was checked; the personal space is stored as
// NULL, the default.
func (s *Store) SelectSpace(ctx context.Context, hash []byte, sc Scope) error {
	if sc.spaceID == 0 {
		return ErrNotFound
	}
	var id any
	if !sc.personal {
		id = sc.spaceID
	}
	_, err := s.w.ExecContext(ctx, "UPDATE sessions SET space_id = ? WHERE token_hash = ? AND user_id = ?", id, hash, sc.userID)
	return err
}

// SessionByHash returns an unexpired session and its active user.
func (s *Store) SessionByHash(ctx context.Context, hash []byte) (Session, User, error) {
	ss, err := scanSession(s.r.QueryRowContext(ctx,
		"SELECT "+sessionCols+" FROM sessions WHERE token_hash = ? AND expires_at > ?", hash, unix(s.now())))
	if err != nil {
		return Session{}, User{}, err
	}
	u, err := s.UserByID(ctx, ss.UserID)
	if err != nil {
		return Session{}, User{}, err
	}
	if !u.IsActive {
		return Session{}, User{}, ErrNotFound
	}
	return ss, u, nil
}

// TouchSession extends a session's sliding expiry.
func (s *Store) TouchSession(ctx context.Context, hash []byte, seen, expires time.Time) error {
	_, err := s.w.ExecContext(ctx, "UPDATE sessions SET last_seen_at = ?, expires_at = ? WHERE token_hash = ?",
		unix(seen), unix(expires), hash)
	return err
}

func (s *Store) ListSessions(ctx context.Context, userID int64) ([]Session, error) {
	rows, err := s.r.QueryContext(ctx,
		"SELECT "+sessionCols+" FROM sessions WHERE user_id = ? AND expires_at > ? ORDER BY last_seen_at DESC",
		userID, unix(s.now()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		ss, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ss)
	}
	return out, rows.Err()
}

func (s *Store) DeleteSession(ctx context.Context, userID int64, hash []byte) error {
	_, err := s.w.ExecContext(ctx, "DELETE FROM sessions WHERE user_id = ? AND token_hash = ?", userID, hash)
	return err
}

func (s *Store) DeleteExpiredSessions(ctx context.Context) (int64, error) {
	res, err := s.w.ExecContext(ctx, "DELETE FROM sessions WHERE expires_at <= ?", unix(s.now()))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeleteOtherSessions signs a user out everywhere except keep.
func (s *Store) DeleteOtherSessions(ctx context.Context, userID int64, keep []byte) error {
	_, err := s.w.ExecContext(ctx, "DELETE FROM sessions WHERE user_id = ? AND token_hash != ?", userID, keep)
	return err
}
