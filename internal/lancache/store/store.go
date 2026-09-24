// Package cachestore is the on-disk slice store with its local SQLite index,
// retention/eviction and verify/rebuild (docs/ARCHITECTURE.md 9).
package cachestore

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/hustenreizjuengling/picache/internal/listing"
)

var errNotImplemented = errors.New("cachestore: not implemented")

// ErrSliceMissing is returned by ReadSlice when a slice is not cached.
var ErrSliceMissing = errors.New("cachestore: slice not cached")

// Options configure a store.
type Options struct {
	Root          string // store root directory (local path or NAS mountpoint)
	IndexPath     string // <data>/cache-index/<storeID>.db (local disk)
	StoreID       string // expected store id (from the storage target)
	SliceSize     int64  // used when the store is created; an existing store keeps its own
	IOConcurrency int    // max concurrent filesystem operations (default 64)
	Log           *slog.Logger
}

// Meta is the object metadata recorded with the first slice.
type Meta struct {
	Service  string
	Host     string
	Path     string // normalised, no query
	GroupKey string
	Total    int64       // full object size in bytes
	Header   http.Header // stored end-to-end response headers (Content-Type, Last-Modified, …)
	NoSlice  bool        // object was fetched without range support
}

// Object is an index entry.
type Object struct {
	ID          string      `json:"id"`
	Service     string      `json:"service"`
	Host        string      `json:"host"`
	Path        string      `json:"path"`
	GroupKey    string      `json:"groupKey"`
	Total       int64       `json:"total"`
	SliceSize   int64       `json:"sliceSize"`
	ContentType string      `json:"contentType"`
	Header      http.Header `json:"-"`
	CreatedAt   time.Time   `json:"createdAt"`
	LastAccess  time.Time   `json:"lastAccess"`
	Hits        int64       `json:"hits"`
	BytesServed int64       `json:"bytesServed"`
	CachedBytes int64       `json:"cachedBytes"`
	SliceCount  int         `json:"sliceCount"`  // slices present
	SlicesTotal int         `json:"slicesTotal"` // ceil(total/sliceSize)
	Pinned      bool        `json:"pinned"`
	NoSlice     bool        `json:"noSlice"`
	ExpiresAt   time.Time   `json:"expiresAt,omitzero"` // lastAccess + retention (set by queries)
}

// SliceReader reads one cached slice.
type SliceReader interface {
	io.ReaderAt
	Size() int64
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
	MaxAge        time.Duration            // inactive retention
	ServiceMaxAge map[string]time.Duration // per-service overrides
	MaxBytes      int64                    // 0 = unlimited
	MinFreeBytes  int64
	FreeBytes     func() (uint64, error) // statfs free space of the store root
}

// EvictResult reports one eviction run.
type EvictResult struct {
	Objects int64            `json:"objects"`
	Bytes   int64            `json:"bytes"`
	Reasons map[string]int64 `json:"reasons"` // reason → objects
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
	Service      string    `json:"service"`
	GroupKey     string    `json:"groupKey"`
	Objects      int64     `json:"objects"`
	CachedBytes  int64     `json:"cachedBytes"`
	TotalBytes   int64     `json:"totalBytes"` // sum of object sizes (complete-ness = cached/total)
	BytesServed  int64     `json:"bytesServed"`
	Hits         int64     `json:"hits"`
	FirstCached  time.Time `json:"firstCached"`
	LastAccess   time.Time `json:"lastAccess"`
	ExpiresAt    time.Time `json:"expiresAt,omitzero"`
	EvictionRisk float64   `json:"evictionRisk"` // 0..1 LRU percentile (1 = next to go)
	Pinned       bool      `json:"pinned"`       // all objects pinned
}

// GroupQuery filters/sorts groups. Sort: "bytes" (default), "lastAccess", "firstCached", "served", "name".
type GroupQuery struct {
	Service string
	Search  string
	Sort    string
	Desc    bool
	Limit   int // default 50, max 500
	Offset  int
	Retention func(service string) time.Duration // to compute ExpiresAt; nil = none
}

// ObjectQuery filters objects. Sort: "lastAccess" (default), "size", "created", "path".
type ObjectQuery struct {
	Service   string
	GroupKey  string
	Search    string
	Sort      string
	Desc      bool
	Limit     int
	Offset    int
	Retention func(service string) time.Duration
}

// Store is safe for concurrent use.
type Store struct {
	opt Options
}

// ObjectID derives the object id from service and normalised path.
func ObjectID(service, path string) string { return "" }

// Open opens or creates the store at opt.Root. It writes .picache-store on
// first use and fails if an existing marker has a different store id.
func Open(ctx context.Context, opt Options) (*Store, error) { return nil, errNotImplemented }

// Close flushes access statistics and closes the index.
func (s *Store) Close() error { return nil }

// ID returns the store id.
func (s *Store) ID() string { return s.opt.StoreID }

// Root returns the store root path.
func (s *Store) Root() string { return s.opt.Root }

// SliceSize returns the store's slice size.
func (s *Store) SliceSize() int64 { return s.opt.SliceSize }

// Lookup returns the object metadata (in-memory, hot path).
func (s *Store) Lookup(id string) (Object, bool) { return Object{}, false }

// HasSlice reports whether slice idx of object id is cached (in-memory).
func (s *Store) HasSlice(id string, idx int64) bool { return false }

// ReadSlice opens a cached slice. Returns ErrSliceMissing if not cached; a
// corrupt/mismatched file is deleted and reported as missing.
func (s *Store) ReadSlice(id string, idx int64) (SliceReader, error) { return nil, ErrSliceMissing }

// SetMeta creates or updates the object record. A different Total than the
// recorded one invalidates existing slices.
func (s *Store) SetMeta(ctx context.Context, id string, m Meta) error { return errNotImplemented }

// WriteSlice stores slice idx (temp → close → rename → index).
func (s *Store) WriteSlice(ctx context.Context, id string, idx int64, data []byte) error {
	return errNotImplemented
}

// Touch records an access (in memory, flushed periodically).
func (s *Store) Touch(id string, bytesServed int64) {}

// Invalidate deletes all slices of an object and its record.
func (s *Store) Invalidate(ctx context.Context, id string, reason string) error {
	return errNotImplemented
}

// OnEvict registers a callback for every removed object (eviction, purge, corruption).
func (s *Store) OnEvict(fn func(o Object, reason string)) {}

// Usage returns totals.
func (s *Store) Usage() Usage { return Usage{} }

// Evict applies retention and size limits once.
func (s *Store) Evict(ctx context.Context, p Policy) (EvictResult, error) {
	return EvictResult{}, errNotImplemented
}

// Verify scans the slice tree and reconciles it with the index. With repair
// it fixes the index and deletes corrupt files; otherwise it only reports.
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

// DeleteObject purges one object.
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
func (s *Store) SetPinned(ctx context.Context, id string, pinned bool) error { return errNotImplemented }

// SetGroupPinned pins/unpins all objects of a group.
func (s *Store) SetGroupPinned(ctx context.Context, service, groupKey string, pinned bool) error {
	return errNotImplemented
}
