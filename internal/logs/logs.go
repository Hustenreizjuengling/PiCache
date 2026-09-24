// Package logs owns logs.db: the DNS query log, cache request events,
// download sessions, SNI pass-through events, evictions and the statistics
// rollups that power the dashboard (docs/ARCHITECTURE.md section 11).
//
// Ingestion (Log*) never blocks: events go into bounded channels and are
// batch-inserted by a background writer; when a channel is full the event is
// dropped and counted.
package logs

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/hustenreizjuengling/picache/internal/listing"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

var errNotImplemented = errors.New("logs: not implemented")

// QueryEvent is one DNS query.
type QueryEvent struct {
	ID         int64     `json:"id"` // set when read back
	Time       time.Time `json:"time"`
	ClientIP   string    `json:"clientIp"`
	ClientName string    `json:"clientName,omitempty"`
	QName      string    `json:"qname"` // lower-case, no trailing dot
	QType      string    `json:"qtype"` // "A", "AAAA", "HTTPS", "TYPE65534"
	Status     string    `json:"status"` // dnsserver status strings (ARCHITECTURE 7.1)
	RCode      string    `json:"rcode"`  // "NOERROR", "NXDOMAIN", …
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
	BytesHit    int64     `json:"bytesHit"` // served from disk
	BytesWAN    int64     `json:"bytesWan"` // fetched upstream for this request
	DurationMs  int64     `json:"durationMs"`
	GroupKey    string    `json:"groupKey"`
	Label       string    `json:"label,omitempty"`
	UserAgent   string    `json:"userAgent,omitempty"`
}

// SNIEvent is one finished pass-through connection.
type SNIEvent struct {
	ID         int64     `json:"id"`
	Time       time.Time `json:"time"` // connection start
	ClientIP   string    `json:"clientIp"`
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
	Client   string   // IP (exact) or name substring
	Domain   string   // substring; "\"exact\"" for exact match
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
	From              time.Time `json:"from"`
	To                time.Time `json:"to"`
	DNSQueries        int64     `json:"dnsQueries"`
	DNSBlocked        int64     `json:"dnsBlocked"`
	DNSCached         int64     `json:"dnsCached"`
	DNSLanCache       int64     `json:"dnsLancache"`
	DNSForwarded      int64     `json:"dnsForwarded"`
	BlockedPercent    float64   `json:"blockedPercent"`
	AvgDNSDurationUs  int64     `json:"avgDnsDurationUs"`
	CacheRequests     int64     `json:"cacheRequests"`
	CacheBytesSent    int64     `json:"cacheBytesSent"`
	CacheBytesHit     int64     `json:"cacheBytesHit"`
	CacheBytesWAN     int64     `json:"cacheBytesWan"`
	ByteHitRatio      float64   `json:"byteHitRatio"` // hit / (hit + wan), 0..1
	SNIBytes          int64     `json:"sniBytes"`
	ActiveClients     int64     `json:"activeClients"`
	ActiveDownloads   int64     `json:"activeDownloads"`
	DroppedLogEvents  uint64    `json:"droppedLogEvents"`
}

// Series is a time series set aligned on Timestamps (unix seconds, bucket start).
type Series struct {
	Step       int64                `json:"step"` // seconds
	Timestamps []int64              `json:"timestamps"`
	Values     map[string][]float64 `json:"values"` // DNS: allowed, blocked, cached, lancache; cache: hit, wan (bytes)
}

// TopKind selects a top list.
type TopKind string

const (
	TopDomains        TopKind = "domains"         // most queried allowed domains
	TopBlockedDomains TopKind = "blocked"         // most blocked domains
	TopClients        TopKind = "clients"         // by query count
	TopCacheClients   TopKind = "cache-clients"   // by cache bytes sent
	TopContent        TopKind = "content"         // cache groups by bytes sent
	TopUpstreams      TopKind = "upstreams"       // by query count (Bytes = avg duration µs)
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
	Search     string // label/group substring
	ActiveOnly bool
	Limit      int // default 50, max 500
	Offset     int
}

// EventFilter selects raw cache/SNI/eviction events (newest first).
type EventFilter struct {
	From, To time.Time
	Client   string
	Service  string
	Search   string // host/path/sni substring
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

// Metrics are internal counters (for /metrics and health).
type Metrics struct {
	Dropped      uint64 `json:"dropped"`
	QueueLength  int    `json:"queueLength"`
	LastFlush    time.Time `json:"lastFlush"`
	DBSizeBytes  int64  `json:"dbSizeBytes"`
}

// Store is the logs.db owner.
type Store struct {
	set *settings.Store
	log *slog.Logger
}

// Open opens (and migrates) logs.db at path.
func Open(ctx context.Context, path string, set *settings.Store, log *slog.Logger) (*Store, error) {
	return &Store{set: set, log: log}, nil
}

// Start runs the batch writer, rollups and retention pruning until ctx ends.
func (s *Store) Start(ctx context.Context) {}

// Close flushes pending events and closes the database.
func (s *Store) Close() error { return nil }

// LogQuery enqueues a query event (non-blocking).
func (s *Store) LogQuery(e QueryEvent) {}

// LogCache enqueues a cache request event (non-blocking). Also feeds download
// sessions and rollups.
func (s *Store) LogCache(e CacheEvent) {}

// LogSNI enqueues a pass-through event (non-blocking).
func (s *Store) LogSNI(e SNIEvent) {}

// LogEviction enqueues an eviction event (non-blocking).
func (s *Store) LogEviction(e EvictionEvent) {}

// SubscribeQueries returns a live feed of query events and a cancel func.
// Slow subscribers miss events (the channel never blocks the producer).
func (s *Store) SubscribeQueries() (<-chan QueryEvent, func()) {
	ch := make(chan QueryEvent)
	return ch, func() {}
}

// SubscribeCache returns a live feed of cache events and a cancel func.
func (s *Store) SubscribeCache() (<-chan CacheEvent, func()) {
	ch := make(chan CacheEvent)
	return ch, func() {}
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

// CacheSeries returns cache bytes (hit, wan) per step, optionally for one service.
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

// GroupClients lists the clients that downloaded a content group.
func (s *Store) GroupClients(ctx context.Context, service, groupKey string) ([]GroupClient, error) {
	return nil, errNotImplemented
}

// Metrics returns internal counters.
func (s *Store) Metrics() Metrics { return Metrics{} }
