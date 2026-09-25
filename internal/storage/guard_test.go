package storage

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/hustenreizjuengling/picache/internal/config"
	cachestore "github.com/hustenreizjuengling/picache/internal/dlcache/store"
)

// bareManager is a Manager without a database (the guard never uses it).
func bareManager(cfg *config.Config, targets ...Target) *Manager {
	m := &Manager{cfg: cfg, log: slog.New(slog.DiscardHandler), sliceSize: func() int64 { return 1 << 20 },
		targets: map[string]*entry{}, kick: make(chan struct{}, 1)}
	m.probeFn = m.probe
	for _, t := range targets {
		m.targets[t.ID] = &entry{t: t, st: pendingStatus(t, "not checked yet")}
	}
	return m
}

// TestGuardLoop runs the real mount guard on the built-in store: online at
// start, offline within one interval after the marker disappears, back
// online after a kick.
func TestGuardLoop(t *testing.T) {
	cfg := testConfig(t)
	mk, err := cachestore.InitRoot(cfg.CacheDir, testID, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	local := Target{ID: LocalTargetID, Name: "Local disk", Kind: KindLocal, Mode: ModeExternal, Path: localPath(cfg), StoreID: mk.StoreID}
	marker, _ := os.ReadFile(filepath.Join(cfg.CacheDir, cachestore.MarkerFile))

	synctest.Test(t, func(t *testing.T) {
		m := bareManager(cfg, local)
		var changes atomic.Int64
		m.OnStatusChange(func(string, Status) { changes.Add(1) })
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { m.Start(ctx); close(done) }()

		synctest.Wait()
		if st := m.Status(LocalTargetID); !st.Online || st.StoreID != testID {
			t.Fatalf("after start: %+v", st)
		}
		if _, _, err := m.StoreRoot(LocalTargetID); err != nil {
			t.Fatal(err)
		}

		os.Remove(filepath.Join(cfg.CacheDir, cachestore.MarkerFile))
		time.Sleep(guardInterval - time.Second)
		synctest.Wait()
		if !m.Status(LocalTargetID).Online {
			t.Fatal("checked before the interval")
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if st := m.Status(LocalTargetID); st.Online || !strings.Contains(st.Reason, "marker") {
			t.Fatalf("after losing the marker: %+v", st)
		}

		if err := os.WriteFile(filepath.Join(cfg.CacheDir, cachestore.MarkerFile), marker, 0o640); err != nil {
			t.Fatal(err)
		}
		m.kickGuard()
		synctest.Wait()
		if !m.Status(LocalTargetID).Online {
			t.Fatalf("after kick: %+v", m.Status(LocalTargetID))
		}
		if n := changes.Load(); n != 3 {
			t.Fatalf("want 3 status changes, got %d", n)
		}
		cancel()
		<-done
	})
}

// TestGuardHungCheck: a check that never returns (hung NAS) is reported as
// not responding after checkTimeout, is not started twice (single flight),
// callers are not blocked, and its late result is still recorded.
func TestGuardHungCheck(t *testing.T) {
	cfg := testConfig(t)
	nas := Target{ID: testID, Name: "NAS", Kind: KindNFS, Mode: ModeExternal, Path: filepath.Join(cfg.MountRoot, "nas")}
	synctest.Test(t, func(t *testing.T) {
		m := bareManager(cfg, nas)
		release := make(chan struct{})
		var probes atomic.Int64
		m.probeFn = func(tg Target) checkResult {
			probes.Add(1)
			<-release
			return checkResult{st: Status{Online: true, StoreRoot: tg.Path, CheckedAt: time.Now()}}
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { m.Start(ctx); close(done) }()

		time.Sleep(checkTimeout)
		synctest.Wait()
		st := m.Status(testID)
		if st.Online || !strings.Contains(st.Reason, "does not respond") {
			t.Fatalf("hung check: %+v", st)
		}
		// Callers read memory only; a fresh check gives up after its timeout.
		if _, err := m.freshCheck(ctx, testID, time.Second); err == nil {
			t.Fatal("freshCheck must time out while the check hangs")
		}
		time.Sleep(3 * guardInterval)
		synctest.Wait()
		if n := probes.Load(); n != 1 {
			t.Fatalf("single flight violated: %d probes", n)
		}

		close(release)
		synctest.Wait()
		if !m.Status(testID).Online {
			t.Fatalf("late result not recorded: %+v", m.Status(testID))
		}
		cancel()
		<-done
	})
}

// TestGuardDiscardsStaleResult: a result for an old configuration (the
// target was relocated while the check ran) is dropped.
func TestGuardDiscardsStaleResult(t *testing.T) {
	cfg := testConfig(t)
	nas := Target{ID: testID, Name: "NAS", Kind: KindLocal, Mode: ModeExternal, Path: filepath.Join(cfg.MountRoot, "nas")}
	synctest.Test(t, func(t *testing.T) {
		m := bareManager(cfg, nas)
		release := make(chan struct{})
		m.probeFn = func(tg Target) checkResult {
			<-release
			return checkResult{st: Status{Online: true, StoreRoot: tg.Path}}
		}
		m.mu.Lock()
		run := m.startLocked(testID, m.targets[testID])
		m.targets[testID].gen++ // relocated meanwhile
		m.mu.Unlock()
		close(release)
		<-run.done
		if m.Status(testID).Online {
			t.Fatal("stale result recorded")
		}
	})
}

func TestShutdownRefusesChecks(t *testing.T) {
	cfg := testConfig(t)
	m := bareManager(cfg, Target{ID: LocalTargetID})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m.Start(ctx) // returns after one pass
	if _, err := m.freshCheck(context.Background(), LocalTargetID, time.Second); err != errShuttingDown {
		t.Fatalf("want errShuttingDown, got %v", err)
	}
}

func TestSignificant(t *testing.T) {
	base := Status{Online: true, TotalBytes: 1000 << 30, FreeBytes: 500 << 30}
	for name, tc := range map[string]struct {
		mod  func(*Status)
		want bool
	}{
		"same":        {func(*Status) {}, false},
		"latency":     {func(s *Status) { s.LatencyMs = 99 }, false},
		"small space": {func(s *Status) { s.FreeBytes -= 100 << 20 }, false},
		"1 GiB":       {func(s *Status) { s.FreeBytes += 1 << 30 }, true},
		"offline":     {func(s *Status) { s.Online = false }, true},
		"reason":      {func(s *Status) { s.Reason = "x" }, true},
		"apply state": {func(s *Status) { s.ApplyState = applyQueued }, true},
		"initialised": {func(s *Status) { s.Initialised = true }, true},
		"writable":    {func(s *Status) { s.Writable = true }, true},
		"total":       {func(s *Status) { s.TotalBytes /= 2 }, true},
	} {
		b := base
		tc.mod(&b)
		if got := significant(base, b); got != tc.want {
			t.Errorf("%s: significant = %v", name, got)
		}
	}
	small := Status{TotalBytes: 10 << 30, FreeBytes: 5 << 30}
	moved, tiny := small, small
	moved.FreeBytes -= 200 << 20 // > 1 % of 10 GiB
	tiny.FreeBytes -= 50 << 20
	if !significant(small, moved) || significant(small, tiny) {
		t.Error("1 % rule")
	}
}
