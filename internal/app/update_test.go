package app

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/settings"
	"github.com/hustenreizjuengling/picache/internal/update"
)

func TestUpdateMode(t *testing.T) {
	for _, tc := range []struct {
		container       string
		service, marker bool
		want            string
	}{
		{"", true, true, update.ModeHelper},
		{"lxc", true, true, update.ModeHelper},
		{"", true, false, update.ModeManual},
		{"", false, true, update.ModeManual}, // started by hand on a host with the helper
		{"docker", true, true, update.ModeDocker},
		{"podman", false, false, update.ModeDocker},
	} {
		if got := updateMode(tc.container, tc.service, tc.marker); got != tc.want {
			t.Errorf("updateMode(%q, %v, %v) = %s, want %s", tc.container, tc.service, tc.marker, got, tc.want)
		}
	}
}

// fakeReleases answers checks with a release or an error and counts them.
type fakeReleases struct {
	mu    sync.Mutex
	rel   *update.Release
	err   error
	calls int
	pre   []bool
}

func (f *fakeReleases) latest(_ context.Context, includePre bool) (*update.Release, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.pre = append(f.pre, includePre)
	return f.rel, f.err
}

func (f *fakeReleases) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// newTestUpdater returns an updater on a fresh picache.db (app_meta
// migrated like at start) running version current in mode.
func newTestUpdater(t *testing.T, current, mode string, f *fakeReleases) (*updater, *db.DB, *settings.Store) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	d, err := db.Open(filepath.Join(dir, "picache.db"), 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if err := d.Migrate(ctx, "app", appMigrations); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.DiscardHandler)
	set, err := settings.Open(ctx, d, log)
	if err != nil {
		t.Fatal(err)
	}
	u := newUpdater(dir, current, d, set, func() string { return mode }, f.latest, log)
	return u, d, set
}

// The result of a check is kept in app_meta and read back at the next
// start; a failed check keeps the release found before.
func TestUpdateCheckPersistsResult(t *testing.T) {
	f := &fakeReleases{rel: &update.Release{Version: "v0.9.1", URL: "https://github.com/x", Notes: "notes"}}
	u, d, set := newTestUpdater(t, "v0.9.0", update.ModeHelper, f)
	o := u.CheckUpdate(t.Context())
	if !o.UpdateAvailable || o.Latest.Version != "v0.9.1" || o.CheckedAt.IsZero() || o.CheckError != "" || o.Mode != update.ModeHelper {
		t.Fatalf("overview %+v", o)
	}

	u2 := newUpdater(u.dataDir, "v0.9.0", d, set, func() string { return update.ModeManual }, f.latest, slog.New(slog.DiscardHandler))
	u2.load(t.Context())
	if o := u2.UpdateOverview(t.Context()); !o.UpdateAvailable || o.Latest.Notes != "notes" || !o.CheckedAt.Equal(u.last.CheckedAt) {
		t.Fatalf("after restart %+v", o)
	}

	f.err = errors.New("release information is not reachable: no route to host")
	u2.lastTry = time.Time{}
	o = u2.CheckUpdate(t.Context())
	if o.CheckError != f.err.Error() || o.Latest == nil || o.Latest.Version != "v0.9.1" {
		t.Fatalf("failed check %+v", o)
	}
}

// On-demand checks reach GitHub at most once per 30 s.
func TestUpdateCheckFloor(t *testing.T) {
	f := &fakeReleases{rel: &update.Release{Version: "v0.9.1"}}
	u, _, _ := newTestUpdater(t, "v0.9.0", update.ModeManual, f)
	now := time.Now()
	u.now = func() time.Time { return now }
	u.CheckUpdate(t.Context())
	now = now.Add(29 * time.Second)
	f.rel = &update.Release{Version: "v0.9.2"}
	if o := u.CheckUpdate(t.Context()); f.count() != 1 || o.Latest.Version != "v0.9.1" {
		t.Fatalf("second check within 30 s: %d calls, %+v", f.count(), o.Latest)
	}
	now = now.Add(2 * time.Second)
	if o := u.CheckUpdate(t.Context()); f.count() != 2 || o.Latest.Version != "v0.9.2" {
		t.Fatalf("check after 31 s: %d calls, %+v", f.count(), o.Latest)
	}
}

// The first check runs 5 minutes after the start, then every 24 h ± 30
// min; with checks disabled nothing is asked; changing the updates settings
// checks at once, with the new pre-release choice.
func TestUpdateCheckSchedule(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakeReleases{}
		ctx := context.Background()
		d, err := db.Open(filepath.Join(t.TempDir(), "picache.db"), 1)
		if err != nil {
			t.Fatal(err)
		}
		defer d.Close()
		log := slog.New(slog.DiscardHandler)
		set, err := settings.Open(ctx, d, log)
		if err != nil {
			t.Fatal(err)
		}
		u := newUpdater(t.TempDir(), "v0.9.0", nil, set, func() string { return update.ModeManual }, f.latest, log)
		rctx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() { u.run(rctx); close(done) }()

		time.Sleep(firstCheckDelay - time.Second)
		synctest.Wait()
		if f.count() != 0 {
			t.Fatal("checked before 5 minutes")
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if f.count() != 1 {
			t.Fatalf("%d checks after 5 minutes", f.count())
		}
		time.Sleep(checkInterval - checkJitter - time.Second)
		synctest.Wait()
		if f.count() != 1 {
			t.Fatal("second check earlier than 23.5 h")
		}
		time.Sleep(2*checkJitter + 2*time.Second)
		synctest.Wait()
		if f.count() != 2 {
			t.Fatalf("%d checks after 24.5 h", f.count())
		}
		time.Sleep(time.Minute) // past the 30 s floor of the last check

		if _, err := set.Update(ctx, func(a *settings.All) error { a.Updates.IncludePrereleases = true; return nil }); err != nil {
			t.Fatal(err)
		}
		u.kickCheck() // what the settings listener in build does
		synctest.Wait()
		if f.count() != 3 || !f.pre[2] {
			t.Fatalf("settings change: %d checks, pre-releases %v", f.count(), f.pre)
		}

		if _, err := set.Update(ctx, func(a *settings.All) error { a.Updates.CheckEnabled = false; return nil }); err != nil {
			t.Fatal(err)
		}
		u.kickCheck()
		time.Sleep(3 * checkInterval)
		synctest.Wait()
		if f.count() != 3 {
			t.Fatalf("checked while disabled: %d", f.count())
		}
		cancel()
		<-done
	})
}

func TestNextCheckDelay(t *testing.T) {
	for range 1000 {
		if d := nextCheckDelay(); d < checkInterval-checkJitter || d > checkInterval+checkJitter {
			t.Fatalf("delay %s", d)
		}
	}
}

// Queueing accepts only the available update of the last check, only with
// the helper and only while no update runs.
func TestQueueUpdate(t *testing.T) {
	f := &fakeReleases{rel: &update.Release{Version: "v0.9.1"}}
	for _, tc := range []struct {
		mode, version, want string
	}{
		{update.ModeManual, "v0.9.1", "the update helper is not installed"},
		{update.ModeDocker, "v0.9.1", "pulling the new image"},
		{update.ModeHelper, "v0.9.2", "not the available update"},
		{update.ModeHelper, "../../x", "not the available update"},
	} {
		u, _, _ := newTestUpdater(t, "v0.9.0", tc.mode, f)
		u.CheckUpdate(t.Context())
		err := u.QueueUpdate(t.Context(), tc.version, "admin")
		if e, ok := apperr.As(err); !ok || e.Kind != apperr.KindConflict || !strings.Contains(e.Message, tc.want) {
			t.Errorf("%s %s: %v", tc.mode, tc.version, err)
		}
		if _, err := os.Stat(filepath.Join(update.RequestsDir(u.dataDir), "request")); err == nil {
			t.Errorf("%s %s: request written", tc.mode, tc.version)
		}
	}

	u, _, _ := newTestUpdater(t, "v0.9.0", update.ModeHelper, f)
	if err := u.QueueUpdate(t.Context(), "v0.9.1", "admin"); apperr.KindOf(err) != apperr.KindConflict {
		t.Fatalf("before any check: %v", err)
	}
	u.CheckUpdate(t.Context())
	if err := u.QueueUpdate(t.Context(), "v0.9.1", "admin"); err != nil {
		t.Fatal(err)
	}
	o := u.UpdateOverview(t.Context())
	if o.Status == nil || !o.Status.Busy() || o.Status.Version != "v0.9.1" || o.Status.From != "v0.9.0" {
		t.Fatalf("status after queueing: %+v", o.Status)
	}
	if err := u.QueueUpdate(t.Context(), "v0.9.1", "admin"); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("second request: %v", err)
	}
	// An up-to-date instance has nothing to queue.
	u, _, _ = newTestUpdater(t, "v0.9.1", update.ModeHelper, f)
	u.CheckUpdate(t.Context())
	if err := u.QueueUpdate(t.Context(), "v0.9.1", "admin"); apperr.KindOf(err) != apperr.KindConflict {
		t.Fatalf("up to date: %v", err)
	}
}
