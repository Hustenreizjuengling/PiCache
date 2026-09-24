package app

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/hustenreizjuengling/picache/internal/api"
	"github.com/hustenreizjuengling/picache/internal/apperr"
	cachestore "github.com/hustenreizjuengling/picache/internal/lancache/store"
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

func (a *App) reconcileStore(ctx context.Context) {
	a.storeMu.Lock()
	defer a.storeMu.Unlock()
	cfg := a.set.Get().Cache
	target := cfg.ActiveStoreID
	cur := a.store.Load()
	tst := a.storage.Status(target)

	root, storeID, err := a.storage.StoreRoot(target)
	if err != nil {
		if cur != nil {
			a.log.Warn("cache store went offline; serving uncached (pass-through)", slog.String("target", target), slog.Any("reason", err))
			a.store.Store(nil)
			_ = cur.Close()
		}
		a.storeFull.Store(false)
		a.storeState.Store(&api.StoreState{TargetID: target, PassThrough: true, Reason: reason(err), Hint: tst.Hint,
			TotalBytes: tst.TotalBytes, FreeBytes: tst.FreeBytes, SDCard: tst.SDCard})
		return
	}
	if cur == nil || cur.ID() != storeID || cur.Root() != root {
		st, err := cachestore.Open(ctx, cachestore.Options{
			Root:      root,
			IndexPath: filepath.Join(a.paths.CacheIndexDir, storeID+".db"),
			StoreID:   storeID,
			Log:       a.log,
		})
		if err != nil {
			a.logStoreErr("cannot open cache store", target, root, err)
			if cur != nil {
				a.store.Store(nil)
				_ = cur.Close()
			}
			a.storeState.Store(&api.StoreState{TargetID: target, StoreID: storeID, PassThrough: true, Reason: "cannot open store: " + reason(err)})
			return
		}
		st.OnEvict(func(o cachestore.Object, why string) {
			a.logs.LogEviction(logs.EvictionEvent{Time: time.Now().UTC(), StoreID: storeID, ObjectID: o.ID,
				Service: o.Service, GroupKey: o.GroupKey, Bytes: o.CachedBytes, Reason: why})
		})
		a.store.Store(st)
		if cur != nil {
			_ = cur.Close()
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
		go func() {
			if _, err := a.EvictNow(context.WithoutCancel(ctx)); err != nil && !errors.Is(err, errNoStore) {
				a.log.Warn("eviction failed", slog.Any("err", err))
			}
		}()
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

// evictLoop applies retention and size limits every minute.
func (a *App) evictLoop(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := a.EvictNow(ctx); err != nil && !errors.Is(err, errNoStore) && !errors.Is(err, cachestore.ErrClosed) {
				a.log.Warn("eviction failed", slog.Any("err", err))
			}
		}
	}
}

var errNoStore = apperr.Unavailable("no cache store is online")

func (a *App) policy() cachestore.Policy {
	c := a.set.Get().Cache
	target := c.ActiveStoreID
	st := a.storage.Status(target)
	return cachestore.Policy{
		MaxAge:       time.Duration(c.MaxAgeDays) * 24 * time.Hour,
		MaxBytes:     c.MaxSizeBytes,
		MinFreeBytes: effectiveMinFree(c, st),
		FreeBytes: func() (uint64, error) {
			s := a.storage.Status(target)
			if s.CheckedAt.IsZero() || s.TotalBytes == 0 {
				return 0, errors.New("free space unknown")
			}
			return s.FreeBytes, nil
		},
	}
}

// StoreState implements api.Runtime.
func (a *App) StoreState() api.StoreState {
	if s := a.storeState.Load(); s != nil {
		return *s
	}
	return api.StoreState{}
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

// EvictNow runs one eviction pass on the active store.
func (a *App) EvictNow(ctx context.Context) (cachestore.EvictResult, error) {
	st := a.store.Load()
	if st == nil {
		return cachestore.EvictResult{}, errNoStore
	}
	res, err := st.Evict(ctx, a.policy())
	if err == nil {
		if res.Full != a.storeFull.Load() {
			a.storeFull.Store(res.Full)
			if res.Full {
				a.log.Warn("cache store is full and nothing more can be evicted (pinned content); new downloads are served uncached")
			}
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
