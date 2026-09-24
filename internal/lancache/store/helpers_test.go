package cachestore

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

const (
	testStoreID = "0123456789abcdef0123456789abcdef"
	testSlice   = 256 << 10 // smallest valid slice size keeps tests fast
)

type testEnv struct {
	t     *testing.T
	dir   string
	root  string
	index string
	opt   Options
}

func newEnv(t *testing.T) *testEnv { return newEnvSize(t, testSlice) }

func newEnvSize(t *testing.T, sliceSize int64) *testEnv {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "store")
	if err := os.Mkdir(root, 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := InitRoot(root, testStoreID, sliceSize); err != nil {
		t.Fatal(err)
	}
	e := &testEnv{t: t, dir: dir, root: root, index: filepath.Join(dir, "cache-index", testStoreID+".db")}
	e.opt = Options{Root: root, IndexPath: e.index, StoreID: testStoreID,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	return e
}

// open opens the store; it is closed at the end of the test.
func (e *testEnv) open() *Store {
	e.t.Helper()
	s, err := Open(context.Background(), e.opt)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { _ = s.Close() })
	return s
}

func newStore(t *testing.T) (*Store, *testEnv) {
	e := newEnv(t)
	return e.open(), e
}

func testMeta(service, path, group string, total int64) (string, Meta) {
	return ObjectID(service, path), Meta{Service: service, Host: "cdn.example.com", Path: path, GroupKey: group,
		Total: total, Header: http.Header{"Content-Type": {"application/x-test"}, "Last-Modified": {"Mon, 02 Jan 2006 15:04:05 GMT"}}}
}

// payload returns deterministic data for slice idx of an object.
func payload(id string, idx, n int64) []byte {
	seed := []byte(id + ":" + strconv.FormatInt(idx, 10) + ";")
	return bytes.Repeat(seed, int(n)/len(seed)+1)[:n]
}

// putObject creates an object and writes all of its slices.
func putObject(t *testing.T, s *Store, service, path, group string, total int64) (string, uint64) {
	t.Helper()
	id, m := testMeta(service, path, group, total)
	gen, err := s.SetMeta(context.Background(), id, m)
	if err != nil {
		t.Fatal(err)
	}
	for i := int64(0); i < slicesFor(total, s.SliceSize()); i++ {
		if err := s.WriteSlice(context.Background(), id, gen, i, payload(id, i, sliceLen(total, s.SliceSize(), i))); err != nil {
			t.Fatal(err)
		}
	}
	return id, gen
}

// counts returns the number of LRU entries and overlay entries.
func (h *heads) counts() (lru, overlay int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.nodes), len(h.overlay)
}

// waitRemovals waits until the background remover has emptied its queue.
func waitRemovals(t *testing.T, s *Store) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for s.queuedRemovals() > 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%d slice files still queued for removal", s.queuedRemovals())
		}
		time.Sleep(time.Millisecond)
	}
}

func mustFlush(t *testing.T, s *Store) {
	t.Helper()
	if err := s.flush(context.Background(), true); err != nil {
		t.Fatal(err)
	}
}

func fileExists(t *testing.T, e *testEnv, id string, idx int64) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(e.root, filepath.FromSlash(sliceName(id, idx))))
	return err == nil
}

func readAll(t *testing.T, s *Store, id string, gen uint64, idx int64) []byte {
	t.Helper()
	r, err := s.ReadSlice(context.Background(), id, gen, idx)
	if err != nil {
		t.Fatalf("ReadSlice(%d): %v", idx, err)
	}
	defer r.Close()
	b := make([]byte, r.Size())
	if _, err := r.ReadAt(b, 0); err != nil && err != io.EOF {
		t.Fatal(err)
	}
	return b
}

// checkAggregates verifies that store_groups and the usage counters match a
// recomputation from the object and slice rows.
func checkAggregates(t *testing.T, s *Store) {
	t.Helper()
	mustFlush(t, s)
	ctx := context.Background()
	var bad int
	if err := s.db.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM store_objects o WHERE
		cached_bytes <> COALESCE((SELECT SUM(size) FROM store_slices s WHERE s.object_id = o.id), 0)
		OR slice_count <> (SELECT COUNT(*) FROM store_slices s WHERE s.object_id = o.id)`).Scan(&bad); err != nil {
		t.Fatal(err)
	}
	if bad != 0 {
		t.Fatalf("%d objects with inconsistent cached_bytes/slice_count", bad)
	}
	if err := s.db.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM (
		SELECT service, group_key, COUNT(*) AS n, SUM(slice_count) AS sl, SUM(cached_bytes) AS cb, SUM(total) AS tb,
			SUM(hits) AS h, SUM(bytes_served) AS bs FROM store_objects GROUP BY service, group_key) x
		FULL OUTER JOIN store_groups g ON g.service = x.service AND g.group_key = x.group_key
		WHERE x.n IS NOT g.objects OR x.sl IS NOT g.slices OR x.cb IS NOT g.cached_bytes OR x.tb IS NOT g.total_bytes
			OR x.h IS NOT g.hits OR x.bs IS NOT g.bytes_served`).Scan(&bad); err != nil {
		t.Fatal(err)
	}
	if bad != 0 {
		t.Fatalf("%d groups with inconsistent aggregates", bad)
	}
	var o, sl, cb int64
	if err := s.db.R.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(slice_count), 0), COALESCE(SUM(cached_bytes), 0)
		FROM store_objects`).Scan(&o, &sl, &cb); err != nil {
		t.Fatal(err)
	}
	if u := s.Usage(); u.Objects != o || u.Slices != sl || u.CachedBytes != cb {
		t.Fatalf("usage %+v, index has objects=%d slices=%d bytes=%d", u, o, sl, cb)
	}
}
