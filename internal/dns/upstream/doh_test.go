package upstream

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/settings"
)

// dohServer is an RFC 8484 server on an httptest TLS listener with HTTP/2.
type dohServer struct {
	srv    *httptest.Server
	pool   *x509.CertPool
	h2     atomic.Int32
	idZero atomic.Int32
}

func startDoH(t *testing.T, answer func(w http.ResponseWriter, q *dns.Msg)) *dohServer {
	t.Helper()
	d := &dohServer{}
	d.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/dns-query" ||
			r.Header.Get("Content-Type") != dohMediaType || r.Header.Get("Accept") != dohMediaType {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if r.ProtoMajor == 2 {
			d.h2.Add(1)
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, 65536))
		q := new(dns.Msg)
		if err := q.Unpack(body); err != nil {
			http.Error(w, "bad message", http.StatusBadRequest)
			return
		}
		if q.Id == 0 {
			d.idZero.Add(1)
		}
		answer(w, q)
	}))
	d.srv.EnableHTTP2 = true
	d.srv.Config.ErrorLog = log.New(io.Discard, "", 0) // expected handshake failures
	d.srv.StartTLS()
	t.Cleanup(d.srv.Close)
	d.pool = x509.NewCertPool()
	d.pool.AddCert(d.srv.Certificate())
	return d
}

func writeMsg(w http.ResponseWriter, m *dns.Msg) {
	b, _ := m.Pack()
	w.Header().Set("Content-Type", dohMediaType)
	_, _ = w.Write(b)
}

func TestDoH(t *testing.T) {
	d := startDoH(t, func(w http.ResponseWriter, q *dns.Msg) { writeMsg(w, answerA(q, "192.0.2.80", 60)) })
	st := newStore(t, func(dd *settings.DNS) { dd.Upstreams = []string{d.srv.URL + "/dns-query"} })
	opts := testOptions()
	opts.rootCAs = d.pool
	r := newTestResolver(t, st, opts, nil)
	defer r.Close()
	for i := range 2 {
		m, info, err := r.Resolve(context.Background(), query("doh"+strconv.Itoa(i)+".example.", dns.TypeA, 999, false))
		if err != nil {
			t.Fatal(err)
		}
		if ip, _ := firstA(t, m); ip != "192.0.2.80" || m.Id != 999 || info.Upstream != d.srv.URL+"/dns-query" {
			t.Fatalf("answer %s id %d info %+v", ip, m.Id, info)
		}
	}
	if d.h2.Load() != 2 || d.idZero.Load() != 2 {
		t.Errorf("h2 requests %d, id-0 queries %d; want 2/2", d.h2.Load(), d.idZero.Load())
	}
}

func TestDoHRejectsBadResponses(t *testing.T) {
	tests := []struct {
		name    string
		answer  func(w http.ResponseWriter, q *dns.Msg)
		wantErr string
	}{
		{name: "http status", wantErr: "HTTP 503", answer: func(w http.ResponseWriter, _ *dns.Msg) {
			http.Error(w, "busy", http.StatusServiceUnavailable)
		}},
		{name: "content type", wantErr: "unexpected content type", answer: func(w http.ResponseWriter, q *dns.Msg) {
			b, _ := answerA(q, "192.0.2.1", 60).Pack()
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write(b)
		}},
		{name: "oversized body", wantErr: "too large", answer: func(w http.ResponseWriter, _ *dns.Msg) {
			w.Header().Set("Content-Type", dohMediaType)
			_, _ = w.Write(make([]byte, maxDoHBody+1))
		}},
		{name: "non-zero id", wantErr: "ID", answer: func(w http.ResponseWriter, q *dns.Msg) {
			m := answerA(q, "192.0.2.1", 60)
			m.Id = 77
			writeMsg(w, m)
		}},
		{name: "question mismatch", wantErr: "does not match", answer: func(w http.ResponseWriter, q *dns.Msg) {
			m := answerA(q, "192.0.2.1", 60)
			m.Question[0].Name = "other.example."
			writeMsg(w, m)
		}},
		{name: "garbage", wantErr: "malformed", answer: func(w http.ResponseWriter, _ *dns.Msg) {
			w.Header().Set("Content-Type", dohMediaType+"; charset=binary")
			_, _ = w.Write([]byte("0123456789abcdef"))
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := startDoH(t, tc.answer)
			st := newStore(t, nil)
			opts := testOptions()
			opts.rootCAs = d.pool
			r := newTestResolver(t, st, opts, nil)
			defer r.Close()
			res := r.Test(context.Background(), d.srv.URL+"/dns-query")
			if res.OK || !strings.Contains(res.Error, tc.wantErr) {
				t.Fatalf("result = %+v, want error containing %q", res, tc.wantErr)
			}
		})
	}
}

func TestDoHUntrustedCertificateFails(t *testing.T) {
	d := startDoH(t, func(w http.ResponseWriter, q *dns.Msg) { writeMsg(w, answerA(q, "192.0.2.80", 60)) })
	st := newStore(t, nil)
	r := newTestResolver(t, st, testOptions(), nil) // system roots only
	defer r.Close()
	res := r.Test(context.Background(), d.srv.URL+"/dns-query")
	if res.OK || !strings.Contains(res.Error, "certificate") {
		t.Fatalf("result = %+v", res)
	}
	if r.ClockGuard() {
		t.Error("a certificate error must not activate the clock guard")
	}
}

// TestDoHHostnameViaBootstrap resolves the DoH hostname through the
// bootstrap server only (the httptest certificate is valid for example.com).
func TestDoHHostnameViaBootstrap(t *testing.T) {
	d := startDoH(t, func(w http.ResponseWriter, q *dns.Msg) { writeMsg(w, answerA(q, "192.0.2.81", 60)) })
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(d.srv.URL, "https://"))
	var bootQueries atomic.Int32
	boot := startDNS(t, func(w dns.ResponseWriter, q *dns.Msg) {
		bootQueries.Add(1)
		m := new(dns.Msg)
		m.SetReply(q)
		if q.Question[0].Name == "example.com." && q.Question[0].Qtype == dns.TypeA {
			m.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: "example.com.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 300}, A: net.IPv4(127, 0, 0, 1)}}
		}
		_ = w.WriteMsg(m)
	})
	url := "https://example.com:" + port + "/dns-query"
	st := newStore(t, func(dd *settings.DNS) {
		dd.Upstreams = []string{url}
		dd.Bootstrap = []string{boot.Addr().String()}
	})
	opts := testOptions()
	opts.rootCAs = d.pool
	opts.plainPort = int(boot.Port())
	r := newTestResolver(t, st, opts, nil)
	defer r.Close()
	for i := range 3 {
		m, _, err := r.Resolve(context.Background(), query("q"+strconv.Itoa(i)+".example.", dns.TypeA, 1, false))
		if err != nil {
			t.Fatal(err)
		}
		if ip, _ := firstA(t, m); ip != "192.0.2.81" {
			t.Fatalf("answer %s", ip)
		}
	}
	if n := bootQueries.Load(); n != 2 { // A + AAAA once, then cached / connection reused
		t.Errorf("bootstrap queries = %d, want 2", n)
	}
}

func TestDoTWithConnectionReuse(t *testing.T) {
	cert, pool := testCert(t)
	var accepted, alpnOK atomic.Int32
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"dot"}})
	if err != nil {
		t.Fatal(err)
	}
	srv := &dns.Server{Listener: countingListener{ln, &accepted}, Net: "tcp-tls",
		Handler: dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
			if cs, ok := w.(dns.ConnectionStater); ok && cs.ConnectionState().NegotiatedProtocol == "dot" {
				alpnOK.Add(1)
			}
			_ = w.WriteMsg(answerA(q, "192.0.2.85", 60))
		})}
	serve(t, srv)

	st := newStore(t, func(d *settings.DNS) { d.Upstreams = []string{"tls://" + ln.Addr().String()}; d.CacheEnabled = false })
	opts := testOptions()
	opts.rootCAs = pool
	r := newTestResolver(t, st, opts, nil)
	defer r.Close()
	for i := range 3 {
		m, _, err := r.Resolve(context.Background(), query("dot.example.", dns.TypeA, uint16(i), false))
		if err != nil {
			t.Fatal(err)
		}
		if ip, _ := firstA(t, m); ip != "192.0.2.85" {
			t.Fatalf("answer %s", ip)
		}
	}
	if accepted.Load() != 1 || alpnOK.Load() != 3 {
		t.Errorf("connections %d (want 1, reused), ALPN dot %d (want 3)", accepted.Load(), alpnOK.Load())
	}
}

func TestDoTReconnectsAfterServerClosedIdleConnection(t *testing.T) {
	cert, pool := testCert(t)
	var accepted atomic.Int32
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"dot"}})
	if err != nil {
		t.Fatal(err)
	}
	// One query per connection, then the server closes it.
	srv := &dns.Server{Listener: countingListener{ln, &accepted}, Net: "tcp-tls", MaxTCPQueries: 1,
		Handler: dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) { _ = w.WriteMsg(answerA(q, "192.0.2.86", 60)) })}
	serve(t, srv)
	st := newStore(t, func(d *settings.DNS) { d.Upstreams = []string{"tls://" + ln.Addr().String()}; d.CacheEnabled = false })
	opts := testOptions()
	opts.rootCAs = pool
	r := newTestResolver(t, st, opts, nil)
	defer r.Close()
	for i := range 3 {
		if _, _, err := r.Resolve(context.Background(), query("dot.example.", dns.TypeA, uint16(i), false)); err != nil {
			t.Fatalf("query %d: %v", i, err)
		}
	}
	if st := r.Stats(); st[0].Errors != 0 {
		t.Errorf("a stale pooled connection counted as upstream error: %+v", st[0])
	}
	if accepted.Load() != 3 {
		t.Errorf("connections = %d, want 3", accepted.Load())
	}
}

type countingListener struct {
	net.Listener
	n *atomic.Int32
}

func (l countingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.n.Add(1)
	}
	return c, err
}
