package dnsserver

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/clients"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/dns/filter"
	"github.com/hustenreizjuengling/picache/internal/dns/upstream"
	"github.com/hustenreizjuengling/picache/internal/logs"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// --- fakes ---

type upCall struct {
	name  string
	qtype uint16
	via   []string
	do    bool
}

type fakeUpstream struct {
	mu     sync.Mutex
	calls  []upCall
	answer func(req *dns.Msg, via []string) (*dns.Msg, upstream.Info, error)
	probe  bool
}

func (f *fakeUpstream) Resolve(_ context.Context, req *dns.Msg) (*dns.Msg, upstream.Info, error) {
	return f.exchange(req, nil)
}

func (f *fakeUpstream) ResolveVia(_ context.Context, req *dns.Msg, via []string) (*dns.Msg, upstream.Info, error) {
	return f.exchange(req, via)
}

func (f *fakeUpstream) Probe(context.Context, netip.Addr) bool { return f.probe }

func (f *fakeUpstream) exchange(req *dns.Msg, via []string) (*dns.Msg, upstream.Info, error) {
	q := req.Question[0]
	opt := req.IsEdns0()
	f.mu.Lock()
	f.calls = append(f.calls, upCall{name: normalizeName(q.Name), qtype: q.Qtype, via: slices.Clone(via), do: opt != nil && opt.Do()})
	answer := f.answer
	f.mu.Unlock()
	if answer != nil {
		return answer(req, via)
	}
	return upAnswer(req), upstream.Info{Upstream: "fake-upstream"}, nil
}

func (f *fakeUpstream) callsFor(name string) []upCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []upCall
	for _, c := range f.calls {
		if c.name == name {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeUpstream) setAnswer(fn func(req *dns.Msg, via []string) (*dns.Msg, upstream.Info, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answer = fn
}

// upAnswer is the default upstream answer: A 198.51.100.7 / AAAA 2001:db8::7.
func upAnswer(req *dns.Msg, extra ...dns.RR) *dns.Msg {
	m := new(dns.Msg)
	m.SetReply(req)
	m.RecursionAvailable = true
	q := req.Question[0]
	m.Answer = append(m.Answer, extra...)
	owner := q.Name
	if len(extra) > 0 {
		if c, ok := extra[len(extra)-1].(*dns.CNAME); ok {
			owner = c.Target
		}
	}
	switch q.Qtype {
	case dns.TypeA:
		m.Answer = append(m.Answer, &dns.A{Hdr: rrHeader(owner, dns.TypeA, 300), A: net.ParseIP("198.51.100.7").To4()})
	case dns.TypeAAAA:
		m.Answer = append(m.Answer, &dns.AAAA{Hdr: rrHeader(owner, dns.TypeAAAA, 300), AAAA: net.ParseIP("2001:db8::7")})
	}
	return m
}

type fakeFilter struct {
	mu      sync.Mutex
	check   map[string]filter.Decision
	rules   map[string]filter.Decision
	matches []filter.Match
}

func newFakeFilter() *fakeFilter {
	return &fakeFilter{check: map[string]filter.Decision{}, rules: map[string]filter.Decision{}}
}

// Check returns check[qname] (the full-precedence decision) if set, else
// the user rule decision.
func (f *fakeFilter) Check(qname string, _ []int64) filter.Decision {
	f.mu.Lock()
	defer f.mu.Unlock()
	if d, ok := f.check[qname]; ok {
		return d
	}
	return f.rules[qname]
}

func (f *fakeFilter) CheckRules(qname string, _ []int64) filter.Decision {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rules[qname]
}

func (f *fakeFilter) Explain(context.Context, string, []int64) ([]filter.Match, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.matches, nil
}

func listBlock(name string) filter.Decision {
	return filter.Decision{Action: filter.ActionBlock, Source: "list", Kind: "subtree", ListID: 3, Name: name}
}

func ruleBlock(pattern string) filter.Decision {
	return filter.Decision{Action: filter.ActionBlock, Source: "rule", Kind: "exact", RuleID: 9, Name: pattern}
}

type fakeServices map[string]string

func (f fakeServices) MatchDNS(qname string) (string, bool) {
	s, ok := f[qname]
	return s, ok
}

type fakeClients struct {
	mu        sync.Mutex
	ids       map[netip.Addr]*clients.Identity
	seen      map[netip.Addr]int
	transient map[netip.Addr]int
}

func (f *fakeClients) Identify(ip netip.Addr) *clients.Identity {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id, ok := f.ids[ip]; ok {
		return id
	}
	return &clients.Identity{IP: ip, GroupIDs: []int64{clients.DefaultGroupID}}
}

func (f *fakeClients) Seen(ip netip.Addr) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen[ip]++
}

func (f *fakeClients) SeenTransient(ip netip.Addr) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.transient[ip]++
}

func (f *fakeClients) set(id *clients.Identity) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ids[id.IP] = id
}

type fakeLogger struct {
	mu     sync.Mutex
	events []logs.QueryEvent
}

func (f *fakeLogger) LogQuery(e logs.QueryEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, e)
}

// waitEvent returns the event for qname after the first `skip` ones (logging
// happens right after the reply is written, so it may lag the client).
func (f *fakeLogger) waitEvent(t *testing.T, qname string, skip int) logs.QueryEvent {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		f.mu.Lock()
		seen := 0
		for _, e := range f.events {
			if e.QName == qname {
				if seen == skip {
					f.mu.Unlock()
					return e
				}
				seen++
			}
		}
		f.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatalf("no query log event for %s", qname)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (f *fakeLogger) count(qname string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, e := range f.events {
		if e.QName == qname {
			n++
		}
	}
	return n
}

// --- environment ---

type testEnv struct {
	t       *testing.T
	srv     *Server
	set     *settings.Store
	up      *fakeUpstream
	flt     *fakeFilter
	cl      *fakeClients
	svc     fakeServices
	logs    *fakeLogger
	dcReady atomic.Bool
	udp     string
	tcp     string
}

var (
	testCacheIP  = netip.MustParseAddr("192.168.1.10")
	testServerV6 = netip.MustParseAddr("fd00::10")
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// newEnv creates a server with fakes; mutate adjusts the settings (router
// resolver off and local domain "lan" by default).
func newEnv(t *testing.T, mutate func(*settings.All)) *testEnv {
	t.Helper()
	ctx := context.Background()
	d, err := db.Open(filepath.Join(t.TempDir(), "picache.db"), 2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	set, err := settings.Open(ctx, d, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := set.Update(ctx, func(a *settings.All) error {
		a.DNS.RouterResolver = ""
		a.DNS.LocalDomain = "lan"
		a.DNS.ServerNames = []string{"picache"}
		if mutate != nil {
			mutate(a)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	e := &testEnv{
		t:    t,
		set:  set,
		up:   &fakeUpstream{},
		flt:  newFakeFilter(),
		cl:   &fakeClients{ids: map[netip.Addr]*clients.Identity{}, seen: map[netip.Addr]int{}, transient: map[netip.Addr]int{}},
		svc:  fakeServices{},
		logs: &fakeLogger{},
	}
	e.dcReady.Store(true)
	srv, err := New(ctx, Deps{
		DB: d, Settings: set, Upstream: e.up, Filter: e.flt, Clients: e.cl, Services: e.svc, Logs: e.logs,
		DownloadCacheReady: func() (bool, string) {
			if e.dcReady.Load() {
				return true, ""
			}
			return false, "cache listener not bound"
		},
		Log: quietLog(),
	})
	if err != nil {
		t.Fatal(err)
	}
	srv.env = hostEnv{
		primary: func() (netip.Addr, error) { return testCacheIP, nil },
		ifaces:  func() ([]interfaceIPv4, bool) { return nil, false },
		host: func() *hostInfo {
			return &hostInfo{
				ifaces:   []hostIface{{name: "eth0", prefixes: []netip.Prefix{netip.PrefixFrom(testCacheIP, 24), netip.PrefixFrom(testServerV6, 64)}}},
				own:      []netip.Addr{testCacheIP, testServerV6},
				primary4: testCacheIP,
				primary6: testServerV6,
			}
		},
		gateway: func() (netip.Addr, error) { return netip.Addr{}, errors.New("no gateway in tests") },
	}
	srv.host.Store(srv.env.host())
	srv.updateCacheIPs(set.Get())
	e.srv = srv
	return e
}

// serve starts the server on 127.0.0.1 UDP and TCP sockets.
func (e *testEnv) serve() *testEnv {
	e.t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		e.t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		e.t.Fatal(err)
	}
	e.udp, e.tcp = pc.LocalAddr().String(), ln.Addr().String()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.srv.Serve(ctx, []net.PacketConn{pc}, []net.Listener{ln}) }()
	e.t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				e.t.Errorf("Serve: %v", err)
			}
		case <-time.After(10 * time.Second):
			e.t.Error("Serve did not return after cancel")
		}
	})
	return e
}

func (e *testEnv) update(fn func(*settings.All)) {
	e.t.Helper()
	if _, err := e.set.Update(context.Background(), func(a *settings.All) error { fn(a); return nil }); err != nil {
		e.t.Fatal(err)
	}
}

func (e *testEnv) addRecord(name, typ, value string) Record {
	e.t.Helper()
	r, err := e.srv.CreateRecord(context.Background(), RecordInput{Name: name, Type: typ, Value: value, Enabled: true})
	if err != nil {
		e.t.Fatalf("create record %s %s %s: %v", name, typ, value, err)
	}
	return r
}

type queryOpt func(*dns.Msg)

func withEDNS(size uint16, do bool) queryOpt {
	return func(m *dns.Msg) { m.SetEdns0(size, do) }
}

// query sends a query over network ("udp" or "tcp") and returns the reply.
func (e *testEnv) query(network, name string, qtype uint16, opts ...queryOpt) *dns.Msg {
	e.t.Helper()
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), qtype)
	for _, o := range opts {
		o(m)
	}
	return e.exchange(network, m)
}

func (e *testEnv) exchange(network string, m *dns.Msg) *dns.Msg {
	e.t.Helper()
	addr := e.udp
	if network == "tcp" {
		addr = e.tcp
	}
	c := &dns.Client{Net: network, Timeout: 3 * time.Second}
	if opt := m.IsEdns0(); opt != nil {
		c.UDPSize = opt.UDPSize()
	}
	r, _, err := c.Exchange(m, addr)
	if err != nil {
		e.t.Fatalf("%s query %s: %v", network, m.Question[0].Name, err)
	}
	if r.Id != m.Id {
		e.t.Fatalf("reply id %d != query id %d", r.Id, m.Id)
	}
	return r
}

// fakeWriter is a dns.ResponseWriter with a chosen remote address.
type fakeWriter struct {
	remote net.Addr
	msgs   []*dns.Msg
	closed bool
}

func udpFrom(ip string) *fakeWriter {
	return &fakeWriter{remote: &net.UDPAddr{IP: net.ParseIP(ip), Port: 40000}}
}

func tcpFrom(ip string) *fakeWriter {
	return &fakeWriter{remote: &net.TCPAddr{IP: net.ParseIP(ip), Port: 40000}}
}

func (w *fakeWriter) LocalAddr() net.Addr         { return &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 53} }
func (w *fakeWriter) RemoteAddr() net.Addr        { return w.remote }
func (w *fakeWriter) WriteMsg(m *dns.Msg) error   { w.msgs = append(w.msgs, m.Copy()); return nil }
func (w *fakeWriter) Write(b []byte) (int, error) { return len(b), nil }
func (w *fakeWriter) Close() error                { w.closed = true; return nil }
func (w *fakeWriter) TsigStatus() error           { return nil }
func (w *fakeWriter) TsigTimersOnly(bool)         {}
func (w *fakeWriter) Hijack()                     {}

// handle runs one query through the handler with a fake writer.
func (e *testEnv) handle(w *fakeWriter, name string, qtype uint16) {
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), qtype)
	(&dnsHandler{s: e.srv, ctx: context.Background()}).ServeDNS(w, m)
}

// answerIPs returns the A/AAAA addresses in rrs.
func answerIPs(rrs []dns.RR) []string {
	var out []string
	for _, rr := range rrs {
		switch v := rr.(type) {
		case *dns.A:
			out = append(out, v.A.String())
		case *dns.AAAA:
			out = append(out, v.AAAA.String())
		}
	}
	return out
}
