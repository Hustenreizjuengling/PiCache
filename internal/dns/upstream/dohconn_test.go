package upstream

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/settings"
)

// blackholeProxy forwards TCP connections to target. blackhole makes the
// connections open so far swallow everything silently in both directions
// (lost NAT state, a router reboot) while they stay open; connections
// accepted later are forwarded again.
type blackholeProxy struct {
	ln     net.Listener
	target string

	mu    sync.Mutex
	dead  []*atomic.Bool // one per connection accepted so far
	conns []net.Conn
}

func startBlackholeProxy(t *testing.T, target string) *blackholeProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &blackholeProxy{ln: ln, target: target}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			up, err := net.Dial("tcp", target)
			if err != nil {
				_ = c.Close()
				continue
			}
			dead := new(atomic.Bool)
			p.mu.Lock()
			p.dead = append(p.dead, dead)
			p.conns = append(p.conns, c, up)
			p.mu.Unlock()
			go pipe(up, c, dead)
			go pipe(c, up, dead)
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		p.mu.Lock()
		defer p.mu.Unlock()
		for _, c := range p.conns {
			_ = c.Close()
		}
	})
	return p
}

// pipe copies from src to dst until dead is set; from then on it reads
// and drops everything.
func pipe(dst, src net.Conn, dead *atomic.Bool) {
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 && !dead.Load() {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (p *blackholeProxy) blackhole() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, d := range p.dead {
		d.Store(true)
	}
}

func (p *blackholeProxy) accepted() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.dead)
}

// TestDoHReplacesDeadConnection: a DoH connection whose packets are
// dropped silently is pinged and closed by the HTTP/2 health check, and
// a new connection answers again soon (without it, queries would time
// out on the dead connection until the server's stream limit was used
// up by cancelled requests).
func TestDoHReplacesDeadConnection(t *testing.T) {
	d := startDoH(t, func(w http.ResponseWriter, q *dns.Msg) { writeMsg(w, answerA(q, "192.0.2.90", 60)) })
	_, port, _ := net.SplitHostPort(d.srv.Listener.Addr().String())
	p := startBlackholeProxy(t, "127.0.0.1:"+port)
	_, pport, _ := net.SplitHostPort(p.ln.Addr().String())
	st := newStore(t, func(dd *settings.DNS) {
		dd.Upstreams = []string{"https://127.0.0.1:" + pport + "/dns-query"}
		dd.CacheEnabled = false
	})
	opts := testOptions()
	opts.rootCAs = d.pool
	opts.attempt = 300 * time.Millisecond
	opts.dohPingAfter, opts.dohPingTimeout = 200*time.Millisecond, 200*time.Millisecond
	r := newTestResolver(t, st, opts, nil)
	defer r.Close()
	ask := func(name string) error {
		_, _, err := r.Resolve(context.Background(), query(name, dns.TypeA, 1, false), noECS)
		return err
	}
	if err := ask("before.example."); err != nil {
		t.Fatal(err)
	}
	p.blackhole()
	start := time.Now()
	for i := 0; ; i++ {
		err := ask("after" + strconv.Itoa(i) + ".example.")
		if err == nil {
			break
		}
		if time.Since(start) > 5*time.Second {
			t.Fatalf("no answer %v after the connection died (%d connections): %v", time.Since(start), p.accepted(), err)
		}
	}
	if p.accepted() < 2 {
		t.Fatalf("answered without a new connection (%d)", p.accepted())
	}
}

// hangingDial is a DoH dial that never connects: it waits for its context
// and records how many dials run at once and whether one outlived bound.
type hangingDial struct {
	mu        sync.Mutex
	running   int
	max       int
	started   int
	overstays atomic.Int32
	bound     time.Duration
}

func (h *hangingDial) dial(ctx context.Context, _, _ string) (net.Conn, error) {
	h.mu.Lock()
	h.running++
	h.started++
	h.max = max(h.max, h.running)
	h.mu.Unlock()
	select {
	case <-ctx.Done():
	case <-time.After(h.bound):
		h.overstays.Add(1)
	}
	h.mu.Lock()
	h.running--
	h.mu.Unlock()
	return nil, ctx.Err()
}

func (h *hangingDial) stats() (running, maxRunning, started int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.running, h.max, h.started
}

// TestDoHDialsAreBounded: while a DoH upstream is black-holed, a dial
// ends with the attempt timeout even though net/http dials detached from
// the request, and concurrent requests share at most dohMaxConns dials
// instead of each starting its own.
func TestDoHDialsAreBounded(t *testing.T) {
	st := newStore(t, func(dd *settings.DNS) {
		dd.Upstreams = []string{"https://192.0.2.1/dns-query"}
		dd.CacheEnabled = false
	})
	h := &hangingDial{bound: 2 * time.Second}
	opts := testOptions()
	opts.attempt = 300 * time.Millisecond
	opts.dohDial = h.dial
	r := newTestResolver(t, st, opts, nil)
	defer r.Close()
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Go(func() {
			_, _, _ = r.Resolve(context.Background(), query("q"+strconv.Itoa(i)+".example.", dns.TypeA, 1, false), noECS)
		})
	}
	wg.Wait()
	deadline := time.Now().Add(h.bound)
	for {
		running, _, _ := h.stats()
		if running == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d dials still pending after %v", running, h.bound)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n := h.overstays.Load(); n != 0 {
		t.Errorf("%d dials outlived %v", n, h.bound)
	}
	if _, maxRunning, started := h.stats(); maxRunning > dohMaxConns || started == 0 {
		t.Errorf("%d dials at once (%d started), want at most %d", maxRunning, started, dohMaxConns)
	}
}

// TestDoHDialReachesTheNextAddress: when the first bootstrap address of a
// DoH upstream drops the packets silently, a dial still reaches the next
// address (each address but the last gets its share of the dial's time)
// instead of spending all of it on the first, so the upstream recovers
// while that address stays black-holed; a dial of addresses that are all
// black-holed still ends with its timeout.
func TestDoHDialReachesTheNextAddress(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		boot := newBootstrap(nil, 53, false)
		boot.cache["doh.example"] = bootEntry{addrs: []netip.Addr{netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2")},
			expires: time.Now().Add(time.Hour)}
		dead := map[string]bool{"192.0.2.1:443": true}
		var tried []string
		d := &bootDialer{boot: boot, timeout: 3 * time.Second, dial: func(ctx context.Context, _, address string) (net.Conn, error) {
			tried = append(tried, address)
			if dead[address] {
				<-ctx.Done()
				return nil, ctx.Err()
			}
			c, peer := net.Pipe()
			peer.Close()
			return c, nil
		}}
		start := time.Now()
		c, err := d.DialContext(context.Background(), "tcp", "doh.example:443")
		if err != nil {
			t.Fatalf("dial: %v (tried %v)", err, tried)
		}
		c.Close()
		if el := time.Since(start); el != 1500*time.Millisecond {
			t.Errorf("connected after %v, want 1.5 s (half of the dial's time on the first address)", el)
		}

		dead["192.0.2.2:443"] = true
		tried = nil
		start = time.Now()
		if _, err := d.DialContext(context.Background(), "tcp", "doh.example:443"); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("dial of black-holed addresses: %v", err)
		}
		if el := time.Since(start); el != 3*time.Second || len(tried) != 2 {
			t.Errorf("gave up after %v with %v, want 3 s and both addresses", el, tried)
		}
	})
}

// TestEncryptedQueriesArePadded: queries to a DoH upstream carry an EDNS
// Padding option that makes them a multiple of 128 bytes; the padding of
// the reply is not kept (nor cached); plain upstreams get no padding.
func TestEncryptedQueriesArePadded(t *testing.T) {
	var sizes []int
	var mu sync.Mutex
	d := startDoH(t, func(w http.ResponseWriter, q *dns.Msg) {
		b, _ := q.Pack()
		mu.Lock()
		if hasPaddingOpt(q) {
			sizes = append(sizes, len(b))
		} else {
			sizes = append(sizes, -len(b))
		}
		mu.Unlock()
		m := answerA(q, "192.0.2.91", 60)
		m.SetEdns0(1232, false)
		opt := m.IsEdns0()
		opt.Option = append(opt.Option, &dns.EDNS0_PADDING{Padding: make([]byte, 300)})
		writeMsg(w, m)
	})
	st := newStore(t, func(dd *settings.DNS) { dd.Upstreams = []string{d.srv.URL + "/dns-query"} })
	opts := testOptions()
	opts.rootCAs = d.pool
	r := newTestResolver(t, st, opts, nil)
	defer r.Close()
	for _, name := range []string{"a.example.", "a-much-longer-name.with.several.labels.example.", "a.example."} {
		m, _, err := r.Resolve(context.Background(), query(name, dns.TypeA, 1, true), noECS)
		if err != nil {
			t.Fatal(err)
		}
		if hasPaddingOpt(m) {
			t.Fatalf("%s: the reply keeps the upstream's padding", name)
		}
	}
	mu.Lock()
	got := sizes
	mu.Unlock()
	if len(got) != 2 { // the third query is a cache hit
		t.Fatalf("upstream queries %v, want 2", got)
	}
	for _, n := range got {
		if n <= 0 || n%queryPadBlock != 0 {
			t.Errorf("query sizes %v: want padded to multiples of %d", got, queryPadBlock)
		}
	}

	var plainPadded atomic.Int32
	addr := startDNS(t, func(w dns.ResponseWriter, q *dns.Msg) {
		if hasPaddingOpt(q) {
			plainPadded.Add(1)
		}
		_ = w.WriteMsg(answerA(q, "192.0.2.92", 60))
	})
	updateDNS(t, st, func(dd *settings.DNS) { dd.Upstreams = []string{addr.String()} })
	if _, _, err := r.Resolve(context.Background(), query("plain.example.", dns.TypeA, 1, true), noECS); err != nil {
		t.Fatal(err)
	}
	if plainPadded.Load() != 0 {
		t.Error("a plain upstream got a padded query")
	}
}

func hasPaddingOpt(m *dns.Msg) bool {
	if opt := m.IsEdns0(); opt != nil {
		for _, o := range opt.Option {
			if o.Option() == dns.EDNS0PADDING {
				return true
			}
		}
	}
	return false
}

// cutListener cuts the TLS handshake of the connections cut picks (by
// number, from 1; nil: none): it reads the ClientHello and then closes the
// connection ("eof") or resets it ("reset"), as dns.quad9.net does to a
// share of new connections. accepted counts every connection.
type cutListener struct {
	net.Listener
	cut      func(n int32) string
	accepted atomic.Int32
}

func (l *cutListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		n := l.accepted.Add(1)
		if l.cut == nil || l.cut(n) == "" {
			return c, nil
		}
		// The whole ClientHello record is read first: a socket closed with
		// unread data resets the connection instead of closing it.
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		var hdr [5]byte
		if _, err := io.ReadFull(c, hdr[:]); err == nil {
			_, _ = io.CopyN(io.Discard, c, int64(binary.BigEndian.Uint16(hdr[3:])))
		}
		if l.cut(n) == "reset" {
			_ = c.(*net.TCPConn).SetLinger(0)
		}
		_ = c.Close()
	}
}

type netConnKey struct{}

// startClosingDoH serves DoH over HTTP/2 (h2) or HTTP/1.1 and answers
// every request, except that drop(n) for the n-th request (from 1) may
// close its connection without a reply: "eof" closes the TCP connection
// (as dns.quad9.net closes a connection it has served a few minutes; no
// GOAWAY, no TLS close_notify), "reset" resets it, "hang" waits until the
// client gives up. cut (nil: none) cuts the TLS handshake of the n-th
// connection (cutListener). conns counts the connections accepted.
func startClosingDoH(t *testing.T, h2 bool, cut, drop func(n int32) string) (u string, pool *x509.CertPool, conns *atomic.Int32) {
	t.Helper()
	var reqs atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		q := new(dns.Msg)
		if err := q.Unpack(body); err != nil {
			http.Error(w, "bad message", http.StatusBadRequest)
			return
		}
		nc := r.Context().Value(netConnKey{}).(*tls.Conn).NetConn().(*net.TCPConn)
		how := ""
		if drop != nil {
			how = drop(reqs.Add(1))
		}
		switch how {
		case "eof":
			_ = nc.Close()
			return
		case "reset":
			_ = nc.SetLinger(0)
			_ = nc.Close()
			return
		case "hang":
			<-r.Context().Done()
			return
		}
		writeMsg(w, answerA(q, "192.0.2.93", 60))
	}))
	srv.EnableHTTP2 = h2
	cl := &cutListener{Listener: srv.Listener, cut: cut}
	srv.Listener = cl
	srv.Config.ConnContext = func(ctx context.Context, c net.Conn) context.Context { return context.WithValue(ctx, netConnKey{}, c) }
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	pool = x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return srv.URL + "/dns-query", pool, &cl.accepted
}

// TestDoHResendsWhenServerClosesReusedConnection: a server that closes a
// connection (dns.quad9.net does so after a few minutes, without GOAWAY)
// or resets it just as a query arrives on it failed that query with
// "unexpected EOF" or "connection reset by peer": net/http sends a POST
// again only when it surely was not processed. Such a query is sent again
// once, at once, over a fresh connection and within the attempt's
// deadline; a query that fails on a new connection is not.
func TestDoHResendsWhenServerClosesReusedConnection(t *testing.T) {
	resolver := func(t *testing.T, u string, pool *x509.CertPool) *Resolver {
		st := newStore(t, func(d *settings.DNS) { d.Upstreams = []string{u}; d.CacheEnabled = false })
		opts := testOptions()
		opts.rootCAs = pool
		return newTestResolver(t, st, opts, nil)
	}
	ask := func(r *Resolver, i int) error {
		_, _, err := r.Resolve(context.Background(), query("q"+strconv.Itoa(i)+".example.", dns.TypeA, uint16(i), false), noECS)
		return err
	}
	for _, tc := range []struct {
		name, how string
		h2        bool
	}{{"h2 eof", "eof", true}, {"h2 reset", "reset", true}, {"http1 eof", "eof", false}, {"http1 reset", "reset", false}} {
		t.Run(tc.name, func(t *testing.T) {
			u, pool, conns := startClosingDoH(t, tc.h2, nil, func(n int32) string {
				if n == 2 {
					return tc.how
				}
				return ""
			})
			r := resolver(t, u, pool)
			defer r.Close()
			for i := range 3 {
				if err := ask(r, i); err != nil {
					t.Fatalf("query %d: %v", i, err)
				}
			}
			if st := r.Stats(); st[0].Errors != 0 {
				t.Errorf("the closed connection counted as an upstream error: %+v", st[0])
			}
			if n := conns.Load(); n != 2 {
				t.Errorf("connections = %d, want 2", n)
			}
		})
	}
	t.Run("new connection", func(t *testing.T) {
		u, pool, conns := startClosingDoH(t, true, nil, func(n int32) string {
			if n == 1 {
				return "eof"
			}
			return ""
		})
		r := resolver(t, u, pool)
		defer r.Close()
		if err := ask(r, 0); err == nil {
			t.Fatal("a query that failed on a new connection was sent again")
		}
		if n := conns.Load(); n != 1 {
			t.Errorf("connections = %d, want 1", n)
		}
	})
	t.Run("deadline", func(t *testing.T) {
		u, pool, _ := startClosingDoH(t, true, nil, func(n int32) string {
			return map[int32]string{2: "eof", 3: "hang"}[n]
		})
		r := resolver(t, u, pool)
		defer r.Close()
		if err := ask(r, 0); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		if err := ask(r, 1); err == nil {
			t.Fatal("answered by a server that does not answer")
		}
		if el := time.Since(start); el > testOptions().attempt*19/10 {
			t.Errorf("gave up after %v, want the attempt timeout %v", el, testOptions().attempt)
		}
	})
}

// TestCutHandshake: only a closed or reset connection counts as a cut
// handshake; certificate errors, alerts, timeouts and refused connections
// do not.
func TestCutHandshake(t *testing.T) {
	reset := &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", errConnReset)}
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{io.EOF, true},
		{fmt.Errorf("handshake: %w", io.ErrUnexpectedEOF), true},
		{reset, true},
		{&url.Error{Op: "Post", URL: "https://dns.example/dns-query", Err: reset}, true},
		{cutHandshakeError{io.EOF}, true},
		{&net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}, false},
		{&tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}, false},
		{tls.AlertError(40), false},
		{context.DeadlineExceeded, false},
		{&net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}, false},
		{errors.New("tls: handshake timeout"), false},
	} {
		if got := cutHandshake(tc.err); got != tc.want {
			t.Errorf("cutHandshake(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

// TestDoHResendsWhenServerCutsHandshake: dns.quad9.net closes or resets
// the TLS handshake of a share of new connections, and the query that
// needed the connection failed ("EOF", "connection reset by peer"). It is
// sent once more over another new connection, within the attempt's
// deadline. Not after a certificate error, and at most once per query: a
// query already sent again after a closed reused connection is not sent a
// third time.
func TestDoHResendsWhenServerCutsHandshake(t *testing.T) {
	resolver := func(t *testing.T, u string, pool *x509.CertPool) *Resolver {
		st := newStore(t, func(d *settings.DNS) { d.Upstreams = []string{u}; d.CacheEnabled = false })
		opts := testOptions()
		opts.rootCAs = pool
		return newTestResolver(t, st, opts, nil)
	}
	ask := func(r *Resolver, i int) error {
		_, _, err := r.Resolve(context.Background(), query("cut"+strconv.Itoa(i)+".example.", dns.TypeA, uint16(i), false), noECS)
		return err
	}
	nth := func(n int32, how string) func(int32) string {
		return func(i int32) string {
			if i == n {
				return how
			}
			return ""
		}
	}
	for _, tc := range []struct {
		name, how string
		h2        bool
	}{{"h2 eof", "eof", true}, {"h2 reset", "reset", true}, {"http1 eof", "eof", false}, {"http1 reset", "reset", false}} {
		t.Run(tc.name, func(t *testing.T) {
			u, pool, conns := startClosingDoH(t, tc.h2, nth(1, tc.how), nil)
			r := resolver(t, u, pool)
			defer r.Close()
			for i := range 2 {
				if err := ask(r, i); err != nil {
					t.Fatalf("query %d: %v", i, err)
				}
			}
			if st := r.Stats(); st[0].Errors != 0 {
				t.Errorf("the cut handshake counted as an upstream error: %+v", st[0])
			}
			if n := conns.Load(); n != 2 {
				t.Errorf("connections = %d, want 2", n)
			}
		})
	}
	t.Run("bad certificate", func(t *testing.T) {
		u, _, conns := startClosingDoH(t, true, nil, nil)
		r := resolver(t, u, x509.NewCertPool())
		defer r.Close()
		if err := ask(r, 0); err == nil {
			t.Fatal("an untrusted certificate was accepted")
		}
		if n := conns.Load(); n != 1 {
			t.Errorf("connections = %d, want 1 (no resend)", n)
		}
	})
	t.Run("once per query", func(t *testing.T) {
		u, pool, conns := startClosingDoH(t, true, nth(2, "reset"), nth(2, "eof"))
		r := resolver(t, u, pool)
		defer r.Close()
		if err := ask(r, 0); err != nil {
			t.Fatal(err)
		}
		if err := ask(r, 1); err == nil {
			t.Fatal("answered although the resend's handshake was cut")
		}
		if n := conns.Load(); n != 2 {
			t.Errorf("connections = %d, want 2 (one resend)", n)
		}
	})
}
