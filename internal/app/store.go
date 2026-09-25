package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hustenreizjuengling/picache/internal/api"
	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/dlcache/services"
	cachestore "github.com/hustenreizjuengling/picache/internal/dlcache/store"
	"github.com/hustenreizjuengling/picache/internal/logs"
	"github.com/hustenreizjuengling/picache/internal/settings"
	"github.com/hustenreizjuengling/picache/internal/storage"
)

// kickStore asks the store loop to re-evaluate the active store.
func (a *App) kickStore() {
	select {
	case a.storeKick <- struct{}{}:
	default:
	}
}

// storeLoop keeps the active cache store open while its target is online and
// closes it (proxy → pass-through) when it goes offline.
func (a *App) storeLoop(ctx context.Context) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		a.reconcileStore(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-a.storeKick:
		}
	}
}

// effectiveMinFree is min(cache.minFreeBytes, 10 % of the filesystem), but
// at least 2 GiB when the store shares its filesystem with the data dir.
func effectiveMinFree(c settings.Cache, st storage.Status) int64 {
	minFree := c.MinFreeBytes
	if st.TotalBytes > 0 {
		if tenth := int64(st.TotalBytes / 10); tenth < minFree {
			minFree = tenth
		}
	}
	if st.SameFSAsData && minFree < 2<<30 {
		minFree = 2 << 30
	}
	return minFree
}

// storeOpenTimeout bounds opening a cache store: its first steps (marker,
// root, directories) run on the storage target, and on a hung NAS they
// block in the kernel.
var storeOpenTimeout = 30 * time.Second

// reconcileStore opens, switches or closes the active store to match the
// active target and its guard status. Calls are serialised by reconcileMu;
// storeMu is held only to publish a store, so closeState never waits for a
// store that is being opened.
func (a *App) reconcileStore(ctx context.Context) {
	a.reconcileMu.Lock()
	defer a.reconcileMu.Unlock()
	cfg := a.set.Get().Cache
	target := cfg.ActiveStoreID
	cur := a.store.Load()
	tst := a.storage.Status(target)

	root, storeID, err := a.storage.StoreRoot(target)
	if err != nil {
		if cur != nil {
			a.log.Warn("cache store went offline; serving uncached (pass-through)", slog.String("target", target), slog.Any("reason", err))
		}
		a.publishStore(nil)
		a.storeState.Store(&api.StoreState{TargetID: target, PassThrough: true, Reason: reason(err), Hint: tst.Hint,
			TotalBytes: tst.TotalBytes, FreeBytes: tst.FreeBytes, SDCard: tst.SDCard})
		return
	}
	if cur == nil || cur.ID() != storeID || cur.Root() != root {
		st, err := a.openStore(ctx, cachestore.Options{
			Root:      root,
			IndexPath: filepath.Join(a.paths.CacheIndexDir, storeID+".db"),
			StoreID:   storeID,
			GroupKey:  func(svc, host, path string) string { return services.GroupFor(svc, host, path).Key },
			Log:       a.log,
		})
		if err != nil {
			a.logStoreErr("cannot open cache store", target, root, err)
			a.publishStore(nil)
			a.storeState.Store(&api.StoreState{TargetID: target, StoreID: storeID, PassThrough: true, Reason: "cannot open store: " + reason(err)})
			return
		}
		st.OnEvict(func(o cachestore.Object, why string) {
			a.logs.LogEviction(logs.EvictionEvent{Time: time.Now().UTC(), StoreID: storeID, ObjectID: o.ID,
				Service: o.Service, GroupKey: o.GroupKey, Bytes: o.CachedBytes, Reason: why})
		})
		if !a.publishStore(st) {
			return // shutting down
		}
		cur = st
		a.lastStoreErr = ""
		a.log.Info("cache store online", slog.String("target", target), slog.String("root", root), slog.String("store", storeID))
	}
	minFree := effectiveMinFree(cfg, tst)
	lowSpace := tst.FreeBytes > 0 && int64(tst.FreeBytes) < minFree
	u := cur.Usage()
	a.storeState.Store(&api.StoreState{TargetID: target, StoreID: storeID, Online: true, Usage: &u,
		SliceSize: cur.SliceSize(), TotalBytes: tst.TotalBytes, FreeBytes: tst.FreeBytes, MinFreeBytes: minFree,
		LowSpace: lowSpace, Full: a.storeFull.Load(), SDCard: tst.SDCard, Hint: tst.Hint})
	if lowSpace {
		a.kickEvict() // the evict loop measures the free space again before it evicts
	}
}

// publishStore makes st (nil: none) the active store and closes the previous
// one. The "store full" flag described the previous store and is reset.
// After closeState it publishes nothing, closes st and returns false.
func (a *App) publishStore(st *cachestore.Store) bool {
	a.storeMu.Lock()
	if a.storeClosed {
		a.storeMu.Unlock()
		if st != nil {
			_ = st.Close()
		}
		return false
	}
	old := a.store.Swap(st)
	a.storeFull.Store(false)
	a.storeMu.Unlock()
	if old != nil && old != st {
		_ = old.Close()
	}
	return true
}

// openStore opens a cache store, bounded by storeOpenTimeout and ctx. An
// open that is given up keeps running in the background and closes its
// store when it returns; until then no other open starts (a hung mount
// would otherwise collect one blocked goroutine per attempt).
func (a *App) openStore(ctx context.Context, opt cachestore.Options) (*cachestore.Store, error) {
	if !a.storeOpening.CompareAndSwap(false, true) {
		return nil, errors.New("an earlier attempt to open the store has not returned yet; the storage does not respond")
	}
	open := a.openCacheStore
	if open == nil {
		open = cachestore.Open
	}
	type result struct {
		st  *cachestore.Store
		err error
	}
	res := make(chan result)
	gaveUp := make(chan struct{})
	go func() {
		st, err := open(context.WithoutCancel(ctx), opt)
		select {
		case res <- result{st, err}:
		case <-gaveUp:
			if st != nil {
				_ = st.Close()
			}
			a.storeOpening.Store(false)
		}
	}()
	t := time.NewTimer(storeOpenTimeout)
	defer t.Stop()
	select {
	case r := <-res:
		a.storeOpening.Store(false)
		return r.st, r.err
	case <-t.C:
		close(gaveUp)
		return nil, fmt.Errorf("the storage did not respond within %s", storeOpenTimeout)
	case <-ctx.Done():
		close(gaveUp)
		return nil, ctx.Err()
	}
}

// logStoreErr logs repeated store errors only when they change or once an hour.
func (a *App) logStoreErr(msg, target, root string, err error) {
	s := err.Error()
	if s == a.lastStoreErr && time.Since(a.lastStoreErrAt) < time.Hour {
		return
	}
	a.lastStoreErr, a.lastStoreErrAt = s, time.Now()
	a.log.Error(msg, slog.String("target", target), slog.String("root", root), slog.Any("err", err))
}

func reason(err error) string {
	if ae, ok := apperr.As(err); ok {
		return ae.Message
	}
	return err.Error()
}

// kickEvict asks the evict loop for a pass now (low free space).
func (a *App) kickEvict() {
	select {
	case a.evictKick <- struct{}{}:
	default:
	}
}

// evictLoop applies retention and size limits every minute and when
// reconcileStore reports low free space. It is the only background caller
// of EvictNow, so low-space passes never pile up.
func (a *App) evictLoop(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-a.evictKick:
		}
		if _, err := a.EvictNow(ctx); err != nil && !errors.Is(err, errNoStore) && !errors.Is(err, cachestore.ErrClosed) &&
			ctx.Err() == nil {
			a.log.Warn("eviction failed", slog.Any("err", err))
		}
	}
}

var errNoStore = apperr.Unavailable("no cache store is online")

// policy returns the eviction policy for a pass on st, with the free space
// of its filesystem measured now (see freeSpace.measure).
func (a *App) policy(st *cachestore.Store) cachestore.Policy {
	c := a.set.Get().Cache
	return evictPolicy(c, a.storage.Status(c.ActiveStoreID), st.Root(), &a.free)
}

// evictPolicy builds the eviction policy from the cache settings and the
// guard status of the store's target. The free space is measured once, right
// before the pass, as cachestore.Evict expects.
func evictPolicy(c settings.Cache, tst storage.Status, root string, free *freeSpace) cachestore.Policy {
	p := cachestore.Policy{
		MaxAge:       time.Duration(c.MaxAgeDays) * 24 * time.Hour,
		MaxBytes:     c.MaxSizeBytes,
		MinFreeBytes: effectiveMinFree(c, tst),
	}
	if p.MinFreeBytes > 0 {
		n, err := free.measure(root, tst)
		p.FreeBytes = func() (uint64, error) { return n, err }
	}
	return p
}

// freeCheckTimeout bounds one free-space measurement of the store (statfs
// blocks in the kernel on a hung NAS).
var freeCheckTimeout = 5 * time.Second

// freeSpace measures the free space of the active store's filesystem for
// the min-free rule of eviction.
type freeSpace struct {
	statfs func(path string) (uint64, bool) // nil: diskFree
	busy   atomic.Bool                      // a measurement is running (it may hang)
	mu     sync.Mutex
	used   time.Time // CheckedAt of the last guard sample handed out
}

// measure returns the free bytes of the filesystem holding root right before
// an eviction pass. Evict removes the deficit it computes from this value,
// so the value must not predate an earlier pass that already removed objects
// for the same deficit. It is therefore measured now (statfs, one at a time,
// bounded by freeCheckTimeout). Only when that is not possible does it fall
// back to the guard's sample (up to 30 s old), and it hands out each sample
// once: later passes skip the min-free rule until the guard measured again.
func (f *freeSpace) measure(root string, sample storage.Status) (uint64, error) {
	if n, ok := f.fresh(root); ok {
		return n, nil
	}
	if sample.CheckedAt.IsZero() || sample.TotalBytes == 0 {
		return 0, errors.New("free space unknown")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !sample.CheckedAt.After(f.used) {
		return 0, errors.New("free space not measured again since the last eviction pass")
	}
	f.used = sample.CheckedAt
	return sample.FreeBytes, nil
}

// fresh runs statfs on root with a timeout; false if it failed, timed out or
// an earlier measurement still hangs.
func (f *freeSpace) fresh(root string) (uint64, bool) {
	if root == "" || !f.busy.CompareAndSwap(false, true) {
		return 0, false
	}
	statfs := f.statfs
	if statfs == nil {
		statfs = diskFree
	}
	type result struct {
		n  uint64
		ok bool
	}
	ch := make(chan result, 1)
	go func() {
		defer f.busy.Store(false)
		n, ok := statfs(root)
		ch <- result{n, ok}
	}()
	t := time.NewTimer(freeCheckTimeout)
	defer t.Stop()
	select {
	case r := <-ch:
		return r.n, r.ok
	case <-t.C:
		return 0, false
	}
}

// StoreState implements api.Runtime. MaxSizeBytes is the current setting.
func (a *App) StoreState() api.StoreState {
	var s api.StoreState
	if p := a.storeState.Load(); p != nil {
		s = *p
	}
	if a.set != nil {
		s.MaxSizeBytes = a.set.Get().Cache.MaxSizeBytes
	}
	return s
}

// ActivateStore switches the cache to another storage target.
func (a *App) ActivateStore(ctx context.Context, targetID string) error {
	if _, _, err := a.storage.StoreRoot(targetID); err != nil {
		return err
	}
	if _, err := a.set.Update(ctx, func(s *settings.All) error { s.Cache.ActiveStoreID = targetID; return nil }); err != nil {
		return err
	}
	a.reconcileStore(ctx)
	return nil
}

// EvictNow runs one eviction pass on the active store. Passes are
// serialised (the free space is measured right before each pass), and the
// result sets the "store full" flag only while the store is still active.
func (a *App) EvictNow(ctx context.Context) (cachestore.EvictResult, error) {
	select {
	case a.evictSem <- struct{}{}:
	case <-ctx.Done():
		return cachestore.EvictResult{}, ctx.Err()
	}
	defer func() { <-a.evictSem }()
	st := a.store.Load()
	if st == nil {
		return cachestore.EvictResult{}, errNoStore
	}
	res, err := st.Evict(ctx, a.policy(st))
	if err == nil {
		a.storeMu.Lock()
		changed := a.store.Load() == st && res.Full != a.storeFull.Load()
		if changed {
			a.storeFull.Store(res.Full)
		}
		a.storeMu.Unlock()
		if changed && res.Full {
			a.log.Warn("cache store is full and nothing more can be evicted (pinned content); new downloads are served uncached")
		}
	}
	return res, err
}

// StartVerify starts a background verify/rebuild of the active store.
func (a *App) StartVerify(repair bool) error {
	st := a.store.Load()
	if st == nil {
		return errNoStore
	}
	a.verifyMu.Lock()
	defer a.verifyMu.Unlock()
	if a.verify.Running {
		return apperr.Conflict("a verification is already running")
	}
	a.verify = api.VerifyState{Running: true, Repair: repair, StartedAt: time.Now().UTC()}
	go func() {
		res, err := st.Verify(context.Background(), repair, func(p cachestore.VerifyProgress) {
			a.verifyMu.Lock()
			a.verify.Progress = p
			a.verifyMu.Unlock()
		})
		a.verifyMu.Lock()
		defer a.verifyMu.Unlock()
		a.verify.Running = false
		a.verify.FinishedAt = time.Now().UTC()
		a.verify.Progress = res.VerifyProgress
		if err != nil {
			a.verify.Error = err.Error()
			a.log.Error("cache verification failed", slog.Any("err", err))
		} else {
			a.log.Info("cache verification finished", slog.Any("result", res))
		}
	}()
	return nil
}

// VerifyState returns the verification state.
func (a *App) VerifyState() api.VerifyState {
	a.verifyMu.Lock()
	defer a.verifyMu.Unlock()
	return a.verify
}
