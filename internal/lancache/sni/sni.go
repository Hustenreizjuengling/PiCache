// Package sni is the TLS pass-through on :443 for LanCache-overridden
// domains (docs/ARCHITECTURE.md 8.3). It reads the ClientHello SNI without
// terminating TLS and relays only allowlisted names.
package sni

import (
	"context"
	"log/slog"
	"net"

	"github.com/hustenreizjuengling/picache/internal/lancache/services"
	"github.com/hustenreizjuengling/picache/internal/logs"
	"github.com/hustenreizjuengling/picache/internal/netutil"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// Deps are the collaborators.
type Deps struct {
	Settings *settings.Store
	Services *services.Registry
	Lookup   netutil.Resolver // bypass resolver (IPv4)
	Logs     *logs.Store
	ACL      *netutil.ACLWatcher
	Log      *slog.Logger
}

// Stats are live counters.
type Stats struct {
	Active   int64 `json:"active"`
	Total    int64 `json:"total"`
	Refused  int64 `json:"refused"`
	BytesUp  int64 `json:"bytesUp"`
	BytesDown int64 `json:"bytesDown"`
}

// Server is the pass-through.
type Server struct {
	d Deps
}

// New creates the server.
func New(d Deps) *Server { return &Server{d: d} }

// Serve accepts connections on ln until ctx ends.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	<-ctx.Done()
	return nil
}

// Stats returns live counters.
func (s *Server) Stats() Stats { return Stats{} }
