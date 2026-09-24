package storage

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
)

var errShuttingDown = apperr.Unavailable("PiCache is shutting down")

// checkRun is one in-flight mount guard check of a target (single flight).
type checkRun struct {
	done    chan struct{} // closed when res is set
	started time.Time
	res     checkResult
}

// checkResult is the outcome of one probe.
type checkResult struct {
	st          Status
	steps       []string // human-readable log for Test
	located     bool     // path, mount and file system are fine (the store directory may be missing)
	rootMissing bool     // the configured sub-directory does not exist yet
	writable    bool     // the write/rename/read/delete test passed
	uninit      bool     // offline only because no store has been initialised or adopted
	notMounted  bool     // the mount point is required but nothing is mounted there
}

// usable reports whether the location works (initialised or not).
func (r checkResult) usable() bool { return r.located && !r.rootMissing && r.writable }

// Start runs the 30 s health loop (statfs with timeout, single-flight) until
// ctx ends. Start blocks until ctx is done and its goroutines have exited;
// checks stuck in the kernel (hung NAS) are waited for at most 10 s.
func (m *Manager) Start(ctx context.Context) {
	t := time.NewTicker(guardInterval)
	defer t.Stop()
	for {
		m.checkAll(ctx)
		select {
		case <-ctx.Done():
			m.shutdown()
			return
		case <-t.C:
		case <-m.kick:
		}
	}
}

// kickGuard makes the guard loop check all targets now.
func (m *Manager) kickGuard() {
	select {
	case m.kick <- struct{}{}:
	default:
	}
}

// checkAll starts a check for every idle target and waits up to
// checkTimeout; targets whose check is still running are reported as not
// responding (the check keeps running and reports when it returns).
func (m *Manager) checkAll(ctx context.Context) {
	type started struct {
		id  string
		run *checkRun
	}
	var runs, hung []started
	now := time.Now()
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	for id, e := range m.targets {
		switch {
		case e.busy:
		case e.run != nil:
			if now.Sub(e.run.started) >= checkTimeout {
				hung = append(hung, started{id, e.run})
			}
		default:
			runs = append(runs, started{id, m.startLocked(id, e)})
		}
	}
	m.mu.Unlock()
	for _, h := range hung {
		m.markHung(h.id, h.run)
	}
	timer := time.NewTimer(checkTimeout)
	defer timer.Stop()
	for i, r := range runs {
		select {
		case <-r.run.done:
		case <-timer.C:
			for _, late := range runs[i:] {
				m.markHung(late.id, late.run)
			}
			return
		case <-ctx.Done():
			return
		}
	}
}

// startLocked starts a check of e (m.mu held, m.closed false).
func (m *Manager) startLocked(id string, e *entry) *checkRun {
	run := &checkRun{done: make(chan struct{}), started: time.Now()}
	e.run = run
	t, gen := e.t, e.gen
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		res := m.probeFn(t)
		if t.ID != LocalTargetID {
			res.st.ApplyState = readApplyState(requestsDir(m.cfg), t.ID, t.Mode == ModeHostApply)
			if res.notMounted && res.st.ApplyState == applyApplied {
				res.st.Hint = appliedNotMountedHint
			}
		}
		m.finish(id, gen, run, res)
	}()
	return run
}

// finish records a check result unless the target changed meanwhile.
func (m *Manager) finish(id string, gen uint64, run *checkRun, res checkResult) {
	var old, st Status
	changed, stale := false, false
	m.mu.Lock()
	run.res = res
	if e := m.targets[id]; e != nil {
		if e.run == run {
			e.run = nil
		}
		switch {
		case e.gen != gen:
			stale = true
		case !m.closed:
			old = e.st
			e.st, e.uninit = res.st, res.uninit
			if significant(e.notified, e.st) {
				e.notified, st, changed = e.st, e.st, true
			}
		}
	}
	m.mu.Unlock()
	if changed { // before releasing waiters: listeners have run when freshCheck returns
		m.logTransition(id, old, st)
		m.notify(id, st)
	}
	close(run.done)
	if stale {
		m.kickGuard() // check the new configuration now
	}
}

// markHung reports a target whose check has not returned in time.
func (m *Manager) markHung(id string, run *checkRun) {
	var old, st Status
	changed := false
	m.mu.Lock()
	e := m.targets[id]
	if e != nil && e.run == run && !m.closed {
		old = e.st
		e.st = Status{
			Reason: fmt.Sprintf("the storage does not respond (health check still running after %s)", checkTimeout),
			Hint: "A hanging NAS or network blocks file system calls. Check the NAS and the network; " +
				"PiCache serves downloads uncached meanwhile.",
			Mounted:    old.Mounted,
			FSType:     old.FSType,
			StoreRoot:  storeRootPath(e.t),
			CheckedAt:  time.Now().UTC(),
			ApplyState: old.ApplyState,
		}
		e.uninit = false
		if significant(e.notified, e.st) {
			e.notified, st, changed = e.st, e.st, true
		}
	}
	m.mu.Unlock()
	if changed {
		m.logTransition(id, old, st)
		m.notify(id, st)
	}
}

// freshCheck runs a new check of id and waits for it (an in-flight check is
// awaited first so the result reflects the current state).
func (m *Manager) freshCheck(ctx context.Context, id string, timeout time.Duration) (checkResult, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		m.mu.Lock()
		if m.closed {
			m.mu.Unlock()
			return checkResult{}, errShuttingDown
		}
		e := m.targets[id]
		if e == nil {
			m.mu.Unlock()
			return checkResult{}, apperr.NotFound("storage target", id)
		}
		run, fresh := e.run, false
		if run == nil {
			run, fresh = m.startLocked(id, e), true
		}
		m.mu.Unlock()
		select {
		case <-run.done:
			if fresh {
				return run.res, nil
			}
		case <-timer.C:
			m.markHung(id, run)
			return checkResult{}, apperr.Unavailable("the storage does not respond (no answer within %s)", timeout)
		case <-ctx.Done():
			return checkResult{}, ctx.Err()
		}
	}
}

// shutdown stops new checks and waits for running ones, but not forever: a
// statfs on a hung hard mount can block in the kernel indefinitely. Checks
// never touch the database, so returning early is safe.
func (m *Manager) shutdown() {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	timer := time.NewTimer(shutdownGrace)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		m.log.Warn("a storage check is still blocked in the kernel (hanging NAS?); not waiting for it")
	}
}

// significant reports whether listeners should hear about the change from
// a to b: state changes, or free space moving by ≥ 1 GiB or ≥ 1 %.
func significant(a, b Status) bool {
	if a.Online != b.Online || a.Reason != b.Reason || a.StoreID != b.StoreID || a.StoreRoot != b.StoreRoot ||
		a.Mounted != b.Mounted || a.ApplyState != b.ApplyState || a.TotalBytes != b.TotalBytes ||
		a.Initialised != b.Initialised || a.Writable != b.Writable {
		return true
	}
	d := max(a.FreeBytes, b.FreeBytes) - min(a.FreeBytes, b.FreeBytes)
	return d >= 1<<30 || (b.TotalBytes > 0 && d >= b.TotalBytes/100)
}

// logTransition logs online/offline changes once (not every check).
func (m *Manager) logTransition(id string, old, st Status) {
	switch {
	case st.Online && !old.Online:
		m.log.Info("storage online", slog.String("target", id), slog.String("root", st.StoreRoot),
			slog.String("fs", st.FSType))
	case !st.Online && (old.Online || old.Reason != st.Reason):
		m.log.Warn("storage offline", slog.String("target", id), slog.String("reason", st.Reason),
			slog.String("hint", st.Hint))
	}
}
