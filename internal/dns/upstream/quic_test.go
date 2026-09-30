package upstream

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"

	"github.com/hustenreizjuengling/picache/internal/settings"
)

// doqServer is an RFC 9250 test server (quic-go's server in tests only).
type doqServer struct {
	addr    netip.AddrPort
	pool    *x509.CertPool
	conns   atomic.Int32
	active  atomic.Int32
	idZero  atomic.Int32
	used0RT atomic.Int32
	// answer builds the reply to a query; a nil reply closes the stream
	// after raw is written (malformed replies).
	answer func(q *dns.Msg) (reply *dns.Msg, raw []byte)
	hold   chan struct{} // closed: queries are answered (nil: at once)
	// written, when set, runs after the reply is written, before the
	// stream's send side is closed.
	written func(str *quic.Stream)
	// cert replaces the self-signed test certificate (pool then trusts
	// nothing).
	cert *tls.Certificate
}

func startDoQ(t *testing.T, s *doqServer, idle time.Duration) *doqServer {
	t.Helper()
	cert, pool := testCert(t)
	s.pool = pool
	if s.cert != nil {
		cert, s.pool = *s.cert, x509.NewCertPool()
	}
	conf := &quic.Config{MaxIdleTimeout: idle, MaxIncomingStreams: 200, Allow0RTT: true}
	ln, err := quic.ListenAddr("127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"doq"}}, conf)
	if err != nil {
		t.Fatal(err)
	}
	s.addr = netip.MustParseAddrPort(ln.Addr().String())
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	t.Cleanup(func() {
		cancel()
		_ = ln.Close()
		wg.Wait()
	})
	wg.Go(func() {
		for {
			conn, err := ln.Accept(ctx)
			if err != nil {
				return
			}
			s.conns.Add(1)
			if conn.ConnectionState().Used0RTT {
				s.used0RT.Add(1)
			}
			wg.Go(func() {
				for {
					str, err := conn.AcceptStream(ctx)
					if err != nil {
						return
					}
					wg.Go(func() { s.serveStream(ctx, str) })
				}
			})
		}
	})
	return s
}

func (s *doqServer) serveStream(ctx context.Context, str *quic.Stream) {
	defer str.Close()
	var hdr [2]byte
	if _, err := io.ReadFull(str, hdr[:]); err != nil {
		return
	}
	body := make([]byte, binary.BigEndian.Uint16(hdr[:]))
	if _, err := io.ReadFull(str, body); err != nil {
		return
	}
	q := new(dns.Msg)
	if q.Unpack(body) != nil {
		return
	}
	if q.Id == 0 {
		s.idZero.Add(1)
	}
	s.active.Add(1)
	if s.hold != nil {
		select {
		case <-s.hold:
		case <-ctx.Done():
		}
	}
	s.active.Add(-1)
	reply, raw := s.answer(q)
	if reply != nil {
		raw, _ = reply.Pack()
		out := make([]byte, 2+len(raw))
		binary.BigEndian.PutUint16(out, uint16(len(raw)))
		copy(out[2:], raw)
		raw = out
	}
	_, _ = str.Write(raw)
	if s.written != nil {
		s.written(str)
	}
}

func TestDoQ(t *testing.T) {
	s := startDoQ(t, &doqServer{answer: func(q *dns.Msg) (*dns.Msg, []byte) { return answerA(q, "192.0.2.99", 60), nil }}, time.Minute)
	up := "quic://" + s.addr.String()
	st := newStore(t, func(d *settings.DNS) { d.Upstreams = []string{up}; d.CacheEnabled = false })
	opts := testOptions()
	opts.rootCAs = s.pool
	r := newTestResolver(t, st, opts, nil)
	defer r.Close()
	for i := range 3 {
		m, info, err := r.Resolve(context.Background(), query("doq"+strconv.Itoa(i)+".example.", dns.TypeA, 4242, false), noECS)
		if err != nil {
			t.Fatal(err)
		}
		if ip, _ := firstA(t, m); ip != "192.0.2.99" || m.Id != 4242 || info.Upstream != up {
			t.Fatalf("answer %s id %d info %+v", ip, m.Id, info)
		}
	}
	if s.conns.Load() != 1 || s.idZero.Load() != 3 || s.used0RT.Load() != 0 {
		t.Fatalf("connections %d, id-0 queries %d, 0-RTT %d", s.conns.Load(), s.idZero.Load(), s.used0RT.Load())
	}
	if stats := r.Stats(); len(stats) != 1 || stats[0].Name != up || stats[0].Upstream != up {
		t.Fatalf("stats %+v", stats)
	}
}

// TestDoQStreamBound: at most 64 queries are in flight on one connection;
// the others wait (bounded by their attempt).
func TestDoQStreamBound(t *testing.T) {
	s := startDoQ(t, &doqServer{answer: func(q *dns.Msg) (*dns.Msg, []byte) { return answerA(q, "192.0.2.1", 60), nil },
		hold: make(chan struct{})}, time.Minute)
	spec, err := settings.ParseUpstream("quic://" + s.addr.String())
	if err != nil {
		t.Fatal(err)
	}
	tr := newDoQ(spec, nil, encOpts{roots: s.pool}, time.Minute)
	defer tr.close()
	const n = doqMaxStreams + 6
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			q := newQuery("bound"+strconv.Itoa(i)+".example.", dns.TypeA, dns.ClassINET, false)
			wire, _ := q.Pack()
			m, err := tr.exchange(ctx, q, wire)
			if err == nil && m.Id != q.Id {
				err = errors.New("id not restored")
			}
			errs <- err
		})
	}
	deadline := time.Now().Add(10 * time.Second)
	for s.active.Load() < doqMaxStreams && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	if got := s.active.Load(); got != doqMaxStreams {
		t.Fatalf("%d queries in flight, want %d", got, doqMaxStreams)
	}
	close(s.hold)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

// TestDoQIdleAndErrors: an idle connection closes and is dialled again; a
// truncated or oversized reply fails the attempt.
func TestDoQIdleAndErrors(t *testing.T) {
	var mode atomic.Int32
	s := startDoQ(t, &doqServer{answer: func(q *dns.Msg) (*dns.Msg, []byte) {
		switch mode.Load() {
		case 1:
			return nil, []byte{0, 40, 1, 2, 3} // 40 bytes announced, 3 sent
		case 2:
			return nil, []byte{0, 5, 1, 2, 3, 4, 5} // shorter than a header
		}
		return answerA(q, "192.0.2.1", 60), nil
	}}, 300*time.Millisecond)
	spec, _ := settings.ParseUpstream("quic://" + s.addr.String())
	tr := newDoQ(spec, nil, encOpts{roots: s.pool}, 300*time.Millisecond)
	defer tr.close()
	ask := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		q := newQuery("idle.example.", dns.TypeA, dns.ClassINET, false)
		wire, _ := q.Pack()
		_, err := tr.exchange(ctx, q, wire)
		return err
	}
	if err := ask(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	if err := ask(); err != nil {
		t.Fatal(err)
	}
	if s.conns.Load() != 2 {
		t.Fatalf("connections %d, want 2 (the idle one closed)", s.conns.Load())
	}
	for _, m := range []int32{1, 2} {
		mode.Store(m)
		if err := ask(); err == nil {
			t.Fatalf("mode %d: malformed reply accepted", m)
		}
	}
}

// TestDoQBadReplyAbandonsStream: a reply whose length is below a DNS
// header fails the attempt and abandons the stream's receive side
// (STOP_SENDING reaches the server), so quic-go can forget the stream
// although the connection stays in use.
func TestDoQBadReplyAbandonsStream(t *testing.T) {
	stopped := make(chan error, 1)
	s := startDoQ(t, &doqServer{
		answer: func(q *dns.Msg) (*dns.Msg, []byte) { return nil, []byte{0, 0, 1, 2, 3} },
		written: func(str *quic.Stream) {
			select {
			case <-str.Context().Done(): // the peer sent STOP_SENDING
				stopped <- context.Cause(str.Context())
			case <-time.After(5 * time.Second):
				stopped <- errors.New("the client never abandoned the stream")
			}
		},
	}, time.Minute)
	spec, _ := settings.ParseUpstream("quic://" + s.addr.String())
	tr := newDoQ(spec, nil, encOpts{roots: s.pool}, time.Minute)
	defer tr.close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	q := newQuery("short.example.", dns.TypeA, dns.ClassINET, false)
	wire, _ := q.Pack()
	if _, err := tr.exchange(ctx, q, wire); !errors.Is(err, errShortReply) {
		t.Fatalf("err %v, want %v", err, errShortReply)
	}
	var serr *quic.StreamError
	if err := <-stopped; !errors.As(err, &serr) || !serr.Remote || serr.ErrorCode != doqErrCancelled {
		t.Fatalf("stream end %v, want STOP_SENDING with DOQ_REQUEST_CANCELLED", err)
	}
}

// startH3 serves handler over HTTP/3 on 127.0.0.1 (quic-go's server in
// tests only).
func startH3(t *testing.T, handler http.HandlerFunc) (netip.AddrPort, *x509.CertPool) {
	t.Helper()
	cert, pool := testCert(t)
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http3.Server{Handler: handler, TLSConfig: http3.ConfigureTLSConfig(&tls.Config{Certificates: []tls.Certificate{cert}})}
	done := make(chan struct{})
	go func() { _ = srv.Serve(pc); close(done) }()
	t.Cleanup(func() { _ = srv.Close(); _ = pc.Close(); <-done })
	return netip.MustParseAddrPort(pc.LocalAddr().String()), pool
}

func TestH3(t *testing.T) {
	var posts, idZero atomic.Int32
	addr, pool := startH3(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, "/dns-query", http.StatusFound)
			return
		case "/big":
			w.Header().Set("Content-Type", dohMediaType)
			_, _ = w.Write(make([]byte, maxDoHBody+10))
			return
		}
		if r.Method != http.MethodPost || r.ProtoMajor != 3 || r.Header.Get("Content-Type") != dohMediaType {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		posts.Add(1)
		body, _ := io.ReadAll(r.Body)
		q := new(dns.Msg)
		if q.Unpack(body) != nil {
			http.Error(w, "bad message", http.StatusBadRequest)
			return
		}
		if q.Id == 0 {
			idZero.Add(1)
		}
		writeMsg(w, answerA(q, "192.0.2.33", 60))
	})
	up := "h3://" + addr.String() + "/dns-query"
	st := newStore(t, func(d *settings.DNS) { d.Upstreams = []string{up}; d.CacheEnabled = false })
	opts := testOptions()
	opts.rootCAs = pool
	r := newTestResolver(t, st, opts, nil)
	defer r.Close()
	m, info, err := r.Resolve(context.Background(), query("h3.example.", dns.TypeA, 77, false), noECS)
	if err != nil {
		t.Fatal(err)
	}
	if ip, _ := firstA(t, m); ip != "192.0.2.33" || m.Id != 77 || info.Upstream != up || posts.Load() != 1 || idZero.Load() != 1 {
		t.Fatalf("answer %s id %d info %+v posts %d id0 %d", ip, m.Id, info, posts.Load(), idZero.Load())
	}
	for path, want := range map[string]string{"/redirect": "HTTP 302", "/big": "response too large"} {
		res := r.Test(context.Background(), "h3://"+addr.String()+path)
		if res.OK || !strings.Contains(res.Error, want) {
			t.Fatalf("%s: %+v", path, res)
		}
	}
}

// TestH3SharedDial: http3.Transport dials a host once, with the context of
// the request that needed the connection, for every request waiting for
// it. A cancelled first request (a parallel loser) must not fail the
// others (they used to fail with "context canceled", counted as errors of
// a healthy upstream); closing the transport ends the dial.
func TestH3SharedDial(t *testing.T) {
	addr, pool := startH3(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		q := new(dns.Msg)
		if q.Unpack(body) != nil {
			http.Error(w, "bad message", http.StatusBadRequest)
			return
		}
		writeMsg(w, answerA(q, "192.0.2.34", 60))
	})
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	slow := func(ctx context.Context, a string, tc *tls.Config, qc *quic.Config) (*quic.Conn, error) {
		once.Do(func() { close(started) })
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return quic.DialAddr(ctx, a, tc, qc)
	}
	life, stop := context.WithCancel(context.Background())
	defer stop()
	tr := &http3.Transport{
		TLSClientConfig: clientTLS(addr.Addr().String(), tls.VersionTLS13, encOpts{roots: pool}, nil),
		QUICConfig:      quicConfig(time.Minute, h3MaxUniStreams),
		Dial:            sharedDial(life, slow),
	}
	defer tr.Close()
	client := &http.Client{Transport: tr}
	u := "https://" + addr.String() + "/dns-query"
	q := query("shared.example.", dns.TypeA, 9, false)
	wire, err := q.Pack()
	if err != nil {
		t.Fatal(err)
	}

	first, cancelFirst := context.WithCancel(context.Background())
	firstErr := make(chan error, 1)
	go func() {
		_, err := exchangeHTTP(first, client, u, q, wire)
		firstErr <- err
	}()
	<-started
	type result struct {
		m   *dns.Msg
		err error
	}
	second := make(chan result, 1)
	go func() {
		m, err := exchangeHTTP(context.Background(), client, u, q, wire)
		second <- result{m, err}
	}()
	cancelFirst()
	if err := <-firstErr; err == nil {
		t.Fatal("the cancelled request got an answer")
	}
	close(release)
	select {
	case r := <-second:
		if r.err != nil {
			t.Fatalf("the waiting request failed with the cancellation of another one: %v", r.err)
		}
		if ip, _ := firstA(t, r.m); ip != "192.0.2.34" {
			t.Fatalf("answer %s", ip)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the waiting request did not finish")
	}

	// Closing the transport (life) ends a dial in progress.
	life2, stop2 := context.WithCancel(context.Background())
	dial := sharedDial(life2, func(ctx context.Context, _ string, _ *tls.Config, _ *quic.Config) (*quic.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	done := make(chan error, 1)
	go func() {
		_, err := dial(context.Background(), addr.String(), nil, nil)
		done <- err
	}()
	stop2()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("dial after close: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("closing did not end the dial")
	}
}

// TestH3ResendsWhenServerClosesReusedConnection: an HTTP/3 server that
// closes a connection just as a query arrives on it failed that query
// ("http3: parsing frame failed: H3_NO_ERROR"): quic-go sends a request
// again only when its stream could not be opened or was rejected. As over
// HTTP/2, the query is sent again once over a fresh connection.
func TestH3ResendsWhenServerClosesReusedConnection(t *testing.T) {
	cert, pool := testCert(t)
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	type quicConnKey struct{}
	var reqs, conns atomic.Int32
	srv := &http3.Server{
		TLSConfig: http3.ConfigureTLSConfig(&tls.Config{Certificates: []tls.Certificate{cert}}),
		ConnContext: func(ctx context.Context, c *quic.Conn) context.Context {
			conns.Add(1)
			return context.WithValue(ctx, quicConnKey{}, c)
		},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			q := new(dns.Msg)
			if q.Unpack(body) != nil {
				http.Error(w, "bad message", http.StatusBadRequest)
				return
			}
			if reqs.Add(1) == 2 {
				_ = r.Context().Value(quicConnKey{}).(*quic.Conn).CloseWithError(0x100, "") // H3_NO_ERROR
				return
			}
			writeMsg(w, answerA(q, "192.0.2.35", 60))
		}),
	}
	done := make(chan struct{})
	go func() { _ = srv.Serve(pc); close(done) }()
	t.Cleanup(func() { _ = srv.Close(); _ = pc.Close(); <-done })
	st := newStore(t, func(d *settings.DNS) {
		d.Upstreams = []string{"h3://" + pc.LocalAddr().String() + "/dns-query"}
		d.CacheEnabled = false
	})
	opts := testOptions()
	opts.rootCAs = pool
	r := newTestResolver(t, st, opts, nil)
	defer r.Close()
	for i := range 3 {
		if _, _, err := r.Resolve(context.Background(), query("h3q"+strconv.Itoa(i)+".example.", dns.TypeA, uint16(i), false), noECS); err != nil {
			t.Fatalf("query %d: %v", i, err)
		}
	}
	if st := r.Stats(); st[0].Errors != 0 {
		t.Errorf("the closed connection counted as an upstream error: %+v", st[0])
	}
	if n := conns.Load(); n != 2 {
		t.Errorf("connections = %d, want 2", n)
	}
}

// TestStampPins: the certificate hashes of a stamp are enforced in addition
// to the normal verification.
func TestStampPins(t *testing.T) {
	d := startDoH(t, func(w http.ResponseWriter, q *dns.Msg) { writeMsg(w, answerA(q, "192.0.2.7", 60)) })
	cert := d.srv.Certificate()
	good := sha256.Sum256(cert.RawTBSCertificate)
	port, _ := strconv.Atoi(strings.TrimPrefix(d.srv.URL[strings.LastIndex(d.srv.URL, ":"):], ":"))
	stamp := func(hash []byte) string {
		b := []byte{0x02, 0, 0, 0, 0, 0, 0, 0, 0}
		addr := "127.0.0.1:" + strconv.Itoa(port)
		b = append(b, byte(len(addr)))
		b = append(b, addr...)
		b = append(b, byte(len(hash)))
		b = append(b, hash...)
		host := "example.com"
		b = append(b, byte(len(host)))
		b = append(b, host...)
		b = append(b, byte(len("/dns-query")))
		b = append(b, "/dns-query"...)
		return "sdns://" + base64.RawURLEncoding.EncodeToString(b)
	}
	bad := sha256.Sum256([]byte("other"))
	for _, tc := range []struct {
		name  string
		up    string
		roots *x509.CertPool
		ok    bool
		want  string
	}{
		{"listed hash", stamp(good[:]), d.pool, true, ""},
		{"no hashes", stamp(nil), d.pool, true, ""},
		{"hash not in the chain", stamp(bad[:]), d.pool, false, "hashes"},
		{"untrusted chain with a listed hash", stamp(good[:]), x509.NewCertPool(), false, "certificate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore(t, func(dd *settings.DNS) { dd.Upstreams = []string{tc.up} })
			opts := testOptions()
			opts.rootCAs = tc.roots
			r := newTestResolver(t, st, opts, nil)
			defer r.Close()
			res := r.Test(context.Background(), tc.up)
			if res.OK != tc.ok || (!tc.ok && !strings.Contains(res.Error, tc.want)) {
				t.Fatalf("%+v", res)
			}
			if stats := r.Stats(); len(stats) != 1 || stats[0].Name != "sdns:doh:example.com" || stats[0].Upstream != tc.up {
				t.Fatalf("stats %+v", stats)
			}
		})
	}
}

// TestClockGuardSkipsQUICAndStamps: only plain upstreams given by IP
// address (and the bootstrap servers) are asked while the clock guard is
// active; quic://, h3:// and stamps count as encrypted.
func TestClockGuardSkipsQUICAndStamps(t *testing.T) {
	stamp := "sdns://" + base64.RawURLEncoding.EncodeToString(append([]byte{0x03, 0, 0, 0, 0, 0, 0, 0, 0, 7},
		append([]byte("9.9.9.9"), append([]byte{0, 13}, "dns.quad9.net"...)...)...))
	st := newStore(t, func(d *settings.DNS) {
		d.Upstreams = []string{"quic://9.9.9.9", "h3://9.9.9.9/dns-query", stamp, "192.0.2.53"}
		d.Bootstrap = []string{"192.0.2.1"}
	})
	opts := testOptions()
	opts.buildDate = time.Now().Add(24 * time.Hour)
	r := newTestResolver(t, st, opts, nil)
	defer r.Close()
	if !r.ClockGuard() {
		t.Fatal("clock guard not active")
	}
	names := r.def.Load().guard.names()
	if strings.Join(names, ",") != "192.0.2.53,192.0.2.1:53" {
		t.Fatalf("guard set %v", names)
	}
}
