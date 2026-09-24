// Package sni is the TLS pass-through on :443 for LanCache-overridden
// domains (docs/ARCHITECTURE.md 8.3). It reads the ClientHello SNI without
// terminating TLS and relays only allowlisted names.
//
// ClientHello: read the 5-byte record header (type 0x16, length ≤ 16384),
// then io.ReadFull the record; if the handshake message is longer, read
// further 0x16 records until complete (16 KiB total, 5 s deadline).
// Post-quantum ClientHellos span several TCP segments.
//
// Relay: write the buffered bytes, then per direction loop
// `src.SetReadDeadline(now+5min); n, err := io.CopyN(dst, src, 4<<20)` on the
// raw *net.TCPConn pair (netutil.UnwrapTCP) so splice(2) is used and the
// idle deadline refreshes every 4 MiB; max lifetime 24 h. Never wrap conns
// in counting readers; count bytes from n.
package sni

import (
	"context"
	"log/slog"
	"net"
	"net/netip"

	"github.com/hustenreizjuengling/picache/internal/clients"
	"github.com/hustenreizjuengling/picache/internal/logs"
	"github.com/hustenreizjuengling/picache/internal/netutil"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// Allowlist is the part of *services.Registry the server uses.
type Allowlist interface {
	SNIAllowed(sni string) (serviceID string, ok bool)
}

// Clients is the part of *clients.Registry the server uses.
type Clients interface {
	Identify(ip netip.Addr) *clients.Identity
}

// SNILogger is the part of *logs.Store the server uses.
type SNILogger interface {
	LogSNI(e logs.SNIEvent)
}

// Deps are the collaborators.
type Deps struct {
	Settings *settings.Store
	Services Allowlist
	Lookup   netutil.Resolver // bypass resolver (IPv4)
	Clients  Clients
	Logs     SNILogger
	ACL      *netutil.ACLWatcher
	Log      *slog.Logger
}

// Stats are live counters.
type Stats struct {
	Active    int64 `json:"active"`
	Total     int64 `json:"total"`
	Refused   int64 `json:"refused"`
	BytesUp   int64 `json:"bytesUp"`
	BytesDown int64 `json:"bytesDown"`
	Listening bool  `json:"listening"`
}

// Server is the pass-through.
type Server struct {
	d Deps
}

// New creates the server.
func New(d Deps) *Server { return &Server{d: d} }

// Serve accepts connections on ln (already wrapped by netutil.LimitListener)
// until ctx ends. Blocks.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	<-ctx.Done()
	return nil
}

// Stats returns live counters.
func (s *Server) Stats() Stats { return Stats{} }
