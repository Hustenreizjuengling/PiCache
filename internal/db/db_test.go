package db

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMigrateIdempotentAndMultiStatement(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "t.db"), 2)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	steps := []string{
		`CREATE TABLE a (id INTEGER PRIMARY KEY, v TEXT); CREATE TABLE b (id INTEGER PRIMARY KEY);`,
		`ALTER TABLE a ADD COLUMN w INTEGER NOT NULL DEFAULT 0;`,
	}
	for range 2 {
		if err := d.Migrate(ctx, "test", steps); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.W.ExecContext(ctx, `INSERT INTO a (v, w) VALUES ('x', 1); INSERT INTO b DEFAULT VALUES;`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := d.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM b`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("count b = %d, err %v", n, err)
	}
	if _, err := d.R.ExecContext(ctx, `INSERT INTO b DEFAULT VALUES`); err == nil {
		t.Fatal("reader pool must be query_only")
	}
	if err := d.Migrate(ctx, "test", steps[:1]); err == nil {
		t.Fatal("expected downgrade refusal")
	}
}

// Root CLI commands open picache.db from a directory the service owns: only
// a regular file is accepted, and its schema is not trusted.
func TestOpenReadOnlyRefusesUntrustedFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "picache.db")
	d, err := Open(path, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Migrate(context.Background(), "test", []string{`CREATE TABLE a (id INTEGER PRIMARY KEY)`}); err != nil {
		t.Fatal(err)
	}
	d.Close()

	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	var trusted int
	if err := ro.R.QueryRow(`PRAGMA trusted_schema`).Scan(&trusted); err != nil || trusted != 0 {
		t.Fatalf("trusted_schema = %d, %v", trusted, err)
	}
	ro.Close()

	if _, err := OpenReadOnly(dir); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("directory: %v", err)
	}
	link := filepath.Join(dir, "link.db")
	if err := os.Symlink(path, link); err != nil {
		t.Skipf("cannot create symbolic links here: %v", err)
	}
	if _, err := OpenReadOnly(link); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("symbolic link: %v", err)
	}
}

// A WAL file keeps at most JournalSizeLimit bytes after a checkpoint.
func TestJournalSizeLimit(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "t.db"), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var limit int64
	if err := d.W.QueryRow(`PRAGMA journal_size_limit`).Scan(&limit); err != nil || limit != JournalSizeLimit {
		t.Fatalf("journal_size_limit = %d, %v", limit, err)
	}
}

// Neither pool trusts the schema: a view or trigger planted in the file (a
// restored backup, a file edited on disk) cannot call functions with side
// effects.
func TestOpenDistrustsSchema(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "t.db"), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for name, pool := range map[string]interface {
		QueryRow(string, ...any) *sql.Row
	}{"writer": d.W, "reader": d.R} {
		var trusted int
		if err := pool.QueryRow(`PRAGMA trusted_schema`).Scan(&trusted); err != nil || trusted != 0 {
			t.Fatalf("%s: trusted_schema = %d, %v", name, trusted, err)
		}
	}
}

// Triggers and views planted in a database are removed, whatever their
// names; tables, indexes and rows stay.
func TestDropTriggersAndViews(t *testing.T) {
	ctx := context.Background()
	d, err := Open(filepath.Join(t.TempDir(), "t.db"), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, q := range []string{
		`CREATE TABLE tokens (id INTEGER PRIMARY KEY, name TEXT)`,
		`CREATE INDEX tokens_name ON tokens(name)`,
		`INSERT INTO tokens (name) VALUES ('a'), ('b')`,
		`CREATE TRIGGER keep BEFORE DELETE ON tokens BEGIN SELECT RAISE(IGNORE); END`,
		`CREATE TRIGGER "odd ""name""" AFTER INSERT ON tokens BEGIN SELECT 1; END`,
		`CREATE VIEW v AS SELECT name FROM tokens`,
		`CREATE TRIGGER v_ins INSTEAD OF INSERT ON v BEGIN SELECT 1; END`,
	} {
		if _, err := d.W.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	dropped, err := DropTriggersAndViews(ctx, d.W)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(dropped, ","); got != `trigger "keep",trigger "odd ""name""",trigger "v_ins",view "v"` {
		t.Fatalf("dropped = %s", got)
	}
	if _, err := d.W.ExecContext(ctx, `DELETE FROM tokens`); err != nil {
		t.Fatal(err)
	}
	var objs, rows int
	if err := d.R.QueryRow(`SELECT (SELECT COUNT(*) FROM sqlite_master WHERE type IN ('trigger', 'view')),
		(SELECT COUNT(*) FROM tokens)`).Scan(&objs, &rows); err != nil || objs != 0 || rows != 0 {
		t.Fatalf("after drop: %d triggers/views, %d rows, %v", objs, rows, err)
	}
	var idx int
	if err := d.R.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'tokens_name'`).Scan(&idx); err != nil || idx != 1 {
		t.Fatalf("the index must stay: %d, %v", idx, err)
	}
	if dropped, err := DropTriggersAndViews(ctx, d.W); err != nil || len(dropped) != 0 {
		t.Fatalf("second run = %v, %v", dropped, err)
	}
}
