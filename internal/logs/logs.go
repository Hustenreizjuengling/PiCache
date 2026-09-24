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
//     below settings.Logs.MaxDBSizeMiB by pruning the oldest raw events; raw
//     inserts pause while the data dir has < 1 GiB free.
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
package logs

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/listing"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

var errNotImplemented = errors.New("logs: not implemented")

// MaxSubscribers bounds concurrent live feeds (all kinds together).
const MaxSubscribers = 16

// QueryEvent is one DNS query.
type QueryEvent struct {
	ID         int64     `json:"id"` // set when read back
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
	Service    string    `json:"service,omitempty"` // LanCache service for status "lancache"
	Upstream   string    `json:"upstream,omitempty"`
	DurationUs int64     `json:"durationUs"`
	Answer     string    `json:"answer,omitempty"` // compact summary, max 256 chars
	DNSSEC     bool      `json:"dnssec,omitempty"` // AD flag set
	Protocol   string    `json:"protocol"`         // udp | tcp
}

// CacheEvent is one client request to the HTTP cache.
type CacheEvent struct {
	ID          int64     `json:"id"`
	Time        time.Time `json:"time"` // request start
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
	Client   string   // IP (exact) or name substring (≥ 3 chars)
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
	DNSLanCache      int64     `json:"dnsLancache"`
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
	ActiveClients    int64     `json:"activeClients"`
	ActiveDownloads  int64     `json:"activeDownloads"`
	DroppedLogEvents uint64    `json:"droppedLogEvents"`
}

// Series is a time series set aligned on Timestamps (unix seconds, bucket
// start). DNS keys are disjoint and sum to all queries: "allowed"
// (forwarded, stale, local, special), "cached", "lancache", "blocked" (all
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

// ClientStat aggregates per client.
type ClientStat struct {
	ClientIP      string    `json:"clientIp"`
	ClientName    string    `json:"clientName,omitempty"`
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
	Disabled    string    `json:"disabled,omitempty"` // set for the Discard store
}

// Store is the logs.db owner.
type Store struct {
	set *settings.Store
	log *slog.Logger
}

// New opens the logs component on d (logs.db, opened by the app) and migrates.
func New(ctx context.Context, d *db.DB, set *settings.Store, log *slog.Logger) (*Store, error) {
	return &Store{set: set, log: log}, nil
}

// Discard returns a Store that drops all events and answers queries with
// apperr.Unavailable (used when logs.db cannot be opened; DNS keeps working).
func Discard(reason string, log *slog.Logger) *Store { return &Store{log: log} }

// Start runs the batch writer, rollups and retention pruning until ctx ends.
// It blocks until ctx is done and pending events are flushed.
func (s *Store) Start(ctx context.Context) { <-ctx.Done() }

// Close closes the store (after Start has returned).
func (s *Store) Close() error { return nil }

// LogQuery enqueues a query event (non-blocking).
func (s *Store) LogQuery(e QueryEvent) {}

// LogCache enqueues a cache request event (non-blocking). Also feeds
// download sessions and rollups.
func (s *Store) LogCache(e CacheEvent) {}

// LogSNI enqueues a pass-through event (non-blocking).
func (s *Store) LogSNI(e SNIEvent) {}

// LogEviction enqueues an eviction event (non-blocking).
func (s *Store) LogEviction(e EvictionEvent) {}

// SubscribeQueries returns a live feed of query events matching filter (nil
// = all) and a cancel func. apperr.TooMany beyond MaxSubscribers.
func (s *Store) SubscribeQueries(filter func(QueryEvent) bool) (<-chan QueryEvent, func(), error) {
	return nil, func() {}, apperr.Unavailable("live feed not available")
}

// SubscribeCache returns a live feed of cache events.
func (s *Store) SubscribeCache(filter func(CacheEvent) bool) (<-chan CacheEvent, func(), error) {
	return nil, func() {}, apperr.Unavailable("live feed not available")
}

// QueryLog returns a page of query events.
func (s *Store) QueryLog(ctx context.Context, f QueryFilter) (QueryPage, error) {
	return QueryPage{}, errNotImplemented
}

// Summary returns dashboard totals for [from, to).
func (s *Store) Summary(ctx context.Context, from, to time.Time) (Summary, error) {
	return Summary{}, errNotImplemented
}

// DNSSeries returns DNS counts by status class per step.
func (s *Store) DNSSeries(ctx context.Context, from, to time.Time, step time.Duration) (Series, error) {
	return Series{}, errNotImplemented
}

// CacheSeries returns cache bytes (hit, wan, sni) per step, optionally for one service.
func (s *Store) CacheSeries(ctx context.Context, from, to time.Time, step time.Duration, service string) (Series, error) {
	return Series{}, errNotImplemented
}

// Top returns a top list.
func (s *Store) Top(ctx context.Context, kind TopKind, from, to time.Time, limit int) ([]TopItem, error) {
	return nil, errNotImplemented
}

// Downloads returns download sessions.
func (s *Store) Downloads(ctx context.Context, f DownloadFilter) (listing.Page[Download], error) {
	return listing.Page[Download]{}, errNotImplemented
}

// CacheRequests returns raw cache events.
func (s *Store) CacheRequests(ctx context.Context, f EventFilter) (listing.Page[CacheEvent], error) {
	return listing.Page[CacheEvent]{}, errNotImplemented
}

// SNIEvents returns pass-through events.
func (s *Store) SNIEvents(ctx context.Context, f EventFilter) (listing.Page[SNIEvent], error) {
	return listing.Page[SNIEvent]{}, errNotImplemented
}

// Evictions returns eviction events.
func (s *Store) Evictions(ctx context.Context, f EventFilter) (listing.Page[EvictionEvent], error) {
	return listing.Page[EvictionEvent]{}, errNotImplemented
}

// ServiceStats aggregates traffic per service for [from, to).
func (s *Store) ServiceStats(ctx context.Context, from, to time.Time) ([]ServiceStat, error) {
	return nil, errNotImplemented
}

// ClientStats aggregates per client for [from, to).
func (s *Store) ClientStats(ctx context.Context, from, to time.Time) ([]ClientStat, error) {
	return nil, errNotImplemented
}

// GroupClients lists the clients that downloaded a content group (within
// the session retention).
func (s *Store) GroupClients(ctx context.Context, service, groupKey string) ([]GroupClient, error) {
	return nil, errNotImplemented
}

// GroupClientCounts counts distinct clients per content group (one query for a page of groups).
func (s *Store) GroupClientCounts(ctx context.Context, refs []GroupRef) (map[GroupRef]int, error) {
	return nil, errNotImplemented
}

// Metrics returns internal counters.
func (s *Store) Metrics() Metrics { return Metrics{} }
