package app

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hustenreizjuengling/picache/internal/api"
	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/notify"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// Scheduled backups (docs/ARCHITECTURE.md 15.2): at a local time of day a
// copy of picache.db with the content of a manual backup (App.Backup: no
// accounts; sealed secrets only with includeSecrets) is written to
// <data>/backups/scheduled or to <store root of a storage target>/
// picache-backups, and the oldest of this installation's scheduled backups
// there are deleted until settings.Backups.Keep remain.
const (
	backupsStateKey  = "backups.last"    // app_meta key of the last run
	scheduledDirName = "scheduled"       // below <data>/backups
	targetBackupDir  = "picache-backups" // below a storage target's store root
	backupStamp      = "20060102T150405Z"
	backupTick       = time.Minute
	catchUpDelay     = 5 * time.Minute // a missed run is made this long after the start
	catchUpSlack     = time.Hour       // missed: the last success is older than the interval + this
	listTimeout      = 5 * time.Second // listing the destination for the overview
	maxBackupEntries = 10_000          // directory entries read per scan
	backupStopWait   = 3 * time.Second // shutdown waits this long for a run
	saveStateTimeout = 5 * time.Second
)

// backupTarget is what the scheduler needs to know about a storage target.
type backupTarget struct {
	Name   string
	Root   string // store root ("" if unknown); valid also while offline
	Online bool
	Reason string // why it is offline
}

// backupDest is the directory a destination writes to: dir below parent.
type backupDest struct {
	parent string
	dir    string
	desc   string // for messages: "the data directory", `the storage target "NAS"`
	local  bool   // parent is <data>/backups (created if missing)
}

func (d backupDest) path() string {
	if d.parent == "" {
		return ""
	}
	return filepath.Join(d.parent, d.dir)
}

// backupsState is the app_meta document of the last run.
type backupsState struct {
	Last          *api.ScheduledBackupRun `json:"last,omitempty"`
	LastSuccessAt time.Time               `json:"lastSuccessAt,omitzero"`
}

// backupScheduler implements api.ScheduledBackups.
type backupScheduler struct {
	log      *slog.Logger
	set      *settings.Store
	cdb      *db.DB // nil: the last run is not persisted (tests)
	dataDir  string
	instance string // installation id as used in file names
	backup   func(ctx context.Context, w io.Writer, includeSecrets bool) error
	target   func(id string) (backupTarget, error)
	emit     func(notify.Message)
	now      func() time.Time
	loc      *time.Location // the host's time zone (time.Local)
	names    *regexp.Regexp // this installation's backup files; group 1: the time stamp
	tmpNames *regexp.Regexp // their temporary files

	ctx     context.Context // runs; cancelled when Start ends
	cancel  context.CancelFunc
	running atomic.Bool // one run at a time
	listing atomic.Bool // a listing for the overview is in progress (it may hang on a NAS)

	mu          sync.Mutex // guards the fields below
	stopped     bool       // Start ended: no new runs
	runs        sync.WaitGroup
	last        *api.ScheduledBackupRun
	lastSuccess time.Time
	catchUpAt   time.Time // a missed run is due then (zero: none)
}

func newBackupScheduler(dataDir, instanceID string, cdb *db.DB, set *settings.Store,
	backup func(context.Context, io.Writer, bool) error, target func(string) (backupTarget, error),
	emit func(notify.Message), log *slog.Logger) *backupScheduler {
	inst := sanitizeFile(instanceID)
	if len(inst) > 64 {
		inst = inst[:64]
	}
	prefix := `^picache-backup-` + regexp.QuoteMeta(inst) + `-(\d{8}T\d{6}Z)\.db`
	b := &backupScheduler{
		log: log.With(slog.String("component", "backup")), set: set, cdb: cdb, dataDir: dataDir, instance: inst,
		backup: backup, target: target, emit: emit, now: time.Now, loc: time.Local,
		names: regexp.MustCompile(prefix + `$`), tmpNames: regexp.MustCompile(prefix + `\.tmp$`),
	}
	b.ctx, b.cancel = context.WithCancel(context.Background())
	return b
}

// load reads the last run from app_meta.
func (b *backupScheduler) load(ctx context.Context) {
	if b.cdb == nil {
		return
	}
	var doc string
	if err := b.cdb.R.QueryRowContext(ctx, `SELECT value FROM app_meta WHERE key = ?`, backupsStateKey).Scan(&doc); err != nil {
		return
	}
	var st backupsState
	if err := json.Unmarshal([]byte(doc), &st); err != nil {
		b.log.Warn("ignoring the stored state of scheduled backups", slog.Any("err", err))
		return
	}
	b.mu.Lock()
	b.last, b.lastSuccess = st.Last, st.LastSuccessAt
	b.mu.Unlock()
}

func (b *backupScheduler) save(st backupsState) {
	if b.cdb == nil {
		return
	}
	doc, err := json.Marshal(st)
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), saveStateTimeout)
		_, err = b.cdb.W.ExecContext(ctx, `INSERT INTO app_meta (key, value) VALUES (?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value`, backupsStateKey, string(doc))
		cancel()
	}
	if err != nil {
		b.log.Warn("cannot store the state of scheduled backups", slog.Any("err", err))
	}
}

// Start checks the schedule every minute until ctx ends (blocks until
// done). A run that is still going then is cancelled and waited for at
// most backupStopWait.
func (b *backupScheduler) Start(ctx context.Context) {
	b.planCatchUp(b.now())
	t := time.NewTicker(backupTick)
	defer t.Stop()
	prev := b.now()
	for {
		select {
		case <-ctx.Done():
			b.mu.Lock()
			b.stopped = true
			b.mu.Unlock()
			b.cancel()
			if !waitFor(&b.runs, backupStopWait) {
				b.log.Warn("a backup did not stop in time")
			}
			return
		case <-t.C:
		}
		now := b.now()
		b.tick(prev, now)
		prev = now
	}
}

// planCatchUp schedules a run catchUpDelay after the start when scheduled
// backups are enabled and the last successful run is older than the
// interval + 1 h (PiCache was down, or the runs failed).
func (b *backupScheduler) planCatchUp(now time.Time) {
	s := b.set.Get().Backups
	if !s.Enabled || !b.missed(s, now) {
		return
	}
	at := now.Add(catchUpDelay)
	b.mu.Lock()
	b.catchUpAt = at
	b.mu.Unlock()
	b.log.Info("a scheduled backup was missed; making one soon", slog.Time("at", at))
}

// missed reports whether the last successful run is older than the
// schedule's interval + catchUpSlack.
func (b *backupScheduler) missed(s settings.Backups, now time.Time) bool {
	b.mu.Lock()
	last := b.lastSuccess
	b.mu.Unlock()
	return last.IsZero() || now.Sub(last) > backupInterval(s)+catchUpSlack
}

// tick starts a run when a scheduled time lies in (prev, now], or when a
// planned catch-up is due and still needed. Each scheduled time is due
// once: it lies in exactly one such interval, also when the clock is set
// back (then no interval contains it again).
func (b *backupScheduler) tick(prev, now time.Time) {
	s := b.set.Get().Backups
	b.mu.Lock()
	catchUp := b.catchUpAt
	if !s.Enabled {
		b.catchUpAt = time.Time{}
	}
	b.mu.Unlock()
	if !s.Enabled {
		return
	}
	due := lastDue(s, now, b.loc)
	switch {
	case due.After(prev):
		b.clearCatchUp()
		if !b.start("scheduled") {
			b.log.Warn("scheduled backup skipped: another backup is running")
		}
	case !catchUp.IsZero() && !now.Before(catchUp):
		b.clearCatchUp()
		if b.missed(s, now) && !b.start("catch-up") {
			b.log.Warn("missed backup skipped: another backup is running")
		}
	}
}

func (b *backupScheduler) clearCatchUp() {
	b.mu.Lock()
	b.catchUpAt = time.Time{}
	b.mu.Unlock()
}

// backupInterval is the time between two scheduled runs.
func backupInterval(s settings.Backups) time.Duration {
	if s.Schedule == "weekly" {
		return 7 * 24 * time.Hour
	}
	return 24 * time.Hour
}

// scheduledOn returns the run time on the local date of day and whether
// the schedule runs on that date. On the date the time of day does not
// exist (the clocks go forward) time.Date moves it forward by the gap; on
// the date it exists twice (the clocks go back) time.Date picks one of the
// two. There is one run per date either way.
func scheduledOn(s settings.Backups, day time.Time, loc *time.Location) (time.Time, bool) {
	if s.Schedule == "weekly" && day.Weekday() != time.Weekday(s.Weekday) {
		return time.Time{}, false
	}
	h, m, _ := settings.ParseClock(s.Time)
	y, mo, d := day.Date()
	return time.Date(y, mo, d, h, m, 0, 0, loc), true
}

// lastDue returns the latest scheduled time at or before now.
func lastDue(s settings.Backups, now time.Time, loc *time.Location) time.Time {
	y, m, d := now.In(loc).Date()
	for i := 0; i <= 8; i++ {
		// Noon: the date arithmetic never touches a DST transition.
		if t, ok := scheduledOn(s, time.Date(y, m, d-i, 12, 0, 0, 0, loc), loc); ok && !t.After(now) {
			return t
		}
	}
	return time.Time{}
}

// nextDue returns the first scheduled time after now.
func nextDue(s settings.Backups, now time.Time, loc *time.Location) time.Time {
	y, m, d := now.In(loc).Date()
	for i := 0; i <= 8; i++ {
		if t, ok := scheduledOn(s, time.Date(y, m, d+i, 12, 0, 0, 0, loc), loc); ok && t.After(now) {
			return t
		}
	}
	return time.Time{}
}

// RunNow starts a run in the background, the same as a scheduled one.
func (b *backupScheduler) RunNow() error {
	if !b.start("manual") {
		return apperr.Conflict("a backup is already running")
	}
	return nil
}

// start begins a run unless one is running or the scheduler has stopped.
func (b *backupScheduler) start(trigger string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stopped || !b.running.CompareAndSwap(false, true) {
		return false
	}
	b.runs.Go(func() {
		defer b.running.Store(false)
		b.run(b.ctx, trigger)
	})
	return true
}

// run writes one backup, applies the retention, records the result and
// sends backup.succeeded or backup.failed.
func (b *backupScheduler) run(ctx context.Context, trigger string) {
	s := b.set.Get().Backups
	at := b.now().UTC().Truncate(time.Second)
	b.mu.Lock()
	if b.last != nil && !at.After(b.last.Time) {
		// File names have whole seconds: a run right after another one
		// takes the next second instead of replacing its file.
		at = b.last.Time.Add(time.Second)
	}
	b.mu.Unlock()
	rec := api.ScheduledBackupRun{Time: at, Destination: s.Destination}
	d, err := b.destination(s.Destination)
	if err == nil {
		rec.File, rec.SizeBytes, err = b.write(ctx, d, s, at)
	}
	if err != nil {
		rec.Error = err.Error()
	} else {
		rec.OK = true
	}
	b.mu.Lock()
	b.last = &rec
	if rec.OK {
		b.lastSuccess = rec.Time
	}
	st := backupsState{Last: &rec, LastSuccessAt: b.lastSuccess}
	b.mu.Unlock()
	b.save(st)
	if !rec.OK {
		b.log.Error("backup failed", slog.String("trigger", trigger), slog.String("destination", s.Destination),
			slog.String("err", rec.Error))
		b.notify(notify.Message{Event: notify.EventBackupFailed, Title: "Scheduled backup failed",
			Message: fmt.Sprintf("The backup to %s failed: %s", d.desc, rec.Error)})
		return
	}
	b.log.Info("backup written", slog.String("trigger", trigger), slog.String("file", filepath.Join(d.path(), rec.File)),
		slog.Int64("bytes", rec.SizeBytes))
	b.notify(notify.Message{Event: notify.EventBackupSucceeded, Title: "Backup written",
		Message: fmt.Sprintf("The backup %s (%s) was written to %s.", rec.File, formatBytes(rec.SizeBytes), d.desc)})
}

func (b *backupScheduler) notify(m notify.Message) {
	if b.emit != nil {
		b.emit(m)
	}
}

// destination resolves a destination setting. A storage target must exist
// and be online (its guard status); the error says why not.
func (b *backupScheduler) destination(id string) (backupDest, error) {
	if id == settings.BackupsLocal {
		return backupDest{parent: filepath.Join(b.dataDir, "backups"), dir: scheduledDirName, desc: "the data directory", local: true}, nil
	}
	t, err := b.target(id)
	if err != nil {
		return backupDest{desc: "the storage target " + id},
			fmt.Errorf("the storage target %s does not exist any more; choose another destination", id)
	}
	d := backupDest{parent: t.Root, dir: targetBackupDir, desc: fmt.Sprintf("the storage target %q", t.Name)}
	if !t.Online {
		reason := t.Reason
		if reason == "" {
			reason = "it is offline"
		}
		return d, fmt.Errorf("the storage target %q is not available: %s", t.Name, reason)
	}
	return d, nil
}

// open opens the backup directory of d, creating it (0750) if create. A
// symbolic link in its place is refused, and so is a directory that was
// swapped while it was opened.
func (d backupDest) open(create bool) (*os.Root, error) {
	if d.parent == "" {
		return nil, errors.New("unknown destination")
	}
	if create && d.local {
		if err := os.MkdirAll(d.parent, 0o750); err != nil {
			return nil, err
		}
	}
	pr, err := os.OpenRoot(d.parent)
	if err != nil {
		return nil, err
	}
	defer pr.Close()
	if create {
		if err := pr.Mkdir(d.dir, 0o750); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
	}
	fi, err := pr.Lstat(d.dir)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("%s is not a directory (symbolic links are not followed)", d.path())
	}
	r, err := pr.OpenRoot(d.dir)
	if err != nil {
		return nil, err
	}
	if rfi, err := r.Stat("."); err != nil || !os.SameFile(fi, rfi) {
		r.Close()
		return nil, fmt.Errorf("%s changed while it was opened", d.path())
	}
	return r, nil
}

// write stores one backup as <name>.tmp and renames it (a partly written
// file never has a backup's name), then deletes the oldest backups of this
// installation beyond keep. The temporary file is created exclusively and
// never through a symbolic link.
func (b *backupScheduler) write(ctx context.Context, d backupDest, s settings.Backups, at time.Time) (string, int64, error) {
	dir, err := d.open(true)
	if err != nil {
		return "", 0, fmt.Errorf("cannot open %s: %w", d.path(), err)
	}
	defer dir.Close()
	b.removeTemp(dir)
	name := "picache-backup-" + b.instance + "-" + at.UTC().Format(backupStamp) + ".db"
	tmp := name + ".tmp"
	f, err := dir.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|oNoFollow, 0o640)
	if err != nil {
		return "", 0, fmt.Errorf("cannot create a file in %s: %w", d.path(), err)
	}
	cw := &countWriter{w: f}
	err = b.backup(ctx, cw, s.IncludeSecrets)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = dir.Rename(tmp, name)
	}
	if err != nil {
		_ = dir.Remove(tmp)
		if ctx.Err() != nil {
			return "", 0, errors.New("cancelled: PiCache is shutting down")
		}
		return "", 0, fmt.Errorf("cannot write the backup to %s: %w", d.path(), err)
	}
	if n, err := b.prune(dir, s.Keep); err != nil {
		b.log.Warn("cannot delete old scheduled backups", slog.String("dir", d.path()), slog.Any("err", err))
	} else if n > 0 {
		b.log.Info("deleted old scheduled backups", slog.String("dir", d.path()), slog.Int("files", n), slog.Int("keep", s.Keep))
	}
	return name, cw.n, nil
}

// prune deletes this installation's oldest backups in dir until keep
// remain. Other files are never touched.
func (b *backupScheduler) prune(dir *os.Root, keep int) (int, error) {
	files, err := b.list(dir)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, f := range files[min(keep, len(files)):] {
		if err := dir.Remove(f.Name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return n, err
		}
		n++
	}
	return n, nil
}

// scan calls fn for at most maxBackupEntries names in dir.
func scan(dir *os.Root, fn func(name string)) error {
	d, err := dir.Open(".")
	if err != nil {
		return err
	}
	defer d.Close()
	for seen := 0; seen < maxBackupEntries; {
		names, err := d.Readdirnames(256)
		for _, n := range names {
			fn(n)
		}
		seen += len(names)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// list returns this installation's backups in dir (regular files only),
// newest first.
func (b *backupScheduler) list(dir *os.Root) ([]api.ScheduledBackupFile, error) {
	out := []api.ScheduledBackupFile{}
	err := scan(dir, func(name string) {
		m := b.names.FindStringSubmatch(name)
		if m == nil {
			return
		}
		fi, err := dir.Lstat(name)
		if err != nil || !fi.Mode().IsRegular() {
			return
		}
		t, err := time.Parse(backupStamp, m[1])
		if err != nil {
			return
		}
		out = append(out, api.ScheduledBackupFile{Name: name, SizeBytes: fi.Size(), Time: t.UTC()})
	})
	if err != nil {
		return nil, err
	}
	// Same prefix and a fixed-width time stamp: name order is time order.
	slices.SortFunc(out, func(x, y api.ScheduledBackupFile) int { return strings.Compare(y.Name, x.Name) })
	return out, nil
}

// removeTemp deletes temporary files left by an interrupted run.
func (b *backupScheduler) removeTemp(dir *os.Root) {
	var tmps []string
	_ = scan(dir, func(name string) {
		if b.tmpNames.MatchString(name) {
			tmps = append(tmps, name)
		}
	})
	for _, name := range tmps {
		if fi, err := dir.Lstat(name); err == nil && !fi.IsDir() {
			_ = dir.Remove(name)
		}
	}
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// formatBytes renders a size for messages (binary units).
func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 4; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}

// --- api.ScheduledBackups ---

// Overview returns the settings, the last and the next run and the
// backups in the current destination. A destination that does not answer
// within listTimeout is reported in FilesError.
func (b *backupScheduler) Overview(ctx context.Context) api.ScheduledBackupsOverview {
	s := b.set.Get().Backups
	now := b.now()
	zone, _ := now.In(b.loc).Zone()
	o := api.ScheduledBackupsOverview{Settings: s, Running: b.running.Load(), TimeZone: zone, Files: []api.ScheduledBackupFile{}}
	b.mu.Lock()
	if b.last != nil {
		last := *b.last
		o.Last = &last
	}
	catchUp := b.catchUpAt
	b.mu.Unlock()
	if s.Enabled {
		o.Next = nextDue(s, now, b.loc).UTC()
		if !catchUp.IsZero() && catchUp.Before(o.Next) {
			o.Next = catchUp.UTC()
		}
	}
	d, err := b.destination(s.Destination)
	o.DestinationPath = d.path()
	if err != nil {
		o.FilesError = err.Error()
		return o
	}
	files, err := b.listBounded(d)
	if err != nil {
		o.FilesError = err.Error()
		return o
	}
	o.Files = files
	return o
}

// listBounded lists the backups of d in a goroutine and gives up after
// listTimeout (a hung NAS blocks in the kernel). Only one listing runs at a
// time, so a hung one leaves at most one goroutine behind.
func (b *backupScheduler) listBounded(d backupDest) ([]api.ScheduledBackupFile, error) {
	if !b.listing.CompareAndSwap(false, true) {
		return nil, errors.New("the destination is still being read; try again in a moment")
	}
	type result struct {
		files []api.ScheduledBackupFile
		err   error
	}
	ch := make(chan result, 1)
	go func() {
		defer b.listing.Store(false)
		dir, err := d.open(false)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			ch <- result{files: []api.ScheduledBackupFile{}}
			return
		case err != nil:
			ch <- result{err: fmt.Errorf("cannot open %s: %w", d.path(), err)}
			return
		}
		defer dir.Close()
		files, err := b.list(dir)
		ch <- result{files, err}
	}()
	t := time.NewTimer(listTimeout)
	defer t.Stop()
	select {
	case r := <-ch:
		return r.files, r.err
	case <-t.C:
		return nil, fmt.Errorf("%s did not answer within %s", d.path(), listTimeout)
	}
}

// openBackup opens the directory of the current destination for a backup
// named name (a scheduled backup of this installation).
func (b *backupScheduler) openBackup(name string) (*os.Root, error) {
	if !b.names.MatchString(name) {
		return nil, apperr.Invalid("name", "not a scheduled backup of this PiCache")
	}
	d, err := b.destination(b.set.Get().Backups.Destination)
	if err != nil {
		return nil, apperr.Unavailable("%s", err.Error())
	}
	dir, err := d.open(false)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, apperr.NotFound("scheduled backup", name)
	case err != nil:
		return nil, apperr.Wrap(apperr.KindUnavailable, err, "cannot open %s", d.path())
	}
	fi, err := dir.Lstat(name)
	if err != nil || !fi.Mode().IsRegular() {
		dir.Close()
		return nil, apperr.NotFound("scheduled backup", name)
	}
	return dir, nil
}

// Open opens a backup of the current destination for download.
func (b *backupScheduler) Open(_ context.Context, name string) (io.ReadCloser, int64, error) {
	dir, err := b.openBackup(name)
	if err != nil {
		return nil, 0, err
	}
	defer dir.Close()
	f, err := dir.OpenFile(name, os.O_RDONLY|oNoFollow, 0)
	if err != nil {
		return nil, 0, apperr.NotFound("scheduled backup", name)
	}
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		f.Close()
		return nil, 0, apperr.NotFound("scheduled backup", name)
	}
	return f, fi.Size(), nil
}

// Delete removes a backup of the current destination.
func (b *backupScheduler) Delete(_ context.Context, name string) error {
	dir, err := b.openBackup(name)
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := dir.Remove(name); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return apperr.NotFound("scheduled backup", name)
		}
		return err
	}
	b.log.Info("scheduled backup deleted", slog.String("file", name))
	return nil
}

// backupTarget describes a storage target for the backup scheduler.
func (a *App) backupTarget(id string) (backupTarget, error) {
	t, err := a.storage.Target(context.Background(), id)
	if err != nil {
		return backupTarget{}, err
	}
	bt := backupTarget{Name: t.Name, Root: a.storage.Status(id).StoreRoot}
	if root, _, err := a.storage.StoreRoot(id); err == nil {
		bt.Root, bt.Online = root, true
	} else {
		bt.Reason = reason(err)
	}
	return bt, nil
}
