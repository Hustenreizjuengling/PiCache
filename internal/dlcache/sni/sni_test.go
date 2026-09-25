package sni

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/clients"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/logs"
	"github.com/hustenreizjuengling/picache/internal/netutil"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

type fakeAllowlist map[string]string

func (f fakeAllowlist) SNIAllowed(sni string) (string, bool) {
	id, ok := f[sni]
	return id, ok
}

type fakeClients struct{ ignore bool }

func (f fakeClients) Identify(ip netip.Addr) *clients.Identity {
	return &clients.Identity{IP: ip, Name: "gaming-pc", IgnoreLogs: f.ignore}
}

type fakeLogs struct{ ch chan logs.SNIEvent }

func (f fakeLogs) LogSNI(e logs.SNIEvent) { f.ch <- e }

func newSettings(t *testing.T, mutate func(*settings.All)) *settings.Store {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "picache.db"), 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	set, err := settings.Open(context.Background(), d, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := set.Update(context.Background(), func(a *settings.All) error {
		a.DownloadCache.Enabled = true
		if mutate != nil {
			mutate(a)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return set
}

type testServer struct {
	*Server
	addr   string
	events chan logs.SNIEvent
	dials  atomic.Int32
	cancel context.CancelFunc
	served chan error
}

// startServer runs the pass-through on 127.0.0.1; dialing is redirected to
// upstreamAddr (the SSRF guard never allows loopback).
func startServer(t *testing.T, set *settings.Store, allow fakeAllowlist, cl Clients, upstreamAddr string) *testServer {
	t.Helper()
	ts := &testServer{events: make(chan logs.SNIEvent, 16), served: make(chan error, 1)}
	ts.Server = New(Deps{Settings: set, Services: allow, Clients: cl, Logs: fakeLogs{ts.events}})
	ts.dial = func(ctx context.Context, host string) (net.Conn, error) {
		ts.dials.Add(1)
		var d net.Dialer
		return d.DialContext(ctx, "tcp", upstreamAddr)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ts.addr = ln.Addr().String()
	ctx, cancel := context.WithCancel(context.Background())
	ts.cancel = cancel
	go func() { ts.served <- ts.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		if err := <-ts.served; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	return ts
}

// echoUpstream accepts one connection, records everything it receives
// until EOF, answers reply and closes.
func echoUpstream(t *testing.T, reply string) (addr string, got <-chan []byte) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	ch := make(chan []byte, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(10 * time.Second))
		b, _ := io.ReadAll(c)
		_, _ = io.WriteString(c, reply)
		ch <- b
	}()
	return ln.Addr().String(), ch
}

func TestRelayEndToEnd(t *testing.T) {
	upAddr, got := echoUpstream(t, "pong")
	ts := startServer(t, newSettings(t, nil), fakeAllowlist{"cdn.example.com": "epicgames"}, fakeClients{}, upAddr)

	hello := records(buildHello(padExt(3000), sniExt(0, "CDN.example.com")), 1200)
	c, err := net.Dial("tcp", ts.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// Write the hello in pieces like separate TCP segments.
	for i := 0; i < len(hello); i += 500 {
		if _, err := c.Write(hello[i:min(i+500, len(hello))]); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := io.WriteString(c, "ping"); err != nil {
		t.Fatal(err)
	}
	_ = c.(*net.TCPConn).CloseWrite()
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	resp, err := io.ReadAll(c)
	if err != nil || string(resp) != "pong" {
		t.Fatalf("response %q, %v", resp, err)
	}
	if b := <-got; !bytes.Equal(b, append(hello, "ping"...)) {
		t.Fatalf("upstream received %d bytes, want the exact hello + payload", len(b))
	}
	select {
	case e := <-ts.events:
		if e.SNI != "cdn.example.com" || e.Service != "epicgames" || e.ClientIP != "127.0.0.1" || e.ClientName != "gaming-pc" ||
			e.BytesUp != int64(len(hello)+4) || e.BytesDown != 4 || e.Time.IsZero() {
			t.Fatalf("event = %+v", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no SNI event")
	}
	waitFor(t, func() bool { return ts.Stats().Active == 0 })
	st := ts.Stats()
	if st.Total != 1 || st.Refused != 0 || st.Active != 0 || st.BytesUp != int64(len(hello)+4) || st.BytesDown != 4 || !st.Listening {
		t.Fatalf("stats = %+v", st)
	}
}

func TestRealTLSThroughRelay(t *testing.T) {
	up := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "hello over "+r.TLS.ServerName)
	}))
	up.StartTLS()
	defer up.Close()
	ts := startServer(t, newSettings(t, nil), fakeAllowlist{"example.com": "test"}, fakeClients{}, up.Listener.Addr().String())

	pool := x509.NewCertPool()
	pool.AddCert(up.Certificate())
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", ts.addr)
		},
		TLSClientConfig: &tls.Config{RootCAs: pool},
	}}
	resp, err := client.Get("https://example.com/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "hello over example.com" {
		t.Fatalf("body = %q", body)
	}
	client.CloseIdleConnections()
	select {
	case e := <-ts.events:
		if e.SNI != "example.com" || e.BytesUp == 0 || e.BytesDown == 0 {
			t.Fatalf("event = %+v", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no SNI event")
	}
}

func TestRefusedConnections(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*settings.All)
		send  []byte
	}{
		{"not allowlisted", nil, records(buildHello(sniExt(0, "evil.example.net")), 4096)},
		{"no sni", nil, records(buildHello(padExt(10)), 4096)},
		{"ip literal sni", nil, records(buildHello(sniExt(0, "127.0.0.1")), 4096)},
		{"not tls", nil, []byte("GET / HTTP/1.1\r\nHost: cdn.example.com\r\n\r\n")},
		{"download cache disabled", func(a *settings.All) { a.DownloadCache.Enabled = false }, records(buildHello(sniExt(0, "cdn.example.com")), 4096)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := startServer(t, newSettings(t, tt.setup), fakeAllowlist{"cdn.example.com": "x"}, fakeClients{}, "127.0.0.1:1")
			c, err := net.Dial("tcp", ts.addr)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			_, _ = c.Write(tt.send)
			_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
			if n, err := c.Read(make([]byte, 1)); n != 0 || err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatalf("connection not closed: n=%d err=%v", n, err)
			}
			waitFor(t, func() bool { return ts.Stats().Refused == 1 && ts.Stats().Active == 0 })
			if ts.dials.Load() != 0 {
				t.Fatal("upstream dialed for a refused connection")
			}
		})
	}
}

func TestHelloTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for the 5 s ClientHello deadline")
	}
	ts := startServer(t, newSettings(t, nil), fakeAllowlist{}, fakeClients{}, "127.0.0.1:1")
	c, err := net.Dial("tcp", ts.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _ = c.Write([]byte{recordTypeHandshake, 3, 1}) // partial header, then silence
	_ = c.SetReadDeadline(time.Now().Add(helloTimeout + 5*time.Second))
	start := time.Now()
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("expected close")
	}
	if d := time.Since(start); d < helloTimeout-time.Second || d > helloTimeout+4*time.Second {
		t.Fatalf("closed after %v", d)
	}
}

func TestIgnoreLogsClient(t *testing.T) {
	upAddr, _ := echoUpstream(t, "")
	ts := startServer(t, newSettings(t, nil), fakeAllowlist{"cdn.example.com": "x"}, fakeClients{ignore: true}, upAddr)
	c, err := net.Dial("tcp", ts.addr)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = c.Write(records(buildHello(sniExt(0, "cdn.example.com")), 4096))
	_ = c.(*net.TCPConn).CloseWrite()
	_, _ = io.ReadAll(c)
	c.Close()
	waitFor(t, func() bool { return ts.Stats().Active == 0 && ts.Stats().Total == 1 })
	select {
	case e := <-ts.events:
		t.Fatalf("event logged for an ignored client: %+v", e)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestServeStopsActiveConnections(t *testing.T) {
	// The upstream never answers and never closes.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var held []net.Conn
	var mu sync.Mutex
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, c)
			mu.Unlock()
		}
	}()
	defer func() {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range held {
			c.Close()
		}
	}()
	ts := startServer(t, newSettings(t, nil), fakeAllowlist{"cdn.example.com": "x"}, fakeClients{}, ln.Addr().String())
	c, err := net.Dial("tcp", ts.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _ = c.Write(records(buildHello(sniExt(0, "cdn.example.com")), 4096))
	waitFor(t, func() bool { return ts.dials.Load() == 1 })
	ts.cancel()
	select {
	case err := <-ts.served:
		ts.served <- err // for the cleanup
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after cancel")
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("client connection not closed: %v", err)
	}
	if st := ts.Stats(); st.Active != 0 || st.Listening {
		t.Fatalf("stats after stop = %+v", st)
	}
}

func TestDialUpstreamSSRFGuard(t *testing.T) {
	var own netip.Addr
	for _, a := range netutil.LocalAddrs() {
		if !a.IsLoopback() && !a.IsLinkLocalUnicast() && a.Is4() {
			own = a
			break
		}
	}
	tests := []struct {
		name         string
		addr         netip.Addr
		allowPrivate bool
	}{
		{"loopback", netip.MustParseAddr("127.0.0.1"), false},
		{"loopback even if private upstreams are allowed", netip.MustParseAddr("127.0.0.1"), true},
		{"private", netip.MustParseAddr("192.168.1.10"), false},
		{"cgnat", netip.MustParseAddr("100.64.0.1"), false},
		{"cloud metadata", netip.MustParseAddr("169.254.169.254"), true},
		{"unspecified", netip.MustParseAddr("0.0.0.0"), true},
	}
	if own.IsValid() {
		tests = append(tests, struct {
			name         string
			addr         netip.Addr
			allowPrivate bool
		}{"own address", own, true})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			set := newSettings(t, func(a *settings.All) { a.DownloadCache.AllowPrivateUpstreams = tt.allowPrivate })
			s := New(Deps{Settings: set, Lookup: func(context.Context, string) ([]netip.Addr, error) {
				return []netip.Addr{tt.addr}, nil
			}})
			c, err := s.dialUpstream(context.Background(), "cdn.example.com")
			if c != nil {
				c.Close()
			}
			if !errors.Is(err, netutil.ErrForbiddenDestination) {
				t.Fatalf("err = %v, want ErrForbiddenDestination", err)
			}
		})
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
