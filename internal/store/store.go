// Package store is Anchor's data access layer. Every method that touches
// user-owned rows takes the owning userID as a required argument and filters
// on it; there is no way to read or change another user's data through it.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/marcelhoffs/anchor/internal/db"
)

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("already exists")
)

// ValidationError is bad user input; its message is safe to show.
type ValidationError struct{ msg string }

func (e ValidationError) Error() string { return e.msg }

func invalid(format string, args ...any) error {
	return ValidationError{fmt.Sprintf(format, args...)}
}

type Store struct {
	w, r *sql.DB
	now  func() time.Time
}

func New(d *db.DB) *Store {
	return &Store{w: d.W, r: d.R, now: time.Now}
}

func (s *Store) Ping(ctx context.Context) error {
	return s.r.PingContext(ctx)
}

func (s *Store) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func unix(t time.Time) int64 { return t.Unix() }

func fromUnix(n int64) time.Time { return time.Unix(n, 0).UTC() }

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}
