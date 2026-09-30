package app

import (
	"bufio"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/config"
	"github.com/hustenreizjuengling/picache/internal/netutil"
)

// pipeListener hands out one end of net.Pipe connections whose remote
// address is chosen by the test.
type pipeListener struct{ conns chan net.Conn }

func (l *pipeListener) Accept() (net.Conn, error) {
	c, ok := <-l.conns
	if !ok {
		return nil, net.ErrClosed
	}
	return c, nil
}
func (l *pipeListener) Close() error   { return nil }
func (l *pipeListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 8080} }

type remoteConn struct {
	net.Conn
	remote net.Addr
}

func (c remoteConn) RemoteAddr() net.Addr { return c.remote }

// startLimited serves pl through l and returns dial, which connects from ip
// and returns the admitted connection (nil: it was closed at once).
func startLimited(t *testing.T, l *webLimiter) (dial func(ip string) net.Conn) {
	t.Helper()
	pl := &pipeListener{conns: make(chan net.Conn)}
	ln := l.Listener(pl)
	accepted := make(chan net.Conn)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				close(accepted)
				return
			}
			accepted <- c
		}
	}()
	t.Cleanup(func() { close(pl.conns) })
	return func(ip string) net.Conn {
		t.Helper()
		server, client := net.Pipe()
		pl.conns <- remoteConn{Conn: server, remote: &net.TCPAddr{IP: net.ParseIP(ip), Port: 40000}}
		closed := make(chan error, 1)
		go func() {
			_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
			_, err := client.Read(make([]byte, 1))
			closed <- err
		}()
		select {
		case c := <-accepted:
			return c
		case err := <-closed:
			if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) {
				t.Fatalf("%s: neither admitted nor closed: %v", ip, err)
			}
			return nil
		}
	}
}

// SEC-1: the web listeners cap the connections per client key and in total;
// a refused connection is closed at once, a closed one frees its slot, and
// trusted proxies are bounded only by the total, loopback by the total and
// its reserve.
func TestWebLimiter(t *testing.T) {
	proxy := netip.MustParseAddr("192.168.1.99")
	l := newWebLimiter(func(ip netip.Addr) bool { return ip == proxy }, webCaps{perClient: 2, perNetwork: 2, total: 5, loopback: 1},
		slog.New(slog.DiscardHandler))
	dial := startLimited(t, l)

	a1, a2 := dial("192.168.1.10"), dial("192.168.1.10")
	if a1 == nil || a2 == nil {
		t.Fatal("the first two connections of a client were refused")
	}
	if dial("192.168.1.10") != nil {
		t.Fatal("a third connection of the client was admitted (per-client cap 2)")
	}
	a2.Close()
	a2.Close() // a second close frees nothing twice
	if a3 := dial("192.168.1.10"); a3 == nil {
		t.Fatal("the slot of a closed connection was not freed")
	}
	// The trusted proxy and loopback: only the total (5) applies.
	p1, p2 := dial(proxy.String()), dial("127.0.0.1")
	lo := dial("::1")
	if p1 == nil || p2 == nil || lo == nil {
		t.Fatal("the proxy or loopback was refused below the total")
	}
	if dial(proxy.String()) != nil || dial("192.168.1.20") != nil {
		t.Fatal("a connection beyond the total was admitted")
	}
	p1.Close()
	if dial("192.168.1.20") == nil {
		t.Fatal("the total's slot of a closed connection was not freed")
	}
	l.mu.Lock()
	active, perKeys := l.active, len(l.per)
	l.mu.Unlock()
	if active != 5 || perKeys != 2 {
		t.Fatalf("active %d, client keys %d; want 5 and 2", active, perKeys)
	}
	// Loopback also gets the reserve beyond the total (1), no more.
	if dial("127.0.0.1") == nil {
		t.Fatal("loopback was refused within its reserve")
	}
	if dial("::1") != nil {
		t.Fatal("loopback was admitted beyond its reserve")
	}
}

// SEC-WEBCAP-1: one host cannot lock this machine out of the web UI. The
// IPv6 addresses of one /64 whose client keys are /128 (on-link, ULA,
// link-local) share a cap, so extra addresses do not multiply the
// per-client cap; and when the network has filled the total, loopback (the
// health check, the CLI) still connects within its reserve.
func TestWebLimiterLoopbackReserveAndIPv6Networks(t *testing.T) {
	l := newWebLimiter(nil, webCaps{perClient: 2, perNetwork: 4, total: 8, loopback: 2}, slog.New(slog.DiscardHandler))
	dial := startLimited(t, l)
	var held []net.Conn
	for _, ip := range []string{"fd00::1", "fd00::1", "fd00::2", "fd00::2"} {
		c := dial(ip)
		if c == nil {
			t.Fatalf("%s was refused below its caps", ip)
		}
		held = append(held, c)
	}
	if dial("fd00::3") != nil {
		t.Fatal("a third address of the /64 was admitted beyond the network's cap (4)")
	}
	// Other /64s and IPv4 fill the total.
	for _, ip := range []string{"fd00:0:0:1::1", "fd00:0:0:1::1", "192.168.1.10", "192.168.1.10"} {
		if dial(ip) == nil {
			t.Fatalf("%s was refused below the total", ip)
		}
	}
	if dial("192.168.1.11") != nil || dial("fd00:0:0:2::1") != nil {
		t.Fatal("a connection beyond the total was admitted")
	}
	// The total is full: this machine still gets in, up to the reserve.
	if dial("127.0.0.1") == nil || dial("::1") == nil {
		t.Fatal("loopback was refused while the network filled the total")
	}
	if dial("127.0.0.1") != nil {
		t.Fatal("loopback was admitted beyond its reserve (2)")
	}
	// A closed connection frees the network's count too.
	held[0].Close()
	if dial("fd00::3") != nil {
		t.Fatal("admitted beyond the total (loopback holds its reserve)")
	}
	l.mu.Lock()
	nets := l.nets[netip.MustParsePrefix("fd00::/64")]
	l.mu.Unlock()
	if nets != 3 {
		t.Fatalf("fd00::/64 holds %d connections, want 3", nets)
	}
}

// CR-3: the web servers serve through the web ACL and the connection
// limiter: with the caps lowered, a loopback connection beyond the total
// and its reserve is closed at accept.
func TestServeWebIsLimited(t *testing.T) {
	defer func(c webCaps) { webLimits = c }(webLimits)
	webLimits = webCaps{perClient: 1, perNetwork: 1, total: 1, loopback: 1}
	a := newApp(&config.Config{DataDir: t.TempDir()}, slog.New(slog.DiscardHandler))
	a.web = netutil.NewWebAccess(newTLSEnv(t, "", "").set, a.log)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a.ln.web = []net.Listener{ln}
	var srv sync.WaitGroup
	servers := a.serveWeb(func(_ string, fn func() error) { srv.Go(func() { _ = fn() }) },
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) }))
	defer func() {
		for _, s := range servers {
			_ = s.Close()
		}
		srv.Wait()
	}()
	// request sends a keep-alive request on a new connection and returns
	// the status line of the reply ("" when the connection was closed).
	request := func() (net.Conn, string) {
		t.Helper()
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.WriteString(c, "GET / HTTP/1.1\r\nHost: localhost\r\n\r\n"); err != nil {
			return c, ""
		}
		line, err := bufio.NewReader(c).ReadString('\n')
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			t.Fatal("the connection was neither served nor closed")
		}
		return c, strings.TrimSpace(line)
	}
	for i := range 2 { // the total and loopback's reserve
		if _, status := request(); status != "HTTP/1.1 200 OK" {
			t.Fatalf("connection %d: %q, want it served", i+1, status)
		}
	}
	if _, status := request(); status != "" {
		t.Fatalf("a connection beyond the caps was served: %q", status)
	}
}
