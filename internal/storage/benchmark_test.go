package storage

import (
	"context"
	"encoding/json/v2"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	cachestore "github.com/hustenreizjuengling/picache/internal/dlcache/store"
)

// benchManager is a manager whose speed tests are small: 64 "MiB" are
// 1 MiB, written in blocks of 64 KiB, with no free space reserve.
func benchManager(t *testing.T) (*Manager, Target) {
	t.Helper()
	cfg := testConfig(t)
	m, _, _ := newTestManager(t, cfg)
	m.benchLim.unit, m.benchLim.block, m.benchLim.reserve = 16<<10, 64<<10, 0
	local, err := m.Target(context.Background(), LocalTargetID)
	if err != nil {
		t.Fatal(err)
	}
	return m, local
}

// waitBench waits until the running speed test has ended.
func waitBench(t *testing.T, m *Manager) BenchmarkRun {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if o := m.Benchmark(); o.Run != nil && o.Run.State != benchRunning {
			return *o.Run
		}
		if time.Now().After(deadline) {
			t.Fatalf("speed test still running: %+v", m.Benchmark().Run)
		}
		time.Sleep(time.Millisecond)
	}
}

// runBench runs a complete speed test and returns the finished run.
func runBench(t *testing.T, m *Manager, id string, active *cachestore.Store) BenchmarkRun {
	t.Helper()
	run, err := m.StartBenchmark(context.Background(), id, 64, active)
	if err != nil {
		t.Fatal(err)
	}
	if run.State != benchRunning || run.TargetID != id || run.SizeMiB != 64 || run.StartedAt.IsZero() || run.Result != nil {
		t.Fatalf("started run %+v", run)
	}
	return waitBench(t, m)
}

// speedtestFiles lists the speed test files in the store root and its tmp/.
func speedtestFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	for _, dir := range []string{root, filepath.Join(root, "tmp")} {
		ents, _ := os.ReadDir(dir)
		for _, e := range ents {
			if strings.Contains(e.Name(), "picache-speedtest-") {
				out = append(out, filepath.Join(dir, e.Name()))
			}
		}
	}
	return out
}

// measured reports whether tp covers n bytes with a plausible speed (tiny
// test sizes can take less than one tick of the Windows clock).
func measured(tp Throughput, n int64) bool {
	return tp.Bytes == n && tp.Seconds >= 0 && tp.BytesPerSec >= 0 && (tp.Seconds == 0 || tp.BytesPerSec > 0)
}

func hasNote(res *BenchmarkResult, substr string) bool {
	return slices.ContainsFunc(res.Notes, func(n string) bool { return strings.Contains(n, substr) })
}

func TestBenchmarkRun(t *testing.T) {
	m, local := benchManager(t)
	run := runBench(t, m, LocalTargetID, nil)
	res := run.Result
	if run.State != benchDone || run.Phase != phaseDone || run.Progress != 1 || run.FinishedAt.IsZero() || run.Error != "" || res == nil {
		t.Fatalf("run %+v", run)
	}
	if res.TargetID != LocalTargetID || res.StoreRoot != local.Path || res.SizeBytes != 1<<20 || !res.TestedAt.Equal(run.StartedAt) {
		t.Fatalf("result %+v", res)
	}
	if !measured(res.Write, 1<<20) || !measured(res.Read.Throughput, 1<<20) {
		t.Fatalf("throughput %+v %+v", res.Write, res.Read)
	}
	if md := res.Metadata; md.Ops != 32 || md.P50Ms < 0 || md.P95Ms < md.P50Ms || md.MaxMs < md.P95Ms {
		t.Fatalf("metadata %+v", md)
	}
	if res.Slices != nil || !hasNote(res, "not the active cache store") {
		t.Fatalf("slices without an active store: %+v %v", res.Slices, res.Notes)
	}
	if res.Read.CacheDropped == hasNote(res, "page cache could not be dropped") {
		t.Fatalf("cacheDropped %v, notes %v", res.Read.CacheDropped, res.Notes)
	}
	if files := speedtestFiles(t, local.Path); len(files) != 0 {
		t.Fatalf("test files left: %v", files)
	}
	if last := m.Benchmark().Last[LocalTargetID]; last.SizeBytes != res.SizeBytes || !last.TestedAt.Equal(res.TestedAt) {
		t.Fatalf("last result %+v", last)
	}
	// The overview hands out copies.
	o := m.Benchmark()
	o.Run.Result.Notes[0] = "changed"
	if m.Benchmark().Run.Result.Notes[0] == "changed" {
		t.Fatal("Benchmark returns shared state")
	}
}

func TestBenchmarkRefusals(t *testing.T) {
	m, local := benchManager(t)
	ctx := context.Background()
	for _, size := range []int{0, 1, 100, 2048, -64} {
		_, err := m.StartBenchmark(ctx, LocalTargetID, size, nil)
		if ae := wantKind(t, err, apperr.KindInvalid); ae.Field != "sizeMiB" {
			t.Fatalf("size %d: field %q", size, ae.Field)
		}
	}
	_, err := m.StartBenchmark(ctx, "ffffffffffffffffffffffffffffffff", 64, nil)
	wantKind(t, err, apperr.KindNotFound)

	// Busy: being initialised or functionally tested.
	for _, busy := range []entry{{busy: true}, {testing: 1}} {
		m.mu.Lock()
		e := m.targets[LocalTargetID]
		e.busy, e.testing = busy.busy, busy.testing
		m.mu.Unlock()
		_, err := m.StartBenchmark(ctx, LocalTargetID, 64, nil)
		if ae := wantKind(t, err, apperr.KindConflict); !strings.Contains(ae.Message, "initialised or tested") {
			t.Fatalf("busy: %q", ae.Message)
		}
		m.mu.Lock()
		e.busy, e.testing = false, 0
		m.mu.Unlock()
	}

	// The same fresh check as Test: a missing directory is not available.
	gone, err := m.Create(ctx, localInput(filepath.Join(m.cfg.MountRoot, "absent")))
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.StartBenchmark(ctx, gone.ID, 64, nil)
	if ae := wantKind(t, err, apperr.KindConflict); !strings.HasPrefix(ae.Message, "the storage target is not available: ") ||
		!strings.Contains(ae.Message, "does not exist") {
		t.Fatalf("unavailable: %q", ae.Message)
	}

	// Free space: size + reserve must be free.
	m.benchLim.reserve = 1 << 30
	m.probeFn = func(t Target) checkResult {
		res := m.probe(t)
		res.st.TotalBytes, res.st.FreeBytes = 100<<30, 1<<30+(1<<20)-1 // one byte short of 1 MiB + 1 GiB
		return res
	}
	_, err = m.StartBenchmark(ctx, LocalTargetID, 64, nil)
	if ae := wantKind(t, err, apperr.KindInvalid); ae.Field != "" || ae.Message != "not enough free space: the test needs 1.0 GiB, 1.0 GiB are free" {
		t.Fatalf("free space: %+v", ae)
	}
	if o := m.Benchmark(); o.Run != nil || len(o.Last) != 0 {
		t.Fatalf("a refused start left a run: %+v", o)
	}
	m.probeFn = func(t Target) checkResult {
		res := m.probe(t)
		res.st.TotalBytes, res.st.FreeBytes = 100<<30, 1<<30+1<<20
		return res
	}
	if run := runBench(t, m, LocalTargetID, nil); run.State != benchDone || run.Result.FreeBytes != 1<<30+1<<20 ||
		hasNote(run.Result, "free space could not be checked") {
		t.Fatalf("enough free space: %+v", run)
	}
	if files := speedtestFiles(t, local.Path); len(files) != 0 {
		t.Fatalf("test files left: %v", files)
	}
}

// blockingWriter makes the write phase wait after its first block until
// release is closed or the run is cancelled.
type blockingWriter struct {
	once    sync.Once
	reached chan struct{}
	release chan struct{}
}

func newBlockingWriter(m *Manager) *blockingWriter {
	w := &blockingWriter{reached: make(chan struct{}), release: make(chan struct{})}
	m.benchLim.slow = func(ctx context.Context, phase string) {
		if phase != phaseWrite {
			return
		}
		w.once.Do(func() { close(w.reached) })
		select {
		case <-w.release:
		case <-ctx.Done():
		}
	}
	return w
}

func (w *blockingWriter) wait(t *testing.T) {
	t.Helper()
	select {
	case <-w.reached:
	case <-time.After(10 * time.Second):
		t.Fatal("the write phase was not reached")
	}
}

func TestBenchmarkOneAtATime(t *testing.T) {
	m, local := benchManager(t)
	ctx := context.Background()
	disk := mkdir(t, filepath.Join(m.cfg.MountRoot, "disk"))
	other, err := m.Create(ctx, localInput(disk))
	if err != nil {
		t.Fatal(err)
	}

	// While the checks of a start run, a second start is refused.
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	m.probeFn = func(t Target) checkResult {
		once.Do(func() { close(entered); <-release })
		return m.probe(t)
	}
	w := newBlockingWriter(m)
	started := make(chan error, 1)
	go func() {
		_, err := m.StartBenchmark(ctx, LocalTargetID, 64, nil)
		started <- err
	}()
	<-entered
	_, err = m.StartBenchmark(ctx, other.ID, 64, nil)
	if ae := wantKind(t, err, apperr.KindConflict); ae.Message != "a speed test is already running" {
		t.Fatalf("second start while checking: %q", ae.Message)
	}
	close(release)
	if err := <-started; err != nil {
		t.Fatal(err)
	}

	// While it runs: no second start (any target), no InitStore on its
	// target, and eviction does not count its test file.
	w.wait(t)
	_, err = m.StartBenchmark(ctx, other.ID, 64, nil)
	wantKind(t, err, apperr.KindConflict)
	_, err = m.InitStore(ctx, LocalTargetID, true)
	if ae := wantKind(t, err, apperr.KindConflict); !strings.Contains(ae.Message, "speed test is running") {
		t.Fatalf("init while running: %q", ae.Message)
	}
	if n := m.BenchmarkBytes(LocalTargetID); n != 64<<10 {
		t.Fatalf("BenchmarkBytes = %d, want one block", n)
	}
	if n := m.BenchmarkBytes(other.ID); n != 0 {
		t.Fatalf("BenchmarkBytes(other) = %d", n)
	}
	if o := m.Benchmark(); o.Run == nil || o.Run.State != benchRunning || o.Run.Phase != phaseWrite || o.Run.Progress <= 0.02 || o.Run.Progress >= 0.42 {
		t.Fatalf("running: %+v", o.Run)
	}
	close(w.release)
	if run := waitBench(t, m); run.State != benchDone {
		t.Fatalf("run %+v", run)
	}
	if m.BenchmarkBytes(LocalTargetID) != 0 {
		t.Fatal("BenchmarkBytes after the run")
	}

	// The next run may start (on another target) and replaces the run.
	m.benchLim.slow = nil
	if run := runBench(t, m, other.ID, nil); run.State != benchDone || run.TargetID != other.ID {
		t.Fatalf("second run %+v", run)
	}
	if o := m.Benchmark(); len(o.Last) != 2 || o.Run.TargetID != other.ID {
		t.Fatalf("overview %+v", o)
	}
	if files := append(speedtestFiles(t, local.Path), speedtestFiles(t, disk)...); len(files) != 0 {
		t.Fatalf("test files left: %v", files)
	}
}

func TestBenchmarkCancel(t *testing.T) {
	m, local := benchManager(t)
	ctx := context.Background()
	if id, ok := m.CancelBenchmark(ctx); ok || id != "" {
		t.Fatal("cancel without a run")
	}
	w := newBlockingWriter(m)
	if _, err := m.StartBenchmark(ctx, LocalTargetID, 64, nil); err != nil {
		t.Fatal(err)
	}
	w.wait(t)
	if files := speedtestFiles(t, local.Path); len(files) != 1 || filepath.Dir(files[0]) != filepath.Join(local.Path, "tmp") {
		t.Fatalf("test file while running: %v", files)
	}
	id, ok := m.CancelBenchmark(ctx)
	if !ok || id != LocalTargetID {
		t.Fatalf("cancel = %q %v", id, ok)
	}
	// CancelBenchmark waits for the run to end.
	o := m.Benchmark()
	if o.Run.State != benchCancelled || o.Run.Result != nil || o.Run.FinishedAt.IsZero() || o.Run.Error != "" || len(o.Last) != 0 {
		t.Fatalf("cancelled run %+v", o)
	}
	if files := speedtestFiles(t, local.Path); len(files) != 0 {
		t.Fatalf("test files left after cancel: %v", files)
	}

	// Cancelled after a phase finished: the partial result is kept.
	m.benchLim.slow = nil
	var once sync.Once
	m.benchLim.metaOps = 1 << 20 // the metadata phase runs until cancelled
	cancelled := make(chan struct{})
	go func() {
		for m.Benchmark().Run.Phase != phaseMetadata {
			time.Sleep(time.Millisecond)
		}
		once.Do(func() { m.CancelBenchmark(ctx); close(cancelled) })
	}()
	if _, err := m.StartBenchmark(ctx, LocalTargetID, 64, nil); err != nil {
		t.Fatal(err)
	}
	<-cancelled
	run := waitBench(t, m)
	if run.State != benchCancelled || run.Result == nil || run.Result.Write.Bytes != 1<<20 || run.Result.Read.Bytes != 1<<20 {
		t.Fatalf("partial result %+v", run)
	}
	if files := speedtestFiles(t, local.Path); len(files) != 0 {
		t.Fatalf("test files left after cancel: %v", files)
	}
}

func TestBenchmarkShutdownCancels(t *testing.T) {
	m, local := benchManager(t)
	w := newBlockingWriter(m)
	if _, err := m.StartBenchmark(context.Background(), LocalTargetID, 64, nil); err != nil {
		t.Fatal(err)
	}
	w.wait(t)
	m.shutdown() // waits for the run
	if run := m.Benchmark().Run; run.State != benchCancelled {
		t.Fatalf("after shutdown: %+v", run)
	}
	if files := speedtestFiles(t, local.Path); len(files) != 0 {
		t.Fatalf("test files left: %v", files)
	}
	_, err := m.StartBenchmark(context.Background(), LocalTargetID, 64, nil)
	wantKind(t, err, apperr.KindUnavailable)
}

func TestBenchmarkBudgets(t *testing.T) {
	m, _ := benchManager(t)
	// Slow storage: every unit of work takes a few ms, so a budget of 1 ns
	// has passed after the first one even with a coarse clock.
	m.benchLim.slow = func(context.Context, string) { time.Sleep(3 * time.Millisecond) }
	small := m.benchLim

	// Phase budgets stop after the first block or operation.
	m.benchLim.write, m.benchLim.meta = time.Nanosecond, time.Nanosecond
	run := runBench(t, m, LocalTargetID, nil)
	res := run.Result
	if run.State != benchDone || res.SizeBytes != 64<<10 || res.Write.Bytes != 64<<10 || res.Read.Bytes != 64<<10 || res.Metadata.Ops != 1 {
		t.Fatalf("budgets: %+v %+v", run, res)
	}
	for _, want := range []string{"The write test stopped at its time limit (1ns) after 64.0 KiB",
		"The file operation test stopped at its time limit (1ns) after 1 operations"} {
		if !hasNote(res, want) {
			t.Errorf("no note %q in %v", want, res.Notes)
		}
	}
	m.benchLim = small
	m.benchLim.read = time.Nanosecond
	run = runBench(t, m, LocalTargetID, nil)
	if res = run.Result; res.SizeBytes != 1<<20 || res.Read.Bytes != 64<<10 || !hasNote(res, "The read test stopped at its time limit (1ns) after 64.0 KiB") {
		t.Fatalf("read budget: %+v", res)
	}

	// The whole run's budget: the write phase stops, later phases are skipped.
	m.benchLim = small
	m.benchLim.total = time.Nanosecond
	run = runBench(t, m, LocalTargetID, nil)
	res = run.Result
	if run.State != benchDone || res.Write.Bytes != 64<<10 || res.Read.Bytes != 0 || res.Metadata.Ops != 0 {
		t.Fatalf("total budget: %+v", res)
	}
	for _, want := range []string{"The write test stopped at the time limit of the whole test (1ns)",
		"The read test was skipped", "The cached content test was skipped", "The file operation test was skipped"} {
		if !hasNote(res, want) {
			t.Errorf("no note %q in %v", want, res.Notes)
		}
	}
}

// TestBenchmarkRemovesStaleFiles: leftovers of a crashed run are removed from
// the store root and tmp/; nothing else is touched. Without tmp/ (an
// uninitialised target) the test file goes to the root.
func TestBenchmarkRemovesStaleFiles(t *testing.T) {
	m, local := benchManager(t)
	const stale = "picache-speedtest-0123456789abcdef.tmp"
	mkdir(t, filepath.Join(local.Path, "tmp")) // created by the first guard check or store open
	keep := []string{
		filepath.Join(local.Path, "tmp", "picache-speedtest-xyz.tmp"),
		filepath.Join(local.Path, "tmp", "0123456789abcdef.tmp"),
		filepath.Join(local.Path, "picache-speedtest-0123456789abcdef.tmp.keep"),
	}
	for _, p := range append(slices.Clone(keep), filepath.Join(local.Path, "tmp", stale), filepath.Join(local.Path, "."+stale)) {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	dirLike := mkdir(t, filepath.Join(local.Path, "tmp", "picache-speedtest-fedcba9876543210.tmp"))
	keep = append(keep, dirLike)
	if run := runBench(t, m, LocalTargetID, nil); run.State != benchDone {
		t.Fatalf("run %+v", run)
	}
	for _, p := range keep {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s was removed: %v", p, err)
		}
	}
	if files := speedtestFiles(t, local.Path); len(files) != 3 { // the names that do not match and the directory
		t.Fatalf("speed test files: %v", files)
	}

	// An uninitialised target has no tmp/: the test file lives in the root.
	dir := mkdir(t, filepath.Join(m.cfg.MountRoot, "new"))
	tg, err := m.Create(context.Background(), localInput(dir))
	if err != nil {
		t.Fatal(err)
	}
	w := newBlockingWriter(m)
	if _, err := m.StartBenchmark(context.Background(), tg.ID, 64, nil); err != nil {
		t.Fatal(err)
	}
	w.wait(t)
	files := speedtestFiles(t, dir)
	if len(files) != 1 || !speedtestRE.MatchString(filepath.Base(files[0])) || !strings.HasPrefix(filepath.Base(files[0]), ".") ||
		filepath.Dir(files[0]) != dir {
		t.Fatalf("test file of an uninitialised target: %v", files)
	}
	close(w.release)
	if run := waitBench(t, m); run.State != benchDone {
		t.Fatalf("run %+v", run)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Fatalf("left in the root: %v", ents)
	}
}

// TestBenchmarkOpenStoreRoot: the speed test repeats the guard's location
// checks on its own handle and never follows links.
func TestBenchmarkOpenStoreRoot(t *testing.T) {
	cfg := testConfig(t)
	m := bareManager(cfg)
	dir := mkdir(t, filepath.Join(cfg.MountRoot, "disk"))
	mkdir(t, filepath.Join(dir, "sub"))
	open := func(tg Target) error {
		t.Helper()
		r, err := m.openStoreRoot(tg)
		if err == nil {
			r.Close()
		}
		return err
	}
	disk := Target{ID: testID, Kind: KindLocal, Mode: ModeExternal, Path: dir}
	if err := open(disk); err != nil {
		t.Fatal(err)
	}
	sub := disk
	sub.Subdir = "sub"
	if err := open(sub); err != nil {
		t.Fatal(err)
	}
	unmounted := disk
	unmounted.RequireMountpoint = true
	if err := open(unmounted); err == nil || !strings.Contains(err.Error(), "nothing is mounted") {
		t.Fatalf("not mounted: %v", err)
	}
	smb := disk
	smb.Kind = KindSMB
	if err := open(smb); err == nil { // a local file system (or unknown type) is no SMB share
		t.Fatal("wrong file system accepted")
	}

	if err := os.Symlink(dir, filepath.Join(cfg.MountRoot, "link")); err != nil {
		t.Skipf("symbolic links not available: %v", err)
	}
	linked := disk
	linked.Path = filepath.Join(cfg.MountRoot, "link")
	if err := open(linked); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("linked path: %v", err)
	}
	if err := os.Symlink(filepath.Join(dir, "sub"), filepath.Join(dir, "sublink")); err != nil {
		t.Fatal(err)
	}
	sub.Subdir = "sublink"
	if err := open(sub); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("linked sub-directory: %v", err)
	}

	// A linked tmp/ is not used: the test file goes to the store root.
	elsewhere := mkdir(t, filepath.Join(cfg.MountRoot, "elsewhere"))
	if err := os.Symlink(elsewhere, filepath.Join(dir, "tmp")); err != nil {
		t.Fatal(err)
	}
	j := &benchJob{m: m, t: disk}
	if j.root, _ = m.openStoreRoot(disk); j.root == nil {
		t.Fatal("cannot open the root")
	}
	defer j.root.Close()
	if err := j.openTestDir(); err != nil || j.dir != j.root || j.prefix != ".picache-speedtest-" {
		t.Fatalf("linked tmp: %v %v %q", err, j.dir == j.root, j.prefix)
	}
}

// TestBenchmarkSlices reads cached slices of a real store on the target.
func TestBenchmarkSlices(t *testing.T) {
	m, local := benchManager(t)
	ctx := context.Background()
	st, err := cachestore.Open(ctx, cachestore.Options{Root: local.Path, StoreID: local.StoreID,
		IndexPath: filepath.Join(t.TempDir(), local.StoreID+".db"), Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	// The active store without content.
	run := runBench(t, m, LocalTargetID, st)
	if run.Result.Slices != nil || !hasNote(run.Result, "there is no cached content yet") {
		t.Fatalf("empty store: %+v %v", run.Result.Slices, run.Result.Notes)
	}

	var total int64
	for i, size := range []int64{3<<20 + 5, 1 << 20, 17} {
		path := "/depot/1/" + string(rune('a'+i))
		id := cachestore.ObjectID("steam", path)
		gen, err := st.SetMeta(ctx, id, cachestore.Meta{Service: "steam", Host: "cdn.example.com", Path: path, GroupKey: "steam:1", Total: size})
		if err != nil {
			t.Fatal(err)
		}
		for idx, off := int64(0), int64(0); off < size; idx, off = idx+1, off+1<<20 {
			if err := st.WriteSlice(ctx, id, gen, idx, make([]byte, min(1<<20, size-off))); err != nil {
				t.Fatal(err)
			}
		}
		total += size
	}
	if _, err := st.Evict(ctx, cachestore.Policy{}); err != nil { // flushes the index
		t.Fatal(err)
	}
	run = runBench(t, m, LocalTargetID, st)
	sl := run.Result.Slices
	if run.State != benchDone || sl == nil || sl.Count != 6 || !measured(sl.Throughput, total) || sl.P50Ms < 0 || sl.P95Ms < sl.P50Ms {
		t.Fatalf("slices %+v, notes %v", sl, run.Result.Notes)
	}

	// Fewer samples than slices.
	m.benchLim.sample = 2
	if sl := runBench(t, m, LocalTargetID, st).Result.Slices; sl == nil || sl.Count != 2 {
		t.Fatalf("sample of 2: %+v", sl)
	}

	// Another target is not the active store, even with the store passed.
	dir := mkdir(t, filepath.Join(m.cfg.MountRoot, "other"))
	tg, err := m.Create(ctx, localInput(dir))
	if err != nil {
		t.Fatal(err)
	}
	if run := runBench(t, m, tg.ID, st); run.Result.Slices != nil || !hasNote(run.Result, "not the active cache store") {
		t.Fatalf("other target: %+v", run.Result)
	}
	// A closed store stops the phase.
	st.Close()
	if run := runBench(t, m, LocalTargetID, st); run.State != benchDone || run.Result.Slices != nil ||
		!hasNote(run.Result, "Cached content was not read") {
		t.Fatalf("closed store: %+v", run.Result)
	}
}

// TestBenchmarkJSON pins the JSON member names the UI relies on.
func TestBenchmarkJSON(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	tp := Throughput{Bytes: 1, Seconds: 2, BytesPerSec: 3}
	run := BenchmarkRun{State: benchDone, TargetID: "local", SizeMiB: 64, Phase: phaseDone, Progress: 1, StartedAt: now, FinishedAt: now,
		Error: "x", Result: &BenchmarkResult{TargetID: "local", FSType: "ext4", StoreRoot: "/cache", TestedAt: now, SizeBytes: 1,
			FreeBytes: 2, Write: tp, Read: ReadThroughput{Throughput: tp, CacheDropped: true},
			Slices:   &SliceReads{Throughput: tp, Count: 1, P50Ms: 1, P95Ms: 2},
			Metadata: MetadataOps{Ops: 32, P50Ms: 1, P95Ms: 2, MaxMs: 3}, Notes: []string{"n"}}}
	b, err := json.Marshal(BenchmarkOverview{Run: &run, Last: map[string]BenchmarkResult{"local": *run.Result}})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	keys := func(v any) []string { return slices.Sorted(maps.Keys(v.(map[string]any))) }
	want := func(what string, v any, names ...string) {
		t.Helper()
		slices.Sort(names)
		if k := keys(v); !slices.Equal(k, names) {
			t.Errorf("%s members %v, want %v", what, k, names)
		}
	}
	want("overview", got, "run", "last")
	r := got["run"].(map[string]any)
	want("run", r, "state", "targetId", "sizeMiB", "phase", "progress", "startedAt", "finishedAt", "error", "result")
	res := r["result"].(map[string]any)
	want("result", res, "targetId", "fsType", "storeRoot", "testedAt", "sizeBytes", "freeBytes", "write", "read", "slices", "metadata", "notes")
	want("write", res["write"], "bytes", "seconds", "bytesPerSec")
	want("read", res["read"], "bytes", "seconds", "bytesPerSec", "cacheDropped")
	want("slices", res["slices"], "bytes", "seconds", "bytesPerSec", "count", "p50Ms", "p95Ms")
	want("metadata", res["metadata"], "ops", "p50Ms", "p95Ms", "maxMs")
	want("last", got["last"], "local")
	if r["startedAt"] != "2026-09-25T12:00:00Z" {
		t.Errorf("startedAt %v", r["startedAt"])
	}

	// Optional members are left out; notes and last are never null.
	b, err = json.Marshal(BenchmarkOverview{Run: &BenchmarkRun{State: benchRunning, Phase: phasePrepare, StartedAt: now}, Last: map[string]BenchmarkResult{}})
	if err != nil {
		t.Fatal(err)
	}
	if s := string(b); !strings.Contains(s, `"last":{}`) || strings.Contains(s, "finishedAt") || strings.Contains(s, "error") ||
		strings.Contains(s, "result") {
		t.Errorf("running run: %s", s)
	}
	b, _ = json.Marshal(BenchmarkResult{Notes: []string{}})
	if s := string(b); !strings.Contains(s, `"notes":[]`) || strings.Contains(s, "slices") {
		t.Errorf("result without slices: %s", s)
	}
	b, _ = json.Marshal(BenchmarkOverview{Last: map[string]BenchmarkResult{}})
	if string(b) != `{"last":{}}` {
		t.Errorf("empty overview: %s", b)
	}
}

func TestBenchmarkHelpers(t *testing.T) {
	ms := func(n int) time.Duration { return time.Duration(n) * time.Millisecond }
	var d []time.Duration
	for i := 20; i >= 1; i-- {
		d = append(d, ms(i))
	}
	if p50, p95, maxMs := percentiles(d); p50 != 10 || p95 != 19 || maxMs != 20 {
		t.Fatalf("percentiles = %v %v %v", p50, p95, maxMs)
	}
	if p50, p95, maxMs := percentiles([]time.Duration{1500 * time.Microsecond}); p50 != 1.5 || p95 != 1.5 || maxMs != 1.5 {
		t.Fatalf("one sample: %v %v %v", p50, p95, maxMs)
	}
	if tp := throughput(100<<20, 2*time.Second); tp.Bytes != 100<<20 || tp.Seconds != 2 || tp.BytesPerSec != 50<<20 {
		t.Fatalf("throughput %+v", tp)
	}
	if tp := throughput(0, 0); tp.BytesPerSec != 0 {
		t.Fatalf("empty throughput %+v", tp)
	}
	for phase, want := range map[string]float64{phasePrepare: 0.01, phaseWrite: 0.22, phaseCleanup: 0.985, phaseDone: 1} {
		if got := phaseProgress(phase, 0.5); got != want {
			t.Errorf("progress(%s, 0.5) = %v", phase, got)
		}
	}
	b := make([]byte, 3*4096)
	varyBlock(b, 7)
	c := slices.Clone(b)
	varyBlock(c, 8)
	for off := 0; off < len(b); off += 4096 {
		if slices.Equal(b[off:off+4096], c[off:off+4096]) {
			t.Fatalf("page %d equal in two blocks", off/4096)
		}
	}
	if fmtBudget(50*time.Second) != "50 s" || fmtBudget(time.Nanosecond) != "1ns" {
		t.Fatal("fmtBudget")
	}
}
