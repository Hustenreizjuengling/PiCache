// Package api is the REST/SSE API under /api/v1 plus the static web UI
// (docs/ARCHITECTURE.md 12, docs/API.md).
//
// Each domain registers its routes in its own file routes_<domain>.go via
// s.route(pattern, permission, handler). Handlers return an error; use the
// apperr constructors for user-facing errors (mapped to 4xx/503), anything
// else becomes a generic 500.
package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/hustenreizjuengling/picache/internal/auth"
	"github.com/hustenreizjuengling/picache/internal/clients"
	"github.com/hustenreizjuengling/picache/internal/config"
	"github.com/hustenreizjuengling/picache/internal/dns/filter"
	dnsserver "github.com/hustenreizjuengling/picache/internal/dns/server"
	"github.com/hustenreizjuengling/picache/internal/dns/upstream"
	"github.com/hustenreizjuengling/picache/internal/lancache/proxy"
	"github.com/hustenreizjuengling/picache/internal/lancache/services"
	"github.com/hustenreizjuengling/picache/internal/lancache/sni"
	cachestore "github.com/hustenreizjuengling/picache/internal/lancache/store"
	"github.com/hustenreizjuengling/picache/internal/logs"
	"github.com/hustenreizjuengling/picache/internal/settings"
	"github.com/hustenreizjuengling/picache/internal/storage"
)

// StoreState describes the active cache store.
type StoreState struct {
	TargetID     string            `json:"targetId"`
	StoreID      string            `json:"storeId"`
	Online       bool              `json:"online"`
	PassThrough  bool              `json:"passThrough"` // proxy serves uncached
	Reason       string            `json:"reason,omitempty"`
	Hint         string            `json:"hint,omitempty"`
	Usage        *cachestore.Usage `json:"usage,omitempty"`
	SliceSize    int64             `json:"sliceSize"`
	TotalBytes   uint64            `json:"totalBytes"` // filesystem size of the store root
	FreeBytes    uint64            `json:"freeBytes"`
	MinFreeBytes int64             `json:"minFreeBytes"` // effective minimum free space
	LowSpace     bool              `json:"lowSpace"`     // free < effective min free: eviction running
	Full         bool              `json:"full"`         // eviction cannot free space: hits served, new content not stored
	SDCard       bool              `json:"sdCard"`
}

// VerifyState describes a running or finished verify/rebuild.
type VerifyState struct {
	Running    bool                      `json:"running"`
	Repair     bool                      `json:"repair"`
	StartedAt  time.Time                 `json:"startedAt,omitzero"`
	FinishedAt time.Time                 `json:"finishedAt,omitzero"`
	Progress   cachestore.VerifyProgress `json:"progress"`
	Error      string                    `json:"error,omitempty"`
}

// HealthCheck is one health check result.
type HealthCheck struct {
	Name    string `json:"name"`
	Status  string `json:"status"` // ok | warn | fail
	Message string `json:"message,omitempty"`
	Hint    string `json:"hint,omitempty"`
}

// Health is the component health summary (authenticated endpoint). OK is
// false if any check fails (warnings keep it true).
type Health struct {
	OK        bool          `json:"ok"`
	Checks    []HealthCheck `json:"checks"`
	CheckedAt time.Time     `json:"checkedAt"`
}

// ListenerInfo describes bound and failed listeners by role
// (dns-udp, dns-tcp, cache, sni, web, web-tls).
type ListenerInfo struct {
	Bound  map[string][]string `json:"bound"`
	Failed map[string]string   `json:"failed,omitempty"` // role → error (non-DNS binds are not fatal)
}

// Runtime is implemented by internal/app: process-level state and actions.
type Runtime interface {
	StartedAt() time.Time
	InstanceID() string
	Config() *config.Config
	Listeners() ListenerInfo
	MasterKeySource() string
	StoreState() StoreState
	ActiveStore() *cachestore.Store // nil when offline
	ActivateStore(ctx context.Context, targetID string) error
	EvictNow(ctx context.Context) (cachestore.EvictResult, error)
	StartVerify(repair bool) error
	VerifyState() VerifyState
	// Backup writes a consistent copy of picache.db without sessions; sealed
	// NAS passwords only if includeSecrets.
	Backup(ctx context.Context, w io.Writer, includeSecrets bool) error
	StageRestore(ctx context.Context, r io.Reader) error // validated, applied on next start
	Restart()                                            // exit with code 75 after the response (systemd/Docker restart)
	Health(ctx context.Context) Health                   // last evaluated health (refreshed every 60 s)
}

// Deps are everything the API talks to.
type Deps struct {
	Config   *config.Config
	Settings *settings.Store
	Auth     *auth.Service
	DNS      *dnsserver.Server
	Upstream *upstream.Resolver
	Filter   *filter.Engine
	Clients  *clients.Registry
	Services *services.Registry
	Proxy    *proxy.Server
	SNI      *sni.Server
	Storage  *storage.Manager
	Logs     *logs.Store
	Runtime  Runtime
	UI       http.Handler // embedded web UI
	Log      *slog.Logger
}

// Server is the API + UI HTTP handler.
type Server struct {
	d       Deps
	log     *slog.Logger
	mux     *http.ServeMux
	handler http.Handler
	hosts   *hostAllowlist
}

// New builds the handler with all routes and middleware.
func New(d Deps) *Server {
	s := &Server{d: d, log: d.Log.With(slog.String("component", "api")), mux: http.NewServeMux()}
	s.hosts = newHostAllowlist(d.Config, d.Settings)

	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = io.WriteString(w, "ok")
	})

	s.registerAuthRoutes()
	s.registerSystemRoutes()
	s.registerSettingsRoutes()
	s.registerDNSRoutes()
	s.registerUpstreamRoutes()
	s.registerFilterRoutes()
	s.registerLanCacheRoutes()
	s.registerCacheRoutes()
	s.registerProxyRoutes()
	s.registerStorageRoutes()
	s.registerLogsRoutes()

	s.mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, s.log, errNotFoundRoute)
	})
	if d.UI != nil {
		s.mux.Handle("GET /", d.UI)
	}
	s.handler = s.middleware(s.mux)
	return s
}

// Handler returns the complete handler (middleware + routes).
func (s *Server) Handler() http.Handler { return s.handler }
