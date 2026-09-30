package app

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/version"
)

// The picache.db schema of 0.17.x is part of every later schema: a
// release only adds steps (1.0.0 added none).
func TestSchemaV017(t *testing.T) {
	cur := ConfigSchemaVersions()
	for c, n := range schemaV017 {
		if cur[c] < n {
			t.Errorf("component %s: schema %d of 0.17.x is newer than this binary's %d", c, n, cur[c])
		}
	}
	for v, want := range map[string]bool{"v0.17.0": true, "v0.17.1": true, "v0.17.0-rc.1": true, "v0.17.0-3-gabcdef0": true,
		"v0.16.1": false, "v0.18.0": false, "v1.0.0": false, "dev": false, "": false} {
		if got := openedBy(v, schemaV017); got != want {
			t.Errorf("openedBy(%q) = %v, want %v", v, got, want)
		}
	}
	newer := map[string]int{"settings": schemaV017["settings"] + 1, "newcomponent": 1}
	if openedBy("v0.17.0", newer) {
		t.Error("0.17.0 opens a newer settings schema")
	}
	if !openedBy("v0.17.0", map[string]int{"settings": 7, "newcomponent": 1}) {
		t.Error("a component 0.17.0 does not know counts")
	}
}

// U3: going back from 1.0.x to 0.17.x keeps the database (1.0.0 added no
// schema step): 0.17.x copies it under 1.0.x's name at its start, records
// itself and runs, and changes made then are only in the live database.
// The next upgrade makes the usual copy named after 0.17.x with those
// changes, logged at INFO without the warning about a version that did not
// migrate the database. Before, it named the database's schema after 1.0.x,
// found 0.17.x's copy under that name and made none.
func TestPreUpgradeCopyAfterGoingBackTo017(t *testing.T) {
	ctx := context.Background()
	a := newRestoreApp(t)
	makeConfigDB(t, a.paths.ConfigDB, "owner", "owner password", "en")
	openLive(t, a)
	oldVersion := version.Version
	t.Cleanup(func() { version.Version = oldVersion })
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := a.cdb.W.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	version.Version = "v1.0.0"
	if err := a.preUpgradeBackup(ctx); err != nil {
		t.Fatal(err)
	}
	// The schema of 1.0.0, which is 0.17.x's (a later binary's steps
	// beyond it are left out here).
	for c, n := range schemaV017 {
		exec(`DELETE FROM schema_migrations WHERE component = ? AND version > ?`, c, n)
	}
	if err := recordSchema(ctx, a.cdb); err != nil {
		t.Fatal(err)
	}
	backups := filepath.Join(a.cfg.DataDir, "backups")
	if err := os.MkdirAll(backups, 0o750); err != nil {
		t.Fatal(err)
	}
	// 0.17.0 starts: its copy under v1.0.0's name, its record, a change.
	exec(`VACUUM INTO ?`, filepath.Join(backups, "picache-v1.0.0-"+time.Now().UTC().Add(-time.Hour).Format(copyTimeLayout)+".db"))
	exec(`UPDATE app_meta SET value = 'v0.17.0' WHERE key = 'binary_version'`)
	exec(`INSERT INTO app_meta (key, value) VALUES ('test.changed', 'under 0.17.0')`)

	var logBuf strings.Builder
	log := slog.New(slog.NewTextHandler(&logBuf, nil))
	version.Version = "v1.0.1"
	if err := PreUpgradeBackup(ctx, a.cdb, a.cfg.DataDir, log); err != nil {
		t.Fatal(err)
	}
	copies, _ := filepath.Glob(filepath.Join(backups, "picache-v0.17.0-*.db"))
	if len(copies) != 1 {
		all, _ := filepath.Glob(filepath.Join(backups, "*"))
		t.Fatalf("copies named after v0.17.0: %v (all: %v); log: %s", copies, all, logBuf.String())
	}
	cp, err := sql.Open("sqlite", copies[0])
	if err != nil {
		t.Fatal(err)
	}
	defer cp.Close()
	var changed string
	if err := cp.QueryRow(`SELECT value FROM app_meta WHERE key = 'test.changed'`).Scan(&changed); err != nil || changed != "under 0.17.0" {
		t.Fatalf("the copy lacks the change made under 0.17.0: %q %v", changed, err)
	}
	out := logBuf.String()
	if strings.Contains(out, "recorded itself without migrating") || !strings.Contains(out, "level=INFO") ||
		!strings.Contains(out, "the previous version ran on this database with the schema of a later version") {
		t.Fatalf("log: %s", out)
	}
}
