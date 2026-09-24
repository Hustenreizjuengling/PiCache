package logs

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// openTestDBs opens logs.db and a settings store in dir. The caller closes them.
func openTestDBs(t *testing.T, dir string) (*db.DB, *db.DB, *settings.Store) {
	t.Helper()
	ldb, err := db.Open(filepath.Join(dir, "logs.db"), 2)
	if err != nil {
		t.Fatal(err)
	}
	cdb, err := db.Open(filepath.Join(dir, "picache.db"), 1)
	if err != nil {
		t.Fatal(err)
	}
	set, err := settings.Open(context.Background(), cdb, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	return ldb, cdb, set
}

// newTestStore returns a store whose writer the test drives directly
// (Start is not running).
func newTestStore(t *testing.T) (*Store, *settings.Store) {
	t.Helper()
	ldb, cdb, set := openTestDBs(t, t.TempDir())
	t.Cleanup(func() { ldb.Close(); cdb.Close() })
	s, err := New(context.Background(), ldb, set, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s, set
}

func updateLogs(t *testing.T, set *settings.Store, fn func(*settings.Logs)) {
	t.Helper()
	if _, err := set.Update(context.Background(), func(a *settings.All) error { fn(&a.Logs); return nil }); err != nil {
		t.Fatal(err)
	}
}

func count(t *testing.T, s *Store, table string) int {
	t.Helper()
	var n int
	if err := s.d.R.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func wantKind(t *testing.T, err error, kind apperr.Kind) {
	t.Helper()
	if apperr.KindOf(err) != kind {
		t.Fatalf("error kind = %v (%v), want %v", apperr.KindOf(err), err, kind)
	}
}

func query(ts time.Time, client, qname, status string) QueryEvent {
	return QueryEvent{Time: ts, ClientIP: client, QName: qname, QType: "A", Status: status, RCode: "NOERROR",
		Protocol: "udp", DurationUs: 1000}
}

func cacheEv(ts time.Time, client, service, group string, sent, hit, wan, durMs int64) CacheEvent {
	return CacheEvent{Time: ts, ClientIP: client, Service: service, Host: "cdn.example.com", Path: "/depot/1/chunk/x",
		Method: "GET", Status: 200, CacheStatus: "HIT", BytesSent: sent, BytesHit: hit, BytesWAN: wan,
		DurationMs: durMs, GroupKey: group, Label: "Label " + group}
}
