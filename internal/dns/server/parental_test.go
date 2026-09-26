package dnsserver

import (
	"context"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/clients"
	"github.com/hustenreizjuengling/picache/internal/dns/filter"
	"github.com/hustenreizjuengling/picache/internal/dns/parental"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// fakeParental blocks the names in block (or every name with all),
// rewrites the names in safe, pauses the groups in paused and records the
// groups it was asked for.
type fakeParental struct {
	mu     sync.Mutex
	block  map[string]parental.Decision
	all    *parental.Decision
	safe   map[string]parental.SafeSearchRewrite
	paused map[int64]parental.GroupPause
	groups [][]int64
}

func (f *fakeParental) SafeSearch(qname string, _ []int64, _ time.Time) (parental.SafeSearchRewrite, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rw, ok := f.safe[qname]
	return rw, ok
}

func (f *fakeParental) FilterGroups(groups []int64, now time.Time) []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.paused) == 0 {
		return groups
	}
	var out []int64
	for _, g := range groups {
		if p, ok := f.paused[g]; !ok || !now.Before(p.Until) {
			out = append(out, g)
		}
	}
	return out
}

func (f *fakeParental) PausedGroups(groups []int64, now time.Time) []parental.GroupPause {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []parental.GroupPause
	for _, g := range groups {
		if p, ok := f.paused[g]; ok && now.Before(p.Until) {
			out = append(out, p)
		}
	}
	return out
}

func (f *fakeParental) Check(qname string, groups []int64, _ time.Time) parental.Decision {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.groups = append(f.groups, slices.Clone(groups))
	if d, ok := f.block[qname]; ok {
		return d
	}
	if f.all != nil {
		return *f.all
	}
	return parental.Decision{}
}

var (
	bedtimeDecision = parental.Decision{Blocked: true, Kind: parental.KindSchedule, GroupID: 2, Group: "Kids", Name: "Bedtime",
		Until: time.Date(2026, 9, 26, 5, 0, 0, 0, time.UTC)}
	homeworkDecision = parental.Decision{Blocked: true, Kind: parental.KindService, GroupID: 2, Group: "Kids", Name: "YouTube",
		Schedule: "Homework time"}
	handDecision  = parental.Decision{Blocked: true, Kind: parental.KindOverride, GroupID: 2, Group: "Kids"}
	steamDecision = parental.Decision{Blocked: true, Kind: parental.KindService, GroupID: 2, Group: "Kids", Name: "Steam"}
)

// parentalEnv is the pipeline environment with parental controls.
func parentalEnv(t *testing.T) (*testEnv, *fakeParental) {
	t.Helper()
	p := &fakeParental{block: map[string]parental.Decision{
		"late.example":              bedtimeDecision,
		"www.youtube.com":           homeworkDecision,
		"byhand.example":            handDecision,
		"lancache.steamcontent.com": steamDecision,
		"school.example":            bedtimeDecision,
	}}
	e := pipelineEnv(t)
	e.srv.d.Parental = p
	e.flt.rules["school.example"] = filter.Decision{Action: filter.ActionAllow, Source: "rule", Kind: "exact", RuleID: 4, Name: "school.example"}
	e.cl.set(&clients.Identity{IP: netip.MustParseAddr("127.0.0.1"), ClientID: 7, Name: "tablet", GroupIDs: []int64{1, 2}})
	return e, p
}

func TestParentalPipeline(t *testing.T) {
	e, p := parentalEnv(t)
	for _, tc := range []struct {
		name, status, reason string
		ips                  []string
	}{
		{"late.example", StatusBlockedSchedule, "Kids: Bedtime", []string{"0.0.0.0"}},
		{"byhand.example", StatusBlockedSchedule, "Kids: blocked by hand", []string{"0.0.0.0"}},
		{"www.youtube.com", StatusBlockedService, "Kids: YouTube (Homework time)", []string{"0.0.0.0"}},
		// Before the download cache answer: a blocked Steam is not cached.
		{"lancache.steamcontent.com", StatusBlockedService, "Kids: Steam", []string{"0.0.0.0"}},
		// A user allow rule for the client lifts the block.
		{"school.example", StatusForwarded, "", []string{"198.51.100.7"}},
		{"www.example.com", StatusForwarded, "", []string{"198.51.100.7"}},
	} {
		r := e.query("udp", tc.name, dns.TypeA, withEDNS(1232, false))
		if got := answerIPs(r.Answer); !slices.Equal(got, tc.ips) {
			t.Errorf("%s: answer %v, want %v", tc.name, got, tc.ips)
		}
		ev := e.logs.waitEvent(t, tc.name, 0)
		if ev.Status != tc.status || ev.Reason != tc.reason {
			t.Errorf("%s: status %q reason %q, want %q %q", tc.name, ev.Status, ev.Reason, tc.status, tc.reason)
		}
		var ede *dns.EDNS0_EDE
		for _, o := range r.IsEdns0().Option {
			if v, ok := o.(*dns.EDNS0_EDE); ok {
				ede = v
			}
		}
		if blocked := tc.reason != ""; blocked != (ede != nil) || blocked && (ede.InfoCode != dns.ExtendedErrorCodeBlocked || ede.ExtraText != tc.reason) {
			t.Errorf("%s: EDE %v", tc.name, ede)
		}
	}
	if c := e.up.callsFor("late.example"); len(c) != 0 {
		t.Errorf("a parental block must not be forwarded: %v", c)
	}
	p.mu.Lock()
	if len(p.groups) == 0 || !slices.Equal(p.groups[0], []int64{1, 2}) {
		t.Errorf("Check got groups %v, want the identity's", p.groups)
	}
	p.mu.Unlock()

	// The blocking mode and blocked TTL of the settings apply.
	e.update(func(a *settings.All) { a.Filter.BlockingMode, a.Filter.BlockedTTL = "nxdomain", 30 })
	if r := e.query("udp", "late.example", dns.TypeAAAA); r.Rcode != dns.RcodeNameError || len(r.Ns) != 1 || r.Ns[0].(*dns.SOA).Minttl != 30 {
		t.Errorf("nxdomain mode: %v", r)
	}
}

// Pausing or disabling blocking does not lift parental controls.
func TestParentalIndependentOfPause(t *testing.T) {
	e, _ := parentalEnv(t)
	ctx := context.Background()
	if _, err := e.srv.SetBlocking(ctx, false, time.Hour); err != nil {
		t.Fatal(err)
	}
	if r := e.query("udp", "late.example", dns.TypeA); !slices.Equal(answerIPs(r.Answer), []string{"0.0.0.0"}) {
		t.Errorf("paused: bedtime must still block, got %v", r.Answer)
	}
	if r := e.query("udp", "ads.example.com", dns.TypeA); !slices.Equal(answerIPs(r.Answer), []string{"198.51.100.7"}) {
		t.Errorf("paused: lists must not block, got %v", r.Answer)
	}
	if _, err := e.srv.SetBlocking(ctx, false, 0); err != nil {
		t.Fatal(err)
	}
	if r := e.query("udp", "www.youtube.com", dns.TypeA); !slices.Equal(answerIPs(r.Answer), []string{"0.0.0.0"}) {
		t.Errorf("disabled: a blocked service must still block, got %v", r.Answer)
	}
	// Even while paused, a user allow rule lifts the block.
	if r := e.query("udp", "school.example", dns.TypeA); !slices.Equal(answerIPs(r.Answer), []string{"198.51.100.7"}) {
		t.Errorf("paused: allow rule, got %v", r.Answer)
	}
}

// Special-use names, local records and this server's names stay reachable
// when everything else is blocked.
func TestParentalKeepsLocalNames(t *testing.T) {
	e, p := parentalEnv(t)
	p.all = &handDecision
	for _, tc := range []struct {
		name  string
		qtype uint16
		ips   []string
	}{
		{"localhost", dns.TypeA, []string{"127.0.0.1"}},
		{"picache.lan", dns.TypeA, []string{"192.168.1.10"}},
		{"nas.example.org", dns.TypeA, []string{"192.168.1.5"}},
		{"elsewhere.example", dns.TypeA, []string{"0.0.0.0"}},
	} {
		if r := e.query("udp", tc.name, tc.qtype); !slices.Equal(answerIPs(r.Answer), tc.ips) {
			t.Errorf("%s: %v, want %v", tc.name, answerIPs(r.Answer), tc.ips)
		}
	}
}

func TestParentalLookupTrace(t *testing.T) {
	e, _ := parentalEnv(t)
	ctx := context.Background()
	caller := netip.MustParseAddr("127.0.0.1")
	for _, tc := range []struct {
		name, status, step string
	}{
		{"late.example", StatusBlockedSchedule, "parental: blocked by Kids: Bedtime until 2026-09-26T05:00:00Z: null reply"},
		{"www.youtube.com", StatusBlockedService, "parental: blocked by Kids: YouTube (Homework time): null reply"},
		{"school.example", StatusForwarded, `parental block lifted by allow rule "school.example" (Kids: Bedtime)`},
		{"www.example.com", StatusForwarded, "parental: no restriction"},
	} {
		res, err := e.srv.Lookup(ctx, LookupRequest{Name: tc.name}, caller)
		if err != nil {
			t.Fatal(err)
		}
		if res.Status != tc.status || !slices.Contains(res.Steps, tc.step) {
			t.Errorf("%s: status %q, steps %q; want %q with %q", tc.name, res.Status, res.Steps, tc.status, tc.step)
		}
	}
}

func TestRefusedTable(t *testing.T) {
	var tab refusedTable
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	tab.add(netip.MustParseAddr("127.0.0.1"), now)
	tab.add(netip.MustParseAddr("::1"), now)
	tab.add(netip.Addr{}, now)
	if got := tab.list(); len(got) != 0 {
		t.Fatalf("loopback and invalid addresses are not recorded: %v", got)
	}
	gua := netip.MustParseAddr("2001:db8:1:2:aaaa:bbbb:cccc:dddd")
	tab.add(gua, now)
	ip := func(i int) netip.Addr { return netip.AddrFrom4([4]byte{203, 0, byte(i >> 8), byte(i)}) }
	for i := range 300 {
		tab.add(ip(i), now.Add(time.Duration(i)*time.Second))
		if i == 99 {
			tab.add(gua, now.Add(99*time.Second)) // refreshed: more recent than 0–99
		}
	}
	got := tab.list()
	if len(got) != maxRefusedSources {
		t.Fatalf("%d entries, want the bound %d", len(got), maxRefusedSources)
	}
	if got[0].Address != ip(299).String() || got[0].Count != 1 || !got[0].Last.Equal(now.Add(299*time.Second)) {
		t.Errorf("newest first: %+v", got[0])
	}
	has := func(a netip.Addr) int {
		return slices.IndexFunc(got, func(r RefusedSource) bool { return r.Address == a.String() })
	}
	if i := has(gua); i < 0 || got[i].Count != 2 || !got[i].Last.Equal(now.Add(99*time.Second)) {
		t.Errorf("the full IPv6 address with its count must be kept: %v", got)
	}
	if has(ip(44)) >= 0 || has(ip(45)) < 0 {
		t.Error("the least recently refused sources must be evicted first")
	}
}

// The ACL drop paths record the source: the reader decorator (UDP) and the
// handler (TCP connections that reach it).
func TestRefusedSourcesRecorded(t *testing.T) {
	e := newEnv(t, nil)
	src := &packetSource{}
	for _, a := range []string{"8.8.8.8", "2001:db8::53", "8.8.8.8", "192.168.1.40"} {
		src.pkts = append(src.pkts, []byte{1})
		src.addrs = append(src.addrs, &net.UDPAddr{IP: net.ParseIP(a), Port: 5353})
	}
	r := e.srv.decorateReader(src).(dns.PacketConnReader)
	_, addr, err := r.ReadPacketConn(nil, time.Second)
	if err != nil || addr.(*net.UDPAddr).IP.String() != "192.168.1.40" {
		t.Fatalf("first allowed packet: %v %v", addr, err)
	}
	w := tcpFrom("198.51.100.9")
	e.handle(w, "example.com", dns.TypeA)
	if !w.closed || len(w.msgs) != 0 {
		t.Fatalf("a refused TCP client gets nothing: %+v", w)
	}
	got := e.srv.RefusedSources()
	var addrs []string
	for _, s := range got {
		addrs = append(addrs, s.Address+"×"+strings.Repeat("I", int(s.Count)))
	}
	if !slices.Equal(addrs, []string{"198.51.100.9×I", "8.8.8.8×II", "2001:db8::53×I"}) {
		t.Fatalf("refused sources %v", addrs)
	}
	if st := e.srv.Stats(); st.Refused != 4 {
		t.Errorf("refused counter %d", st.Refused)
	}
}

func TestHostNetwork(t *testing.T) {
	e := newEnv(t, nil)
	e.srv.env.host = func() *hostInfo {
		return &hostInfo{ifaces: []hostIface{
			{name: "lo", prefixes: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/8"), netip.MustParsePrefix("::1/128")}},
			{name: "eth0", prefixes: []netip.Prefix{netip.MustParsePrefix("192.168.1.10/24"), netip.MustParsePrefix("fd00::10/64"),
				netip.MustParsePrefix("fe80::1/64")}},
			{name: "docker0", prefixes: []netip.Prefix{netip.MustParsePrefix("172.17.0.1/16")}},
			{name: "wg0", prefixes: []netip.Prefix{netip.MustParsePrefix("10.8.0.1/24")}},
		}}
	}
	h := e.srv.HostNetwork()
	want := []netip.Prefix{netip.MustParsePrefix("192.168.1.10/24"), netip.MustParsePrefix("fd00::10/64"), netip.MustParsePrefix("fe80::1/64")}
	if h.Bridge || !slices.Equal(h.Prefixes, want) {
		t.Fatalf("host network %+v", h)
	}
	// A Docker container on a bridge network.
	e.srv.env.container = "docker"
	e.srv.env.primary = func() (netip.Addr, error) { return netip.MustParseAddr("172.17.0.2"), nil }
	e.srv.updateCacheIPs(e.set.Get())
	if h := e.srv.HostNetwork(); !h.Bridge {
		t.Fatal("bridge mode not reported")
	}
}
