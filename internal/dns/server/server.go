// Package dnsserver serves DNS over UDP and TCP and implements the request
// pipeline of docs/ARCHITECTURE.md 7.1: ACL, rate limit, hardening, client
// identity, special-use names, local records, LanCache overrides, special
// domains, filtering, conditional forwarding / router resolver, upstream
// resolution, CNAME inspection, reply shaping and logging. It also owns local
// DNS records and conditional forwarders.
//
// Serving: UDP with (&dns.Server{PacketConn: pc, Handler: h}).ActivateAndServe()
// (miekg replies from the query's destination address via IP_PKTINFO) and TCP
// with Listener: ln, MaxTCPQueries 128, IdleTimeout 8 s. No custom
// ReadFrom/WriteTo loops. The rate limiter is swept every 10 s.
//
// Tables (picache.db, component "dns"): dns_records, dns_forwarders.
//
// Bounds: CNAME chains (local and upstream) are followed at most 8 hops with
// a visited set (else SERVFAIL, status "error"); records that reference
// themselves are rejected.
package dnsserver

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"time"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/clients"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/dns/filter"
	"github.com/hustenreizjuengling/picache/internal/dns/upstream"
	"github.com/hustenreizjuengling/picache/internal/logs"
	"github.com/hustenreizjuengling/picache/internal/netutil"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

var errNotImplemented = errors.New("dnsserver: not implemented")

// Query statuses (stable strings stored in logs.db and used by the API/UI).
const (
	StatusForwarded      = "forwarded"
	StatusCached         = "cached"
	StatusStale          = "stale"
	StatusLocal          = "local"
	StatusSpecial        = "special"
	StatusLanCache       = "lancache"
	StatusBlockedList    = "blocked-list"
	StatusBlockedRule    = "blocked-rule"
	StatusBlockedRegex   = "blocked-regex"
	StatusBlockedCNAME   = "blocked-cname"
	StatusBlockedSpecial = "blocked-special"
	StatusRefused        = "refused"
	StatusError          = "error"
)

// Consumer-side interfaces (implemented by the concrete packages; fakes in tests).

// Filter is the part of *filter.Engine the server uses.
type Filter interface {
	Check(qname string, groups []int64) filter.Decision
	CheckRules(qname string, groups []int64) filter.Decision
	Explain(ctx context.Context, qname string, groups []int64) ([]filter.Match, error)
}

// Services is the part of *services.Registry the server uses.
type Services interface {
	MatchDNS(qname string) (serviceID string, ok bool)
}

// Clients is the part of *clients.Registry the server uses.
type Clients interface {
	Identify(ip netip.Addr) *clients.Identity
	Seen(ip netip.Addr)
}

// Upstream is the part of *upstream.Resolver the server uses.
type Upstream interface {
	Resolve(ctx context.Context, req *dns.Msg) (*dns.Msg, upstream.Info, error)
	ResolveVia(ctx context.Context, req *dns.Msg, upstreams []string) (*dns.Msg, upstream.Info, error)
	Probe(ctx context.Context, server netip.Addr) bool
}

// QueryLogger is the part of *logs.Store the server uses.
type QueryLogger interface {
	LogQuery(e logs.QueryEvent)
}

// Deps are the collaborators of the server.
type Deps struct {
	DB       *db.DB
	Settings *settings.Store
	Upstream Upstream
	Filter   Filter
	Clients  Clients
	Services Services
	Logs     QueryLogger
	ACL      *netutil.ACLWatcher
	// LanCacheReady reports whether overrides may be answered: the cache
	// HTTP listener is bound and a valid cache IPv4 is known. When false,
	// overrides are skipped (never answer with an unusable address).
	LanCacheReady func() (ok bool, reason string)
	// Container is the detected container type ("docker", "podman", "lxc",
	// "") for cache-IP auto-detection (Docker bridge IPs are never used).
	Container string
	Log       *slog.Logger
}

// Record is a local DNS record.
type Record struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"` // lower-case FQDN without trailing dot; "*.x" = subdomains of x
	Type      string    `json:"type"` // A | AAAA | CNAME | TXT
	Value     string    `json:"value"`
	TTL       uint32    `json:"ttl"`
	Enabled   bool      `json:"enabled"`
	Comment   string    `json:"comment"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// RecordInput creates or updates a record (TTL 0 → 300). A CNAME may not
// share its name with any other record (apperr.Conflict).
type RecordInput struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Value   string `json:"value"`
	TTL     uint32 `json:"ttl"`
	Enabled bool   `json:"enabled"`
	Comment string `json:"comment"`
}

// Forwarder sends a domain (apex + subdomains; "*.x" = subdomains only) to
// specific upstreams, e.g. "fritz.box" → 192.168.178.1 or
// "178.168.192.in-addr.arpa" → 192.168.178.1.
type Forwarder struct {
	ID        int64     `json:"id"`
	Domain    string    `json:"domain"`
	Upstreams []string  `json:"upstreams"`
	Enabled   bool      `json:"enabled"`
	Comment   string    `json:"comment"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// ForwarderInput creates or updates a forwarder.
type ForwarderInput struct {
	Domain    string   `json:"domain"`
	Upstreams []string `json:"upstreams"`
	Enabled   bool     `json:"enabled"`
	Comment   string   `json:"comment"`
}

// LookupRequest is a test query from the UI.
type LookupRequest struct {
	Name     string `json:"name"`
	Type     string `json:"type"`     // default "A"
	ClientIP string `json:"clientIp"` // evaluate as this client (default: the caller)
}

// LookupResult explains how PiCache would answer.
type LookupResult struct {
	Name       string         `json:"name"`
	Type       string         `json:"type"`
	Status     string         `json:"status"`
	RCode      string         `json:"rcode"`
	Answers    []string       `json:"answers"` // RR strings
	Reason     string         `json:"reason,omitempty"`
	Upstream   string         `json:"upstream,omitempty"`
	DurationUs int64          `json:"durationUs"`
	GroupIDs   []int64        `json:"groupIds"`
	Steps      []string       `json:"steps"`   // pipeline trace, human-readable
	Matches    []filter.Match `json:"matches"` // all filter matches
}

// BlockingStatus is the global blocking state.
type BlockingStatus struct {
	Enabled     bool       `json:"enabled"` // effective now
	PausedUntil *time.Time `json:"pausedUntil,omitempty"`
	Permanent   bool       `json:"permanent"` // disabled until re-enabled
}

// CacheIPStatus describes the effective LanCache answer addresses.
type CacheIPStatus struct {
	IPv4   []string `json:"ipv4"`
	IPv6   []string `json:"ipv6"`
	Auto   bool     `json:"auto"`             // auto-detected (not configured)
	Ready  bool     `json:"ready"`            // overrides are being answered
	Reason string   `json:"reason,omitempty"` // why not ready / warnings
}

// RouterStatus describes the router resolver.
type RouterStatus struct {
	Mode    string `json:"mode"`    // off | auto | manual
	Address string `json:"address"` // effective resolver IP ("" if none)
	Answers bool   `json:"answers"` // probe succeeded
	Domain  string `json:"domain"`  // effective local domain
}

// Stats are live counters.
type Stats struct {
	Queries        int64                 `json:"queries"`
	QPS            float64               `json:"qps"` // 1-minute average
	Refused        int64                 `json:"refused"`
	RateLimited    int64                 `json:"rateLimited"`
	InFlight       int64                 `json:"inFlight"`
	TopRateLimited []netutil.RateLimited `json:"topRateLimited"`
}

// Server is the DNS server.
type Server struct {
	d Deps
}

// New creates the server and migrates its tables (records, forwarders).
func New(ctx context.Context, d Deps) (*Server, error) { return &Server{d: d}, nil }

// Serve answers queries on the pre-bound sockets until ctx ends (blocks).
func (s *Server) Serve(ctx context.Context, udp []net.PacketConn, tcp []net.Listener) error {
	<-ctx.Done()
	return nil
}

// Lookup runs the pipeline for a test query without sending the reply
// anywhere (does not log to the query log).
func (s *Server) Lookup(ctx context.Context, req LookupRequest, caller netip.Addr) (LookupResult, error) {
	return LookupResult{}, errNotImplemented
}

// SetBlocking enables/disables blocking; pause > 0 disables for that long.
func (s *Server) SetBlocking(ctx context.Context, enabled bool, pause time.Duration) (BlockingStatus, error) {
	return BlockingStatus{}, errNotImplemented
}

// Blocking returns the current blocking state.
func (s *Server) Blocking() BlockingStatus { return BlockingStatus{} }

// CacheIPs returns the effective LanCache answer addresses (auto-detection
// is recomputed every 5 minutes).
func (s *Server) CacheIPs() CacheIPStatus { return CacheIPStatus{} }

// Router returns the router-resolver state.
func (s *Server) Router() RouterStatus { return RouterStatus{} }

// Stats returns live counters.
func (s *Server) Stats() Stats { return Stats{} }

// Records lists local records.
func (s *Server) Records(ctx context.Context) ([]Record, error) { return nil, errNotImplemented }

// CreateRecord adds a record.
func (s *Server) CreateRecord(ctx context.Context, in RecordInput) (Record, error) {
	return Record{}, errNotImplemented
}

// UpdateRecord updates a record.
func (s *Server) UpdateRecord(ctx context.Context, id int64, in RecordInput) (Record, error) {
	return Record{}, errNotImplemented
}

// DeleteRecord deletes a record.
func (s *Server) DeleteRecord(ctx context.Context, id int64) error { return errNotImplemented }

// Forwarders lists conditional forwarders.
func (s *Server) Forwarders(ctx context.Context) ([]Forwarder, error) { return nil, errNotImplemented }

// CreateForwarder adds a forwarder.
func (s *Server) CreateForwarder(ctx context.Context, in ForwarderInput) (Forwarder, error) {
	return Forwarder{}, errNotImplemented
}

// UpdateForwarder updates a forwarder.
func (s *Server) UpdateForwarder(ctx context.Context, id int64, in ForwarderInput) (Forwarder, error) {
	return Forwarder{}, errNotImplemented
}

// DeleteForwarder deletes a forwarder.
func (s *Server) DeleteForwarder(ctx context.Context, id int64) error { return errNotImplemented }
