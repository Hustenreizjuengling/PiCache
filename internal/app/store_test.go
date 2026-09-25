package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/config"
	"github.com/hustenreizjuengling/picache/internal/db"
	cachestore "github.com/hustenreizjuengling/picache/internal/dlcache/store"
	"github.com/hustenreizjuengling/picache/internal/logs"
	"github.com/hustenreizjuengling/picache/internal/secrets"
	"github.com/hustenreizjuengling/picache/internal/settings"
	"github.com/hustenreizjuengling/picache/internal/storage"
)

const testSlice = 256 << 10 // smallest valid slice size

// newTestApp builds the parts of an App that the store logic needs: config
// database, settings, storage manager (guard running, built-in store
// online) and a discarding log store.
func newTestApp(t *testing.T) *App {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{DataDir: filepath.Join(dir, "data"), CacheDir: filepath.Join(dir, "cache"),
		MountRoot: filepath.Join(dir, "mnt")}
	log := slog.New(slog.DiscardHandler)
	a := newApp(cfg, log)
	if err := a.prepareDirs(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var err error
	if a.cdb, err = db.Open(a.paths.ConfigDB, 2); err != nil {
		t.Fatal(err)
	}
	if a.set, err = settings.Open(ctx, a.cdb, log); err != nil {
		t.Fatal(err)
	}
	if a.box, err = secrets.Open(a.paths.MasterKeyFile); err != nil {
		t.Fatal(err)
	}
	if a.storage, err = storage.New(ctx, a.cdb, a.box, cfg, func() int64 { return testSlice }, log); err != nil {
		t.Fatal(err)
	}
	a.logs = logs.Discard("test", log)
	gctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { a.storage.Start(gctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		<-done
		a.closeState()
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, _, err := a.storage.StoreRoot(storage.LocalTargetID); err == nil {
			return a
		} else if time.Now().After(deadline) {
			t.Fatalf("built-in store not online: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The "store full" flag belongs to the store it was computed for: a newly
// opened store must not start in no-store mode.
func TestStoreFullResetWhenAnotherStoreIsPublished(t *testing.T) {
	a := newTestApp(t)
	a.storeFull.Store(true) // the previous store was full
	a.reconcileStore(context.Background())
	if a.store.Load() == nil {
		t.Fatalf("no store opened: %+v", a.StoreState())
	}
	if a.storeFull.Load() || a.StoreState().Full {
		t.Fatal("the new store inherited the full flag of the previous one")
	}
}

// A store open that hangs on the storage must neither block reconcileStore
// for good nor make closeState (shutdown) wait for it.
func TestHungStoreOpenDoesNotBlockShutdown(t *testing.T) {
	a := newTestApp(t)
	defer func(d time.Duration) { storeOpenTimeout = d }(storeOpenTimeout)
	storeOpenTimeout = 200 * time.Millisecond
	entered, release := make(chan struct{}), make(chan struct{})
	var opened atomic.Pointer[cachestore.Store]
	var calls atomic.Int32
	a.openCacheStore = func(ctx context.Context, opt cachestore.Options) (*cachestore.Store, error) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-release // the marker read hangs on a dead NAS
		st, err := cachestore.Open(ctx, opt)
		opened.Store(st)
		return st, err
	}
	ctx := context.Background()
	done := make(chan struct{})
	go func() { a.reconcileStore(ctx); close(done) }()
	<-entered
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("reconcileStore waits for the hung open")
	}
	if st := a.StoreState(); !st.PassThrough || !strings.Contains(st.Reason, "did not respond") {
		t.Fatalf("store state %+v", st)
	}
	// No second open is started while the first one hangs.
	a.reconcileStore(ctx)
	if st := a.StoreState(); !strings.Contains(st.Reason, "has not returned yet") || calls.Load() != 1 {
		t.Fatalf("store state %+v after %d opens", st, calls.Load())
	}

	closed := make(chan struct{})
	go func() { a.closeState(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("closeState waits for the hung open")
	}

	close(release) // the mount recovers: the abandoned open closes its store
	deadline := time.Now().Add(5 * time.Second)
	for a.storeOpening.Load() {
		if time.Now().After(deadline) {
			t.Fatal("the abandoned open never finished")
		}
		time.Sleep(10 * time.Millisecond)
	}
	st := opened.Load()
	if st == nil {
		t.Fatal("the store was not opened")
	}
	if _, err := st.Evict(ctx, cachestore.Policy{}); !errors.Is(err, cachestore.ErrClosed) {
		t.Fatalf("abandoned store still open: %v", err)
	}
	if a.store.Load() != nil {
		t.Fatal("a store was published after closeState")
	}
}

// putObjects stores n objects of one slice each.
func putObjects(t *testing.T, st *cachestore.Store, n int) {
	t.Helper()
	ctx := context.Background()
	for i := range n {
		path := fmt.Sprintf("/depot/1/chunk/%02d", i)
		id := cachestore.ObjectID("steam", path)
		gen, err := st.SetMeta(ctx, id, cachestore.Meta{Service: "steam", Host: "cdn.example.com", Path: path,
			GroupKey: "steam:depot:1", Total: testSlice, Header: http.Header{"Content-Type": {"application/x-test"}}})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.WriteSlice(ctx, id, gen, 0, make([]byte, testSlice)); err != nil {
			t.Fatal(err)
		}
	}
}

// Low-space eviction runs more often than the guard samples the free space.
// A pass must not evict the deficit again that an earlier pass already
// freed because it still sees the same stale sample.
func TestEvictionDoesNotReuseAStaleFreeSample(t *testing.T) {
	const minFree = 100 * testSlice
	staleFree := uint64(minFree - 3*testSlice) // guard sample: 3 slices short
	sample := storage.Status{Online: true, CheckedAt: time.Now(), TotalBytes: 1 << 40, FreeBytes: staleFree}
	c := settings.Defaults().Cache
	c.MinFreeBytes, c.MaxAgeDays, c.MaxSizeBytes = minFree, 0, 0

	for _, tc := range []struct {
		name  string
		fresh bool // statfs works (Linux); else only the guard sample exists
	}{{"fresh statfs", true}, {"guard sample only", false}} {
		t.Run(tc.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "store")
			const id = "0123456789abcdef0123456789abcdef"
			if err := os.Mkdir(root, 0o750); err != nil {
				t.Fatal(err)
			}
			if _, err := cachestore.InitRoot(root, id, testSlice); err != nil {
				t.Fatal(err)
			}
			st, err := cachestore.Open(context.Background(), cachestore.Options{Root: root, StoreID: id,
				IndexPath: filepath.Join(filepath.Dir(root), "index.db"), Log: slog.New(slog.DiscardHandler)})
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			putObjects(t, st, 20)
			var removed atomic.Int64 // the filesystem gains what eviction removes
			st.OnEvict(func(o cachestore.Object, _ string) { removed.Add(o.CachedBytes) })
			free := &freeSpace{statfs: func(string) (uint64, bool) { return staleFree + uint64(removed.Load()), tc.fresh }}

			pass := func() cachestore.EvictResult {
				t.Helper()
				res, err := st.Evict(context.Background(), evictPolicy(c, sample, root, free))
				if err != nil {
					t.Fatal(err)
				}
				return res
			}
			first := pass()
			wantBytes := int64(minFree)*105/100 - int64(staleFree)
			if first.Bytes < wantBytes || first.Bytes > wantBytes+testSlice {
				t.Fatalf("first pass evicted %d bytes, deficit %d", first.Bytes, wantBytes)
			}
			if again := pass(); again.Objects != 0 {
				t.Fatalf("second pass with the same guard sample evicted %d more objects", again.Objects)
			}
			// The guard measures again: the space is there, nothing more to do.
			sample := sample
			sample.CheckedAt, sample.FreeBytes = sample.CheckedAt.Add(30*time.Second), staleFree+uint64(removed.Load())
			if res, err := st.Evict(context.Background(), evictPolicy(c, sample, root, free)); err != nil || res.Objects != 0 {
				t.Fatalf("pass after a new sample: %+v %v", res, err)
			}
		})
	}
}

// A measurement stuck in the kernel (hung NAS) times out and is not
// started again while it hangs.
func TestFreeSpaceMeasurementIsBounded(t *testing.T) {
	defer func(d time.Duration) { freeCheckTimeout = d }(freeCheckTimeout)
	freeCheckTimeout = 50 * time.Millisecond
	release := make(chan struct{})
	var calls atomic.Int32
	f := &freeSpace{statfs: func(string) (uint64, bool) { calls.Add(1); <-release; return 1, true }}
	sample := storage.Status{CheckedAt: time.Now(), TotalBytes: 100, FreeBytes: 42}
	start := time.Now()
	if n, err := f.measure("/srv/picache/nas", sample); err != nil || n != 42 {
		t.Fatalf("hung statfs: %d %v (want the guard sample)", n, err)
	}
	if _, err := f.measure("/srv/picache/nas", sample); err == nil {
		t.Fatal("the same guard sample was handed out twice")
	}
	if time.Since(start) > 2*time.Second || calls.Load() != 1 {
		t.Fatalf("took %v with %d statfs calls", time.Since(start), calls.Load())
	}
	close(release)
}

func TestStoreStateReportsMaxSize(t *testing.T) {
	a := newTestApp(t)
	if _, err := a.set.Update(context.Background(), func(s *settings.All) error {
		s.Cache.MaxSizeBytes = 500 << 30
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	a.reconcileStore(context.Background())
	if got := a.StoreState().MaxSizeBytes; got != 500<<30 {
		t.Fatalf("maxSizeBytes = %d", got)
	}
}

// The background components get their own time to stop after the HTTP
// servers, even when an open download used up the HTTP grace period.
func TestShutdownWaitsForComponentsAfterHTTPGrace(t *testing.T) {
	defer func(h, s, c time.Duration) { httpShutdownGrace, serverStopWait, componentStopWait = h, s, c }(
		httpShutdownGrace, serverStopWait, componentStopWait)
	httpShutdownGrace, serverStopWait, componentStopWait = 200*time.Millisecond, time.Second, 5*time.Second

	a := newApp(&config.Config{DataDir: t.TempDir()}, slog.New(slog.DiscardHandler))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	stuck := make(chan struct{})
	defer close(stuck)
	hs := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select { // a long download
		case <-stuck:
		case <-r.Context().Done():
		}
	})}
	var srv, bg sync.WaitGroup
	srv.Go(func() { _ = hs.Serve(ln) })
	resp, err := http.Get("http://" + ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var flushed atomic.Bool
	bg.Go(func() { // a component flushing its last batch after the cancel
		time.Sleep(500 * time.Millisecond)
		flushed.Store(true)
	})
	start := time.Now()
	a.shutdown([]*http.Server{hs}, &srv, &bg)
	if !flushed.Load() {
		t.Fatalf("shutdown returned after %v without waiting for the components", time.Since(start))
	}
}
