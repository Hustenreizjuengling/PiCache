// Package sni is the TLS pass-through on :443 for the domains the download
// cache answers in DNS (docs/ARCHITECTURE.md 8.3). It reads the ClientHello
// SNI without terminating TLS and relays only allowlisted names.
//
// ClientHello: read the 5-byte record header (type 0x16, length ≤ 16384),
// then io.ReadFull the record; if the handshake message is longer, read
// further 0x16 records until complete (16 KiB total, 5 s deadline).
// Post-quantum ClientHellos span several TCP segments.
//
// Relay: write the buffered bytes, then per direction loop
// `src.SetReadDeadline(…); n, err := io.CopyN(dst, src, 4<<20)` on the
// raw *net.TCPConn pair (netutil.UnwrapTCP) so splice(2) is used; max
// lifetime 24 h. Never wrap conns in counting readers; count bytes from n.
// A connection is idle when no byte moved in either direction for 5 min:
// each direction wakes up at least every 30 s to check the shared activity
// time, so a long download with a silent upload direction is not cut.
package sni

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hustenreizjuengling/picache/internal/clients"
	"github.com/hustenreizjuengling/picache/internal/logs"
	"github.com/hustenreizjuengling/picache/internal/netutil"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// Timeouts.
const (
	helloTimeout   = 5 * time.Second
	dialTimeout    = 10 * time.Second
	replayTimeout  = 10 * time.Second
	maxLifetime    = 24 * time.Hour
	warnEvery      = time.Hour
	ownAddrRefresh = time.Minute
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
	d   Deps
	log *slog.Logger

	active, total, refused atomic.Int64
	bytesUp, bytesDown     atomic.Int64
	listeners              atomic.Int32
	lastWarn               atomic.Int64 // unix nanos of the last dial warning

	// dial connects to host:443 (tests replace it to reach loopback).
	dial func(ctx context.Context, host string) (net.Conn, error)

	ownMu sync.Mutex
	own   []netip.Addr
	ownAt time.Time
}

// New creates the server.
func New(d Deps) *Server {
	log := d.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	s := &Server{d: d, log: log.With(slog.String("component", "sni"))}
	s.dial = s.dialUpstream
	return s
}

// Serve accepts connections on ln (already wrapped by netutil.LimitListener)
// until ctx ends. Blocks until every connection it accepted is closed.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	s.listeners.Add(1)
	defer s.listeners.Add(-1)
	stop := context.AfterFunc(ctx, func() { _ = ln.Close() })
	defer stop()
	var wg sync.WaitGroup
	defer wg.Wait()
	var backoff time.Duration
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, net.ErrClosed) {
				return err
			}
			// Transient (e.g. EMFILE): back off instead of giving up.
			backoff = min(max(2*backoff, 5*time.Millisecond), time.Second)
			s.log.Debug("accept failed", slog.Any("err", err), slog.Duration("retry_in", backoff))
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(backoff):
			}
			continue
		}
		backoff = 0
		wg.Go(func() { s.handle(ctx, c) })
	}
}

// Stats returns live counters.
func (s *Server) Stats() Stats {
	return Stats{
		Active:    s.active.Load(),
		Total:     s.total.Load(),
		Refused:   s.refused.Load(),
		BytesUp:   s.bytesUp.Load(),
		BytesDown: s.bytesDown.Load(),
		Listening: s.listeners.Load() > 0,
	}
}

// handle serves one client connection.
func (s *Server) handle(ctx context.Context, c net.Conn) {
	s.total.Add(1)
	s.active.Add(1)
	defer s.active.Add(-1)
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	start := time.Now()

	ip := netutil.AddrFromNet(c.RemoteAddr())
	if s.d.ACL != nil && !s.d.ACL.Get().Allowed(ip) {
		s.refuse(ip, "", "client not allowed")
		return
	}
	raw, name, err := readHello(c)
	if err != nil {
		s.refuse(ip, "", err.Error())
		return
	}
	if s.d.Settings != nil && !s.d.Settings.Get().DownloadCache.Enabled {
		s.refuse(ip, name, "the download cache is disabled")
		return
	}
	service, ok := "", false
	if s.d.Services != nil {
		service, ok = s.d.Services.SNIAllowed(name)
	}
	if !ok {
		s.refuse(ip, name, "server name not allowed")
		return
	}

	dctx, cancel := context.WithTimeout(ctx, dialTimeout)
	up, err := s.dial(dctx, name)
	cancel()
	if err != nil {
		if errors.Is(err, netutil.ErrForbiddenDestination) {
			s.refused.Add(1)
		}
		s.warnDial(ip, name, err)
		return
	}
	defer up.Close()
	stopUp := context.AfterFunc(ctx, func() { _ = up.Close() })
	defer stopUp()

	if err := up.SetWriteDeadline(time.Now().Add(replayTimeout)); err != nil {
		return
	}
	n, err := up.Write(raw)
	s.bytesUp.Add(int64(n))
	if err != nil {
		return
	}
	bytesUp, bytesDown := s.relay(c, up, start.Add(maxLifetime))
	s.logEvent(start, ip, name, service, int64(n)+bytesUp, bytesDown)
}

// readHello reads the ClientHello within helloTimeout and extracts the SNI.
func readHello(c net.Conn) (raw []byte, name string, err error) {
	if err := c.SetReadDeadline(time.Now().Add(helloTimeout)); err != nil {
		return nil, "", err
	}
	raw, hello, err := readClientHello(c)
	if err != nil {
		return nil, "", err
	}
	if name, err = serverName(hello); err != nil {
		return nil, "", err
	}
	return raw, name, c.SetReadDeadline(time.Time{})
}

func (s *Server) refuse(ip netip.Addr, name, reason string) {
	s.refused.Add(1)
	s.log.Debug("connection refused", slog.String("client", ip.String()), slog.String("sni", name), slog.String("reason", reason))
}

// warnDial logs upstream connection failures at most hourly at WARN.
func (s *Server) warnDial(ip netip.Addr, name string, err error) {
	lvl := slog.LevelDebug
	now := time.Now().UnixNano()
	if last := s.lastWarn.Load(); now-last >= int64(warnEvery) && s.lastWarn.CompareAndSwap(last, now) {
		lvl = slog.LevelWarn
	}
	s.log.Log(context.Background(), lvl, "cannot connect to the upstream server",
		slog.String("client", ip.String()), slog.String("sni", name), slog.Any("err", err))
}

func (s *Server) logEvent(start time.Time, ip netip.Addr, name, service string, up, down int64) {
	if s.d.Logs == nil {
		return
	}
	var clientName string
	var noLog, noStats bool // clients ignoreLogs, ignoreStats
	if s.d.Clients != nil {
		if id := s.d.Clients.Identify(ip); id != nil {
			if id.IgnoreLogs && id.IgnoreStats {
				return
			}
			clientName, noLog, noStats = id.Name, id.IgnoreLogs, id.IgnoreStats
		}
	}
	s.d.Logs.LogSNI(logs.SNIEvent{
		Time:       start.UTC(),
		ClientIP:   ip.String(),
		ClientName: clientName,
		SNI:        name,
		Service:    service,
		BytesUp:    up,
		BytesDown:  down,
		DurationMs: time.Since(start).Milliseconds(),
		NoLog:      noLog,
		NoStats:    noStats,
	})
}

// dialUpstream resolves host with the bypass resolver and connects to
// host:443 through the SSRF guard: never this machine, loopback or
// link-local; private addresses only with downloadCache.allowPrivateUpstreams.
func (s *Server) dialUpstream(ctx context.Context, host string) (net.Conn, error) {
	if s.d.Lookup == nil {
		return nil, errors.New("no resolver configured")
	}
	d := netutil.SafeDialer{
		Resolve: s.d.Lookup,
		AllowPrivate: func(context.Context) bool {
			return s.d.Settings != nil && s.d.Settings.Get().DownloadCache.AllowPrivateUpstreams
		},
		Timeout:  dialTimeout,
		OwnAddrs: s.ownAddrs,
	}
	return d.DialContext(ctx, "tcp", net.JoinHostPort(host, "443"))
}

// ownAddrs returns this machine's addresses, refreshed at most every minute.
func (s *Server) ownAddrs() []netip.Addr {
	s.ownMu.Lock()
	defer s.ownMu.Unlock()
	if s.ownAt.IsZero() || time.Since(s.ownAt) >= ownAddrRefresh {
		s.own, s.ownAt = netutil.LocalAddrs(), time.Now()
	}
	return s.own
}
