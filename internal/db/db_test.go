package db

import (
	"context"
	"path/filepath"
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
