// Package cachestore is the on-disk slice store with its local SQLite index,
// retention/eviction and verify/rebuild (docs/ARCHITECTURE.md 9).
//
// Memory model: nothing is loaded at Open. Hot-path lookups (Head, HasSlice)
// read the index DB by primary key through a bounded LRU of compact entries
// (default 65 536). Access statistics accumulate in a dirty map and are
// flushed every 30 s. Index writes from SetMeta/WriteSlice are batched (one
// transaction every 250 ms or 512 rows). Until a change is committed its
// entry lives in an in-memory overlay (bounded by the pending-write limit),
// so Head/HasSlice/ReadSlice see every change immediately while listings may
// lag by one batch. A `store_groups` aggregate table is maintained in the
// same transactions so Groups()/Services() never scan all objects.
//
// Generations are allocated from a store-wide counter (persisted in
// store_meta), so a generation is never reused for an object, not even after
// the object was deleted and cached again.
//
// Locking: per-object stripe locks serialise changes of one object (the
// rename of a slice file happens under it); removals install a tombstone
// first, so no new slice of a removed object can appear while its files are
// deleted. Files that could not be deleted within the caller's deadline and
// the files of a discarded generation go to a background remover, which
// deletes a file under the object lock only while the object does not
// reference that slice. Evict never removes an object in use (Use). Lock
// order: I/O semaphore → stripe lock → leaf mutexes.
//
// Tables (index DB, component "cachestore"): store_objects, store_slices,
// store_groups, store_pinned_groups, store_meta.
//
// Concurrency and lifecycle: all methods are safe for concurrent use. Close
// may run concurrently with any other method: it marks the store closed,
// cancels running Evict/Verify, waits up to 10 s for in-flight writes,
// flushes statistics and closes the index. Afterwards every method returns
// ErrClosed (Head/HasSlice report "not present"); SliceReaders opened before
// Close stay readable until closed. Nothing panics.
//
// Hostile content: the store root may be a NAS. All file access goes through
// os.Root; slice headers are validated (see ARCHITECTURE 9.1) and anything
// inconsistent is treated as corrupt.
package cachestore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
)

var (
	// ErrSliceMissing is returned by ReadSlice when a slice is not cached.
	ErrSliceMissing = errors.New("cachestore: slice not cached")
	// ErrStale is returned when the object changed (new generation) or was
	// removed since the caller read its head. The proxy aborts a response
	// whose headers were already sent.
	ErrStale = errors.New("cachestore: object changed")
	// ErrClosed is returned after Close. The proxy treats it like a miss and
	// continues the request as pass-through.
	ErrClosed = errors.New("cachestore: store closed")
)

// MaxTotal is the largest object PiCache caches (larger ones are passed through).
const MaxTotal int64 = 1 << 40 // 1 TiB

// Options configure a store.
type Options struct {
	Root          string // store root directory (local path or NAS mountpoint), already initialised (InitRoot)
	IndexPath     string // <data>/cache-index/<storeID>.db (local disk)
	StoreID       string // expected store id (must match the marker)
	IOConcurrency int    // max concurrent filesystem operations (default 64)
	LRUEntries    int    // head cache size (default 65536)
	Log           *slog.Logger
	// GroupKey derives the content group of an object rebuilt from its slice
	// headers by Verify (services.GroupFor(...).Key). Default: "<service>:<host>".
	GroupKey func(service, host, path string) string
}

// Meta is the object metadata recorded before the first slice is written.
type Meta struct {
	Service  string
	Host     string
	Path     string // canonical path, no query
	GroupKey string
	Total    int64       // full object size in bytes (≤ MaxTotal)
	Header   http.Header // filtered response headers (ARCHITECTURE 8.2 step 10), ≤ 4 KiB
	NoSlice  bool        // object was fetched without range support
}

// ObjectHead is the compact hot-path view of an object.
type ObjectHead struct {
	ID           string
	Gen          uint64 // incremented whenever the object is (re)created or its total changes
	Total        int64
	SliceSize    int64
	ContentType  string
	LastModified string
	Header       http.Header // stored headers to replay (already filtered)
	NoSlice      bool
	Present      []uint64 // bitmap of cached slices (bit i = slice i)
}

// Has reports whether slice idx is present according to the head.
func (h *ObjectHead) Has(idx int64) bool {
	if idx < 0 || idx/64 >= int64(len(h.Present)) {
		return false
	}
	return h.Present[idx/64]&(1<<uint(idx%64)) != 0
}

// Object is a full index entry (listings, UI).
type Object struct {
	ID          string    `json:"id"`
	Service     string    `json:"service"`
	Host        string    `json:"host"`
	Path        string    `json:"path"`
	GroupKey    string    `json:"groupKey"`
	Total       int64     `json:"total"`
	SliceSize   int64     `json:"sliceSize"`
	ContentType string    `json:"contentType"`
	CreatedAt   time.Time `json:"createdAt"`
	LastAccess  time.Time `json:"lastAccess"`
	Hits        int64     `json:"hits"`
	BytesServed int64     `json:"bytesServed"`
	CachedBytes int64     `json:"cachedBytes"`
	SliceCount  int64     `json:"sliceCount"`  // slices present
	SlicesTotal int64     `json:"slicesTotal"` // ceil(total/sliceSize)
	Pinned      bool      `json:"pinned"`
	NoSlice     bool      `json:"noSlice"`
	ExpiresAt   time.Time `json:"expiresAt,omitzero"` // lastAccess + retention (filled by queries)
}

// SliceReader reads one cached slice. Offsets and Size refer to slice data
// only (the file header is excluded). File reads hold the IO semaphore for
// their own duration only, never while waiting for w.
type SliceReader interface {
	io.ReaderAt
	Size() int64
	// WriteRange copies n bytes starting at off to w. While a stream slot
	// is free it uses the underlying *os.File so that net/http can use
	// sendfile(2) when w is the unwrapped http.ResponseWriter (or
	// implements io.ReaderFrom); otherwise it copies through a small
	// buffer (see sliceReader.WriteRange).
	WriteRange(w io.Writer, off, n int64) (int64, error)
	Close() error
}

// Usage summarises the store.
type Usage struct {
	StoreID     string `json:"storeId"`
	Objects     int64  `json:"objects"`
	Slices      int64  `json:"slices"`
	CachedBytes int64  `json:"cachedBytes"`
	SliceSize   int64  `json:"sliceSize"`
}

// Policy drives Evict.
type Policy struct {
	MaxAge   time.Duration // inactive retention
	MaxBytes int64         // 0 = unlimited
	// MinFreeBytes is the effective minimum free space. FreeBytes is a sample
	// up to 30 s old; Evict calls it once per run, computes
	// deficit = MinFreeBytes*105/100 − free and deletes LRU unpinned objects
	// until the sum of their CachedBytes ≥ deficit. An error skips the rule.
	MinFreeBytes int64
	FreeBytes    func() (uint64, error)
}

// EvictResult reports one eviction run.
type EvictResult struct {
	Objects int64            `json:"objects"`
	Bytes   int64            `json:"bytes"`
	Reasons map[string]int64 `json:"reasons"` // reason → objects
	Full    bool             `json:"full"`    // limits could not be met (everything left is pinned)
}

// VerifyProgress is reported during Verify.
type VerifyProgress struct {
	FilesScanned int64 `json:"filesScanned"`
	BytesScanned int64 `json:"bytesScanned"`
	Added        int64 `json:"added"`
	Removed      int64 `json:"removed"`
	Corrupt      int64 `json:"corrupt"`
}

// VerifyResult is the outcome of Verify.
type VerifyResult struct {
	VerifyProgress
	Duration time.Duration `json:"duration"`
}

// ServiceUsage aggregates cached content per service.
type ServiceUsage struct {
	Service     string    `json:"service"`
	Objects     int64     `json:"objects"`
	Groups      int64     `json:"groups"`
	CachedBytes int64     `json:"cachedBytes"`
	BytesServed int64     `json:"bytesServed"`
	LastAccess  time.Time `json:"lastAccess,omitzero"`
}

// GroupUsage aggregates cached content per content group.
type GroupUsage struct {
	Service     string    `json:"service"`
	GroupKey    string    `json:"groupKey"`
	Objects     int64     `json:"objects"`
	CachedBytes int64     `json:"cachedBytes"`
	TotalBytes  int64     `json:"totalBytes"` // sum of object sizes (completeness = cached/total)
	BytesServed int64     `json:"bytesServed"`
	Hits        int64     `json:"hits"`
	FirstCached time.Time `json:"firstCached"`
	LastAccess  time.Time `json:"lastAccess"`
	ExpiresAt   time.Time `json:"expiresAt,omitzero"`
	Pinned      bool      `json:"pinned"` // group-level pin (new objects inherit it)
}

// GroupQuery filters/sorts groups. Sort: "bytes" (default), "lastAccess",
// "firstCached", "served", "name" (= group key).
type GroupQuery struct {
	Service    string
	GroupKey   string   // exact match (detail view)
	Search     string   // substring of the group key
	SearchKeys []string // with Search: a group also matches if its key is listed (label search via services.SearchLabels)
	Sort       string
	Desc       bool
	Limit      int // default 50, max 500
	Offset     int
	Retention  time.Duration // to compute ExpiresAt (0 = none)
}

// ObjectQuery filters objects. Sort: "lastAccess" (default), "size", "created", "path".
type ObjectQuery struct {
	Service   string
	GroupKey  string
	Search    string // path substring
	Sort      string
	Desc      bool
	Limit     int // default 50, max 500
	Offset    int
	Retention time.Duration
}

// Store is safe for concurrent use.
type Store struct {
	opt       Options
	log       *slog.Logger
	root      *os.Root
	db        *db.DB
	sliceSize int64
	groupKey  func(service, host, path string) string

	ctx       context.Context // cancelled when Close starts (aborts Evict, Verify and waits)
	cancel    context.CancelFunc
	state     atomic.Int64  // number of in-flight calls, | closedBit once Close started
	idle      chan struct{} // signalled when the last in-flight call leaves after Close started
	closeOnce sync.Once
	closeErr  error

	sem     chan struct{} // filesystem I/O semaphore
	streams chan struct{} // zero-copy stream slots of SliceReader.WriteRange
	locks   [numStripes]sync.Mutex
	heads   *heads
	gen     atomic.Uint64 // last allocated generation (store-wide, never reused while open)
	usage   usageCounters

	pendMu      sync.Mutex
	pending     []indexOp
	flushMu     sync.Mutex
	kick        chan struct{}
	stopFlush   chan struct{}
	flusherDone chan struct{}

	statsMu sync.Mutex
	stats   map[string]*statDelta

	dirMu sync.Mutex
	dirs  map[string]struct{} // shard directories known to exist (≤ 65 792 by construction)

	retryMu sync.Mutex
	retry   map[retryRemoval]struct{} // failed removals (≤ maxRetries)

	remMu   sync.Mutex
	rem     map[string]*removalQueue // slice files queued for the background remover
	remKick chan struct{}

	useMu sync.Mutex
	inUse map[string]int // objects being served (Use), skipped by Evict

	evictSem  chan struct{}
	verifySem chan struct{}

	cbMu    sync.Mutex
	onEvict []func(Object, string)

	errMu     sync.Mutex
	lastErr   string
	lastErrAt time.Time
}

const (
	numStripes      = 1024           // per-object lock stripes
	closedBit       = int64(1) << 62 // in state: Close has started
	closeWait       = 10 * time.Second
	defaultIO       = 64
	defaultLRU      = 65536
	lruBytesPerSlot = 512 // LRU byte budget per entry slot (bounds memory for large headers/bitmaps)
	maxCallbacks    = 16
)

var errInvalidID = apperr.Invalid("id", "malformed object id")

// Open opens the store at opt.Root. The root must have been initialised with
// InitRoot and its marker must carry opt.StoreID. If the index DB cannot be
// opened or migrated it is moved aside, a fresh index is created and a
// background Verify(repair) rebuilds it from the slice headers.
func Open(ctx context.Context, opt Options) (*Store, error) {
	if !ValidStoreID(opt.StoreID) {
		return nil, errors.New("cachestore: invalid store id")
	}
	if opt.Root == "" || opt.IndexPath == "" {
		return nil, errors.New("cachestore: root and index path are required")
	}
	if opt.IOConcurrency <= 0 {
		opt.IOConcurrency = defaultIO
	}
	opt.IOConcurrency = min(opt.IOConcurrency, 1024)
	if opt.LRUEntries <= 0 {
		opt.LRUEntries = defaultLRU
	}
	opt.LRUEntries = min(max(opt.LRUEntries, 16), 1<<22)
	if opt.Log == nil {
		opt.Log = slog.Default()
	}
	log := opt.Log.With(slog.String("component", "cachestore"))

	m, err := ReadMarker(opt.Root)
	if err != nil {
		return nil, err
	}
	if m.StoreID != opt.StoreID {
		return nil, fmt.Errorf("cachestore: store marker has id %s, expected %s", m.StoreID, opt.StoreID)
	}
	root, err := os.OpenRoot(opt.Root)
	if err != nil {
		return nil, err
	}
	for _, d := range []string{"tmp", "slices"} {
		if err := root.Mkdir(d, 0o750); err != nil && !errors.Is(err, fs.ErrExist) {
			_ = root.Close()
			return nil, fmt.Errorf("cachestore: create %s: %w", d, err)
		}
	}
	idx, err := openIndex(ctx, opt.IndexPath, opt.StoreID, m.SliceSize)
	rebuild := false
	if err != nil {
		if _, serr := os.Stat(opt.IndexPath); serr != nil {
			_ = root.Close()
			return nil, fmt.Errorf("cachestore: open index: %w", err)
		}
		log.Warn("cache index is unusable; moving it aside and rebuilding it from the slice files", slog.Any("err", err))
		if merr := moveAside(opt.IndexPath); merr != nil {
			_ = root.Close()
			return nil, fmt.Errorf("cachestore: move broken index aside: %w", errors.Join(err, merr))
		}
		if idx, err = openIndex(ctx, opt.IndexPath, opt.StoreID, m.SliceSize); err != nil {
			_ = root.Close()
			return nil, fmt.Errorf("cachestore: create index: %w", err)
		}
		rebuild = true
	}

	s := &Store{
		opt:         opt,
		log:         log,
		root:        root,
		db:          idx,
		sliceSize:   m.SliceSize,
		groupKey:    opt.GroupKey,
		idle:        make(chan struct{}, 1),
		sem:         make(chan struct{}, opt.IOConcurrency),
		streams:     make(chan struct{}, opt.IOConcurrency),
		heads:       newHeads(opt.LRUEntries, int64(opt.LRUEntries)*lruBytesPerSlot),
		kick:        make(chan struct{}, 1),
		stopFlush:   make(chan struct{}),
		flusherDone: make(chan struct{}),
		stats:       map[string]*statDelta{},
		dirs:        map[string]struct{}{},
		retry:       map[retryRemoval]struct{}{},
		rem:         map[string]*removalQueue{},
		remKick:     make(chan struct{}, 1),
		inUse:       map[string]int{},
		evictSem:    make(chan struct{}, 1),
		verifySem:   make(chan struct{}, 1),
	}
	if s.groupKey == nil {
		s.groupKey = func(service, host, _ string) string { return service + ":" + host }
	}
	var gen int64
	err = idx.W.QueryRowContext(ctx, `SELECT num FROM store_meta WHERE key = 'gen'`).Scan(&gen)
	if err == nil {
		err = s.usage.load(ctx, idx.W)
	}
	if err != nil {
		_ = idx.Close()
		_ = root.Close()
		return nil, fmt.Errorf("cachestore: read index: %w", err)
	}
	s.gen.Store(uint64(gen))
	s.ctx, s.cancel = context.WithCancel(context.Background())

	go s.flusher()
	go s.remover()
	s.enter() // the background task counts as in-flight; Close cancels it
	go s.background(rebuild)
	return s, nil
}

// background removes stale temp files and, after an index reset, rebuilds
// the index from the slice files.
func (s *Store) background(rebuild bool) {
	defer s.leave()
	s.cleanTmp(s.ctx, time.Hour)
	if !rebuild {
		return
	}
	res, err := s.verify(s.ctx, true, nil)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			s.log.Error("index rebuild failed", slog.Any("err", err))
		}
		return
	}
	s.log.Info("index rebuilt from slice files", slog.Int64("files", res.FilesScanned),
		slog.Int64("added", res.Added), slog.Int64("corrupt", res.Corrupt), slog.Duration("duration", res.Duration))
}

// enter registers an in-flight call; false once the store is closed.
func (s *Store) enter() bool {
	if s.state.Add(1)&closedBit != 0 {
		s.leave()
		return false
	}
	return true
}

func (s *Store) leave() {
	if s.state.Add(-1) == closedBit {
		select {
		case s.idle <- struct{}{}:
		default:
		}
	}
}

func (s *Store) closed() bool { return s.state.Load()&closedBit != 0 }

// opCtx derives a context that is also cancelled when Close starts.
func (s *Store) opCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.ctx, cancel)
	return ctx, func() { stop(); cancel() }
}

// Close: see the package documentation.
func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		s.state.Or(closedBit)
		s.cancel()
		timer := time.NewTimer(closeWait)
		defer timer.Stop()
	wait:
		for s.state.Load() != closedBit {
			select {
			case <-s.idle:
			case <-timer.C:
				s.log.Warn("closing cache store with operations still running", slog.Int64("inflight", s.state.Load()&^closedBit))
				break wait
			}
		}
		close(s.stopFlush)
		<-s.flusherDone
		if n := s.queuedRemovals(); n > 0 {
			s.log.Warn("closing cache store with slice file removals pending; Verify with repair removes the files",
				slog.Int("slices", n))
		}
		s.closeErr = errors.Join(s.db.Close(), s.root.Close())
	})
	return s.closeErr
}

// ID returns the store id.
func (s *Store) ID() string { return s.opt.StoreID }

// Root returns the store root path.
func (s *Store) Root() string { return s.opt.Root }

// SliceSize returns the store's slice size (from its marker).
func (s *Store) SliceSize() int64 { return s.sliceSize }

// lockFor returns the lock stripe of a (valid) object id.
func (s *Store) lockFor(id string) *sync.Mutex {
	h := 0
	for i := range 3 {
		c := int(id[i])
		if c >= 'a' {
			c -= 'a' - 10
		} else {
			c -= '0'
		}
		h = h<<4 | c
	}
	return &s.locks[h%numStripes]
}

// lookup returns the current entry of id (nil if unknown). Tombstones are
// returned as they are (busy != nil).
func (s *Store) lookup(ctx context.Context, id string) (*entry, error) {
	if e := s.heads.get(id); e != nil {
		return e, nil
	}
	mu := s.lockFor(id)
	mu.Lock()
	defer mu.Unlock()
	return s.lookupLocked(ctx, id)
}

// lookupLocked is lookup with the object's stripe lock held, so loading
// from the index and caching cannot race with a change of the object.
func (s *Store) lookupLocked(ctx context.Context, id string) (*entry, error) {
	if e := s.heads.get(id); e != nil {
		return e, nil
	}
	e, err := s.loadEntry(ctx, id)
	if err != nil || e == nil {
		return nil, err
	}
	s.heads.setClean(id, e)
	return e, nil
}

// Head returns the compact object view (LRU → index DB). ok=false if unknown.
func (s *Store) Head(ctx context.Context, id string) (h ObjectHead, ok bool, err error) {
	if !s.enter() {
		return ObjectHead{}, false, ErrClosed
	}
	defer s.leave()
	if !ValidObjectID(id) {
		return ObjectHead{}, false, errInvalidID
	}
	e, err := s.lookup(ctx, id)
	if err != nil || e == nil || e.busy != nil {
		return ObjectHead{}, false, err
	}
	return ObjectHead{
		ID:           id,
		Gen:          e.gen,
		Total:        e.total,
		SliceSize:    s.sliceSize,
		ContentType:  e.header.Get("Content-Type"),
		LastModified: e.header.Get("Last-Modified"),
		Header:       e.header.Clone(),
		NoSlice:      e.noSlice,
		Present:      slices.Clone(e.present),
	}, true, nil
}

// HasSlice reports whether slice idx of object id (generation gen) is cached.
func (s *Store) HasSlice(ctx context.Context, id string, gen uint64, idx int64) bool {
	if !s.enter() {
		return false
	}
	defer s.leave()
	if !ValidObjectID(id) {
		return false
	}
	e, err := s.lookup(ctx, id)
	return err == nil && e != nil && e.busy == nil && e.gen == gen && e.has(idx)
}

// Touch records an access (in memory, flushed periodically): the last
// access time always, and a hit with bytesServed bytes served from the
// cache when bytesServed > 0.
func (s *Store) Touch(id string, bytesServed int64) {
	if s.closed() || !ValidObjectID(id) {
		return
	}
	now := time.Now().UnixMilli()
	s.statsMu.Lock()
	d, ok := s.stats[id]
	if !ok {
		if len(s.stats) >= maxDirtyStats {
			s.statsMu.Unlock()
			s.kickFlush()
			return
		}
		d = &statDelta{}
		s.stats[id] = d
	}
	if bytesServed > 0 {
		d.hits++
		d.bytes += bytesServed
	}
	d.last = max(d.last, now)
	n := len(s.stats)
	s.statsMu.Unlock()
	if n >= statsSoftLimit {
		s.kickFlush()
	}
}

// touched reports whether id has unflushed access statistics, i.e. it was
// used after the statistics were last written.
func (s *Store) touched(id string) bool {
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	_, ok := s.stats[id]
	return ok
}

// Use marks object id as being served until release is called: Evict
// never removes an object in use (manual removals and invalidation do).
// release is idempotent and safe after Close.
func (s *Store) Use(id string) (release func()) {
	s.useMu.Lock()
	s.inUse[id]++
	s.useMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.useMu.Lock()
			if n := s.inUse[id] - 1; n > 0 {
				s.inUse[id] = n
			} else {
				delete(s.inUse, id)
			}
			s.useMu.Unlock()
		})
	}
}

// used reports whether id is in use (Use).
func (s *Store) used(id string) bool {
	s.useMu.Lock()
	defer s.useMu.Unlock()
	return s.inUse[id] > 0
}

// OnEvict registers a callback for every removed object (eviction, purge,
// corruption, invalidation). Called outside locks.
func (s *Store) OnEvict(fn func(o Object, reason string)) {
	if fn == nil {
		return
	}
	s.cbMu.Lock()
	defer s.cbMu.Unlock()
	if len(s.onEvict) < maxCallbacks {
		s.onEvict = append(s.onEvict, fn)
	}
}

func (s *Store) notifyRemoved(objs []Object, reason string) {
	if len(objs) == 0 {
		return
	}
	s.cbMu.Lock()
	cbs := slices.Clone(s.onEvict)
	s.cbMu.Unlock()
	for _, o := range objs {
		for _, fn := range cbs {
			fn(o, reason)
		}
	}
}

// Usage returns totals (from the aggregate table; cheap).
func (s *Store) Usage() Usage {
	return Usage{
		StoreID:     s.opt.StoreID,
		Objects:     s.usage.objects.Load(),
		Slices:      s.usage.slices.Load(),
		CachedBytes: s.usage.cached.Load(),
		SliceSize:   s.sliceSize,
	}
}

// acquireIO takes a slot of the filesystem I/O semaphore. An ended ctx
// fails even if a slot is free.
func (s *Store) acquireIO(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case s.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Store) releaseIO() { <-s.sem }
