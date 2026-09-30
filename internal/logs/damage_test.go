package logs

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/db"
)

// fillAndCorrupt writes n query rows into the logs.db of dir, closes it
// and overwrites a leaf page of logs_queries in the middle of the file.
func fillAndCorrupt(t *testing.T, dir string, n int) {
	t.Helper()
	ldb, cdb, set := openTestDBs(t, dir)
	if _, err := New(context.Background(), ldb, set, nil); err != nil {
		t.Fatal(err)
	}
	ts := time.Now().Add(-time.Hour).UnixMilli()
	if _, err := ldb.W.Exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < ?)
		INSERT INTO logs_queries (ts, client_ip, qname, qtype, status) SELECT ? + i, '192.168.1.10', 'host' || i || '.example', 'A', 'forwarded' FROM n`,
		n, ts); err != nil {
		t.Fatal(err)
	}
	var page int64
	err := ldb.R.QueryRow(`SELECT pageno FROM dbstat WHERE name = 'logs_queries' AND pagetype = 'leaf' ORDER BY pageno LIMIT 1 OFFSET 10`).Scan(&page)
	ldb.Close()
	cdb.Close()
	if err != nil {
		t.Skipf("dbstat unavailable: %v", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "logs.db"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteAt(bytes.Repeat([]byte{0xff}, 4096), (page-1)*4096); err != nil {
		t.Fatal(err)
	}
}

// A logs.db damaged in the middle (not in its header) opens, but reads and
// writes of the damaged pages fail. The store records the damage (the
// health check warns) and writes the marker the next start checks; before,
// the errors only counted as dropped events and the health check said ok.
func TestDamagedLogsDBReported(t *testing.T) {
	dir := t.TempDir()
	fillAndCorrupt(t, dir, 20000)
	ldb, cdb, set := openTestDBs(t, dir)
	defer ldb.Close()
	defer cdb.Close()
	s, err := New(context.Background(), ldb, set, nil)
	if err != nil {
		t.Fatalf("a logs.db damaged in the middle must open: %v", err)
	}
	if m := s.Metrics(); m.Damaged != "" {
		t.Fatalf("damaged before a read: %q", m.Damaged)
	}
	_, err = s.QueryLog(context.Background(), QueryFilter{Domain: "nomatch-zzz", Limit: 100})
	if err == nil {
		t.Skip("the damaged page was not read")
	}
	if !db.Corrupt(err) {
		t.Fatalf("read error %v is not SQLITE_CORRUPT", err)
	}
	if m := s.Metrics(); !strings.Contains(m.Damaged, "malformed") {
		t.Fatalf("metrics %+v", m)
	}
	if _, err := os.Stat(DamagedMarker(filepath.Join(dir, "logs.db"))); err != nil {
		t.Fatalf("no marker: %v", err)
	}
}
