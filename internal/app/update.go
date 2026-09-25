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
	"github.com/hustenreizjuengling/picache/internal/settings"
	"github.com/hustenreizjuengling/picache/internal/update"
)

// Release check schedule (docs/ARCHITECTURE.md 14.3).
const (
	updateCheckKey   = "update.last_check" // app_meta key of the last result
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

	checkMu sync.Mutex // one check at a time
	queueMu sync.Mutex // one queue decision at a time
	mu      sync.Mutex // guards last, lastTry, lastErr
	last    update.CheckResult
	lastTry time.Time // start of the last check (the 30 s floor)
	lastErr string    // last logged check error (logged when it changes)
	kick    chan struct{}
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

// load reads the last check result from app_meta, so it survives restarts.
func (u *updater) load(ctx context.Context) {
	if u.cdb == nil {
		return
	}
	var doc string
	err := u.cdb.R.QueryRowContext(ctx, `SELECT value FROM app_meta WHERE key = ?`, updateCheckKey).Scan(&doc)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			u.log.Debug("no stored update check result", slog.Any("err", err))
		}
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

func (u *updater) save(ctx context.Context, res update.CheckResult) {
	if u.cdb == nil {
		return
	}
	b, err := json.Marshal(res)
	if err == nil {
		_, err = u.cdb.W.ExecContext(ctx, `INSERT INTO app_meta (key, value) VALUES (?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value`, updateCheckKey, string(b))
	}
	if err != nil {
		u.log.Warn("cannot store the update check result", slog.Any("err", err))
	}
}

// run checks 5 minutes after the start and then every 24 h (±30 min) while
// checks are enabled; a changed updates setting checks at once.
func (u *updater) run(ctx context.Context) {
	t := time.NewTimer(firstCheckDelay)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
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
	return res
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
