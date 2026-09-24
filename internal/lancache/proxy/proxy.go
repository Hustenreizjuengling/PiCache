// Package proxy is the LanCache-compatible HTTP cache on :80
// (docs/ARCHITECTURE.md 8.2): heartbeat, ACL, loop detection, host
// classification, special paths, slice cache with streaming request
// collapsing, read-ahead, no-range handling, redirects and SSRF-safe
// upstream fetching.
//
// Table (picache.db, component "proxy"): proxy_noslice_hosts.
package proxy

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"time"

	"github.com/hustenreizjuengling/picache/internal/clients"
	"github.com/hustenreizjuengling/picache/internal/db"
	cachestore "github.com/hustenreizjuengling/picache/internal/lancache/store"
	"github.com/hustenreizjuengling/picache/internal/logs"
	"github.com/hustenreizjuengling/picache/internal/netutil"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

var errNotImplemented = errors.New("proxy: not implemented")

// Consumer-side interfaces (implemented by the concrete packages; fakes in tests).

// SliceStore is the part of *cachestore.Store the proxy uses.
type SliceStore interface {
	ID() string
	SliceSize() int64
	Head(ctx context.Context, id string) (cachestore.ObjectHead, bool, error)
	ReadSlice(ctx context.Context, id string, gen uint64, idx int64) (cachestore.SliceReader, error)
	SetMeta(ctx context.Context, id string, m cachestore.Meta) (uint64, error)
	WriteSlice(ctx context.Context, id string, gen uint64, idx int64, data []byte) error
	Invalidate(ctx context.Context, id string, reason string) error
	Touch(id string, bytesServed int64)
}

// Classifier is the part of *services.Registry the proxy uses.
type Classifier interface {
	Classify(host, userAgent, path string) (serviceID string, enabled, known bool)
	Label(groupKey string) string
}

// Clients is the part of *clients.Registry the proxy uses.
type Clients interface {
	Identify(ip netip.Addr) *clients.Identity
}

// CacheLogger is the part of *logs.Store the proxy uses.
type CacheLogger interface {
	LogCache(e logs.CacheEvent)
}

// Deps are the collaborators of the proxy.
type Deps struct {
	DB       *db.DB // for persisted no-slice hosts
	Settings *settings.Store
	Services Classifier
	Lookup   netutil.Resolver  // bypass resolver (IPv4)
	Store    func() SliceStore // current store; nil → pass-through mode
	// StoreFull reports that the store cannot free space (no new fills).
	StoreFull  func() bool
	Clients    Clients
	Logs       CacheLogger
	ACL        *netutil.ACLWatcher
	InstanceID string // value of X-LanCache-Processed-By
	Log        *slog.Logger
}

// Transfer is a live client request.
type Transfer struct {
	ID         string    `json:"id"`
	ClientIP   string    `json:"clientIp"`
	ClientName string    `json:"clientName,omitempty"`
	Service    string    `json:"service"`
	Host       string    `json:"host"`
	Path       string    `json:"path"` // no query
	GroupKey   string    `json:"groupKey"`
	Label      string    `json:"label"`
	Started    time.Time `json:"started"`
	BytesSent  int64     `json:"bytesSent"`
	BytesHit   int64     `json:"bytesHit"`
	BytesWAN   int64     `json:"bytesWan"`
	Total      int64     `json:"total"` // expected bytes for this response (0 = unknown)
	RateBps    float64   `json:"rateBps"`
}

// ActiveDownload aggregates live requests per (client, service, group) with
// a 10 s rate window; entries stay 30 s after the last request.
type ActiveDownload struct {
	ClientIP   string    `json:"clientIp"`
	ClientName string    `json:"clientName,omitempty"`
	Service    string    `json:"service"`
	GroupKey   string    `json:"groupKey"`
	Label      string    `json:"label"`
	Started    time.Time `json:"started"`
	LastSeen   time.Time `json:"lastSeen"`
	InFlight   int       `json:"inFlight"`
	BytesSent  int64     `json:"bytesSent"`
	BytesHit   int64     `json:"bytesHit"`
	BytesWAN   int64     `json:"bytesWan"`
	RateBps    float64   `json:"rateBps"`
}

// Stats are live counters.
type Stats struct {
	Requests          int64    `json:"requests"`
	ActiveClients     int64    `json:"activeClients"`
	ActiveFills       int64    `json:"activeFills"`
	BytesHit          int64    `json:"bytesHit"`
	BytesWAN          int64    `json:"bytesWan"`
	Refused           int64    `json:"refused"`
	Errors            int64    `json:"errors"`
	PassThrough       bool     `json:"passThrough"`                 // no store available
	SteamHostsRefused []string `json:"steamHostsRefused,omitempty"` // recent refused hosts with Steam UA (last 20)
}

// NoSliceHost is a host that answered range requests without range support.
type NoSliceHost struct {
	Host     string    `json:"host"`
	Failures int       `json:"failures"`
	Marked   bool      `json:"marked"` // requests go without Range
	Since    time.Time `json:"since"`
}

// Server is the HTTP cache.
type Server struct {
	d Deps
}

// New creates the proxy (migrates its table).
func New(ctx context.Context, d Deps) (*Server, error) { return &Server{d: d}, nil }

// Handler returns the :80 handler. The caller wraps the listener with
// netutil.LimitListener; the handler sets a 60 s write deadline before
// each write to the client.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not implemented", http.StatusNotImplemented)
	})
}

// Start runs housekeeping (active-download expiry, no-slice decay). Blocks
// until ctx is done.
func (s *Server) Start(ctx context.Context) { <-ctx.Done() }

// Active returns live transfers.
func (s *Server) Active() []Transfer { return nil }

// ActiveDownloads returns live downloads aggregated per client and content.
func (s *Server) ActiveDownloads() []ActiveDownload { return nil }

// Stats returns live counters.
func (s *Server) Stats() Stats { return Stats{} }

// NoSliceHosts lists hosts detected without range support.
func (s *Server) NoSliceHosts(ctx context.Context) ([]NoSliceHost, error) {
	return nil, errNotImplemented
}

// ResetNoSlice clears the no-slice state of a host.
func (s *Server) ResetNoSlice(ctx context.Context, host string) error { return errNotImplemented }
