// Package proxy is the LanCache-compatible HTTP cache on :80
// (docs/ARCHITECTURE.md 8.2): heartbeat, ACL, loop detection, host
// classification, special paths, slice cache with request collapsing,
// read-ahead, no-range handling, redirects and SSRF-safe upstream fetching.
package proxy

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/hustenreizjuengling/picache/internal/clients"
	"github.com/hustenreizjuengling/picache/internal/db"
	cachestore "github.com/hustenreizjuengling/picache/internal/lancache/store"
	"github.com/hustenreizjuengling/picache/internal/lancache/services"
	"github.com/hustenreizjuengling/picache/internal/logs"
	"github.com/hustenreizjuengling/picache/internal/netutil"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

var errNotImplemented = errors.New("proxy: not implemented")

// Deps are the collaborators of the proxy.
type Deps struct {
	DB         *db.DB // for persisted no-slice hosts
	Settings   *settings.Store
	Services   *services.Registry
	Lookup     netutil.Resolver         // bypass resolver (IPv4)
	Store      func() *cachestore.Store // current store; nil → pass-through mode
	Clients    *clients.Registry
	Logs       *logs.Store
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

// Stats are live counters.
type Stats struct {
	Requests      int64 `json:"requests"`
	ActiveClients int64 `json:"activeClients"`
	ActiveFills   int64 `json:"activeFills"`
	BytesHit      int64 `json:"bytesHit"`
	BytesWAN      int64 `json:"bytesWan"`
	Refused       int64 `json:"refused"`
	Errors        int64 `json:"errors"`
	PassThrough   bool  `json:"passThrough"` // no store available
}

// NoSliceHost is a host that answered range requests with 200.
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

// Handler returns the :80 handler.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not implemented", http.StatusNotImplemented)
	})
}

// Active returns live transfers.
func (s *Server) Active() []Transfer { return nil }

// Stats returns live counters.
func (s *Server) Stats() Stats { return Stats{} }

// NoSliceHosts lists hosts detected without range support.
func (s *Server) NoSliceHosts(ctx context.Context) ([]NoSliceHost, error) { return nil, errNotImplemented }

// ResetNoSlice clears the no-slice state of a host.
func (s *Server) ResetNoSlice(ctx context.Context, host string) error { return errNotImplemented }
