// Package upstream forwards DNS queries to upstream resolvers (UDP, TCP, DoT,
// DoH) with a response cache, serve-stale, in-flight de-duplication and the
// upstream modes load_balance/parallel/strict (docs/ARCHITECTURE.md 7.4).
//
// LookupIP resolves names for PiCache itself (cache proxy, SNI, list and
// cache-domains downloads) and deliberately bypasses local records, LanCache
// overrides and filtering.
package upstream

import (
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"time"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/settings"
)

var errNotImplemented = errors.New("upstream: not implemented")

// Info describes how a response was obtained.
type Info struct {
	Upstream string        // upstream that answered ("" for cache hits)
	Cached   bool          // served from the response cache
	Stale    bool          // served stale (refresh in background)
	RTT      time.Duration // upstream round-trip time (0 for cache hits)
}

// UpstreamStat is per-upstream health for the UI.
type UpstreamStat struct {
	Upstream   string    `json:"upstream"`
	Queries    int64     `json:"queries"`
	Errors     int64     `json:"errors"`
	AvgRTTMs   float64   `json:"avgRttMs"` // EWMA
	LastError  string    `json:"lastError,omitempty"`
	LastErrorAt time.Time `json:"lastErrorAt,omitzero"`
	Healthy    bool      `json:"healthy"`
}

// CacheStat describes the response cache.
type CacheStat struct {
	Entries   int   `json:"entries"`
	Capacity  int   `json:"capacity"`
	Hits      int64 `json:"hits"`
	Misses    int64 `json:"misses"`
	StaleHits int64 `json:"staleHits"`
}

// TestResult is the outcome of testing one upstream string.
type TestResult struct {
	Upstream string  `json:"upstream"`
	OK       bool    `json:"ok"`
	RTTMs    float64 `json:"rttMs"`
	Answer   string  `json:"answer,omitempty"`
	Error    string  `json:"error,omitempty"`
}

// Resolver is safe for concurrent use. It follows settings changes.
type Resolver struct {
	set *settings.Store
	log *slog.Logger
}

// New creates a resolver from the current settings and subscribes to changes.
func New(set *settings.Store, log *slog.Logger) (*Resolver, error) {
	return &Resolver{set: set, log: log}, nil
}

// Start runs background maintenance (stale refresh workers, health) until ctx ends.
func (r *Resolver) Start(ctx context.Context) {}

// Close releases connections.
func (r *Resolver) Close() error { return nil }

// Resolve answers req via the configured upstreams (with cache). req is not
// modified; the returned message is a private copy whose Id equals req.Id.
func (r *Resolver) Resolve(ctx context.Context, req *dns.Msg) (*dns.Msg, Info, error) {
	return nil, Info{}, errNotImplemented
}

// ResolveVia answers req via the given upstreams (conditional forwarding,
// local PTR resolvers). Cached separately per upstream set.
func (r *Resolver) ResolveVia(ctx context.Context, req *dns.Msg, upstreams []string) (*dns.Msg, Info, error) {
	return nil, Info{}, errNotImplemented
}

// LookupIP resolves host to addresses via the default upstreams, bypassing
// all local data. IPv4 only unless want6. Cached by TTL.
func (r *Resolver) LookupIP(ctx context.Context, host string, want6 bool) ([]netip.Addr, error) {
	return nil, errNotImplemented
}

// LookupPTR resolves the hostname of ip via the given plain-DNS servers
// (local PTR upstreams); "" if none.
func (r *Resolver) LookupPTR(ctx context.Context, ip netip.Addr, servers []string) (string, error) {
	return "", errNotImplemented
}

// Test resolves a fixed name through one upstream string.
func (r *Resolver) Test(ctx context.Context, upstream string) TestResult {
	return TestResult{Upstream: upstream, Error: errNotImplemented.Error()}
}

// Stats returns per-upstream health.
func (r *Resolver) Stats() []UpstreamStat { return nil }

// CacheStats returns response cache counters.
func (r *Resolver) CacheStats() CacheStat { return CacheStat{} }

// FlushCache empties the response cache.
func (r *Resolver) FlushCache() {}
