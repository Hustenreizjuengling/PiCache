package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// OPS-11: a broken logs.db is moved aside and replaced by a fresh one, and
// only the newest broken copy is kept: repeated corruption or downgrades do
// not fill the data disk with copies of up to logs.maxDbSizeMiB each.
func TestOpenLogsKeepsOneBrokenCopy(t *testing.T) {
	ctx := context.Background()
	a := syncApp(t)
	older := []string{a.paths.LogsDB + ".broken-20260101T000000", a.paths.LogsDB + "-wal.broken-20260101T000000",
		a.paths.LogsDB + ".broken-20260201T000000", a.paths.LogsDB + "-shm.broken-20260201T000000"}
	for _, f := range append(older, a.paths.LogsDB) {
		if err := os.WriteFile(f, []byte("not a database, not a database, not a database"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	a.openLogs(ctx)
	t.Cleanup(func() {
		_ = a.logs.Close()
		if a.ldb != nil {
			_ = a.ldb.Close()
		}
	})
	if a.ldb == nil {
		t.Fatal("no fresh logs.db was created")
	}
	for _, f := range older {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Errorf("the older broken copy %s was kept (%v)", filepath.Base(f), err)
		}
	}
	if copies, _ := filepath.Glob(a.paths.LogsDB + ".broken-*"); len(copies) != 1 {
		t.Fatalf("broken copies %v, want the one just made", copies)
	}
}
