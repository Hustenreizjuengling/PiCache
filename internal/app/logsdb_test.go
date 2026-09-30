package app

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/logs"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// logsApp returns an app with its settings for openLogs.
func logsApp(t *testing.T) *App {
	t.Helper()
	a := newRestoreApp(t)
	openLive(t, a)
	set, err := settings.Open(context.Background(), a.cdb, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	a.set = set
	return a
}

func closeLogs(a *App) {
	if a.ldb != nil {
		a.ldb.Close()
		a.ldb = nil
	}
}

// brokenCopies returns the logs.db.broken-* files.
func brokenCopies(a *App) []string {
	m, _ := filepath.Glob(a.paths.LogsDB + ".broken-*")
	return m
}

// An intact logs.db that PiCache may not write (owned by root after a copy
// without chown) is kept as it is and named, like picache.db; before, it
// was moved aside as broken and the history disappeared.
func TestOpenLogsKeepsUnwritableFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root may write every file")
	}
	ctx := context.Background()
	a := logsApp(t)
	a.openLogs(ctx)
	closeLogs(a)
	if err := os.Chmod(a.paths.LogsDB, 0o400); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(a.paths.LogsDB, 0o600) })
	a.openLogs(ctx)
	closeLogs(a)
	if b := brokenCopies(a); len(b) != 0 {
		t.Fatalf("an intact logs.db was moved aside: %v", b)
	}
	if _, err := os.Stat(a.paths.LogsDB); err != nil {
		t.Fatal(err)
	}
	if m := a.logs.Metrics(); !strings.Contains(m.Disabled, "is not writable by PiCache") {
		t.Fatalf("metrics %+v", m)
	}
}

// A logs.db reported damaged while PiCache ran (the marker) is checked at
// the next start and moved aside; the marker is removed. Without the
// marker the damaged file (it opens) would be kept forever.
func TestOpenLogsMovesReportedDamage(t *testing.T) {
	ctx := context.Background()
	a := logsApp(t)
	a.openLogs(ctx)
	ts := time.Now().Add(-time.Hour).UnixMilli()
	if _, err := a.ldb.W.Exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 20000)
		INSERT INTO logs_queries (ts, client_ip, qname, qtype, status) SELECT ? + i, '192.168.1.10', 'host' || i || '.example', 'A', 'forwarded' FROM n`,
		ts); err != nil {
		t.Fatal(err)
	}
	var page int64
	err := a.ldb.R.QueryRow(`SELECT pageno FROM dbstat WHERE name = 'logs_queries' AND pagetype = 'leaf' ORDER BY pageno LIMIT 1 OFFSET 10`).Scan(&page)
	closeLogs(a)
	if err != nil {
		t.Skipf("dbstat unavailable: %v", err)
	}
	f, err := os.OpenFile(a.paths.LogsDB, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteAt(bytes.Repeat([]byte{0xff}, 4096), (page-1)*4096)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	// Without the marker the file opens and is kept.
	a.openLogs(ctx)
	closeLogs(a)
	if b := brokenCopies(a); len(b) != 0 {
		t.Fatalf("moved aside without a report: %v", b)
	}
	marker := logs.DamagedMarker(a.paths.LogsDB)
	if err := os.WriteFile(marker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	a.openLogs(ctx)
	defer closeLogs(a)
	if b := brokenCopies(a); len(b) != 1 {
		t.Fatalf("the damaged logs.db was not moved aside: %v", b)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("marker left: %v", err)
	}
	if m := a.logs.Metrics(); m.Disabled != "" {
		t.Fatalf("no fresh logs.db: %+v", m)
	}
}

// The check "logs": a damaged logs.db warns, and events of failed writes
// are not reported as dropped under load.
func TestLogsHealth(t *testing.T) {
	if st, msg, hint := logsHealth(logs.Metrics{Damaged: "database disk image is malformed (11)", Dropped: 3, WriteFailed: 3}); st != "warn" ||
		!strings.HasPrefix(msg, "logs.db is damaged: ") || !strings.Contains(hint, "restart PiCache") {
		t.Errorf("damaged: %s %q %q", st, msg, hint)
	}
	if st, msg, _ := logsHealth(logs.Metrics{Dropped: 10, WriteFailed: 4}); st != "ok" ||
		msg != "6 events dropped under load; 4 events could not be written (see the log)" {
		t.Errorf("drops: %s %q", st, msg)
	}
	if st, msg, _ := logsHealth(logs.Metrics{Dropped: 4, WriteFailed: 4}); st != "ok" || strings.Contains(msg, "under load") {
		t.Errorf("write errors only: %s %q", st, msg)
	}
	if st, msg, _ := logsHealth(logs.Metrics{}); st != "ok" || msg != "" {
		t.Errorf("fine: %s %q", st, msg)
	}
}
