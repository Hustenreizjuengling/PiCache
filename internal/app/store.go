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
	"github.com/hustenreizjuengling/picache/internal/settings"
	"github.com/hustenreizjuengling/picache/internal/logs"
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

func (a *App) reconcileStore(ctx context.Context) {
	a.storeMu.Lock()
	defer a.storeMu.Unlock()
	target := a.set.Get().Cache.ActiveStoreID
	cur := a.store.Load()

	root, storeID, err := a.storage.StoreRoot(target)
	if err != nil {
		if cur != nil {
			a.log.Warn("cache store went offline; serving uncached (pass-through)", slog.String("target", target), slog.Any("reason", err))
			a.store.Store(nil)
			_ = cur.Close()
		}
		a.storeState.Store(&api.StoreState{TargetID: target, Online: false, PassThrough: true, Reason: reason(err)})
		return
	}
	if cur != nil && cur.ID() == storeID && cur.Root() == root {
		u := cur.Usage()
		a.storeState.Store(&api.StoreState{TargetID: target, StoreID: storeID, Online: true, Usage: &u, SliceSize: cur.SliceSize()})
		return
	}
	st, err := cachestore.Open(ctx, cachestore.Options{
		Root:      root,
		IndexPath: filepath.Join(a.paths.CacheIndexDir, storeID+".db"),
		StoreID:   storeID,
		SliceSize: a.set.Get().Cache.SliceSizeBytes,
		Log:       a.log,
	})
	if err != nil {
		a.log.Error("cannot open cache store", slog.String("target", target), slog.String("root", root), slog.Any("err", err))
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
	u := st.Usage()
	a.storeState.Store(&api.StoreState{TargetID: target, StoreID: storeID, Online: true, Usage: &u, SliceSize: st.SliceSize()})
	a.log.Info("cache store online", slog.String("target", target), slog.String("root", root), slog.String("store", storeID))
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
			if _, err := a.EvictNow(ctx); err != nil && !errors.Is(err, errNoStore) {
				a.log.Warn("eviction failed", slog.Any("err", err))
			}
		}
	}
}

var errNoStore = apperr.Unavailable("no cache store is online")

func (a *App) policy() cachestore.Policy {
	c := a.set.Get().Cache
	p := cachestore.Policy{
		MaxAge:        time.Duration(c.MaxAgeDays) * 24 * time.Hour,
		ServiceMaxAge: map[string]time.Duration{},
		MaxBytes:      c.MaxSizeBytes,
		MinFreeBytes:  c.MinFreeBytes,
	}
	for svc, d := range c.ServiceMaxAgeDays {
		p.ServiceMaxAge[svc] = time.Duration(d) * 24 * time.Hour
	}
	target := c.ActiveStoreID
	p.FreeBytes = func() (uint64, error) {
		st := a.storage.Status(target)
		if st.CheckedAt.IsZero() {
			return 0, errors.New("free space unknown")
		}
		return st.FreeBytes, nil
	}
	return p
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
		return apperr.Wrap(apperr.KindUnavailable, err, "target %q is not usable: %s", targetID, reason(err))
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
	return st.Evict(ctx, a.policy())
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

