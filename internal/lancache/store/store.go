// Package cachestore is the on-disk slice store with its local SQLite index,
// retention/eviction and verify/rebuild (docs/ARCHITECTURE.md 9).
//
// Memory model: nothing is loaded at Open. Hot-path lookups (Head, HasSlice)
// read the index DB by primary key through a bounded LRU of compact entries
// (default 65 536). Access statistics accumulate in a dirty map and are
// flushed every 30 s. Index writes from SetMeta/WriteSlice are batched (one
// transaction every 250 ms or 512 rows). A `store_groups` aggregate table is
// maintained in the same transactions so Groups()/Services() never scan all
// objects.
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
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/hustenreizjuengling/picache/internal/listing"
)

var errNotImplemented = errors.New("cachestore: not implemented")

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

var objectIDRE = regexp.MustCompile(`^[0-9a-f]{32}$`)

// ValidObjectID reports whether id is a well-formed object id.
func ValidObjectID(id string) bool { return objectIDRE.MatchString(id) }

// Options configure a store.
type Options struct {
	Root          string // store root directory (local path or NAS mountpoint), already initialised (InitRoot)
	IndexPath     string // <data>/cache-index/<storeID>.db (local disk)
	StoreID       string // expected store id (must match the marker)
	IOConcurrency int    // max concurrent filesystem operations (default 64)
	LRUEntries    int    // head cache size (default 65536)
	Log           *slog.Logger
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
	return h.Present[idx/64]&(1<<(uint(idx)%64)) != 0
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
// only (the file header is excluded). Each call acquires the IO semaphore for
// its own duration only.
type SliceReader interface {
	io.ReaderAt
	Size() int64
	// WriteRange copies n bytes starting at off to w. It uses the underlying
	// *os.File so that net/http can use sendfile(2) when w is the unwrapped
	// http.ResponseWriter (or implements io.ReaderFrom).
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
	opt Options
}

// ObjectID derives the object id: first 32 hex chars of SHA-256(service + "\x00" + path).
func ObjectID(service, path string) string { return "" }

// Open opens the store at opt.Root. The root must have been initialised with
// InitRoot and its marker must carry opt.StoreID. If the index DB cannot be
// opened or migrated it is moved aside, a fresh index is created and a
// background Verify(repair) rebuilds it from the slice headers.
func Open(ctx context.Context, opt Options) (*Store, error) { return nil, errNotImplemented }

// Close: see the package documentation.
func (s *Store) Close() error { return nil }

// ID returns the store id.
func (s *Store) ID() string { return s.opt.StoreID }

// Root returns the store root path.
func (s *Store) Root() string { return s.opt.Root }

// SliceSize returns the store's slice size (from its marker).
func (s *Store) SliceSize() int64 { return 1 << 20 }

// Head returns the compact object view (LRU → index DB). ok=false if unknown.
func (s *Store) Head(ctx context.Context, id string) (h ObjectHead, ok bool, err error) {
	return ObjectHead{}, false, errNotImplemented
}

// HasSlice reports whether slice idx of object id (generation gen) is cached.
func (s *Store) HasSlice(ctx context.Context, id string, gen uint64, idx int64) bool { return false }

// ReadSlice opens a cached slice of generation gen. ErrSliceMissing if not
// cached, ErrStale if the object changed; a corrupt/mismatched file is
// deleted and reported as missing.
func (s *Store) ReadSlice(ctx context.Context, id string, gen uint64, idx int64) (SliceReader, error) {
	return nil, ErrSliceMissing
}

// SetMeta creates or updates the object record and returns its generation.
// A new record or a different Total increments the generation and discards
// existing slices. New objects of a pinned group are pinned.
func (s *Store) SetMeta(ctx context.Context, id string, m Meta) (gen uint64, err error) {
	return 0, errNotImplemented
}

// WriteSlice stores slice idx of generation gen (temp → close → rename →
// index). It returns ErrStale if the record is gone or gen differs, and an
// error unless len(data) == min(SliceSize, Total−idx·SliceSize). crc32c of
// data is recorded in the slice header.
func (s *Store) WriteSlice(ctx context.Context, id string, gen uint64, idx int64, data []byte) error {
	return errNotImplemented
}

// Touch records an access (in memory, flushed periodically).
func (s *Store) Touch(id string, bytesServed int64) {}

// Invalidate deletes all slices of an object and its record.
func (s *Store) Invalidate(ctx context.Context, id string, reason string) error {
	return errNotImplemented
}

// OnEvict registers a callback for every removed object (eviction, purge,
// corruption, invalidation). Called outside locks.
func (s *Store) OnEvict(fn func(o Object, reason string)) {}

// Usage returns totals (from the aggregate table; cheap).
func (s *Store) Usage() Usage { return Usage{} }

// Evict applies retention and size limits once. Calls are serialised; a
// concurrent call waits and then runs its own pass.
func (s *Store) Evict(ctx context.Context, p Policy) (EvictResult, error) {
	return EvictResult{}, errNotImplemented
}

// Verify scans the slice tree and reconciles it with the index (checks
// headers, sizes and crc32c). With repair it fixes the index and deletes
// corrupt files; otherwise it only reports.
func (s *Store) Verify(ctx context.Context, repair bool, progress func(VerifyProgress)) (VerifyResult, error) {
	return VerifyResult{}, errNotImplemented
}

// Services aggregates per service.
func (s *Store) Services(ctx context.Context) ([]ServiceUsage, error) { return nil, errNotImplemented }

// Groups lists content groups.
func (s *Store) Groups(ctx context.Context, q GroupQuery) (listing.Page[GroupUsage], error) {
	return listing.Page[GroupUsage]{}, errNotImplemented
}

// Objects lists objects.
func (s *Store) Objects(ctx context.Context, q ObjectQuery) (listing.Page[Object], error) {
	return listing.Page[Object]{}, errNotImplemented
}

// DeleteObject purges one object (apperr.Invalid for a malformed id).
func (s *Store) DeleteObject(ctx context.Context, id string) error { return errNotImplemented }

// DeleteGroup purges a content group; returns bytes freed.
func (s *Store) DeleteGroup(ctx context.Context, service, groupKey string) (int64, error) {
	return 0, errNotImplemented
}

// DeleteService purges all content of a service; returns bytes freed.
func (s *Store) DeleteService(ctx context.Context, service string) (int64, error) {
	return 0, errNotImplemented
}

// SetPinned pins/unpins one object (pinned objects are never evicted).
func (s *Store) SetPinned(ctx context.Context, id string, pinned bool) error {
	return errNotImplemented
}

// SetGroupPinned pins/unpins a group persistently (existing and future objects).
func (s *Store) SetGroupPinned(ctx context.Context, service, groupKey string, pinned bool) error {
	return errNotImplemented
}
