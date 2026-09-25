// Package logs owns logs.db: the DNS query log, cache request events,
// download sessions, SNI pass-through events, evictions and the statistics
// rollups that power the dashboard (docs/ARCHITECTURE.md section 11).
//
// Responsibilities:
//   - Producers call Log* for every event unless the client's
//     Identity.IgnoreLogs is true. This package anonymises client IPs (if
//     enabled) before storage and before the live feed, and with
//     QueryLogEnabled=false still updates rollups but stores no query rows.
//   - Ingestion never blocks: events go into bounded channels; a single
//     writer goroutine batch-inserts every 5 s or 5000 rows, updates rollups
//     and fans events out to live subscribers (buffer 256, dropped when full).
//   - Rollups: dns_minute / dns_hourly (counts by status class), cache_minute
//     / cache_hourly (bytes by service), dns_top_hourly (kind domain |
//     blocked | client | upstream, top 1000 keys per hour and kind) and
//     cache_top_hourly (kind client | content). Top/ClientStats/ServiceStats/
//     Summary read only rollups. Minute rollups are kept 48 h, hourly ones
//     settings.Logs.StatsRetentionDays.
//   - Retention: raw tables by the configured hours/days, and logs.db is kept
//     below settings.Logs.MaxDBSizeMiB by pruning the oldest entries of the
//     raw events, download sessions and hourly top lists, each shortened by
//     the same share of its retention (never the last hour; the small count
//     rollups are kept); raw inserts pause while the data dir has < 1 GiB
//     free. Freed pages are returned to the filesystem in small
//     incremental-vacuum steps so the WAL stays small.
//   - Queries run with a 10 s timeout behind a semaphore of 2; substring
//     searches need ≥ 3 characters; series requests with more than 1500
//     points are rejected (apperr.Invalid).
//   - Migrations never rewrite large tables at start; an incompatible change
//     drops and recreates the table.
//
// Tables (logs.db, component "logs"): logs_queries, logs_cache_requests,
// logs_downloads, logs_sni, logs_evictions, logs_dns_minute, logs_dns_hourly,
// logs_cache_minute, logs_cache_hourly, logs_dns_top_hourly,
// logs_cache_top_hourly.
//
// Implementation notes:
//   - Count rollups (minute/hour) are upserted with every batch. Cache and
//     SNI bytes are spread over the minutes/hours a transfer spanned, so
//     throughput charts do not spike when a long transfer ends.
//   - The hourly top tables are kept in memory for the current hour (bounded
//     key sets per kind), checkpointed every 10 minutes, at the end of the
//     hour and at shutdown (top 1000 per kind). Queries merge the in-memory
//     current hour, so top lists are live.
//   - Summary, series and service statistics read the minute rollups for
//     ranges that start within the last 48 h and the hourly ones otherwise;
//     top lists and client statistics read the hourly top tables. The start
//     of a range is aligned down to that resolution (series: to the step;
//     Summary.TopFrom reports the start of the top lists).
//   - Live events carry a per-process sequence number (Seq) because their
//     database IDs are assigned only when the batch is written.
//   - Anonymisation masks IPv4 to /16 and IPv6 to /48 and also drops client
//     names (a name identifies a client as well as its address).
//   - While the query log is disabled no query rows are stored and the live
//     query feed stays silent; statistics are still counted.
package logs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/listing"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// MaxSubscribers bounds concurrent live feeds (all kinds together).
const MaxSubscribers = 16

const (
	flushInterval = 5 * time.Second // batch writer cadence
	batchRows     = 5000            // flush early at this many buffered raw rows
	liveBuffer    = 256             // per live subscriber; slow subscribers lose events

	// Ingestion queue capacities; events beyond are dropped and counted.
	queryQueue    = 16384
	cacheQueue    = 8192
	sniQueue      = 2048
	evictionQueue = 8192

	sessionGap    = 120 * time.Second // a new download session starts after this gap
	sessionActive = 30 * time.Second  // a session is active if its last request is this recent

	minFreeDiskBytes = 1 << 30 // raw inserts pause below this much free space on the data disk
)

// QueryEvent is one DNS query.
type QueryEvent struct {
	ID         int64     `json:"id"`           // set when read back
	Seq        uint64    `json:"seq,omitzero"` // live feed only: positive, unique per process (IDs are assigned later)
	Time       time.Time `json:"time"`
	ClientIP   string    `json:"clientIp"`
	ClientName string    `json:"clientName,omitempty"`
	QName      string    `json:"qname"`            // lower-case, no trailing dot
	QType      string    `json:"qtype"`            // "A", "AAAA", "HTTPS", "TYPE65534"
	Status     string    `json:"status"`           // dnsserver status strings (ARCHITECTURE 7.1)
	RCode      string    `json:"rcode"`            // "NOERROR", "NXDOMAIN", …
	Reason     string    `json:"reason,omitempty"` // list name, rule pattern or special-domain name
	ListID     int64     `json:"listId,omitempty"`
	RuleID     int64     `json:"ruleId,omitempty"`
	Service    string    `json:"service,omitempty"` // download service for status "override"
	Upstream   string    `json:"upstream,omitempty"`
	DurationUs int64     `json:"durationUs"`
	Answer     string    `json:"answer,omitempty"` // compact summary, max 256 chars
	DNSSEC     bool      `json:"dnssec,omitempty"` // AD flag set
	Protocol   string    `json:"protocol"`         // udp | tcp
}

// CacheEvent is one client request to the HTTP cache.
type CacheEvent struct {
	ID          int64     `json:"id"`
	Seq         uint64    `json:"seq,omitzero"` // live feed only: positive, unique per process (IDs are assigned later)
	Time        time.Time `json:"time"`         // request start
	ClientIP    string    `json:"clientIp"`
	ClientName  string    `json:"clientName,omitempty"`
	Service     string    `json:"service"`
	Host        string    `json:"host"`
	Path        string    `json:"path"` // never includes the query string
	Method      string    `json:"method"`
	Status      int       `json:"status"`
	CacheStatus string    `json:"cacheStatus"` // HIT | MISS | PARTIAL | BYPASS | PASS (not cacheable) | ERROR
	Range       string    `json:"range,omitempty"`
	BytesSent   int64     `json:"bytesSent"`
	BytesHit    int64     `json:"bytesHit"`    // served from disk
	BytesWAN    int64     `json:"bytesWan"`    // fetched upstream for this request
	BytesStored int64     `json:"bytesStored"` // written to the store by this request's fills
	DurationMs  int64     `json:"durationMs"`
	GroupKey    string    `json:"groupKey"`
	Label       string    `json:"label,omitempty"`
	UserAgent   string    `json:"userAgent,omitempty"` // truncated to 256 chars
}

// SNIEvent is one finished pass-through connection.
type SNIEvent struct {
	ID         int64     `json:"id"`
	Time       time.Time `json:"time"` // connection start
	ClientIP   string    `json:"clientIp"`
	ClientName string    `json:"clientName,omitempty"`
	SNI        string    `json:"sni"`
	Service    string    `json:"service"`
	BytesUp    int64     `json:"bytesUp"`
	BytesDown  int64     `json:"bytesDown"`
	DurationMs int64     `json:"durationMs"`
}

// EvictionEvent records a removed cache object.
type EvictionEvent struct {
	ID       int64     `json:"id"`
	Time     time.Time `json:"time"`
	StoreID  string    `json:"storeId"`
	ObjectID string    `json:"objectId"`
	Service  string    `json:"service"`
	GroupKey string    `json:"groupKey"`
	Bytes    int64     `json:"bytes"`
	Reason   string    `json:"reason"` // inactive | size | min-free | manual | corrupt | invalidated
}

// QueryFilter selects query-log entries. Zero values mean "no filter".
// Default time range: last hour. Results are newest first.
type QueryFilter struct {
	From, To time.Time
	Clients  []string // any of (ORed, at most 256): IP (exact) or name substring (≥ 3 chars)
	Domain   string   // substring (≥ 3 chars); "\"exact\"" for exact match
	Status   []string // any of
	QType    string
	Upstream string
	Cursor   string // opaque, from QueryPage.Next
	Limit    int    // default 100, max 1000
}

// QueryPage is a cursor page of the query log.
type QueryPage = listing.Page[QueryEvent]

// Summary are dashboard totals for a time range (from rollups).
type Summary struct {
	From             time.Time `json:"from"`
	To               time.Time `json:"to"`
	DNSQueries       int64     `json:"dnsQueries"`
	DNSBlocked       int64     `json:"dnsBlocked"`
	DNSCached        int64     `json:"dnsCached"`
	DNSDownloadCache int64     `json:"dnsDownloadCache"`
	DNSForwarded     int64     `json:"dnsForwarded"`
	BlockedPercent   float64   `json:"blockedPercent"`
	AvgDNSDurationUs int64     `json:"avgDnsDurationUs"`
	CacheRequests    int64     `json:"cacheRequests"`
	CacheBytesSent   int64     `json:"cacheBytesSent"`
	CacheBytesHit    int64     `json:"cacheBytesHit"`
	CacheBytesWAN    int64     `json:"cacheBytesWan"`
	CacheBytesStored int64     `json:"cacheBytesStored"`
	EvictedBytes     int64     `json:"evictedBytes"`
	ByteHitRatio     float64   `json:"byteHitRatio"` // hit / (hit + wan), 0..1
	SNIBytes         int64     `json:"sniBytes"`
	// ActiveClients counts the distinct clients with DNS queries or cache
	// requests in [TopFrom, To); ActiveDownloads counts download sessions
	// whose last request was within the last 30 s (independent of the range).
	ActiveClients    int64  `json:"activeClients"`
	ActiveDownloads  int64  `json:"activeDownloads"`
	DroppedLogEvents uint64 `json:"droppedLogEvents"`
	// TopFrom is where the hourly top tables start for this range: From
	// aligned down to the full hour. Top lists, client statistics and
	// ActiveClients cover [TopFrom, To), up to an hour more than the range
	// (a 15-minute range at 10:05 covers 09:00–10:05).
	TopFrom time.Time `json:"topFrom"`
}

// Series is a time series set aligned on Timestamps (unix seconds, bucket
// start). DNS keys are disjoint and sum to all queries: "allowed"
// (forwarded, stale, local, special), "cached", "override", "blocked" (all
// blocked-*), "other" (refused, error). Cache keys (bytes): "hit", "wan", "sni".
type Series struct {
	Step       int64                `json:"step"` // seconds
	Timestamps []int64              `json:"timestamps"`
	Values     map[string][]float64 `json:"values"`
}

// TopKind selects a top list.
type TopKind string

const (
	TopDomains        TopKind = "domains"       // most queried allowed domains
	TopBlockedDomains TopKind = "blocked"       // most blocked domains
	TopClients        TopKind = "clients"       // by query count
	TopCacheClients   TopKind = "cache-clients" // by cache bytes sent
	TopContent        TopKind = "content"       // cache groups by bytes sent
	TopUpstreams      TopKind = "upstreams"     // by query count (Bytes = avg duration µs)
)

// TopItem is one entry of a top list.
type TopItem struct {
	Key   string `json:"key"`
	Label string `json:"label,omitempty"`
	Count int64  `json:"count"`
	Bytes int64  `json:"bytes,omitempty"`
	Extra string `json:"extra,omitempty"` // e.g. service for content
	// Addresses are the client addresses of a device (most active first)
	// when the clients or cache-clients list is grouped by device (API
	// ?group=device; Key is then the most active address).
	Addresses []string `json:"addresses,omitempty"`
}

// Download is a download session: requests of one client for one content
// group without a gap longer than 120 s.
type Download struct {
	ID         int64     `json:"id"`
	ClientIP   string    `json:"clientIp"`
	ClientName string    `json:"clientName,omitempty"`
	Service    string    `json:"service"`
	GroupKey   string    `json:"groupKey"`
	Label      string    `json:"label"`
	FirstSeen  time.Time `json:"firstSeen"`
	LastSeen   time.Time `json:"lastSeen"`
	Requests   int64     `json:"requests"`
	BytesSent  int64     `json:"bytesSent"`
	BytesHit   int64     `json:"bytesHit"`
	BytesWAN   int64     `json:"bytesWan"`
	Active     bool      `json:"active"` // last request within 30 s
}

// DownloadFilter selects download sessions (newest first).
type DownloadFilter struct {
	From, To   time.Time
	Client     string
	Service    string
	GroupKey   string
	Search     string // label/group substring (≥ 3 chars)
	ActiveOnly bool
	Limit      int // default 50, max 500
	Offset     int
}

// EventFilter selects raw cache/SNI/eviction events (newest first).
type EventFilter struct {
	From, To time.Time
	Client   string
	Service  string
	Search   string // host/path/sni substring (≥ 3 chars)
	Status   string // cache status or eviction reason
	Cursor   string
	Limit    int // default 100, max 1000
}

// ServiceStat aggregates cache and pass-through traffic per service.
type ServiceStat struct {
	Service        string `json:"service"`
	Requests       int64  `json:"requests"`
	BytesSent      int64  `json:"bytesSent"`
	BytesHit       int64  `json:"bytesHit"`
	BytesWAN       int64  `json:"bytesWan"`
	SNIConnections int64  `json:"sniConnections"`
	SNIBytes       int64  `json:"sniBytes"`
}

// ClientStat aggregates per client address, or per device when grouped by
// the API (?group=device: ClientIP is then the most recently active
// address and the counters are summed).
type ClientStat struct {
	ClientIP   string `json:"clientIp"`
	ClientName string `json:"clientName,omitempty"`
	// ClientID and MAC describe the device of the address (filled in by the
	// API from the clients registry): the configured client (0 if none) and
	// the MAC ("" if unknown).
	ClientID int64  `json:"clientId,omitzero"`
	MAC      string `json:"mac,omitempty"`
	// Addresses are the addresses the row covers: [ClientIP], or every
	// address of the device in the range (most recent first) when grouped.
	Addresses     []string  `json:"addresses"`
	Queries       int64     `json:"queries"`
	Blocked       int64     `json:"blocked"`
	CacheBytes    int64     `json:"cacheBytes"`
	CacheHitBytes int64     `json:"cacheHitBytes"`
	LastSeen      time.Time `json:"lastSeen"`
}

// GroupClient is one client that downloaded a content group.
type GroupClient struct {
	ClientIP   string    `json:"clientIp"`
	ClientName string    `json:"clientName,omitempty"`
	Sessions   int64     `json:"sessions"`
	BytesSent  int64     `json:"bytesSent"`
	LastSeen   time.Time `json:"lastSeen"`
}

// GroupRef identifies a content group.
type GroupRef struct {
	Service  string
	GroupKey string
}

// Metrics are internal counters (for /metrics and health).
type Metrics struct {
	Dropped     uint64    `json:"dropped"`
	QueueLength int       `json:"queueLength"`
	LastFlush   time.Time `json:"lastFlush,omitzero"`
	DBSizeBytes int64     `json:"dbSizeBytes"`
	Disabled    string    `json:"disabled,omitempty"`  // set for the Discard store
	LiveDropped uint64    `json:"liveDropped"`         // live-feed events not delivered to slow subscribers
	TopOverflow uint64    `json:"topOverflow"`         // events not counted in the hourly top lists (key limit of the hour reached)
	RawPaused   bool      `json:"rawPaused,omitempty"` // raw inserts paused: the data disk has < 1 GiB free
}

// Store is the logs.db owner.
type Store struct {
	d        *db.DB
	set      *settings.Store
	log      *slog.Logger
	disabled string // reason; set only for the Discard store

	queries   chan QueryEvent
	cache     chan CacheEvent
	sni       chan SNIEvent
	evictions chan EvictionEvent

	dropped   atomic.Uint64
	pending   atomic.Int64 // raw rows buffered in the writer
	lastFlush atomic.Int64 // unix ms of the last successful batch
	dbSize    atomic.Int64
	paused    atomic.Bool // raw inserts paused (data disk low)
	started   atomic.Bool
	closed    atomic.Bool

	sem  chan struct{} // bounds concurrent read queries
	live hub
	top  topSet
	w    *writer // state owned by the Start goroutine

	autoVacuum bool // logs.db uses incremental auto-vacuum
	// diskFree reports the free bytes of the filesystem holding dir.
	diskFree func(dir string) (uint64, bool)
}

// New opens the logs component on d (logs.db, opened by the app) and migrates.
func New(ctx context.Context, d *db.DB, set *settings.Store, log *slog.Logger) (*Store, error) {
	if d == nil || d.W == nil || d.R == nil {
		return nil, errors.New("logs: database is not open for writing")
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	s := &Store{
		d:         d,
		set:       set,
		log:       log.With(slog.String("component", "logs")),
		queries:   make(chan QueryEvent, queryQueue),
		cache:     make(chan CacheEvent, cacheQueue),
		sni:       make(chan SNIEvent, sniQueue),
		evictions: make(chan EvictionEvent, evictionQueue),
		sem:       make(chan struct{}, queryConcurrency),
		diskFree:  diskFree,
	}
	av, err := enableAutoVacuum(ctx, d)
	if err != nil {
		return nil, fmt.Errorf("logs: auto-vacuum: %w", err)
	}
	s.autoVacuum = av
	if err := d.Migrate(ctx, "logs", migrations); err != nil {
		return nil, err
	}
	now := time.Now()
	if err := s.loadTop(ctx, hourStart(now.UnixMilli())); err != nil {
		return nil, fmt.Errorf("logs: load hourly top lists: %w", err)
	}
	s.w = newWriter(s, now)
	if _, _, err := s.refreshSize(ctx); err != nil {
		return nil, fmt.Errorf("logs: database size: %w", err)
	}
	return s, nil
}

// Discard returns a Store that drops all events and answers queries with
// apperr.Unavailable (used when logs.db cannot be opened; DNS keeps working).
func Discard(reason string, log *slog.Logger) *Store {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if reason == "" {
		reason = "logs.db unavailable"
	}
	s := &Store{log: log.With(slog.String("component", "logs")), disabled: reason}
	s.live.close()
	return s
}

// Start runs the batch writer, rollups and retention pruning until ctx ends.
// It blocks until ctx is done and pending events are flushed.
func (s *Store) Start(ctx context.Context) {
	if s.disabled != "" || !s.started.CompareAndSwap(false, true) {
		<-ctx.Done()
		return
	}
	defer func() {
		// The app closes logs.db after Start returns: refuse new queries
		// and wait for running ones (each ends within queryTimeout).
		s.closed.Store(true)
		s.live.close()
		for range cap(s.sem) {
			s.sem <- struct{}{}
		}
	}()
	s.w.run(ctx)
}

// Close closes the store (after Start has returned).
func (s *Store) Close() error {
	s.closed.Store(true)
	s.live.close()
	return nil
}

// LogQuery enqueues a query event (non-blocking).
func (s *Store) LogQuery(e QueryEvent) {
	if s.queries == nil {
		return
	}
	select {
	case s.queries <- e:
	default:
		s.dropped.Add(1)
	}
}

// LogCache enqueues a cache request event (non-blocking). Also feeds
// download sessions and rollups.
func (s *Store) LogCache(e CacheEvent) {
	if s.cache == nil {
		return
	}
	select {
	case s.cache <- e:
	default:
		s.dropped.Add(1)
	}
}

// LogSNI enqueues a pass-through event (non-blocking).
func (s *Store) LogSNI(e SNIEvent) {
	if s.sni == nil {
		return
	}
	select {
	case s.sni <- e:
	default:
		s.dropped.Add(1)
	}
}

// LogEviction enqueues an eviction event (non-blocking).
func (s *Store) LogEviction(e EvictionEvent) {
	if s.evictions == nil {
		return
	}
	select {
	case s.evictions <- e:
	default:
		s.dropped.Add(1)
	}
}

// SubscribeQueries returns a live feed of query events matching filter (nil
// = all) and a cancel func. apperr.TooMany beyond MaxSubscribers.
func (s *Store) SubscribeQueries(filter func(QueryEvent) bool) (<-chan QueryEvent, func(), error) {
	if s.disabled == "" && !s.cfg().QueryLogEnabled {
		return nil, func() {}, apperr.Unavailable("the query log is disabled in the log settings")
	}
	return subscribe(&s.live, &s.live.queries, filter)
}

// SubscribeCache returns a live feed of cache events.
func (s *Store) SubscribeCache(filter func(CacheEvent) bool) (<-chan CacheEvent, func(), error) {
	return subscribe(&s.live, &s.live.cache, filter)
}

// Metrics returns internal counters.
func (s *Store) Metrics() Metrics {
	m := Metrics{Dropped: s.dropped.Load(), Disabled: s.disabled}
	if s.disabled != "" {
		return m
	}
	m.QueueLength = len(s.queries) + len(s.cache) + len(s.sni) + len(s.evictions) + int(s.pending.Load())
	if ms := s.lastFlush.Load(); ms != 0 {
		m.LastFlush = time.UnixMilli(ms).UTC()
	}
	m.DBSizeBytes = s.dbSize.Load()
	m.LiveDropped = s.live.dropped.Load()
	m.TopOverflow = s.top.overflow.Load()
	m.RawPaused = s.paused.Load()
	return m
}

// defaultLogs is used when no settings store is wired (tests, tools).
var defaultLogs = settings.Defaults().Logs

// cfg returns the current log settings.
func (s *Store) cfg() settings.Logs {
	if s.set == nil {
		return defaultLogs
	}
	return s.set.Get().Logs
}

// dataDir is the directory holding logs.db (checked for free space).
func (s *Store) dataDir() string { return filepath.Dir(s.d.Path) }
