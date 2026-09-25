package storage

// Storage speed test (docs/ARCHITECTURE.md 10.6). It measures how fast the
// store root of a target writes and reads (a temporary test file), how fast
// cached slices of the active store are read (the hit path) and how long
// small file operations take, so the admin can tell whether the storage,
// the network or PiCache limits cache hits. One run at a time, in the
// background (not tied to the request that started it); DELETE and the
// shutdown cancel it. Results are kept in memory only.
//
// Every file operation goes through os.Root and never follows a symbolic
// link. Test files are named picache-speedtest-<16 hex>.tmp in the store's
// tmp/ (whose *.tmp files the store cleans at start), else
// .picache-speedtest-<16 hex>.tmp in the store root; leftovers of a crash
// are removed by the next run on the target. The run does not take the
// store's I/O semaphore: the storage is loaded for up to two minutes.

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sync"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	cachestore "github.com/hustenreizjuengling/picache/internal/dlcache/store"
)

// BenchmarkDefaultMiB is the test size when the request does not name one.
const BenchmarkDefaultMiB = 256

// benchSizes are the allowed test sizes in MiB.
var benchSizes = []int{64, BenchmarkDefaultMiB, 1024}

// Run states and phases.
const (
	benchRunning   = "running"
	benchDone      = "done"
	benchFailed    = "failed"
	benchCancelled = "cancelled"

	phasePrepare  = "prepare"
	phaseWrite    = "write"
	phaseRead     = "read"
	phaseSlices   = "slices"
	phaseMetadata = "metadata"
	phaseCleanup  = "cleanup"
	phaseDone     = "done"
)

// benchCancelWait bounds how long CancelBenchmark waits for the run to end.
const benchCancelWait = 5 * time.Second

// BenchmarkRun is the running or most recent speed test.
type BenchmarkRun struct {
	State      string           `json:"state"` // running | done | failed | cancelled
	TargetID   string           `json:"targetId"`
	SizeMiB    int              `json:"sizeMiB"`
	Phase      string           `json:"phase"`    // prepare | write | read | slices | metadata | cleanup | done
	Progress   float64          `json:"progress"` // 0..1 over the whole run
	StartedAt  time.Time        `json:"startedAt"`
	FinishedAt time.Time        `json:"finishedAt,omitzero"`
	Error      string           `json:"error,omitempty"`  // why it failed (user-facing)
	Result     *BenchmarkResult `json:"result,omitempty"` // when done; partial after a cancel or failure once a phase finished
}

// BenchmarkResult are the numbers of one speed test.
type BenchmarkResult struct {
	TargetID  string    `json:"targetId"`
	FSType    string    `json:"fsType"` // "" if unknown (non-Linux)
	StoreRoot string    `json:"storeRoot"`
	TestedAt  time.Time `json:"testedAt"`
	SizeBytes int64     `json:"sizeBytes"` // bytes written (less than requested when the write budget ran out)
	FreeBytes uint64    `json:"freeBytes"` // free space before the test (0 if unknown)
	// Write: sequential 1 MiB blocks; the time includes the final fsync.
	Write Throughput `json:"write"`
	// Read: sequential read of the test file after dropping it from the page cache.
	Read ReadThroughput `json:"read"`
	// Slices: reads of cached slices (the hit path); only for the active
	// store with cached content.
	Slices   *SliceReads `json:"slices,omitempty"`
	Metadata MetadataOps `json:"metadata"` // create 4 KiB + fsync + rename + delete
	Notes    []string    `json:"notes"`    // user-facing remarks (skipped phase, budget reached, …)
}

// Throughput is an amount of data moved in a time.
type Throughput struct {
	Bytes       int64   `json:"bytes"`
	Seconds     float64 `json:"seconds"`
	BytesPerSec float64 `json:"bytesPerSec"`
}

// ReadThroughput is the sequential read of the test file.
type ReadThroughput struct {
	Throughput
	CacheDropped bool `json:"cacheDropped"` // the file was dropped from the page cache first (Linux)
}

// SliceReads are the reads of cached slices; Seconds is the sum of the
// per-slice times (open → close).
type SliceReads struct {
	Throughput
	Count int     `json:"count"`
	P50Ms float64 `json:"p50Ms"`
	P95Ms float64 `json:"p95Ms"`
}

// MetadataOps are the small-file operations (per-iteration latency).
type MetadataOps struct {
	Ops   int     `json:"ops"`
	P50Ms float64 `json:"p50Ms"`
	P95Ms float64 `json:"p95Ms"`
	MaxMs float64 `json:"maxMs"`
}

// BenchmarkOverview is returned by Benchmark.
type BenchmarkOverview struct {
	Run  *BenchmarkRun              `json:"run,omitempty"` // the running or most recent run (until the next one starts)
	Last map[string]BenchmarkResult `json:"last"`          // last completed result per target
}

// benchLimits bound a run (tests shrink them).
type benchLimits struct {
	unit    int64         // bytes per requested MiB
	block   int64         // bytes per write/read call
	reserve uint64        // free space left beyond the test file
	total   time.Duration // the whole run
	write   time.Duration // phase budgets
	read    time.Duration
	slices  time.Duration
	meta    time.Duration
	sample  int // cached slices read
	metaOps int // metadata iterations
	// slow runs after every unit of work of a phase (tests: slow storage).
	slow func(ctx context.Context, phase string)
}

var defaultBenchLimits = benchLimits{
	unit: 1 << 20, block: 1 << 20, reserve: 1 << 30,
	total: 120 * time.Second, write: 50 * time.Second, read: 40 * time.Second, slices: 20 * time.Second, meta: 10 * time.Second,
	sample: 128, metaOps: 32,
}

// benchPhases are the phases in order with the progress at their start.
var benchPhases = []struct {
	name  string
	start float64
}{{phasePrepare, 0}, {phaseWrite, 0.02}, {phaseRead, 0.42}, {phaseSlices, 0.77}, {phaseMetadata, 0.90},
	{phaseCleanup, 0.97}, {phaseDone, 1}}

// phaseProgress is the progress of the whole run when frac of phase is done.
func phaseProgress(phase string, frac float64) float64 {
	for i, p := range benchPhases[:len(benchPhases)-1] {
		if p.name == phase {
			return math.Round((p.start+(benchPhases[i+1].start-p.start)*min(max(frac, 0), 1))*1000) / 1000
		}
	}
	return 1
}

// benchState is the speed test state of a Manager. Lock order: mu before
// Manager.mu.
type benchState struct {
	mu       sync.Mutex
	starting string             // target whose run is being checked (StartBenchmark)
	run      *BenchmarkRun      // the running or most recent run
	cancel   context.CancelFunc // cancels the running run
	done     chan struct{}      // closed when the running run has ended
	written  int64              // bytes of the running run's test file
	last     map[string]BenchmarkResult
}

var speedtestRE = regexp.MustCompile(`^\.?picache-speedtest-[0-9a-f]{16}\.tmp$`)

// StartBenchmark starts a speed test of target id with sizeMiB (64, 256 or
// 1024) in the background. It first runs the functional test's fresh check
// (mount guard, write test) and checks the free space (size + 1 GiB).
// active is the open cache store (nil if none): its cached slices are read
// only if it is this target's store. ctx bounds the checks, not the run.
func (m *Manager) StartBenchmark(ctx context.Context, id string, sizeMiB int, active *cachestore.Store) (BenchmarkRun, error) {
	if !slices.Contains(benchSizes, sizeMiB) {
		return BenchmarkRun{}, apperr.Invalid("sizeMiB", "must be 64, 256 or 1024")
	}
	if _, ok := m.get(id); !ok {
		return BenchmarkRun{}, apperr.NotFound("storage target", id)
	}
	b := &m.bench
	b.mu.Lock()
	if b.starting != "" || (b.run != nil && b.run.State == benchRunning) {
		b.mu.Unlock()
		return BenchmarkRun{}, apperr.Conflict("a speed test is already running")
	}
	b.starting = id
	b.mu.Unlock()
	j, err := m.prepareBench(ctx, id, sizeMiB, active)

	b.mu.Lock()
	defer b.mu.Unlock()
	b.starting = ""
	if err != nil {
		return BenchmarkRun{}, err
	}
	m.mu.Lock()
	closed := m.closed
	if !closed {
		m.wg.Add(1)
	}
	m.mu.Unlock()
	if closed { // shutdown cancels b.cancel after setting closed, so this run would never be cancelled
		return BenchmarkRun{}, errShuttingDown
	}
	runCtx, cancel := context.WithCancel(context.Background())
	b.run, b.cancel, b.done, b.written = j.run, cancel, make(chan struct{}), 0
	go func() {
		defer m.wg.Done()
		j.execute(runCtx)
	}()
	m.log.Info("storage speed test started", slog.String("target", id), slog.Int("sizeMiB", sizeMiB))
	return j.run.clone(), nil
}

// prepareBench checks the target like Test and returns the job of a new run.
func (m *Manager) prepareBench(ctx context.Context, id string, sizeMiB int, active *cachestore.Store) (*benchJob, error) {
	m.mu.Lock()
	e := m.targets[id]
	busy := e != nil && (e.busy || e.testing > 0)
	m.mu.Unlock()
	switch {
	case e == nil:
		return nil, apperr.NotFound("storage target", id)
	case busy:
		return nil, apperr.Conflict("the storage target is being initialised or tested; try again in a moment")
	}
	res, err := m.freshCheck(ctx, id, testTimeout)
	switch {
	case err != nil && (apperr.KindOf(err) != apperr.KindUnavailable || errors.Is(err, errShuttingDown)):
		return nil, err // cancelled, shutting down, deleted meanwhile
	case err != nil: // the storage does not respond
		return nil, apperr.Conflict("the storage target is not available: %s", errMessage(err))
	case !res.usable():
		reason := res.st.Reason
		if reason == "" {
			reason = "the storage is not usable"
		}
		return nil, apperr.Conflict("the storage target is not available: %s", reason)
	}
	t, ok := m.get(id)
	if !ok {
		return nil, apperr.NotFound("storage target", id)
	}
	lim := m.benchLim
	size := int64(sizeMiB) * lim.unit
	now := time.Now().UTC()
	j := &benchJob{m: m, t: t, lim: lim, size: size, active: active,
		run: &BenchmarkRun{State: benchRunning, TargetID: id, SizeMiB: sizeMiB, Phase: phasePrepare, StartedAt: now},
		res: BenchmarkResult{TargetID: id, FSType: res.st.FSType, StoreRoot: res.st.StoreRoot, TestedAt: now,
			FreeBytes: res.st.FreeBytes, Notes: []string{}}}
	if res.st.TotalBytes == 0 { // statfs is not available on this platform
		j.note("The free space could not be checked on this platform.")
	} else if need := uint64(size) + lim.reserve; res.st.FreeBytes < need {
		return nil, &apperr.Error{Kind: apperr.KindInvalid, Message: fmt.Sprintf("not enough free space: the test needs %s, %s are free",
			formatBytes(need), formatBytes(res.st.FreeBytes))}
	}
	return j, nil
}

// Benchmark returns the running or most recent run and the last completed
// result per target.
func (m *Manager) Benchmark() BenchmarkOverview {
	b := &m.bench
	b.mu.Lock()
	defer b.mu.Unlock()
	o := BenchmarkOverview{Last: make(map[string]BenchmarkResult, len(b.last))}
	if b.run != nil {
		r := b.run.clone()
		o.Run = &r
	}
	for id, res := range b.last {
		o.Last[id] = res.clone()
	}
	return o
}

// CancelBenchmark cancels the running speed test and waits (up to 5 s or
// until ctx ends) for it to stop and remove its files. It returns the
// target of the cancelled run, or false if none was running.
func (m *Manager) CancelBenchmark(ctx context.Context) (string, bool) {
	b := &m.bench
	b.mu.Lock()
	if b.run == nil || b.run.State != benchRunning {
		b.mu.Unlock()
		return "", false
	}
	id, done := b.run.TargetID, b.done
	b.cancel()
	b.mu.Unlock()
	t := time.NewTimer(benchCancelWait)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
	case <-ctx.Done():
	}
	return id, true
}

// BenchmarkBytes returns how many bytes the test file of a speed test
// running on target id occupies (eviction must not count them as used).
func (m *Manager) BenchmarkBytes(id string) uint64 {
	b := &m.bench
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.run == nil || b.run.State != benchRunning || b.run.TargetID != id {
		return 0
	}
	return uint64(b.written)
}

// benchmarkOn reports whether a speed test is being started or runs on id.
func (m *Manager) benchmarkOn(id string) bool {
	b := &m.bench
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.starting == id || (b.run != nil && b.run.State == benchRunning && b.run.TargetID == id)
}

// cancelBench cancels a running speed test without waiting (shutdown).
func (m *Manager) cancelBench() {
	b := &m.bench
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.run != nil && b.run.State == benchRunning {
		b.cancel()
	}
}

func (r BenchmarkRun) clone() BenchmarkRun {
	if r.Result != nil {
		res := r.Result.clone()
		r.Result = &res
	}
	return r
}

func (r BenchmarkResult) clone() BenchmarkResult {
	r.Notes = slices.Clone(r.Notes)
	if r.Slices != nil {
		s := *r.Slices
		r.Slices = &s
	}
	return r
}

// benchJob is one run. Its fields belong to the run's goroutine; run is
// shared and changed under Manager.bench.mu.
type benchJob struct {
	m        *Manager
	t        Target
	lim      benchLimits
	size     int64 // requested bytes
	active   *cachestore.Store
	run      *BenchmarkRun
	res      BenchmarkResult
	finished bool      // a phase finished: the result is worth keeping
	deadline time.Time // end of the whole run
	buf      []byte    // one block of non-compressible data
	root     *os.Root  // the store root
	dir      *os.Root  // where the test files go (tmp/ or root)
	prefix   string    // file name prefix in dir
	file     string    // the test file ("" until created)
}

// failure is an error with a user-facing message.
type failure struct {
	msg string
	err error
}

func (f *failure) Error() string { return f.msg }
func (f *failure) Unwrap() error { return f.err }

// fsFailure describes a file system error of a step for the admin.
func (j *benchJob) fsFailure(step string, err error) error {
	msg := step + " failed: " + errText(err)
	if hint := j.m.writeHint(err); hint != "" {
		msg += ". " + hint
	}
	return &failure{msg: msg, err: err}
}

// execute runs all phases and records the outcome.
func (j *benchJob) execute(ctx context.Context) {
	start := time.Now()
	j.deadline = start.Add(j.lim.total)
	err := j.phases(ctx)
	j.finish(ctx, err, time.Since(start))
}

func (j *benchJob) phases(ctx context.Context) error {
	root, err := j.m.openStoreRoot(j.t)
	if err != nil {
		return j.fsFailure("opening the store directory", err)
	}
	j.root = root
	defer root.Close()
	if err := j.openTestDir(); err != nil {
		return err
	}
	defer j.cleanup()
	removeStaleTestFiles(ctx, root)
	if j.dir != root {
		removeStaleTestFiles(ctx, j.dir)
	}
	j.buf = make([]byte, min(j.lim.block, j.size))
	rand.Read(j.buf) // once per run; varyBlock makes every block different

	for _, p := range []struct {
		name string
		fn   func(context.Context) error
	}{{phaseWrite, j.write}, {phaseRead, j.read}, {phaseSlices, j.readSlices}, {phaseMetadata, j.metadata}} {
		if err := ctx.Err(); err != nil {
			return err
		}
		j.progress(p.name, 0, -1)
		if p.name != phaseWrite && !time.Now().Before(j.deadline) {
			j.note(fmt.Sprintf("The %s test was skipped: the whole test reached its time limit (%s).", phaseLabel[p.name], fmtBudget(j.lim.total)))
			continue
		}
		if err := p.fn(ctx); err != nil {
			return err
		}
		j.finished = true
	}
	return nil
}

var phaseLabel = map[string]string{phaseWrite: "write", phaseRead: "read", phaseSlices: "cached content", phaseMetadata: "file operation"}

// openTestDir picks the store's tmp/ (a real directory, not a link) or the
// store root for the test files.
func (j *benchJob) openTestDir() error {
	j.dir, j.prefix = j.root, ".picache-speedtest-"
	fi, err := j.root.Lstat("tmp")
	if err != nil || !fi.IsDir() {
		return nil
	}
	d, err := j.root.OpenRoot("tmp")
	if err != nil {
		return j.fsFailure("opening the store's tmp directory", err)
	}
	if dfi, err := d.Stat("."); err != nil || !os.SameFile(fi, dfi) {
		d.Close()
		return &failure{msg: "the store's tmp directory changed while it was opened"}
	}
	j.dir, j.prefix = d, "picache-speedtest-"
	return nil
}

// cleanup removes the test file and closes tmp/ (the caller closes the root).
func (j *benchJob) cleanup() {
	j.progress(phaseCleanup, 0, -1)
	if j.file != "" {
		if err := j.dir.Remove(j.file); err != nil && !errors.Is(err, fs.ErrNotExist) {
			j.note(fmt.Sprintf("The test file could not be removed (%s); the next speed test removes it.", errText(err)))
			j.m.log.Warn("cannot remove the speed test file", slog.String("target", j.t.ID), slog.String("file", j.file), slog.Any("err", err))
		}
	}
	if j.dir != j.root {
		j.dir.Close()
	}
}

// newName returns a fresh test file name.
func (j *benchJob) newName() string {
	var b [8]byte
	rand.Read(b[:])
	return j.prefix + hex.EncodeToString(b[:]) + ".tmp"
}

// phaseDeadline is when a phase with budget d that starts now has to stop.
func (j *benchJob) phaseDeadline(d time.Duration) time.Time {
	if t := time.Now().Add(d); t.Before(j.deadline) {
		return t
	}
	return j.deadline
}

// budgetNote explains a phase stopped by its budget or the whole run's.
func (j *benchJob) budgetNote(what string, budget time.Duration, deadline time.Time, done string) {
	limit := "its time limit (" + fmtBudget(budget) + ")"
	if deadline.Equal(j.deadline) {
		limit = "the time limit of the whole test (" + fmtBudget(j.lim.total) + ")"
	}
	j.note(fmt.Sprintf("%s stopped at %s after %s; the result covers what was done.", what, limit, done))
}

func (j *benchJob) write(ctx context.Context) error {
	name := j.newName()
	f, err := j.dir.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL|oNoFollow, 0o600)
	if err != nil {
		return j.fsFailure("creating the test file", err)
	}
	j.file = name
	deadline := j.phaseDeadline(j.lim.write)
	start := time.Now()
	var written int64
	err = func() error {
		defer f.Close()
		for i := uint64(0); written < j.size; i++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			if written > 0 && !time.Now().Before(deadline) {
				j.budgetNote("The write test", j.lim.write, deadline, formatBytes(uint64(written)))
				break
			}
			b := j.buf[:min(int64(len(j.buf)), j.size-written)]
			varyBlock(b, i)
			if _, err := f.Write(b); err != nil {
				return j.fsFailure("writing the test file", err)
			}
			written += int64(len(b))
			j.progress(phaseWrite, float64(written)/float64(j.size), written)
			j.unitDone(ctx, phaseWrite)
		}
		if err := f.Sync(); err != nil {
			return j.fsFailure("syncing the test file", err)
		}
		return nil
	}()
	if err != nil {
		return err
	}
	j.res.SizeBytes = written
	j.res.Write = throughput(written, time.Since(start))
	return nil
}

// varyBlock stamps the block number into every 4 KiB of the random block,
// so no two blocks (or 4 KiB pages) are equal and deduplication cannot
// shortcut the writes; the data stays non-compressible.
func varyBlock(b []byte, n uint64) {
	for off := 0; off+8 <= len(b); off += 4096 {
		binary.LittleEndian.PutUint64(b[off:], n)
	}
}

func (j *benchJob) read(ctx context.Context) error {
	f, err := j.dir.OpenFile(j.file, os.O_RDONLY|oNoFollow, 0)
	if err != nil {
		return j.fsFailure("opening the test file", err)
	}
	defer f.Close()
	j.res.Read.CacheDropped = cachestore.DropPageCache(f)
	if !j.res.Read.CacheDropped {
		j.note("The page cache could not be dropped on this platform, so the read test may have read from memory.")
	}
	if j.t.Kind != KindLocal || isNetworkFSName(j.res.FSType) {
		j.note("A NAS may still answer reads from its own memory, so the read speed can be higher than its disks deliver. " +
			"Reading cached content uses older data and comes closer to real cache hits.")
	}
	deadline := j.phaseDeadline(j.lim.read)
	start := time.Now()
	var n int64
	for n < j.res.SizeBytes {
		if err := ctx.Err(); err != nil {
			return err
		}
		if n > 0 && !time.Now().Before(deadline) {
			j.budgetNote("The read test", j.lim.read, deadline, formatBytes(uint64(n)))
			break
		}
		k, err := io.ReadFull(f, j.buf[:min(int64(len(j.buf)), j.res.SizeBytes-n)])
		n += int64(k)
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return &failure{msg: "the test file is shorter than what was written", err: err}
		}
		if err != nil {
			return j.fsFailure("reading the test file", err)
		}
		j.progress(phaseRead, float64(n)/float64(j.res.SizeBytes), -1)
		j.unitDone(ctx, phaseRead)
	}
	j.res.Read.Throughput = throughput(n, time.Since(start))
	return nil
}

// readSlices reads up to lim.sample random cached slices of the active
// store (only when it is this target's store).
func (j *benchJob) readSlices(ctx context.Context) error {
	skip := "Cached content was not read: "
	if j.active == nil || j.t.StoreID == "" || j.active.ID() != j.t.StoreID {
		j.note(skip + "this storage target is not the active cache store.")
		return nil
	}
	deadline := j.phaseDeadline(j.lim.slices)
	sctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	refs, err := j.active.SampleSlices(sctx, j.lim.sample)
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case err != nil:
		j.note(skip + "the cache index could not be sampled (" + err.Error() + ").")
		return nil
	case len(refs) == 0:
		j.note(skip + "there is no cached content yet.")
		return nil
	}
	var lat []time.Duration
	var bytes int64
	var busy time.Duration // sum of the per-slice times
	failed, dropped := 0, true
	for i, ref := range refs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if i > 0 && !time.Now().Before(deadline) {
			j.budgetNote("Reading cached content", j.lim.slices, deadline, fmt.Sprintf("%d slices", len(lat)))
			break
		}
		start := time.Now()
		n, d, err := j.active.ReadSliceUncached(sctx, ref, j.buf)
		el := time.Since(start)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, cachestore.ErrClosed) {
			j.note("Reading cached content stopped: the cache store was closed.")
			break
		}
		if sctx.Err() != nil {
			j.budgetNote("Reading cached content", j.lim.slices, deadline, fmt.Sprintf("%d slices", len(lat)))
			break
		}
		if err != nil {
			failed++
			continue
		}
		lat = append(lat, el)
		bytes, busy = bytes+n, busy+el
		dropped = dropped && d
		j.progress(phaseSlices, float64(i+1)/float64(len(refs)), -1)
		j.unitDone(ctx, phaseSlices)
	}
	if failed > 0 {
		j.note(fmt.Sprintf("%d sampled slices could not be read (removed meanwhile or unreadable).", failed))
	}
	if len(lat) == 0 {
		j.note(skip + "no sampled slice could be read.")
		return nil
	}
	if !dropped && j.res.Read.CacheDropped {
		j.note("Some cached slices could not be dropped from the page cache and may have been read from memory.")
	}
	p50, p95, _ := percentiles(lat)
	j.res.Slices = &SliceReads{Throughput: throughput(bytes, busy), Count: len(lat), P50Ms: p50, P95Ms: p95}
	return nil
}

// metadata measures create 4 KiB + fsync + close, rename and delete.
func (j *benchJob) metadata(ctx context.Context) error {
	deadline := j.phaseDeadline(j.lim.meta)
	data := j.buf[:min(len(j.buf), 4096)]
	var lat []time.Duration
	for i := range j.lim.metaOps {
		if err := ctx.Err(); err != nil {
			return err
		}
		if i > 0 && !time.Now().Before(deadline) {
			j.budgetNote("The file operation test", j.lim.meta, deadline, fmt.Sprintf("%d operations", i))
			break
		}
		a, b := j.newName(), j.newName()
		start := time.Now()
		if err := j.fileOp(a, b, data); err != nil {
			_ = j.dir.Remove(a)
			_ = j.dir.Remove(b)
			return err
		}
		lat = append(lat, time.Since(start))
		j.progress(phaseMetadata, float64(i+1)/float64(j.lim.metaOps), -1)
		j.unitDone(ctx, phaseMetadata)
	}
	p50, p95, maxMs := percentiles(lat)
	j.res.Metadata = MetadataOps{Ops: len(lat), P50Ms: p50, P95Ms: p95, MaxMs: maxMs}
	return nil
}

func (j *benchJob) fileOp(a, b string, data []byte) error {
	f, err := j.dir.OpenFile(a, os.O_WRONLY|os.O_CREATE|os.O_EXCL|oNoFollow, 0o600)
	if err != nil {
		return j.fsFailure("creating a small file", err)
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return j.fsFailure("writing a small file", err)
	}
	if err := j.dir.Rename(a, b); err != nil {
		return j.fsFailure("renaming a small file", err)
	}
	if err := j.dir.Remove(b); err != nil {
		return j.fsFailure("deleting a small file", err)
	}
	return nil
}

// note adds a user-facing remark to the result.
func (j *benchJob) note(s string) { j.res.Notes = append(j.res.Notes, s) }

// progress publishes the phase and the fraction of it done; written ≥ 0
// updates the size of the test file.
func (j *benchJob) progress(phase string, frac float64, written int64) {
	b := &j.m.bench
	b.mu.Lock()
	defer b.mu.Unlock()
	j.run.Phase, j.run.Progress = phase, phaseProgress(phase, frac)
	if written >= 0 {
		b.written = written
	}
}

// unitDone runs the test hook after a unit of work (block, slice, operation).
func (j *benchJob) unitDone(ctx context.Context, phase string) {
	if j.lim.slow != nil {
		j.lim.slow(ctx, phase)
	}
}

// finish publishes the outcome of the run.
func (j *benchJob) finish(ctx context.Context, err error, took time.Duration) {
	m := j.m
	b := &m.bench
	b.mu.Lock()
	r := j.run
	r.Phase, r.FinishedAt = phaseDone, time.Now().UTC()
	switch {
	case err == nil:
		r.State, r.Progress = benchDone, 1
		if b.last == nil {
			b.last = map[string]BenchmarkResult{}
		}
		b.last[j.t.ID] = j.res.clone()
	case ctx.Err() != nil:
		r.State = benchCancelled
	default:
		r.State, r.Error = benchFailed, errMessage(err)
	}
	if err == nil || j.finished {
		res := j.res.clone()
		r.Result = &res
	}
	b.written = 0
	close(b.done)
	b.mu.Unlock()

	log := m.log.With(slog.String("target", j.t.ID), slog.Int("sizeMiB", r.SizeMiB), slog.Duration("duration", took.Round(time.Millisecond)))
	switch r.State {
	case benchDone:
		log.Info("storage speed test finished", slog.Float64("writeBytesPerSec", j.res.Write.BytesPerSec),
			slog.Float64("readBytesPerSec", j.res.Read.BytesPerSec))
	case benchCancelled:
		log.Info("storage speed test cancelled")
	default:
		log.Warn("storage speed test failed", slog.Any("err", err))
	}
}

// openStoreRoot opens the store root of t for the speed test and repeats
// the guard's location checks on the handle (the storage may have been
// unmounted since the check): no symbolic links below the mount root, a
// mount point where one is required, the expected file system type.
func (m *Manager) openStoreRoot(t Target) (*os.Root, error) {
	var base *os.Root
	var err error
	if t.ID == LocalTargetID {
		base, err = os.OpenRoot(t.Path) // the configured cache directory may be a link (like the guard)
	} else {
		if err := noSymlinks(m.cfg.MountRoot, t.Path); err != nil {
			return nil, err
		}
		base, err = openVerifiedDir(t.Path)
	}
	if err != nil {
		return nil, err
	}
	fi, err := base.Stat(".")
	if err == nil && t.RequireMountpoint {
		if mounted, _ := mountState(t.Path, fi); !mounted {
			err = fmt.Errorf("nothing is mounted at %s", t.Path)
		}
	}
	if err == nil {
		info, serr := statDir(base)
		if reason := fsMismatch(t, info, serr == nil); reason != "" {
			err = errors.New(reason)
		}
	}
	if err != nil {
		base.Close()
		return nil, err
	}
	if t.Subdir == "" {
		return base, nil
	}
	defer base.Close()
	sub := filepath.FromSlash(t.Subdir)
	sfi, err := base.Lstat(sub)
	if err != nil {
		return nil, err
	}
	if !sfi.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", storeRootPath(t))
	}
	r, err := base.OpenRoot(sub)
	if err != nil {
		return nil, err
	}
	if rfi, err := r.Stat("."); err != nil || !os.SameFile(sfi, rfi) {
		r.Close()
		return nil, fmt.Errorf("%s changed while it was opened", storeRootPath(t))
	}
	return r, nil
}

// removeStaleTestFiles removes test files left by a run that died (bounded).
func removeStaleTestFiles(ctx context.Context, r *os.Root) {
	d, err := r.Open(".")
	if err != nil {
		return
	}
	defer d.Close()
	for seen := 0; seen < maxScan && ctx.Err() == nil; {
		ents, err := d.ReadDir(256)
		seen += len(ents)
		for _, de := range ents {
			if !de.IsDir() && speedtestRE.MatchString(de.Name()) {
				_ = r.Remove(de.Name())
			}
		}
		if err != nil {
			return
		}
	}
}

func isNetworkFSName(fsType string) bool {
	return fsType == fsNames[magicCIFS] || fsType == fsNames[magicSMB2] || fsType == fsNames[magicNFS]
}

// throughput computes bytes per second (0 for an empty measurement).
func throughput(n int64, d time.Duration) Throughput {
	t := Throughput{Bytes: n, Seconds: math.Round(d.Seconds()*1e6) / 1e6}
	if d > 0 {
		t.BytesPerSec = math.Round(float64(n) / d.Seconds())
	}
	return t
}

// percentiles returns p50, p95 and the maximum in milliseconds (nearest rank).
func percentiles(d []time.Duration) (p50, p95, maxMs float64) {
	if len(d) == 0 {
		return 0, 0, 0
	}
	s := slices.Clone(d)
	slices.Sort(s)
	rank := func(p float64) float64 { return ms(s[int(math.Ceil(p*float64(len(s))))-1]) }
	return rank(0.5), rank(0.95), ms(s[len(s)-1])
}

func ms(d time.Duration) float64 { return math.Round(float64(d.Microseconds())) / 1000 }

// fmtBudget renders a time limit ("50 s").
func fmtBudget(d time.Duration) string {
	if d >= time.Second && d%time.Second == 0 {
		return fmt.Sprintf("%d s", d/time.Second)
	}
	return d.String()
}
