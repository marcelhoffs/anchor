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
}

const sessionCols = "token_hash, user_id, mfa_passed, coalesce(user_agent, ''), coalesce(ip, ''), created_at, expires_at, last_seen_at"

func scanSession(row interface{ Scan(...any) error }) (Session, error) {
	var ss Session
	var created, expires, seen int64
	err := row.Scan(&ss.TokenHash, &ss.UserID, &ss.MFAPassed, &ss.UserAgent, &ss.IP, &created, &expires, &seen)
	ss.CreatedAt, ss.ExpiresAt, ss.LastSeenAt = fromUnix(created), fromUnix(expires), fromUnix(seen)
	return ss, notFound(err)
}

func (s *Store) CreateSession(ctx context.Context, ss Session) error {
	_, err := s.w.ExecContext(ctx,
		`INSERT INTO sessions (token_hash, user_id, mfa_passed, user_agent, ip, created_at, expires_at, last_seen_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		ss.TokenHash, ss.UserID, ss.MFAPassed, ss.UserAgent, ss.IP, unix(ss.CreatedAt), unix(ss.ExpiresAt), unix(ss.LastSeenAt))
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
