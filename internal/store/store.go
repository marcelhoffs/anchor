// Package store is Anchor's data access layer. Bookmarks, tags and saved searches
// live in spaces (a user's personal space, or a shared one), and every
// method that touches them takes a Scope, which only Access and its siblings
// hand out after checking the user is a member; writes also check the
// member's role. Account data (sessions, tokens, second factors) is filtered
// on the owning userID. There is no way to reach data a user has no access
// to through this package.
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

// ValidationError is bad user input; its message is safe to show. Format
// and Args are kept apart so the web interface can translate them. Args
// makes it uncomparable: test for one with errors.As, never == or
// errors.Is (ErrInvalidURL included).
type ValidationError struct {
	Format string
	Args   []any
}

func (e ValidationError) Error() string { return fmt.Sprintf(e.Format, e.Args...) }

// invalid's format must be a string literal: it is a translation key.
func invalid(format string, args ...any) error {
	return ValidationError{format, args}
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
