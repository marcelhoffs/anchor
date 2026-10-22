// Package db opens Burrow's SQLite database and applies migrations.
package db

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"path/filepath"
	"runtime"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrations embed.FS

// DB holds a single writer connection (SQLite allows one writer at a time)
// and a pool of reader connections.
type DB struct {
	W *sql.DB
	R *sql.DB
}

const pragmas = "_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)" +
	"&_pragma=busy_timeout(5000)&_pragma=temp_store(MEMORY)"

func Open(path string) (*DB, error) {
	dsn := "file:" + (&url.URL{Path: filepath.ToSlash(path)}).EscapedPath() + "?" + pragmas
	w, err := sql.Open("sqlite", dsn+"&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	w.SetConnMaxIdleTime(0)
	if err := w.Ping(); err != nil {
		w.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}

	r, err := sql.Open("sqlite", dsn+"&_pragma=query_only(ON)")
	if err != nil {
		w.Close()
		return nil, err
	}
	r.SetMaxOpenConns(max(4, runtime.NumCPU()))
	return &DB{W: w, R: r}, nil
}

func (d *DB) Close() error {
	return errors.Join(d.R.Close(), d.W.Close())
}

// Migrate applies all pending migrations.
func (d *DB) Migrate(ctx context.Context) error {
	sub, err := fs.Sub(migrations, "migrations")
	if err != nil {
		return err
	}
	p, err := goose.NewProvider(goose.DialectSQLite3, d.W, sub)
	if err != nil {
		return err
	}
	_, err = p.Up(ctx)
	return err
}

// Backup writes a consistent snapshot of the database to path. It is safe to
// run while the server is serving requests.
func (d *DB) Backup(ctx context.Context, path string) error {
	_, err := d.W.ExecContext(ctx, "VACUUM INTO ?", path)
	return err
}
