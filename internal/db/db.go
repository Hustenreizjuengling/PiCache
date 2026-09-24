// Package db opens SQLite databases (pure Go, modernc.org/sqlite) with a
// single-connection writer pool and a small read-only reader pool, and runs
// per-component schema migrations.
//
// Databases MUST be on local disk: SQLite WAL does not work on network
// filesystems. Open refuses NFS/CIFS paths on Linux.
package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // driver "sqlite"
)

// DB bundles the writer and reader pools of one SQLite file.
//
// Use W for every statement that writes (and for transactions that read then
// write). Use R for plain reads; R connections are query_only.
type DB struct {
	W    *sql.DB
	R    *sql.DB
	Path string
}

// Open opens (creating if needed) the database at path with WAL, foreign keys
// and a 5 s busy timeout. readers is the reader pool size (>= 1).
func Open(path string, readers int) (*DB, error) {
	if strings.ContainsAny(path, "?#") {
		return nil, errors.New("db: path must not contain '?' or '#'")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("db: create dir: %w", err)
	}
	if err := checkLocalFS(filepath.Dir(path)); err != nil {
		return nil, err
	}
	if readers < 1 {
		readers = 1
	}
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)" +
		"&_pragma=foreign_keys(1)&_pragma=temp_store(MEMORY)"
	w, err := sql.Open("sqlite", dsn+"&_txlock=immediate")
	if err != nil {
		return nil, fmt.Errorf("db: open writer: %w", err)
	}
	w.SetMaxOpenConns(1)
	w.SetMaxIdleConns(1)
	w.SetConnMaxLifetime(0)
	if err := w.Ping(); err != nil {
		w.Close()
		return nil, fmt.Errorf("db: ping %s: %w", path, err)
	}
	r, err := sql.Open("sqlite", dsn+"&_query_only=1")
	if err != nil {
		w.Close()
		return nil, fmt.Errorf("db: open reader: %w", err)
	}
	r.SetMaxOpenConns(readers)
	r.SetMaxIdleConns(readers)
	if err := os.Chmod(path, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
		// Best effort; on Windows chmod is mostly a no-op.
		_ = err
	}
	return &DB{W: w, R: r, Path: path}, nil
}

// Close closes both pools.
func (d *DB) Close() error {
	return errors.Join(d.R.Close(), d.W.Close())
}

// Tx runs fn in a write transaction (BEGIN IMMEDIATE) and commits if fn
// returns nil, otherwise rolls back.
func (d *DB) Tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// Migrate applies the schema steps of one component. steps[i] is the SQL for
// version i+1 and may contain several statements. Applied versions are
// recorded in schema_migrations; steps must never be edited once released,
// only appended.
func (d *DB) Migrate(ctx context.Context, component string, steps []string) error {
	if _, err := d.W.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		component  TEXT    NOT NULL,
		version    INTEGER NOT NULL,
		applied_at INTEGER NOT NULL,
		PRIMARY KEY (component, version)
	)`); err != nil {
		return fmt.Errorf("db: migrations table: %w", err)
	}
	var current int
	if err := d.W.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version), 0) FROM schema_migrations WHERE component = ?`, component,
	).Scan(&current); err != nil {
		return fmt.Errorf("db: read migration version for %s: %w", component, err)
	}
	if current > len(steps) {
		return fmt.Errorf("db: %s schema version %d is newer than this binary (%d); refusing to downgrade", component, current, len(steps))
	}
	for v := current + 1; v <= len(steps); v++ {
		err := d.Tx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, steps[v-1]); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx,
				`INSERT INTO schema_migrations (component, version, applied_at) VALUES (?, ?, ?)`,
				component, v, time.Now().UnixMilli())
			return err
		})
		if err != nil {
			return fmt.Errorf("db: migrate %s to v%d: %w", component, v, err)
		}
	}
	return nil
}

// NowMs returns the current time as unix milliseconds (the storage format for
// all timestamps).
func NowMs() int64 { return time.Now().UnixMilli() }

// Ms converts a time to unix milliseconds (0 for the zero time).
func Ms(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

// Time converts unix milliseconds to time.Time (zero time for 0).
func Time(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}
