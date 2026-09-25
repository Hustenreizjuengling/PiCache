package app

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	mrand "math/rand/v2"
	"os"
	"sync"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/notify"
	"github.com/hustenreizjuengling/picache/internal/settings"
	"github.com/hustenreizjuengling/picache/internal/update"
)

// Release check schedule (docs/ARCHITECTURE.md 14.3).
const (
	updateCheckKey   = "update.last_check" // app_meta key of the last result
	updateNotifyKey  = "update.notified"   // app_meta key: versions and runs already notified
	runPollInterval  = time.Minute         // how often the state of an update run is read
	firstCheckDelay  = 5 * time.Minute
	checkInterval    = 24 * time.Hour
	checkJitter      = 30 * time.Minute
	minCheckInterval = 30 * time.Second // on-demand checks: faster calls get the last result
)

// updater checks for new releases and queues updates for the root helper.
// It implements api.Updater; it never downloads or installs anything.
type updater struct {
	log     *slog.Logger
	set     *settings.Store
	cdb     *db.DB // nil: the result is not persisted (tests)
	dataDir string
	current string // version of the running binary
	// mode reports helper, docker or manual (see updateMode).
	mode  func() string
	check func(ctx context.Context, includePre bool) (*update.Release, error)
	now   func() time.Time
	emit  func(notify.Message) // nil: no notifications

	checkMu sync.Mutex // one check at a time
	queueMu sync.Mutex // one queue decision at a time
	mu      sync.Mutex // guards last, lastTry, lastErr
	last    update.CheckResult
	lastTry time.Time // start of the last check (the 30 s floor)
	lastErr string    // last logged check error (logged when it changes)
	seen    updateSeen
	kick    chan struct{}
}

// updateSeen is what the update notifications have reported (app_meta
// update.notified), so that a restart does not report it again.
type updateSeen struct {
	Available string `json:"available,omitempty"` // version reported as available
	Run       string `json:"run,omitempty"`       // startedAt of the last finished run seen
}

func newUpdater(dataDir, current string, cdb *db.DB, set *settings.Store, mode func() string,
	check func(ctx context.Context, includePre bool) (*update.Release, error), log *slog.Logger) *updater {
	return &updater{log: log.With(slog.String("component", "update")), set: set, cdb: cdb, dataDir: dataDir,
		current: current, mode: mode, check: check, now: time.Now, kick: make(chan struct{}, 1)}
}

// updateMode decides how updates are installed: by the root helper when
// install.sh installed it (marker) and PiCache runs as a systemd service,
// by pulling a new image in a Docker/Podman container, by hand otherwise.
func updateMode(container string, systemdService, marker bool) string {
	switch {
	case container == "docker" || container == "podman":
		return update.ModeDocker
	case marker && systemdService:
		return update.ModeHelper
	}
	return update.ModeManual
}

// runsAsSystemdService reports whether systemd started this process (it
// sets INVOCATION_ID for every service it runs).
func runsAsSystemdService(systemd bool) bool { return systemd && os.Getenv("INVOCATION_ID") != "" }

// load reads the last check result and what was notified from app_meta,
// so they survive restarts.
func (u *updater) load(ctx context.Context) {
	if u.cdb == nil {
		return
	}
	var seen updateSeen
	if doc, ok := u.loadMeta(ctx, updateNotifyKey); ok {
		if err := json.Unmarshal([]byte(doc), &seen); err != nil {
			u.log.Warn("ignoring the stored update notification state", slog.Any("err", err))
		}
	}
	u.mu.Lock()
	u.seen = seen
	u.mu.Unlock()
	doc, ok := u.loadMeta(ctx, updateCheckKey)
	if !ok {
		return
	}
	var res update.CheckResult
	if err := json.Unmarshal([]byte(doc), &res); err != nil {
		u.log.Warn("ignoring the stored update check result", slog.Any("err", err))
		return
	}
	u.mu.Lock()
	u.last = res
	u.mu.Unlock()
}

// loadMeta reads an app_meta value.
func (u *updater) loadMeta(ctx context.Context, key string) (string, bool) {
	var doc string
	err := u.cdb.R.QueryRowContext(ctx, `SELECT value FROM app_meta WHERE key = ?`, key).Scan(&doc)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			u.log.Debug("cannot read stored update state", slog.String("key", key), slog.Any("err", err))
		}
		return "", false
	}
	return doc, true
}

func (u *updater) save(ctx context.Context, res update.CheckResult) {
	u.saveMeta(ctx, updateCheckKey, res)
}

// saveMeta stores v as JSON in app_meta (not persisted without a database).
func (u *updater) saveMeta(ctx context.Context, key string, v any) {
	if u.cdb == nil {
		return
	}
	b, err := json.Marshal(v)
	if err == nil {
		_, err = u.cdb.W.ExecContext(context.WithoutCancel(ctx), `INSERT INTO app_meta (key, value) VALUES (?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, string(b))
	}
	if err != nil {
		u.log.Warn("cannot store update state", slog.String("key", key), slog.Any("err", err))
	}
}

// run checks 5 minutes after the start and then every 24 h (±30 min) while
// checks are enabled; a changed updates setting checks at once. It reads
// the state of update runs at the start and every minute (watchRun).
func (u *updater) run(ctx context.Context) {
	t := time.NewTimer(firstCheckDelay)
	defer t.Stop()
	poll := time.NewTicker(runPollInterval)
	defer poll.Stop()
	u.watchRun(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-poll.C:
			u.watchRun(ctx)
			continue
		case <-u.kick:
			if u.set.Get().Updates.CheckEnabled {
				u.checkNow(ctx)
			}
			continue
		case <-t.C:
		}
		if u.set.Get().Updates.CheckEnabled {
			u.checkNow(ctx)
		}
		t.Reset(nextCheckDelay())
	}
}

// nextCheckDelay is 24 h ± 30 min, so installations do not all ask GitHub
// at the same time of day.
func nextCheckDelay() time.Duration {
	return checkInterval - checkJitter + time.Duration(mrand.Int64N(int64(2*checkJitter)+1))
}

// kickCheck asks run to check soon (the updates settings changed).
func (u *updater) kickCheck() {
	select {
	case u.kick <- struct{}{}:
	default:
	}
}

// checkNow asks GitHub for the newest eligible release, at most once per
// 30 s. A failed check keeps the release found before and records why.
func (u *updater) checkNow(ctx context.Context) update.CheckResult {
	u.checkMu.Lock()
	defer u.checkMu.Unlock()
	u.mu.Lock()
	if !u.lastTry.IsZero() && u.now().Sub(u.lastTry) < minCheckInterval {
		res := u.last
		u.mu.Unlock()
		return res
	}
	u.lastTry = u.now()
	prev := u.last
	u.mu.Unlock()

	cctx, cancel := context.WithTimeout(ctx, update.CheckTimeout)
	rel, err := u.check(cctx, u.set.Get().Updates.IncludePrereleases)
	cancel()
	res := update.CheckResult{Latest: rel, CheckedAt: u.now().UTC()}
	if err != nil {
		res.Latest, res.Error = prev.Latest, err.Error()
	}
	u.mu.Lock()
	u.last = res
	logErr := res.Error != u.lastErr
	u.lastErr = res.Error
	u.mu.Unlock()
	u.save(ctx, res)
	switch {
	case err != nil && logErr:
		u.log.Warn("update check failed", slog.String("err", res.Error))
	case err == nil && (prev.Latest == nil || rel == nil || prev.Latest.Version != rel.Version):
		if o := u.overview(res); o.UpdateAvailable {
			u.log.Info("a new PiCache release is available", slog.String("version", o.Latest.Version),
				slog.String("running", u.current), slog.String("url", o.Latest.URL))
		}
	}
	if err == nil {
		u.notifyAvailable(ctx, u.overview(res))
	}
	return res
}

// notifyAvailable sends update.available once per version.
func (u *updater) notifyAvailable(ctx context.Context, o update.Overview) {
	if !o.UpdateAvailable || o.Latest == nil {
		return
	}
	u.mu.Lock()
	known := u.seen.Available == o.Latest.Version
	u.seen.Available = o.Latest.Version
	seen := u.seen
	u.mu.Unlock()
	if known {
		return
	}
	u.saveMeta(ctx, updateNotifyKey, seen)
	u.notify(notify.Message{Event: notify.EventUpdateAvailable, Title: "PiCache " + o.Latest.Version + " is available",
		Message: fmt.Sprintf("PiCache %s is available (running %s). Release notes: %s\nInstall it under System → Updates.",
			o.Latest.Version, u.current, o.Latest.URL)})
}

// watchRun reports a finished update run once: update.installed when it
// succeeded and this process runs the new version (the helper restarted
// it), update.failed when it failed or was rolled back. A run is known by
// its start time; the first run seen after an upgrade to a version with
// notifications is reported too (there are no channels yet then).
func (u *updater) watchRun(ctx context.Context) {
	st := update.ReadStatus(u.dataDir, u.current, u.now())
	if st == nil || st.State == update.StateRunning {
		return
	}
	key := st.StartedAt.UTC().Format(time.RFC3339Nano)
	u.mu.Lock()
	known := u.seen.Run == key
	u.seen.Run = key
	seen := u.seen
	u.mu.Unlock()
	if known {
		return
	}
	u.saveMeta(ctx, updateNotifyKey, seen)
	switch st.State {
	case update.StateSucceeded:
		if st.Version == u.current {
			u.notify(notify.Message{Event: notify.EventUpdateInstalled, Title: "PiCache " + st.Version + " installed",
				Message: fmt.Sprintf("The update from %s to %s finished successfully.", st.From, st.Version)})
		}
	case update.StateFailed, update.StateRolledBack:
		title, what := "Update to "+st.Version+" failed", "failed"
		if st.State == update.StateRolledBack {
			title, what = "Update to "+st.Version+" rolled back", "was rolled back; "+st.From+" runs again"
		}
		msg := fmt.Sprintf("The update from %s to %s %s (step %s).", st.From, st.Version, what, st.Step)
		if st.Message != "" {
			msg += "\n" + st.Message
		}
		u.notify(notify.Message{Event: notify.EventUpdateFailed, Title: title, Message: msg})
	}
}

func (u *updater) notify(m notify.Message) {
	if u.emit != nil {
		u.emit(m)
	}
}

func (u *updater) overview(last update.CheckResult) update.Overview {
	s := u.set.Get().Updates
	st := update.ReadStatus(u.dataDir, u.current, u.now())
	return update.NewOverview(u.current, u.mode(), s.CheckEnabled, s.IncludePrereleases, last, st)
}

// --- api.Updater ---

func (u *updater) UpdateOverview(context.Context) update.Overview {
	u.mu.Lock()
	last := u.last
	u.mu.Unlock()
	return u.overview(last)
}

func (u *updater) CheckUpdate(ctx context.Context) update.Overview {
	return u.overview(u.checkNow(ctx))
}

func (u *updater) QueueUpdate(ctx context.Context, version, requestedBy string) error {
	u.queueMu.Lock()
	defer u.queueMu.Unlock()
	o := u.UpdateOverview(ctx)
	switch {
	case o.Mode == update.ModeDocker:
		return apperr.Conflict("PiCache runs in a container: update it by pulling the new image (%s)", update.DockerCommand)
	case o.Mode != update.ModeHelper:
		return apperr.Conflict("the update helper is not installed (run install.sh without --without-updater); update on the host with: %s",
			o.Commands.CLI)
	case o.Status.Busy():
		return apperr.Conflict("an update is already running")
	case !o.UpdateAvailable || o.Latest.Version != version:
		return apperr.Conflict("the requested version is not the available update; check for updates again")
	}
	err := update.QueueRequest(u.dataDir, update.Request{Version: version, RequestedAt: u.now().UTC(), RequestedBy: requestedBy})
	if err != nil {
		return fmt.Errorf("queue the update: %w", err)
	}
	u.log.Info("update queued for the update helper", slog.String("version", version), slog.String("by", requestedBy))
	return nil
}
