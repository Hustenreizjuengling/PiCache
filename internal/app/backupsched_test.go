package app

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
	_ "time/tzdata" // Europe/Berlin on machines without a zone database

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/notify"
	"github.com/hustenreizjuengling/picache/internal/secrets"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

const testInstance = "picache-0123456789ab"

func berlin(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func daily(clock string) settings.Backups {
	return settings.Backups{Enabled: true, Schedule: "daily", Time: clock, Keep: 7, Destination: settings.BackupsLocal}
}

// Scheduled times in local time, across both DST switches of 2026 in
// Berlin: one run per date, at the wall-clock time where it exists.
func TestScheduleTimes(t *testing.T) {
	loc := berlin(t)
	at := func(s string) time.Time {
		t.Helper()
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	weekly := daily("22:15")
	weekly.Schedule, weekly.Weekday = "weekly", 3 // Wednesday
	for _, tc := range []struct {
		s          settings.Backups
		now        string
		last, next string
	}{
		{daily("03:30"), "2026-09-25T12:00:00+02:00", "2026-09-25T03:30:00+02:00", "2026-09-26T03:30:00+02:00"},
		{daily("03:30"), "2026-09-25T03:30:00+02:00", "2026-09-25T03:30:00+02:00", "2026-09-26T03:30:00+02:00"},
		{daily("03:30"), "2026-09-25T03:29:59+02:00", "2026-09-24T03:30:00+02:00", "2026-09-25T03:30:00+02:00"},
		{daily("00:00"), "2026-09-25T00:00:00+02:00", "2026-09-25T00:00:00+02:00", "2026-09-26T00:00:00+02:00"},
		// Spring forward (29 March, 02:00 → 03:00): 02:30 does not exist
		// and is run at 03:30 CEST.
		{daily("02:30"), "2026-03-29T12:00:00+02:00", "2026-03-29T03:30:00+02:00", "2026-03-30T02:30:00+02:00"},
		{daily("02:30"), "2026-03-28T12:00:00+01:00", "2026-03-28T02:30:00+01:00", "2026-03-29T03:30:00+02:00"},
		// Fall back (25 October, 03:00 → 02:00): 02:30 exists twice, one is run.
		{daily("02:30"), "2026-10-25T12:00:00+01:00", "2026-10-25T02:30:00+01:00", "2026-10-26T02:30:00+01:00"},
		{daily("02:30"), "2026-10-25T02:40:00+02:00", "2026-10-24T02:30:00+02:00", "2026-10-25T02:30:00+01:00"},
		{weekly, "2026-09-25T12:00:00+02:00", "2026-09-23T22:15:00+02:00", "2026-09-30T22:15:00+02:00"},
		{weekly, "2026-09-23T22:15:00+02:00", "2026-09-23T22:15:00+02:00", "2026-09-30T22:15:00+02:00"},
	} {
		now := at(tc.now)
		if got := lastDue(tc.s, now, loc); !got.Equal(at(tc.last)) {
			t.Errorf("%s %s at %s: last %s, want %s", tc.s.Schedule, tc.s.Time, tc.now, got.In(loc), tc.last)
		}
		if got := nextDue(tc.s, now, loc); !got.Equal(at(tc.next)) {
			t.Errorf("%s %s at %s: next %s, want %s", tc.s.Schedule, tc.s.Time, tc.now, got.In(loc), tc.next)
		}
	}
}

// schedEnv is a scheduler with a fake backup writer and storage target.
type schedEnv struct {
	b   *backupScheduler
	set *settings.Store
	dir string

	mu     sync.Mutex
	runs   []time.Time // when the backup writer was called (scheduler clock)
	events []notify.Message
	target backupTarget
	block  chan struct{} // non-nil: the writer waits for it
	fail   error
}

func newSchedEnv(t *testing.T) *schedEnv {
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
	e := &schedEnv{set: set, dir: dir}
	e.b = newBackupScheduler(dir, testInstance, d, set, e.write, e.targetInfo, e.emit, log)
	return e
}

func (e *schedEnv) write(ctx context.Context, w io.Writer, includeSecrets bool) error {
	e.mu.Lock()
	e.runs = append(e.runs, e.b.now())
	block, fail := e.block, e.fail
	e.mu.Unlock()
	if block != nil {
		<-block
	}
	if fail != nil {
		return fail
	}
	_, err := io.WriteString(w, "SQLite format 3\x00 backup")
	return err
}

func (e *schedEnv) targetInfo(id string) (backupTarget, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if id != "0123456789abcdef0123456789abcdef" {
		return backupTarget{}, apperr.NotFound("storage target", id)
	}
	return e.target, nil
}

func (e *schedEnv) emit(m notify.Message) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, m)
}

func (e *schedEnv) setBackups(t *testing.T, b settings.Backups) {
	t.Helper()
	if _, err := e.set.Update(context.Background(), func(a *settings.All) error { a.Backups = b; return nil }); err != nil {
		t.Fatal(err)
	}
}

func (e *schedEnv) runTimes(loc *time.Location) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for _, r := range e.runs {
		out = append(out, r.In(loc).Format("2006-01-02 15:04 MST"))
	}
	return out
}

// runLoop runs the scheduler (in a synctest bubble) with its clock set to
// start for d of bubble time and returns the run times.
func (e *schedEnv) runLoop(t *testing.T, loc *time.Location, start time.Time, d time.Duration) []string {
	t.Helper()
	shift := start.Sub(time.Now())
	e.b.now = func() time.Time { return time.Now().Add(shift) }
	e.b.loc = loc
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.b.Start(ctx); close(done) }()
	time.Sleep(d)
	synctest.Wait()
	cancel()
	<-done
	return e.runTimes(loc)
}

func TestScheduledRunsDaily(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		loc := berlin(t)
		e := newSchedEnv(t)
		e.setBackups(t, daily("02:30"))
		start := time.Date(2026, 3, 27, 12, 0, 0, 0, loc)
		e.b.lastSuccess = start.Add(-10 * time.Hour) // not missed
		got := e.runLoop(t, loc, start, 4*24*time.Hour)
		want := []string{"2026-03-28 02:30 CET", "2026-03-29 03:30 CEST", "2026-03-30 02:30 CEST", "2026-03-31 02:30 CEST"}
		if !slices.Equal(got, want) {
			t.Fatalf("runs %v, want %v", got, want)
		}
	})
}

func TestScheduledRunsFallBack(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		loc := berlin(t)
		e := newSchedEnv(t)
		e.setBackups(t, daily("02:30"))
		start := time.Date(2026, 10, 24, 12, 0, 0, 0, loc)
		e.b.lastSuccess = start.Add(-time.Hour)
		got := e.runLoop(t, loc, start, 2*24*time.Hour)
		if want := []string{"2026-10-25 02:30 CET", "2026-10-26 02:30 CET"}; !slices.Equal(got, want) {
			t.Fatalf("runs %v, want %v (02:30 exists twice on 25 October; one run)", got, want)
		}
	})
}

func TestScheduledRunsWeekly(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		loc := berlin(t)
		e := newSchedEnv(t)
		s := daily("22:15")
		s.Schedule, s.Weekday = "weekly", 3
		e.setBackups(t, s)
		start := time.Date(2026, 9, 24, 12, 0, 0, 0, loc) // a Thursday
		e.b.lastSuccess = start.Add(-24 * time.Hour)
		got := e.runLoop(t, loc, start, 15*24*time.Hour)
		if want := []string{"2026-09-30 22:15 CEST", "2026-10-07 22:15 CEST"}; !slices.Equal(got, want) {
			t.Fatalf("runs %v, want %v", got, want)
		}
	})
}

// A run missed while PiCache was down is made 5 minutes after the start
// when the last success is older than the interval + 1 h.
func TestScheduledCatchUp(t *testing.T) {
	for _, tc := range []struct {
		name     string
		schedule string
		ago      time.Duration // last success before the start; 0 = never
		want     []string
	}{
		{"never", "daily", 0, []string{"2026-09-25 12:05 CEST"}},
		{"missed daily", "daily", 26 * time.Hour, []string{"2026-09-25 12:05 CEST"}},
		{"recent daily", "daily", 24 * time.Hour, nil},
		{"recent weekly", "weekly", 6 * 24 * time.Hour, nil},
		{"missed weekly", "weekly", 8 * 24 * time.Hour, []string{"2026-09-25 12:05 CEST"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				loc := berlin(t)
				e := newSchedEnv(t)
				s := daily("03:30")
				s.Schedule, s.Weekday = tc.schedule, 1 // Mondays
				e.setBackups(t, s)
				start := time.Date(2026, 9, 25, 12, 0, 0, 0, loc) // a Friday
				if tc.ago > 0 {
					e.b.lastSuccess = start.Add(-tc.ago)
				}
				if got := e.runLoop(t, loc, start, 10*time.Hour); !slices.Equal(got, tc.want) {
					t.Fatalf("runs %v, want %v", got, tc.want)
				}
			})
		})
	}
}

// Disabled: nothing runs and no next run is shown; RunNow still works and
// refuses a second run while one is going.
func TestScheduledDisabledAndRunNow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		loc := berlin(t)
		e := newSchedEnv(t)
		s := daily("03:30")
		s.Enabled = false
		e.setBackups(t, s)
		if got := e.runLoop(t, loc, time.Date(2026, 9, 25, 12, 0, 0, 0, loc), 3*24*time.Hour); len(got) != 0 {
			t.Fatalf("runs while disabled: %v", got)
		}
		if o := e.b.Overview(t.Context()); !o.Next.IsZero() || o.Running || o.Last != nil {
			t.Fatalf("overview %+v", o)
		}
	})
	e := newSchedEnv(t)
	e.block = make(chan struct{})
	if err := e.b.RunNow(); err != nil {
		t.Fatal(err)
	}
	if err := e.b.RunNow(); apperr.KindOf(err) != apperr.KindConflict {
		t.Fatalf("second run: %v", err)
	}
	if !e.b.Overview(t.Context()).Running {
		t.Fatal("not running")
	}
	close(e.block)
	e.b.runs.Wait()
	if err := e.b.RunNow(); err != nil {
		t.Fatalf("after the run: %v", err)
	}
	e.b.runs.Wait()
}

// Runs within one second get their own files: the second one takes the
// next second instead of replacing the first file.
func TestScheduledRunsSameSecond(t *testing.T) {
	e := newSchedEnv(t)
	at := time.Date(2026, 9, 25, 15, 10, 3, 400_000_000, time.UTC)
	e.b.now = func() time.Time { return at }
	for range 2 {
		if err := e.b.RunNow(); err != nil {
			t.Fatal(err)
		}
		e.b.runs.Wait()
	}
	o := e.b.Overview(t.Context())
	if len(o.Files) != 2 || o.Files[0].Time.Sub(o.Files[1].Time) != time.Second || !o.Last.Time.Equal(o.Files[0].Time) {
		t.Fatalf("files %+v, last %+v", o.Files, o.Last)
	}
	if o.TimeZone == "" {
		t.Fatal("no time zone")
	}
}

// touch creates a file in dir with content.
func touch(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
}

// A run writes the file atomically with the documented name, keeps the
// newest "keep" backups of this installation and never touches other files.
func TestScheduledRetention(t *testing.T) {
	e := newSchedEnv(t)
	s := daily("03:30")
	s.Keep = 3
	e.setBackups(t, s)
	dir := filepath.Join(e.dir, "backups", "scheduled")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	ours := func(stamp string) string { return "picache-backup-" + testInstance + "-" + stamp + ".db" }
	for _, st := range []string{"20260101T033000Z", "20260102T033000Z", "20260103T033000Z", "20260104T033000Z"} {
		touch(t, dir, ours(st), "old")
	}
	others := []string{
		"picache-backup-picache-ffffffffffff-20250101T033000Z.db", // another installation
		ours("20250101T033000Z") + ".bak",
		"picache-backup-" + testInstance + "-2025.db",
		"notes.txt",
		"picache-0.3.1-20250101T000000.db", // a pre-upgrade copy's name
	}
	for _, n := range others {
		touch(t, dir, n, "keep me")
	}
	if err := os.Mkdir(filepath.Join(dir, ours("20000101T000000Z")), 0o750); err != nil { // a directory, oldest name
		t.Fatal(err)
	}
	symlink := ours("19990101T000000Z")
	haveLink := os.Symlink(filepath.Join(dir, "notes.txt"), filepath.Join(dir, symlink)) == nil
	touch(t, dir, ours("20250101T000000Z")+".tmp", "partial") // left by an interrupted run

	now := time.Date(2026, 9, 25, 1, 30, 0, 0, time.UTC)
	e.b.now = func() time.Time { return now }
	if err := e.b.RunNow(); err != nil {
		t.Fatal(err)
	}
	e.b.runs.Wait()
	o := e.b.Overview(t.Context())
	if o.Last == nil || !o.Last.OK || o.Last.File != ours("20260925T013000Z") || o.Last.SizeBytes != 23 ||
		o.Last.Destination != "local" || !o.Last.Time.Equal(now) {
		t.Fatalf("last %+v", o.Last)
	}
	var names []string
	for _, f := range o.Files {
		names = append(names, f.Name)
	}
	if want := []string{ours("20260925T013000Z"), ours("20260104T033000Z"), ours("20260103T033000Z")}; !slices.Equal(names, want) {
		t.Fatalf("files %v, want %v", names, want)
	}
	if o.Files[0].SizeBytes != 23 || !o.Files[1].Time.Equal(time.Date(2026, 1, 4, 3, 30, 0, 0, time.UTC)) {
		t.Fatalf("file entries %+v", o.Files)
	}
	if o.DestinationPath != dir || o.FilesError != "" {
		t.Fatalf("destination %q %q", o.DestinationPath, o.FilesError)
	}
	for _, n := range append(others, ours("20000101T000000Z")) {
		if _, err := os.Lstat(filepath.Join(dir, n)); err != nil {
			t.Errorf("%s was touched: %v", n, err)
		}
	}
	if haveLink {
		if fi, err := os.Lstat(filepath.Join(dir, symlink)); err != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Errorf("the symbolic link was touched: %v", err)
		}
		if b, _ := os.ReadFile(filepath.Join(dir, "notes.txt")); string(b) != "keep me" {
			t.Error("the link target was changed")
		}
	}
	if _, err := os.Stat(filepath.Join(dir, ours("20250101T000000Z")+".tmp")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("leftover temporary file: %v", err)
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(filepath.Join(dir, ours("20260925T013000Z")))
		if fi.Mode().Perm()&^0o640 != 0 {
			t.Errorf("file mode %v", fi.Mode())
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.events) != 1 || e.events[0].Event != notify.EventBackupSucceeded ||
		!strings.Contains(e.events[0].Message, ours("20260925T013000Z")) || !strings.Contains(e.events[0].Message, "the data directory") {
		t.Fatalf("events %+v", e.events)
	}
}

// A storage target must be online: otherwise the run fails with the
// reason and backup.failed is sent; online, the backup goes to
// <store root>/picache-backups.
func TestScheduledTargetDestination(t *testing.T) {
	e := newSchedEnv(t)
	root := filepath.Join(t.TempDir(), "store")
	if err := os.MkdirAll(root, 0o750); err != nil {
		t.Fatal(err)
	}
	e.target = backupTarget{Name: "NAS", Root: root, Reason: "the share is not mounted at /srv/picache/nas"}
	s := daily("03:30")
	s.Destination = "0123456789abcdef0123456789abcdef"
	e.setBackups(t, s)
	if err := e.b.RunNow(); err != nil {
		t.Fatal(err)
	}
	e.b.runs.Wait()
	o := e.b.Overview(t.Context())
	const want = `the storage target "NAS" is not available: the share is not mounted at /srv/picache/nas`
	if o.Last == nil || o.Last.OK || o.Last.Error != want || o.Last.Destination != s.Destination || o.FilesError != want ||
		o.DestinationPath != filepath.Join(root, targetBackupDir) {
		t.Fatalf("offline: %+v", o)
	}
	e.mu.Lock()
	ev := slices.Clone(e.events)
	runs := len(e.runs)
	e.mu.Unlock()
	if len(ev) != 1 || ev[0].Event != notify.EventBackupFailed || !strings.Contains(ev[0].Message, want) || runs != 0 {
		t.Fatalf("events %+v, %d writes", ev, runs)
	}
	if _, err := os.Stat(filepath.Join(root, targetBackupDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("wrote to an offline target")
	}

	e.mu.Lock()
	e.target.Online = true
	e.mu.Unlock()
	if err := e.b.RunNow(); err != nil {
		t.Fatal(err)
	}
	e.b.runs.Wait()
	o = e.b.Overview(t.Context())
	if !o.Last.OK || len(o.Files) != 1 {
		t.Fatalf("online: %+v", o)
	}
	fi, err := os.Stat(filepath.Join(root, targetBackupDir, o.Last.File))
	if err != nil || fi.Size() != o.Last.SizeBytes {
		t.Fatalf("backup on the target: %v", err)
	}
	if runtime.GOOS != "windows" {
		di, _ := os.Stat(filepath.Join(root, targetBackupDir))
		if di.Mode().Perm()&^0o750 != 0 {
			t.Errorf("directory mode %v", di.Mode())
		}
	}

	// A symbolic link in place of the backup directory is not followed.
	if err := os.RemoveAll(filepath.Join(root, targetBackupDir)); err != nil {
		t.Fatal(err)
	}
	elsewhere := t.TempDir()
	if err := os.Symlink(elsewhere, filepath.Join(root, targetBackupDir)); err == nil {
		if err := e.b.RunNow(); err != nil {
			t.Fatal(err)
		}
		e.b.runs.Wait()
		if last := e.b.Overview(t.Context()).Last; last.OK || !strings.Contains(last.Error, "symbolic links are not followed") {
			t.Fatalf("symlinked directory: %+v", last)
		}
		if ents, _ := os.ReadDir(elsewhere); len(ents) != 0 {
			t.Fatalf("wrote through the link: %v", ents)
		}
	}

	// A deleted target and a failing writer are reported too.
	s.Destination = "ffffffffffffffffffffffffffffffff"
	e.setBackups(t, s)
	_ = e.b.RunNow()
	e.b.runs.Wait()
	if last := e.b.Overview(t.Context()).Last; last.OK || !strings.Contains(last.Error, "does not exist any more") {
		t.Fatalf("unknown target: %+v", last)
	}
	s.Destination = settings.BackupsLocal
	e.setBackups(t, s)
	e.fail = errors.New("backup: disk I/O error")
	_ = e.b.RunNow()
	e.b.runs.Wait()
	last := e.b.Overview(t.Context()).Last
	if last.OK || !strings.Contains(last.Error, "disk I/O error") {
		t.Fatalf("failing writer: %+v", last)
	}
	if ents, _ := os.ReadDir(filepath.Join(e.dir, "backups", "scheduled")); len(ents) != 0 {
		t.Fatalf("a failed run left files: %v", ents)
	}
}

// The last run survives a restart (app_meta backups.last) and counts for
// the catch-up.
func TestScheduledStatePersisted(t *testing.T) {
	e := newSchedEnv(t)
	e.setBackups(t, daily("03:30"))
	if err := e.b.RunNow(); err != nil {
		t.Fatal(err)
	}
	e.b.runs.Wait()
	b2 := newBackupScheduler(e.dir, testInstance, e.b.cdb, e.set, e.write, e.targetInfo, e.emit, slog.New(slog.DiscardHandler))
	b2.load(t.Context())
	if b2.last == nil || !b2.last.OK || b2.lastSuccess.IsZero() || b2.missed(daily("03:30"), time.Now()) {
		t.Fatalf("after restart: %+v %v", b2.last, b2.lastSuccess)
	}
}

// Download and delete accept only this installation's backups in the
// current destination.
func TestScheduledOpenDelete(t *testing.T) {
	e := newSchedEnv(t)
	e.setBackups(t, daily("03:30"))
	if err := e.b.RunNow(); err != nil {
		t.Fatal(err)
	}
	e.b.runs.Wait()
	name := e.b.Overview(t.Context()).Last.File
	f, size, err := e.b.Open(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(f)
	f.Close()
	if int64(len(b)) != size || !bytes.HasPrefix(b, []byte("SQLite format 3")) {
		t.Fatalf("read %q (%d)", b, size)
	}
	dir := filepath.Join(e.dir, "backups", "scheduled")
	touch(t, dir, "picache-backup-picache-ffffffffffff-20250101T033000Z.db", "other")
	for _, bad := range []string{"../picache.db", "picache-backup-picache-ffffffffffff-20250101T033000Z.db", "notes.txt", name + ".tmp"} {
		if _, _, err := e.b.Open(t.Context(), bad); apperr.KindOf(err) != apperr.KindInvalid {
			t.Errorf("open %q: %v", bad, err)
		}
		if err := e.b.Delete(t.Context(), bad); apperr.KindOf(err) != apperr.KindInvalid {
			t.Errorf("delete %q: %v", bad, err)
		}
	}
	missing := "picache-backup-" + testInstance + "-20200101T000000Z.db"
	if _, _, err := e.b.Open(t.Context(), missing); apperr.KindOf(err) != apperr.KindNotFound {
		t.Errorf("open missing: %v", err)
	}
	if err := e.b.Delete(t.Context(), name); err != nil {
		t.Fatal(err)
	}
	if err := e.b.Delete(t.Context(), name); apperr.KindOf(err) != apperr.KindNotFound {
		t.Fatalf("delete twice: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "picache-backup-picache-ffffffffffff-20250101T033000Z.db")); err != nil {
		t.Fatal(err)
	}
	// An offline target cannot be read.
	e.target = backupTarget{Name: "NAS", Reason: "offline"}
	s := daily("03:30")
	s.Destination = "0123456789abcdef0123456789abcdef"
	e.setBackups(t, s)
	if _, _, err := e.b.Open(t.Context(), missing); apperr.KindOf(err) != apperr.KindUnavailable {
		t.Fatalf("offline: %v", err)
	}
}

// Scheduled backups have the content of a manual backup: no accounts, and
// sealed NAS passwords and notification secrets only with includeSecrets.
func TestScheduledBackupContent(t *testing.T) {
	ctx := context.Background()
	a := newRestoreApp(t)
	makeConfigDB(t, a.paths.ConfigDB, "owner", "owner password", "en")
	openLive(t, a)
	box, _ := secrets.New(make([]byte, 32))
	n, err := notify.New(ctx, a.cdb, box, notify.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	secret := "tk_secret_token"
	if _, err := n.Create(ctx, notify.ChannelInput{Name: "ntfy", Kind: notify.KindNtfy, URL: "https://ntfy.sh/t", Secret: &secret}); err != nil {
		t.Fatal(err)
	}
	set, err := settings.Open(ctx, a.cdb, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	b := newBackupScheduler(a.cfg.DataDir, testInstance, a.cdb, set, a.Backup, nil, nil, slog.New(slog.DiscardHandler))
	for i, include := range []bool{false, true} {
		s := daily("03:30")
		s.IncludeSecrets = include
		if _, err := set.Update(ctx, func(x *settings.All) error { x.Backups = s; return nil }); err != nil {
			t.Fatal(err)
		}
		at := time.Date(2026, 9, 25, 1, 30, i, 0, time.UTC)
		b.now = func() time.Time { return at }
		if err := b.RunNow(); err != nil {
			t.Fatal(err)
		}
		b.runs.Wait()
		last := b.Overview(ctx).Last
		if !last.OK {
			t.Fatalf("run: %+v", last)
		}
		d, err := sql.Open("sqlite", filepath.Join(a.cfg.DataDir, "backups", "scheduled", last.File))
		if err != nil {
			t.Fatal(err)
		}
		var users, channels, sealed int
		err = d.QueryRow(`SELECT (SELECT COUNT(*) FROM auth_users), (SELECT COUNT(*) FROM notify_channels),
			(SELECT COUNT(*) FROM notify_channels WHERE secret_sealed IS NOT NULL)`).Scan(&users, &channels, &sealed)
		d.Close()
		if err != nil {
			t.Fatal(err)
		}
		want := 0
		if include {
			want = 1
		}
		if users != 0 || channels != 1 || sealed != want {
			t.Fatalf("includeSecrets=%v: %d users, %d channels, %d sealed secrets", include, users, channels, sealed)
		}
	}
}

// A manual backup drops the sealed notification secrets unless they are
// included; the plain secret is never in a backup.
func TestBackupScrubsNotificationSecrets(t *testing.T) {
	ctx := context.Background()
	a := newRestoreApp(t)
	makeConfigDB(t, a.paths.ConfigDB, "owner", "owner password", "en")
	openLive(t, a)
	box, _ := secrets.New(make([]byte, 32))
	n, err := notify.New(ctx, a.cdb, box, notify.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	secret := "Bearer webhook-secret-123"
	c, err := n.Create(ctx, notify.ChannelInput{Name: "ha", Kind: notify.KindWebhook, URL: "http://10.0.0.2/h", Secret: &secret})
	if err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := a.cdb.R.QueryRow(`SELECT secret_sealed FROM notify_channels WHERE id = ?`, c.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	for _, include := range []bool{false, true} {
		var buf bytes.Buffer
		if err := a.Backup(ctx, &buf, include); err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(buf.Bytes(), []byte("webhook-secret-123")) {
			t.Fatal("the plain secret is in the backup")
		}
		if got := bytes.Contains(buf.Bytes(), []byte(stored)); got != include {
			t.Fatalf("includeSecrets=%v: sealed secret in the backup = %v", include, got)
		}
	}
}
