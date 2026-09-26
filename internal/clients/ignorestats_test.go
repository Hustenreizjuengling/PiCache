package clients

import (
	"context"
	"encoding/json/v2"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/hustenreizjuengling/picache/internal/db"
)

// Clients v3 splits ignoreLogs: a 0.11 database (clients v2) keeps the
// meaning of the single flag (ignore_stats = ignore_logs).
func TestMigrateIgnoreStats(t *testing.T) {
	ctx := context.Background()
	cdb, err := db.Open(filepath.Join(t.TempDir(), "picache.db"), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer cdb.Close()
	if err := cdb.Migrate(ctx, "clients", migrations[:2]); err != nil {
		t.Fatal(err)
	}
	if _, err := cdb.W.ExecContext(ctx, `INSERT INTO client_clients (name, comment, download_cache_bypass, ignore_logs, created_at, updated_at)
		VALUES ('A-Quiet', '', 0, 1, 1, 1), ('B-Normal', '', 0, 0, 1, 1)`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		r, err := New(ctx, cdb, nil, quiet())
		if err != nil {
			t.Fatal(err)
		}
		list, err := r.Clients(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(list) != 2 || !list[0].IgnoreLogs || !list[0].IgnoreStats || list[1].IgnoreLogs || list[1].IgnoreStats {
			t.Fatalf("clients after the migration: %+v", list)
		}
	}
	var v int
	if err := cdb.R.QueryRow(`SELECT MAX(version) FROM schema_migrations WHERE component = 'clients'`).Scan(&v); err != nil || v != len(migrations) {
		t.Fatalf("clients schema v%d (%v)", v, err)
	}
}

// An API client of 0.11 sends no ignoreStats (or null): it takes the value
// of ignoreLogs on create and on update; an explicit value is kept.
func TestIgnoreStatsInput(t *testing.T) {
	ctx := context.Background()
	r := newTestRegistry(t, false)
	decode := func(body string) ClientInput {
		t.Helper()
		var in ClientInput
		if err := json.Unmarshal([]byte(body), &in); err != nil {
			t.Fatal(err)
		}
		return in
	}
	c, err := r.CreateClient(ctx, decode(`{"name":"TV","identifiers":["192.168.1.20"],"ignoreLogs":true}`))
	if err != nil || !c.IgnoreLogs || !c.IgnoreStats {
		t.Fatalf("create without ignoreStats: %+v %v", c, err)
	}
	c, err = r.UpdateClient(ctx, c.ID, decode(`{"name":"TV","identifiers":["192.168.1.20"],"ignoreLogs":false,"ignoreStats":null}`))
	if err != nil || c.IgnoreLogs || c.IgnoreStats {
		t.Fatalf("update with null: %+v %v", c, err)
	}
	c, err = r.UpdateClient(ctx, c.ID, decode(`{"name":"TV","identifiers":["192.168.1.20"],"ignoreLogs":true,"ignoreStats":false}`))
	if err != nil || !c.IgnoreLogs || c.IgnoreStats {
		t.Fatalf("update with explicit value: %+v %v", c, err)
	}
	id := r.Identify(ip("192.168.1.20"))
	if !id.IgnoreLogs || id.IgnoreStats {
		t.Fatalf("identity %+v", id)
	}
	c2, err := r.CreateClient(ctx, decode(`{"name":"Phone","identifiers":["192.168.1.21"],"ignoreStats":true}`))
	if err != nil || c2.IgnoreLogs || !c2.IgnoreStats {
		t.Fatalf("stats only: %+v %v", c2, err)
	}
	if id := r.Identify(ip("192.168.1.21")); id.IgnoreLogs || !id.IgnoreStats {
		t.Fatalf("identity %+v", id)
	}
}

// Seen data is written every max(1 minute, logs.flushSeconds).
func TestSeenFlushInterval(t *testing.T) {
	r := newTestRegistry(t, true)
	if r.seenInterval() != time.Minute {
		t.Fatalf("default %v", r.seenInterval())
	}
	secs := 30
	r.SetFlushInterval(func() time.Duration { return time.Duration(secs) * time.Second })
	if r.seenInterval() != time.Minute {
		t.Fatalf("30 s: %v", r.seenInterval())
	}
	secs = 300
	if r.seenInterval() != 5*time.Minute {
		t.Fatalf("300 s: %v", r.seenInterval())
	}
	dir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		cdb, err := db.Open(filepath.Join(dir, "picache.db"), 1)
		if err != nil {
			t.Fatal(err)
		}
		defer cdb.Close()
		ldb, err := db.Open(filepath.Join(dir, "logs.db"), 1)
		if err != nil {
			t.Fatal(err)
		}
		defer ldb.Close()
		r, err := New(context.Background(), cdb, ldb, quiet())
		if err != nil {
			t.Fatal(err)
		}
		r.SetFlushInterval(func() time.Duration { return 300 * time.Second })
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { r.seenLoop(ctx); close(done) }()
		defer func() { cancel(); <-done }()
		r.Seen(ip("192.168.1.5"))
		stored := func() int {
			var n int
			if err := ldb.R.QueryRow(`SELECT COUNT(*) FROM clients_seen`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			return n
		}
		time.Sleep(2 * time.Minute)
		synctest.Wait()
		if stored() != 0 {
			t.Fatal("written before the interval")
		}
		time.Sleep(3*time.Minute + time.Second)
		synctest.Wait()
		if stored() != 1 {
			t.Fatal("not written after the interval")
		}
	})
}
