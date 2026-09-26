package upstream

import (
	"context"
	"net"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"
	"unicode/utf8"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/settings"
)

// reply builds an upstream reply to q with rcode, RA and answer records.
func reply(q *dns.Msg, rcode int, ra bool, rrs ...dns.RR) *dns.Msg {
	m := new(dns.Msg)
	m.SetRcode(q, rcode)
	m.RecursionAvailable = ra
	m.Answer = rrs
	return m
}

func aRR(name, ip string) dns.RR {
	return &dns.A{Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 300}, A: net.ParseIP(ip).To4()}
}

func aaaaRR(name, ip string) dns.RR {
	return &dns.AAAA{Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: 300}, AAAA: net.ParseIP(ip)}
}

func cnameRR(name, target string) dns.RR {
	return &dns.CNAME{Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeCNAME, Class: dns.ClassINET, Ttl: 300}, Target: target}
}

// withEDE adds an OPT with the given EDE options to m.
func withEDE(m *dns.Msg, edes ...*dns.EDNS0_EDE) *dns.Msg {
	m.SetEdns0(1232, false)
	opt := m.IsEdns0()
	for _, e := range edes {
		opt.Option = append(opt.Option, e)
	}
	return m
}

func TestClassify(t *testing.T) {
	const name = "blocked.example."
	qA := newQuery(name, dns.TypeA, dns.ClassINET, false)
	qAAAA := newQuery(name, dns.TypeAAAA, dns.ClassINET, false)
	qMX := newQuery(name, dns.TypeMX, dns.ClassINET, false)
	for _, tc := range []struct {
		name string
		m    *dns.Msg
		q    uint16
		host string
		want string // kind, "" = not blocked
	}{
		{"EDE 15", withEDE(reply(qA, dns.RcodeNameError, true), &dns.EDNS0_EDE{InfoCode: 15, ExtraText: "blocked"}), dns.TypeA, "dns.example", BlockEDE},
		{"EDE 16", withEDE(reply(qA, dns.RcodeSuccess, true, aRR(name, "192.0.2.1")), &dns.EDNS0_EDE{InfoCode: 16}), dns.TypeA, "dns.example", BlockEDE},
		{"EDE 17 on SERVFAIL", withEDE(reply(qMX, dns.RcodeServerFailure, true), &dns.EDNS0_EDE{InfoCode: 17}), dns.TypeMX, "dns.example", BlockEDE},
		{"EDE 18 is not a block", withEDE(reply(qA, dns.RcodeRefused, true), &dns.EDNS0_EDE{InfoCode: 18}), dns.TypeA, "dns.example", ""},
		{"EDE wins over null-ip", withEDE(reply(qA, dns.RcodeSuccess, true, aRR(name, "0.0.0.0")), &dns.EDNS0_EDE{InfoCode: 15}), dns.TypeA, "dns.example", BlockEDE},
		{"null A", reply(qA, dns.RcodeSuccess, true, aRR(name, "0.0.0.0")), dns.TypeA, "dns.example", BlockNullIP},
		{"null AAAA", reply(qAAAA, dns.RcodeSuccess, true, aaaaRR(name, "::")), dns.TypeAAAA, "dns.example", BlockNullIP},
		{"null after CNAME", reply(qA, dns.RcodeSuccess, true, cnameRR(name, "x.example."), aRR("x.example.", "0.0.0.0")), dns.TypeA, "dns.example", BlockNullIP},
		{"one real address", reply(qA, dns.RcodeSuccess, true, aRR(name, "0.0.0.0"), aRR(name, "192.0.2.1")), dns.TypeA, "dns.example", ""},
		{"CNAME only", reply(qA, dns.RcodeSuccess, true, cnameRR(name, "x.example.")), dns.TypeA, "dns.example", ""},
		{"null MX query", reply(qMX, dns.RcodeSuccess, true, aRR(name, "0.0.0.0")), dns.TypeMX, "dns.example", ""},
		{"block page", reply(qA, dns.RcodeSuccess, true, aRR(name, "146.112.61.104"), aRR(name, "146.112.61.110")), dns.TypeA, "dns.example", BlockBlockPage},
		{"block page AAAA", reply(qAAAA, dns.RcodeSuccess, true, aaaaRR(name, "::ffff:146.112.61.106")), dns.TypeAAAA, "dns.example", BlockBlockPage},
		{"next to the block pages", reply(qA, dns.RcodeSuccess, true, aRR(name, "146.112.61.111")), dns.TypeA, "dns.example", ""},
		{"block page and a real address", reply(qA, dns.RcodeSuccess, true, aRR(name, "146.112.61.104"), aRR(name, "192.0.2.1")), dns.TypeA, "dns.example", ""},
		{"Quad9 NXDOMAIN without RA", reply(qA, dns.RcodeNameError, false), dns.TypeA, "dns.quad9.net", BlockNXDomainNoRA},
		{"Quad9 NXDOMAIN with RA", reply(qA, dns.RcodeNameError, true), dns.TypeA, "dns.quad9.net", ""},
		{"Quad9 NOERROR without RA", reply(qA, dns.RcodeSuccess, false), dns.TypeA, "dns.quad9.net", ""},
		{"other NXDOMAIN without RA", reply(qA, dns.RcodeNameError, false), dns.TypeA, "dns.example", ""},
		{"Quad9 plain IPv6", reply(qA, dns.RcodeNameError, false), dns.TypeA, "2620:00fe::00fe", BlockNXDomainNoRA},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blocking, _ := parseEDE(tc.m)
			got := classify(tc.m, tc.q, tc.host, blocking)
			switch {
			case tc.want == "" && got != nil:
				t.Fatalf("classified as %+v", got)
			case tc.want != "" && (got == nil || got.Kind != tc.want || got.Host != tc.host):
				t.Fatalf("got %+v, want %s", got, tc.want)
			}
		})
	}
	for _, h := range []string{"dns.quad9.net", "dns9.quad9.net", "dns11.quad9.net", "9.9.9.9", "149.112.112.112", "9.9.9.11",
		"149.112.112.11", "2620:fe::fe", "2620:fe::9", "2620:fe::11", "2620:fe::fe:11", "DNS.Quad9.NET"} {
		if !isQuad9Filtering(h) {
			t.Errorf("%s is a filtering Quad9 endpoint", h)
		}
	}
	for _, h := range []string{"dns10.quad9.net", "9.9.9.10", "1.1.1.1", "quad9.net", "security.cloudflare-dns.com"} {
		if isQuad9Filtering(h) {
			t.Errorf("%s is not a filtering Quad9 endpoint", h)
		}
	}
}

func TestParseEDE(t *testing.T) {
	q := newQuery("x.example.", dns.TypeA, dns.ClassINET, false)
	edes := func(codes ...uint16) []*dns.EDNS0_EDE {
		var out []*dns.EDNS0_EDE
		for i, c := range codes {
			out = append(out, &dns.EDNS0_EDE{InfoCode: c, ExtraText: "t" + string(rune('a'+i))})
		}
		return out
	}
	for _, tc := range []struct {
		name          string
		codes         []uint16
		block, logged string // "code/text", "" = nil
	}{
		{"none", nil, "", ""},
		{"blocking only", []uint16{15}, "15/ta", "15/ta"},
		{"first EDE logged", []uint16{3, 22}, "", "3/ta"},
		{"blocking after another", []uint16{3, 17}, "17/tb", "17/tb"},
		{"first blocking wins", []uint16{16, 15}, "16/ta", "16/ta"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := reply(q, dns.RcodeSuccess, true)
			if tc.codes != nil {
				withEDE(m, edes(tc.codes...)...)
			}
			b, l := parseEDE(m)
			str := func(e *EDE) string {
				if e == nil {
					return ""
				}
				return strconv.Itoa(int(e.Code)) + "/" + e.Text
			}
			if str(b) != tc.block || str(l) != tc.logged {
				t.Fatalf("blocking %q logged %q, want %q %q", str(b), str(l), tc.block, tc.logged)
			}
		})
	}
	// Only the first 16 options are examined.
	m := reply(q, dns.RcodeSuccess, true)
	m.SetEdns0(1232, false)
	opt := m.IsEdns0()
	for range 16 {
		opt.Option = append(opt.Option, &dns.EDNS0_COOKIE{Code: dns.EDNS0COOKIE, Cookie: "0102030405060708"})
	}
	opt.Option = append(opt.Option, &dns.EDNS0_EDE{InfoCode: 15})
	if b, l := parseEDE(m); b != nil || l != nil {
		t.Errorf("an EDE after 16 options was examined: %v %v", b, l)
	}
}

func TestSanitizeEDEText(t *testing.T) {
	for in, want := range map[string]string{
		"Blocked (malware)":        "Blocked (malware)",
		"a\x00b\tc\nd\x7fe\u0085f": "abcdef",
		"x‮evil‬y⁦z⁩":              "xevilyz",
		"؜‎‏ok":                    "ok",
		"bad\xff\xfeutf8":          "badutf8",
		"Grüße":                    "Grüße",
	} {
		if got := SanitizeEDEText(in); got != want {
			t.Errorf("SanitizeEDEText(%q) = %q, want %q", in, got, want)
		}
	}
	long := strings.Repeat("ä", 150) // 300 bytes
	got := SanitizeEDEText(long)
	if len(got) > 200 || !utf8.ValidString(got) || len(got) != 200 {
		t.Errorf("cut to %d bytes (valid %v)", len(got), utf8.ValidString(got))
	}
	odd := "x" + strings.Repeat("ä", 150) // the cut falls into a rune
	if got := SanitizeEDEText(odd); len(got) != 199 || !utf8.ValidString(got) {
		t.Errorf("cut at a rune boundary: %d bytes", len(got))
	}
}

// Blocks of the default set are classified at fetch time and returned with
// fresh, cached and stale answers; the lifetime is dns.upstreamBlockedTtl,
// beyond the 1 h negative cap, also for an NXDOMAIN without SOA.
func TestUpstreamBlockCaching(t *testing.T) {
	const quad9 = "9.9.9.9"
	st := newStore(t, func(d *settings.DNS) {
		d.Upstreams = []string{quad9}
		d.UpstreamMode = "strict"
		d.UpstreamBlockedTTL = 7200
	})
	synctest.Test(t, func(t *testing.T) {
		f := &fakeTransport{fn: func(_ context.Context, q *dns.Msg) (*dns.Msg, error) {
			return reply(q, dns.RcodeNameError, false), nil // Quad9's block: NXDOMAIN, RA=0, no SOA
		}}
		r := newTestResolver(t, st, testOptions(), map[string]*fakeTransport{quad9: f})
		defer r.Close()
		ctx := context.Background()
		_, info, err := r.Resolve(ctx, query("malware.example.", dns.TypeA, 1, false), noECS)
		if err != nil {
			t.Fatal(err)
		}
		if info.Block == nil || info.Block.Kind != BlockNXDomainNoRA || info.Block.Host != quad9 || info.Upstream != quad9 ||
			info.Block.Reason() != "9.9.9.9: nxdomain-no-ra" {
			t.Fatalf("fresh: %+v %+v", info, info.Block)
		}
		time.Sleep(3601 * time.Second) // beyond the 1 h negative cap
		_, info, _ = r.Resolve(ctx, query("MALWARE.example.", dns.TypeA, 2, false), noECS)
		if !info.Cached || info.Stale || info.Block == nil || info.Block.Kind != BlockNXDomainNoRA || info.Upstream != "" || f.calls() != 1 {
			t.Fatalf("cached: %+v, calls %d", info, f.calls())
		}
		time.Sleep(3600 * time.Second) // expired: stale
		_, info, _ = r.Resolve(ctx, query("malware.example.", dns.TypeA, 3, false), noECS)
		if !info.Stale || info.Block == nil {
			t.Fatalf("stale: %+v", info)
		}
	})
}

// Without the response cache nothing is stored, blocked or not.
func TestUpstreamBlockNotCachedWithoutCache(t *testing.T) {
	st := newStore(t, func(d *settings.DNS) {
		d.Upstreams = []string{up1}
		d.CacheEnabled = false
	})
	f := &fakeTransport{fn: func(_ context.Context, q *dns.Msg) (*dns.Msg, error) {
		return reply(q, dns.RcodeSuccess, true, aRR(q.Question[0].Name, "0.0.0.0")), nil
	}}
	r := newTestResolver(t, st, testOptions(), map[string]*fakeTransport{up1: f})
	defer r.Close()
	for i := range 2 {
		_, info, err := r.Resolve(context.Background(), query("ads.example.", dns.TypeA, uint16(i), false), noECS)
		if err != nil || info.Block == nil || info.Block.Kind != BlockNullIP || info.Cached {
			t.Fatalf("query %d: %+v %v", i, info, err)
		}
	}
	if f.calls() != 2 || r.CacheStats().Entries != 0 {
		t.Errorf("calls %d, entries %d", f.calls(), r.CacheStats().Entries)
	}
}

// ResolveVia answers (forwarders, the router, local PTR upstreams) are
// never classified, even with RA=0 or 0.0.0.0; their EDE is still parsed.
func TestResolveViaNotClassified(t *testing.T) {
	const router = "192.168.178.1"
	st := newStore(t, nil)
	f := &fakeTransport{fn: func(_ context.Context, q *dns.Msg) (*dns.Msg, error) {
		if q.Question[0].Name == "nx.fritz.box." {
			return withEDE(reply(q, dns.RcodeNameError, false), &dns.EDNS0_EDE{InfoCode: 15, ExtraText: "local block"}), nil
		}
		return reply(q, dns.RcodeSuccess, false, aRR(q.Question[0].Name, "0.0.0.0")), nil
	}}
	r := newTestResolver(t, st, testOptions(), map[string]*fakeTransport{router: f})
	defer r.Close()
	_, info, err := r.ResolveVia(context.Background(), query("null.fritz.box.", dns.TypeA, 1, false), []string{router})
	if err != nil || info.Block != nil {
		t.Fatalf("null answer via the router: %+v %v", info, err)
	}
	_, info, err = r.ResolveVia(context.Background(), query("nx.fritz.box.", dns.TypeA, 1, false), []string{router})
	if err != nil || info.Block != nil || info.EDE == nil || info.EDE.Code != 15 || info.EDE.Text != "local block" {
		t.Fatalf("EDE via the router: %+v %v", info, err)
	}
	// LookupIP shares the default set's fetches and ignores the class.
}

// LookupIP shares the fetches and the cache of the default set and
// ignores the classification.
func TestLookupIPIgnoresClassification(t *testing.T) {
	st := newStore(t, oneUpstream(nil))
	f := &fakeTransport{fn: func(_ context.Context, q *dns.Msg) (*dns.Msg, error) {
		return reply(q, dns.RcodeSuccess, true, aRR(q.Question[0].Name, "0.0.0.0")), nil
	}}
	r := newTestResolver(t, st, testOptions(), map[string]*fakeTransport{up1: f})
	defer r.Close()
	addrs, err := r.LookupIP(context.Background(), "null.example", false)
	if err != nil || len(addrs) != 1 || !addrs[0].IsUnspecified() {
		t.Fatalf("LookupIP = %v %v", addrs, err)
	}
	if _, info, _ := r.Resolve(context.Background(), query("null.example.", dns.TypeA, 1, false), noECS); !info.Cached || info.Block == nil {
		t.Errorf("Resolve after LookupIP: %+v (shared cache, classified)", info)
	}
}

// FuzzParseEDE feeds arbitrary OPT option data (untrusted upstream bytes)
// through the DNS library's unpacking and the EDE parser.
func FuzzParseEDE(f *testing.F) {
	f.Add([]byte{0, 15, 0, 6, 0, 15, 'h', 'i', 0xff, 0x0a})
	f.Add([]byte{0, 15, 0, 2, 0, 17})
	f.Add([]byte{0, 10, 0, 8, 1, 2, 3, 4, 5, 6, 7, 8, 0, 15, 0, 3, 0, 16, 0xe2})
	f.Fuzz(func(t *testing.T, rdata []byte) {
		if len(rdata) > 4096 {
			return
		}
		q := newQuery("x.example.", dns.TypeA, dns.ClassINET, false)
		m := reply(q, dns.RcodeSuccess, true)
		wire, err := m.Pack()
		if err != nil {
			t.Fatal(err)
		}
		// Append an OPT RR with the fuzzed RDATA to the additional section.
		wire[11]++ // ARCOUNT (fits: the reply has none)
		opt := []byte{0, 0, 41, 0x04, 0xd0, 0, 0, 0, 0, byte(len(rdata) >> 8), byte(len(rdata))}
		wire = append(append(wire, opt...), rdata...)
		var parsed dns.Msg
		if parsed.Unpack(wire) != nil {
			return
		}
		b, l := parseEDE(&parsed)
		for _, e := range []*EDE{b, l} {
			if e == nil {
				continue
			}
			if len(e.Text) > maxEDEText || !utf8.ValidString(e.Text) {
				t.Fatalf("text not bounded: %q", e.Text)
			}
			for _, r := range e.Text {
				if unwantedEDERune(r) {
					t.Fatalf("control or bidi character %U kept", r)
				}
			}
		}
		if b != nil && (b != l || !blockingEDE(b.Code)) {
			t.Fatalf("blocking %+v logged %+v", b, l)
		}
		_ = classify(&parsed, dns.TypeA, "dns.example", b)
	})
}

// The block names the upstream by its host only: never the path, query
// or port of a DoH URL (a profile ID).
func TestBlockHostOfDoHURL(t *testing.T) {
	const doh = "https://dns.example:8443/dns-query/profile-5ecret?x=1"
	st := newStore(t, func(d *settings.DNS) { d.Upstreams = []string{doh}; d.CacheEnabled = false })
	f := &fakeTransport{fn: func(_ context.Context, q *dns.Msg) (*dns.Msg, error) {
		return reply(q, dns.RcodeSuccess, true, aRR(q.Question[0].Name, "0.0.0.0")), nil
	}}
	r := newTestResolver(t, st, testOptions(), map[string]*fakeTransport{doh: f})
	defer r.Close()
	_, info, err := r.Resolve(context.Background(), query("ads.example.", dns.TypeA, 1, false), noECS)
	if err != nil || info.Block == nil || info.Block.Host != "dns.example" || info.Block.Reason() != "dns.example: null-ip" || info.Upstream != doh {
		t.Fatalf("info %+v %+v %v", info, info.Block, err)
	}
}
