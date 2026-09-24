package filter

import (
	"context"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
)

const (
	checkInterval  = time.Minute // scheduler tick (due lists, group reconciliation)
	maxFailedRetry = time.Hour   // failed lists are retried at least this often
)

// RefreshList re-downloads one list now and recompiles (waits for completion).
func (e *Engine) RefreshList(ctx context.Context, id int64) (List, error) {
	if !e.beginOp() {
		return List{}, apperr.Unavailable("filtering is shutting down")
	}
	defer e.inflight.Done()
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	stop := context.AfterFunc(e.base, cancel)
	defer stop()

	l, err := e.list(id)
	if err != nil {
		return List{}, err
	}
	if !l.Enabled {
		return List{}, apperr.Conflict("the list is disabled")
	}
	changed, err := e.refresh(ctx, id, true)
	if err != nil {
		if ctx.Err() != nil {
			return List{}, apperr.Unavailable("the refresh was cancelled or timed out")
		}
		return List{}, err
	}
	if changed {
		e.compile()
	}
	return e.list(id)
}

// RefreshAll re-downloads all enabled lists and recompiles (background).
func (e *Engine) RefreshAll(ctx context.Context) error {
	e.mu.Lock()
	for _, rt := range e.lists {
		if rt.Enabled {
			rt.wantDownload = true
		}
	}
	e.mu.Unlock()
	e.signal()
	return nil
}

func (e *Engine) interval() time.Duration {
	return time.Duration(e.set.Get().Filter.UpdateIntervalHours) * time.Hour
}

// loadCached parses the cached copies of all enabled lists (at start).
func (e *Engine) loadCached(ctx context.Context) {
	type job struct {
		id          int64
		kind, plain string
	}
	var jobs []job
	e.mu.Lock()
	for _, rt := range sortedLists(e.lists) {
		if rt.Enabled && rt.parsed == nil {
			jobs = append(jobs, job{rt.ID, rt.Kind, rt.PlainDomains})
		}
	}
	e.mu.Unlock()
	for _, j := range jobs {
		var p *parsed
		err := os.ErrNotExist
		if e.hasCache(j.id) {
			if err = e.lockDownloads(ctx); err != nil {
				return
			}
			p, err = e.parseFile(ctx, e.cachePath(j.id), j.kind, j.plain)
			e.unlockDownloads()
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				e.log.Warn("cannot read the cached copy of a list", slog.Int64("id", j.id), slog.Any("err", err))
			}
		}
		e.mu.Lock()
		if rt, ok := e.lists[j.id]; ok && rt.Enabled && rt.parsed == nil {
			switch {
			case err == nil && rt.Kind == j.kind && rt.PlainDomains == j.plain:
				rt.parsed = p
				e.parseGen++
			case err != nil && !rt.LastChecked.IsZero():
				rt.wantDownload = true // the copy is missing or unreadable: fetch it again now
			}
		}
		e.mu.Unlock()
	}
}

// updateLoop runs requested and scheduled list work and reconciles group
// memberships every minute.
func (e *Engine) updateLoop(ctx context.Context) {
	t := time.NewTicker(checkInterval)
	defer t.Stop()
	lastErr := ""
	for {
		e.runPending(ctx)
		select {
		case <-ctx.Done():
			return
		case <-e.wake:
		case <-t.C:
			err := e.ReloadGroups(ctx)
			if ctx.Err() != nil {
				return
			}
			msg := ""
			if err != nil {
				msg = err.Error()
			}
			if msg != lastErr && msg != "" { // log a failure only when it changes
				e.log.Warn("reload group memberships", slog.Any("err", err))
			}
			lastErr = msg
		}
	}
}

// runPending processes lists that need work, one at a time.
func (e *Engine) runPending(ctx context.Context) {
	for ctx.Err() == nil {
		id, download, ok := e.nextJob()
		if !ok {
			return
		}
		if _, err := e.refresh(ctx, id, download); err != nil && ctx.Err() == nil {
			e.log.Warn("list update failed", slog.Int64("id", id), slog.Any("err", err))
		}
	}
}

// nextJob returns the enabled list with the lowest ID that was requested or
// is due, and whether it must be downloaded (else only re-parsed).
func (e *Engine) nextJob() (id int64, download, ok bool) {
	interval, now := e.interval(), e.now()
	e.mu.Lock()
	defer e.mu.Unlock()
	var best *listRT
	for _, rt := range e.lists {
		if !rt.Enabled {
			continue
		}
		dl := rt.wantDownload || due(rt, now, interval)
		if (dl || rt.wantReparse) && (best == nil || rt.ID < best.ID) {
			best, download = rt, dl
		}
	}
	if best == nil {
		return 0, false, false
	}
	return best.ID, download, true
}

// due reports whether a scheduled download of rt is due: never checked, or
// the (jittered) update interval elapsed; failed lists are retried hourly.
// With interval 0 (manual updates) only never-checked lists are due.
func due(rt *listRT, now time.Time, interval time.Duration) bool {
	if rt.LastChecked.IsZero() {
		return true
	}
	if interval <= 0 {
		return false
	}
	wait := interval
	if (rt.Status == statusFailedCached || rt.Status == statusFailedEmpty) && wait > maxFailedRetry {
		wait = maxFailedRetry
	}
	return now.Sub(rt.LastChecked) >= time.Duration(float64(wait)*rt.jitter)
}

func (e *Engine) lockDownloads(ctx context.Context) error {
	select {
	case e.dlSem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (e *Engine) unlockDownloads() { <-e.dlSem }

// listUpdate is the new state of a list after a refresh.
type listUpdate struct {
	status, lastError               string
	updated, checked, success       time.Time
	entries, invalid, unsupported   int
	size                            int64
	etag, lastModified, contentHash string
}

func (u *listUpdate) fail(err error, hasCache bool) {
	u.status, u.lastError = statusFailedEmpty, errorText(err)
	if hasCache {
		u.status = statusFailedCached
	}
}

func (u *listUpdate) setCounts(p *parsed) {
	u.entries, u.invalid, u.unsupported = p.entries, p.invalid, p.unsupported
}

// refresh downloads a list (if download) and re-parses its cached copy if
// the loaded parse result is missing or stale, records the status and
// reports whether the parse result changed. Download failures are recorded
// as status, not returned.
func (e *Engine) refresh(ctx context.Context, id int64, download bool) (bool, error) {
	if err := e.lockDownloads(ctx); err != nil {
		return false, err
	}
	defer e.unlockDownloads()
	e.busy.Add(1)
	defer e.busy.Add(-1)

	e.mu.Lock()
	rt, ok := e.lists[id]
	if !ok {
		e.mu.Unlock()
		return false, apperr.NotFound("list", id)
	}
	cfg := *rt
	rt.wantDownload, rt.wantReparse = false, false
	e.mu.Unlock()

	up := listUpdate{
		status: cfg.Status, lastError: cfg.LastError, updated: cfg.LastUpdated, checked: cfg.LastChecked,
		success: cfg.LastSuccess, entries: cfg.Entries, invalid: cfg.Invalid, unsupported: cfg.Unsupported,
		size: cfg.SizeBytes, etag: cfg.etag, lastModified: cfg.lastModified, contentHash: cfg.hash,
	}
	hasCache := e.hasCache(id)
	needParse := cfg.parsed == nil || cfg.parsed.kind != cfg.Kind || cfg.parsed.plain != cfg.PlainDomains
	var p *parsed
	var tmp string
	if download {
		now := e.now()
		up.checked = now
		f, err := e.fetchList(ctx, id, cfg.URL, cfg.etag, cfg.lastModified, hasCache && cfg.hash != "")
		if f.tmp != "" && (err != nil || f.hash == cfg.hash && hasCache) {
			_ = os.Remove(f.tmp)
		}
		switch {
		case ctx.Err() != nil:
			return false, ctx.Err()
		case err != nil:
			up.fail(err, hasCache)
			e.log.Warn("list download failed", slog.Int64("id", id), slog.String("url", redactURL(cfg.URL)), slog.String("err", up.lastError))
		case f.notModified || f.hash == cfg.hash && hasCache:
			up.status, up.lastError, up.success = statusUnchanged, "", now
			if !f.notModified {
				up.etag, up.lastModified = f.etag, f.lastModified
			}
		default:
			parsedTmp, err := e.parseFile(ctx, f.tmp, cfg.Kind, cfg.PlainDomains)
			if err != nil {
				_ = os.Remove(f.tmp)
				if ctx.Err() != nil {
					return false, ctx.Err()
				}
				up.fail(err, hasCache)
				e.log.Warn("list rejected", slog.Int64("id", id), slog.String("url", redactURL(cfg.URL)), slog.String("err", up.lastError))
				break
			}
			p, tmp, needParse = parsedTmp, f.tmp, false
			up.status, up.lastError, up.success, up.updated = statusOK, "", now, now
			up.etag, up.lastModified, up.contentHash, up.size = f.etag, f.lastModified, f.hash, f.size
			up.setCounts(p)
		}
	}
	if needParse && p == nil && hasCache {
		cached, err := e.parseFile(ctx, e.cachePath(id), cfg.Kind, cfg.PlainDomains)
		switch {
		case ctx.Err() != nil:
			return false, ctx.Err()
		case err != nil:
			up.status, up.lastError = statusFailedEmpty, "cached copy: "+errorText(err)
		default:
			p = cached
			up.setCounts(p)
		}
	}
	return e.applyRefresh(ctx, cfg.URL, id, up, p, tmp), nil
}

// applyRefresh stores the outcome of refresh unless the list was deleted or
// re-pointed to another URL meanwhile.
func (e *Engine) applyRefresh(ctx context.Context, url string, id int64, up listUpdate, p *parsed, tmp string) bool {
	e.mu.Lock()
	rt, ok := e.lists[id]
	if !ok || rt.URL != url {
		e.mu.Unlock()
		if tmp != "" {
			_ = os.Remove(tmp)
		}
		return false
	}
	if tmp != "" {
		if err := os.Rename(tmp, e.cachePath(id)); err != nil {
			_ = os.Remove(tmp)
			e.log.Error("store list copy", slog.Int64("id", id), slog.Any("err", err))
			p = nil
			up.fail(err, e.hasCache(id))
			up.lastError = "cannot store the downloaded list"
		}
	}
	rt.Status, rt.LastError = up.status, up.lastError
	rt.LastUpdated, rt.LastChecked, rt.LastSuccess = up.updated, up.checked, up.success
	rt.Entries, rt.Invalid, rt.Unsupported, rt.SizeBytes = up.entries, up.invalid, up.unsupported, up.size
	rt.etag, rt.lastModified, rt.hash = up.etag, up.lastModified, up.contentHash
	rt.jitter = newJitter()
	changed := p != nil && rt.Enabled && rt.Kind == p.kind && rt.PlainDomains == p.plain
	if changed {
		rt.parsed = p
		e.parseGen++
	}
	e.mu.Unlock()
	if changed {
		e.requestCompile()
	}

	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if _, err := e.db.W.ExecContext(wctx, `UPDATE filter_lists SET status = ?, last_error = ?, last_updated = ?,
		last_checked = ?, last_success = ?, entries = ?, invalid = ?, unsupported = ?, size_bytes = ?,
		etag = ?, last_modified = ?, content_hash = ? WHERE id = ? AND url = ?`,
		up.status, up.lastError, db.Ms(up.updated), db.Ms(up.checked), db.Ms(up.success), up.entries,
		up.invalid, up.unsupported, up.size, up.etag, up.lastModified, up.contentHash, id, url); err != nil {
		e.log.Error("save list status", slog.Int64("id", id), slog.Any("err", err))
	}
	return changed
}

// parseFile parses a list file.
func (e *Engine) parseFile(ctx context.Context, path, kind, plain string) (*parsed, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parseList(ctx, io.LimitReader(f, e.maxBytes), kind, plain)
}
