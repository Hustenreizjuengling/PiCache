package upstream

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/netip"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// newStore opens a settings store in a temp dir and applies mutate to the
// DNS section. Call it outside synctest bubbles.
func newStore(t *testing.T, mutate func(d *settings.DNS)) *settings.Store {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "settings.db"), 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	st, err := settings.Open(context.Background(), d, discardLog())
	if err != nil {
		t.Fatal(err)
	}
	if mutate != nil {
		updateDNS(t, st, mutate)
	}
	return st
}

func updateDNS(t *testing.T, st *settings.Store, mutate func(d *settings.DNS)) {
	t.Helper()
	if _, err := st.Update(context.Background(), func(a *settings.All) error {
		mutate(&a.DNS)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func testOptions() options {
	return options{plainPort: 53, attempt: time.Second}
}

// newTestResolver creates a resolver whose upstreams named in fakes use the
// fake transports; all others use the real ones.
func newTestResolver(t *testing.T, st *settings.Store, opts options, fakes map[string]*fakeTransport) *Resolver {
	t.Helper()
	if fakes != nil {
		opts.transport = func(spec settings.UpstreamSpec) transport {
			if f := fakes[spec.Raw]; f != nil {
				return f
			}
			return nil
		}
	}
	return newResolver(st, discardLog(), opts)
}

// start runs r.Start in the background; the returned func stops it and
// closes r.
func start(r *Resolver) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		r.Start(ctx)
		close(done)
	}()
	return func() {
		cancel()
		<-done
		_ = r.Close()
	}
}

// fakeTransport answers with fn and records the queries it saw.
type fakeTransport struct {
	fn func(ctx context.Context, q *dns.Msg) (*dns.Msg, error)

	mu      sync.Mutex
	queries []*dns.Msg
}

func (f *fakeTransport) exchange(ctx context.Context, q *dns.Msg, _ []byte) (*dns.Msg, error) {
	f.mu.Lock()
	f.queries = append(f.queries, q.Copy())
	f.mu.Unlock()
	return f.fn(ctx, q)
}

func (f *fakeTransport) close() {}

func (f *fakeTransport) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.queries)
}

// replyA answers q with one A record (ttl) for every question.
func replyA(ip string, ttl uint32) func(context.Context, *dns.Msg) (*dns.Msg, error) {
	return func(_ context.Context, q *dns.Msg) (*dns.Msg, error) { return answerA(q, ip, ttl), nil }
}

func answerA(q *dns.Msg, ip string, ttl uint32) *dns.Msg {
	m := new(dns.Msg)
	m.SetReply(q)
	m.RecursionAvailable = true
	if q.Question[0].Qtype == dns.TypeA {
		m.Answer = append(m.Answer, &dns.A{
			Hdr: dns.RR_Header{Name: q.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: ttl},
			A:   net.ParseIP(ip).To4(),
		})
	}
	return m
}

func rcodeReply(rcode int) func(context.Context, *dns.Msg) (*dns.Msg, error) {
	return func(_ context.Context, q *dns.Msg) (*dns.Msg, error) {
		m := new(dns.Msg)
		m.SetRcode(q, rcode)
		return m, nil
	}
}

func soa(zone string, ttl, minttl uint32) *dns.SOA {
	return &dns.SOA{
		Hdr: dns.RR_Header{Name: zone, Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: ttl},
		Ns:  "ns." + zone, Mbox: "hostmaster." + zone, Serial: 1, Refresh: 1800, Retry: 900, Expire: 604800, Minttl: minttl,
	}
}

// query builds a client request with the given id and optional DO bit.
func query(name string, qtype uint16, id uint16, do bool) *dns.Msg {
	m := new(dns.Msg)
	m.SetQuestion(name, qtype)
	m.Id = id
	if do {
		m.SetEdns0(4096, true)
	}
	return m
}

// startDNS serves h over UDP and TCP on the same 127.0.0.1 port. The TCP
// port is chosen first: Windows hosts reserve TCP port ranges (Hyper-V)
// that UDP ephemeral ports may fall into.
func startDNS(t *testing.T, h dns.HandlerFunc) netip.AddrPort {
	t.Helper()
	var lastErr error
	for range 50 {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		pc, err := net.ListenPacket("udp", ln.Addr().String())
		if err != nil {
			_ = ln.Close()
			lastErr = err
			continue
		}
		serve(t, &dns.Server{PacketConn: pc, Handler: h})
		serve(t, &dns.Server{Listener: ln, Handler: h})
		return netip.MustParseAddrPort(pc.LocalAddr().String())
	}
	t.Fatalf("no free UDP/TCP port pair: %v", lastErr)
	return netip.AddrPort{}
}

func serve(t *testing.T, s *dns.Server) {
	t.Helper()
	started := make(chan struct{})
	s.NotifyStartedFunc = func() { close(started) }
	go func() { _ = s.ActivateAndServe() }()
	<-started
	t.Cleanup(func() { _ = s.Shutdown() })
}

// testCert returns a self-signed certificate for 127.0.0.1 and "dot.test"
// and a pool that trusts it.
func testCert(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "picache test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
		DNSNames:              []string{"dot.test"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool
}

func firstA(t *testing.T, m *dns.Msg) (string, uint32) {
	t.Helper()
	for _, rr := range m.Answer {
		if a, ok := rr.(*dns.A); ok {
			return a.A.String(), a.Hdr.Ttl
		}
	}
	t.Fatalf("no A record in %v", m)
	return "", 0
}
