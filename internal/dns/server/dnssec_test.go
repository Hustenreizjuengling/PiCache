package dnsserver

import (
	"context"
	"database/sql"
	"errors"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/clients"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/dns/parental"
	"github.com/hustenreizjuengling/picache/internal/dns/upstream"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

func withCD() queryOpt { return func(m *dns.Msg) { m.CheckingDisabled = true } }

var (
	secureV = &upstream.Verdict{Status: "secure", Zone: "example.com"}
	bogusV  = &upstream.Verdict{Status: "bogus", Zone: "example.com", Reason: "bad signature",
		EDE: &upstream.EDE{Code: 6, Text: "example.com: bad signature"}}
	insecureAlgV = &upstream.Verdict{Status: "insecure", Zone: "example.org", Reason: "unsupported algorithm",
		EDE: &upstream.EDE{Code: 1, Text: "example.org: unsupported algorithm"}}
)

// verdictFor answers every query with upAnswer and the verdict of its
// name (nil: none); the message's AD follows the verdict like the upstream
// package's in validate mode.
func verdictFor(verdicts map[string]*upstream.Verdict) func(req *dns.Msg, via []string) (*dns.Msg, upstream.Info, error) {
	return func(req *dns.Msg, via []string) (*dns.Msg, upstream.Info, error) {
		m := upAnswer(req)
		v := verdicts[normalizeName(req.Question[0].Name)]
		m.AuthenticatedData = v != nil && v.Status == "secure"
		return m, upstream.Info{Upstream: "fake-upstream", DNSSEC: v}, nil
	}
}

func ednsEDE(m *dns.Msg) *dns.EDNS0_EDE {
	if opt := m.IsEdns0(); opt != nil {
		for _, o := range opt.Option {
			if e, ok := o.(*dns.EDNS0_EDE); ok {
				return e
			}
		}
	}
	return nil
}

func TestBogusIsServfail(t *testing.T) {
	e := newEnv(t, nil).serve()
	e.up.setAnswer(verdictFor(map[string]*upstream.Verdict{"bad.example.com": bogusV}))
	// CD=0: SERVFAIL, status error, the reason and the EDE.
	m := e.query("udp", "bad.example.com", dns.TypeA, withEDNS(1232, true))
	if m.Rcode != dns.RcodeServerFailure || len(m.Answer) != 0 {
		t.Fatalf("CD=0: %v", m)
	}
	if ede := ednsEDE(m); ede == nil || ede.InfoCode != 6 || ede.ExtraText != "example.com: bad signature" {
		t.Fatalf("EDE %+v", ede)
	}
	ev := e.logs.waitEvent(t, "bad.example.com", 0)
	if ev.Status != StatusError || ev.Reason != "dnssec: example.com: bad signature" || ev.DNSSECStatus != "bogus" || ev.DNSSEC {
		t.Fatalf("logged %+v", ev)
	}
	// CD=1: the data, AD clear, logged bogus; the next CD=0 client still
	// gets SERVFAIL (the cache is shared).
	m = e.query("udp", "bad.example.com", dns.TypeA, withEDNS(1232, true), withCD())
	if m.Rcode != dns.RcodeSuccess || len(m.Answer) != 1 || m.AuthenticatedData || !m.CheckingDisabled {
		t.Fatalf("CD=1: %v", m)
	}
	if ev := e.logs.waitEvent(t, "bad.example.com", 1); ev.Status != StatusForwarded || ev.DNSSECStatus != "bogus" {
		t.Fatalf("logged %+v", ev)
	}
	if m = e.query("udp", "bad.example.com", dns.TypeA, withEDNS(1232, true)); m.Rcode != dns.RcodeServerFailure {
		t.Fatalf("CD=0 after CD=1: %v", m)
	}
	if c := e.up.last(); !c.do {
		t.Fatal("DO not handed to the upstream package")
	}
	// Counters since the start.
	if st := e.srv.Stats().DNSSEC; st.Bogus != 3 {
		t.Fatalf("counters %+v", st)
	}
	if bogus, total := e.srv.DNSSECLastHour(); bogus != 3 || total != 3 {
		t.Fatalf("last hour %d of %d", bogus, total)
	}
}

func TestCDEchoedAndSecureAD(t *testing.T) {
	for _, mode := range []string{settings.DNSSECOff, settings.DNSSECPassthrough, settings.DNSSECValidate} {
		t.Run(mode, func(t *testing.T) {
			e := newEnv(t, func(a *settings.All) { a.DNS.DNSSECMode = mode }).serve()
			e.up.setAnswer(verdictFor(map[string]*upstream.Verdict{"www.example.com": secureV}))
			m := e.query("udp", "www.example.com", dns.TypeA, withEDNS(1232, true), withCD())
			if !m.CheckingDisabled || !m.AuthenticatedData {
				t.Fatalf("CD %v AD %v", m.CheckingDisabled, m.AuthenticatedData)
			}
			if m = e.query("udp", "www.example.com", dns.TypeA); m.CheckingDisabled || m.AuthenticatedData {
				t.Fatalf("without CD and DO: CD %v AD %v", m.CheckingDisabled, m.AuthenticatedData)
			}
			ev := e.logs.waitEvent(t, "www.example.com", 0)
			if ev.DNSSECStatus != "secure" || !ev.DNSSEC {
				t.Fatalf("logged %+v", ev)
			}
		})
	}
}

func TestInsecureEDE(t *testing.T) {
	e := newEnv(t, nil).serve()
	e.up.setAnswer(verdictFor(map[string]*upstream.Verdict{"www.example.org": insecureAlgV}))
	m := e.query("udp", "www.example.org", dns.TypeA, withEDNS(1232, false))
	if ede := ednsEDE(m); m.Rcode != dns.RcodeSuccess || ede == nil || ede.InfoCode != 1 {
		t.Fatalf("insecure answer: %v", m)
	}
}

func TestDNS64AndDNSSEC(t *testing.T) {
	nodata := func(req *dns.Msg, v *upstream.Verdict) (*dns.Msg, upstream.Info, error) {
		m := new(dns.Msg)
		m.SetReply(req)
		m.Ns = []dns.RR{&dns.SOA{Hdr: rrHeader("example.com.", dns.TypeSOA, 300), Ns: "ns.example.com.", Mbox: "h.example.com.", Minttl: 300}}
		m.AuthenticatedData = v.Status == "secure"
		return m, upstream.Info{Upstream: "fake-upstream", DNSSEC: v}, nil
	}
	answer := func(aVerdict *upstream.Verdict) func(req *dns.Msg, via []string) (*dns.Msg, upstream.Info, error) {
		return func(req *dns.Msg, via []string) (*dns.Msg, upstream.Info, error) {
			if req.Question[0].Qtype == dns.TypeAAAA {
				return nodata(req, secureV)
			}
			m := upAnswer(req)
			m.AuthenticatedData = aVerdict.Status == "secure"
			return m, upstream.Info{Upstream: "fake-upstream", DNSSEC: aVerdict}, nil
		}
	}
	e := newEnv(t, func(a *settings.All) { a.DNS.DNS64.Enabled = true }).serve()
	// A secure A answer: synthesised AAAA without AD, logged secure.
	e.up.setAnswer(answer(secureV))
	m := e.query("udp", "v4only.example.com", dns.TypeAAAA, withEDNS(1232, true))
	if len(answerIPs(m.Answer)) != 1 || m.AuthenticatedData {
		t.Fatalf("synthesis: %v", m)
	}
	if ev := e.logs.waitEvent(t, "v4only.example.com", 0); ev.DNSSECStatus != "secure" || ev.Reason != ReasonDNS64 {
		t.Fatalf("logged %+v", ev)
	}
	// DO and CD: no synthesis (RFC 6147 5.5).
	m = e.query("udp", "v4only.example.com", dns.TypeAAAA, withEDNS(1232, true), withCD())
	if len(m.Answer) != 0 || m.Rcode != dns.RcodeSuccess {
		t.Fatalf("DO+CD: %v", m)
	}
	// A bogus A answer: SERVFAIL instead of the NODATA answer; the logged
	// status is the worst of both fetches.
	e.up.setAnswer(answer(bogusV))
	m = e.query("udp", "v4bad.example.com", dns.TypeAAAA, withEDNS(1232, true))
	if m.Rcode != dns.RcodeServerFailure {
		t.Fatalf("bogus A: %v", m)
	}
	if ev := e.logs.waitEvent(t, "v4bad.example.com", 0); ev.DNSSECStatus != "bogus" || ev.Status != StatusError {
		t.Fatalf("logged %+v", ev)
	}
}

func TestBogusTargets(t *testing.T) {
	t.Run("local CNAME", func(t *testing.T) {
		e := newEnv(t, nil).serve()
		e.addRecord("alias.lan", "CNAME", "bad.example.com")
		e.up.setAnswer(verdictFor(map[string]*upstream.Verdict{"bad.example.com": bogusV}))
		m := e.query("udp", "alias.lan", dns.TypeA, withEDNS(1232, true))
		if m.Rcode != dns.RcodeServerFailure {
			t.Fatalf("local CNAME: %v", m)
		}
		if ev := e.logs.waitEvent(t, "alias.lan", 0); ev.DNSSECStatus != "bogus" || !strings.HasPrefix(ev.Reason, "dnssec: ") {
			t.Fatalf("logged %+v", ev)
		}
		e.up.setAnswer(verdictFor(map[string]*upstream.Verdict{"bad.example.com": secureV}))
		m = e.query("udp", "alias.lan", dns.TypeA, withEDNS(1232, true))
		if m.Rcode != dns.RcodeSuccess || m.AuthenticatedData {
			t.Fatalf("secure target: %v", m)
		}
		if ev := e.logs.waitEvent(t, "alias.lan", 1); ev.DNSSECStatus != "secure" {
			t.Fatalf("logged %+v", ev)
		}
	})
	t.Run("safe search", func(t *testing.T) {
		e := newEnv(t, nil)
		e.srv.d.Parental = &fakeParental{safe: map[string]parental.SafeSearchRewrite{
			"www.bing.com": {Target: "strict.bing.com", Group: "Kids"}}}
		e.serve()
		e.up.setAnswer(verdictFor(map[string]*upstream.Verdict{"strict.bing.com": bogusV}))
		m := e.query("udp", "www.bing.com", dns.TypeA, withEDNS(1232, true))
		if m.Rcode != dns.RcodeServerFailure {
			t.Fatalf("safe search: %v", m)
		}
		if ede := ednsEDE(m); ede == nil || ede.InfoCode != 6 {
			t.Fatalf("EDE %+v", ede)
		}
		ev := e.logs.waitEvent(t, "www.bing.com", 0)
		if ev.Status != StatusError || ev.DNSSECStatus != "bogus" || !strings.HasPrefix(ev.Reason, "safe search: dnssec: ") {
			t.Fatalf("logged %+v", ev)
		}
	})
}

func TestRoutesNotValidated(t *testing.T) {
	e := newEnv(t, func(a *settings.All) {
		a.DNS.DNSSECMode = settings.DNSSECValidate
		a.DNS.LocalPTRUpstreams = []string{"192.168.1.1"}
	})
	ctx := context.Background()
	if _, err := e.srv.CreateForwarder(ctx, ForwarderInput{Domains: []string{"corp.example"}, Upstreams: []string{"192.0.2.53"},
		Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.srv.CreateForwarder(ctx, ForwarderInput{Domains: []string{"public.example"}, Upstreams: []string{"192.0.2.54"},
		Enabled: true, Validate: true}); err != nil {
		t.Fatal(err)
	}
	lookup := func(name, qtype string) LookupResult {
		t.Helper()
		res, err := e.srv.Lookup(ctx, LookupRequest{Name: name, Type: qtype, ClientIP: "192.168.1.50"}, netip.MustParseAddr("192.168.1.50"))
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	res := lookup("www.corp.example", "A")
	if res.DNSSECStatus != "" || !hasStep(res.Steps, "dnssec: not validated (conditional forwarder corp.example)") {
		t.Fatalf("forwarder without validate: %+v", res)
	}
	if c := e.up.last(); c.validate {
		t.Fatal("ResolveValidating for a forwarder without validate")
	}
	res = lookup("50.1.168.192.in-addr.arpa", "PTR")
	if res.DNSSECStatus != "" || !hasStep(res.Steps, "dnssec: not validated (local PTR upstreams)") {
		t.Fatalf("local PTR upstreams: %+v", res)
	}
	e.up.setAnswer(verdictFor(map[string]*upstream.Verdict{"www.public.example": secureV}))
	res = lookup("www.public.example", "A")
	if res.DNSSECStatus != "secure" || !hasStep(res.Steps, "dnssec: secure (example.com)") || !e.up.last().validate {
		t.Fatalf("validating forwarder: %+v %+v", res, e.up.last())
	}
}

func hasStep(steps []string, want string) bool {
	for _, s := range steps {
		if s == want {
			return true
		}
	}
	return false
}

func TestLookupDNSSECFields(t *testing.T) {
	e := newEnv(t, nil)
	e.up.setAnswer(verdictFor(map[string]*upstream.Verdict{"bad.example.com": bogusV, "old.example.com": {Status: "indeterminate",
		Reason: "stale answer"}}))
	res, err := e.srv.Lookup(context.Background(), LookupRequest{Name: "bad.example.com"}, netip.MustParseAddr("192.168.1.50"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusError || res.RCode != "SERVFAIL" || res.DNSSECStatus != "bogus" || res.DNSSECReason != "bad signature" ||
		res.DNSSECEDE == nil || res.DNSSECEDE.Code != 6 || !hasStep(res.Steps, "dnssec: bogus (example.com: bad signature)") {
		t.Fatalf("bogus: %+v", res)
	}
	res, _ = e.srv.Lookup(context.Background(), LookupRequest{Name: "old.example.com"}, netip.MustParseAddr("192.168.1.50"))
	if res.DNSSECStatus != "indeterminate" || !hasStep(res.Steps, "dnssec: indeterminate (stale answer)") {
		t.Fatalf("indeterminate: %+v", res)
	}
	// A local CNAME whose target is resolved upstream traces the target's
	// verdict like any other fetch.
	e.up.setAnswer(verdictFor(map[string]*upstream.Verdict{"www.example.com": secureV}))
	e.addRecord("alias.lan", "CNAME", "www.example.com")
	res, _ = e.srv.Lookup(context.Background(), LookupRequest{Name: "alias.lan"}, netip.MustParseAddr("192.168.1.50"))
	if res.DNSSECStatus != "secure" || !hasStep(res.Steps, "dnssec: secure (example.com)") {
		t.Fatalf("local CNAME target: %+v", res)
	}
}

func TestForwarderValidateRules(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	for _, in := range []ForwarderInput{
		{Domains: []string{"corp.example"}, Upstreams: []string{DefaultTarget}, Validate: true},
		{Domains: []string{Unqualified}, Upstreams: []string{"192.0.2.1"}, Validate: true},
		{Domains: []string{"box.lan"}, Upstreams: []string{"192.0.2.1"}, Validate: true},
		{Domains: []string{"x.home.arpa"}, Upstreams: []string{"192.0.2.1"}, Validate: true},
		{Domains: []string{"public.example", "178.168.192.in-addr.arpa"}, Upstreams: []string{"192.0.2.1"}, Validate: true},
	} {
		_, err := e.srv.CreateForwarder(ctx, in)
		var ae *apperr.Error
		if !errors.As(err, &ae) || ae.Field != "validate" || ae.Message != errValidate {
			t.Errorf("%v: %v", in.Domains, err)
		}
	}
	f, err := e.srv.CreateForwarder(ctx, ForwarderInput{Domains: []string{"public.example"}, Upstreams: []string{"192.0.2.1"},
		Enabled: true, Validate: true})
	if err != nil || !f.Validate {
		t.Fatalf("valid: %+v %v", f, err)
	}
	// The import keeps the flag of an updated line; a line whose new
	// domains break the rule is an error of field validate.
	res, err := e.srv.ImportForwarders(ctx, ForwarderImport{Text: "[/public.example/]192.0.2.9\n[/new.example/]192.0.2.8"})
	if err != nil || !res.Applied {
		t.Fatalf("import: %+v %v", res, err)
	}
	fs, _ := e.srv.Forwarders(ctx)
	for _, f := range fs {
		if want := f.Domain == "public.example"; f.Validate != want {
			t.Errorf("%s: validate %v", f.Domain, f.Validate)
		}
	}
	res, err = e.srv.ImportForwarders(ctx, ForwarderImport{Text: "[/public.example//]192.0.2.9"})
	if err != nil || res.Applied || len(res.Errors) != 1 || res.Errors[0].Field != "validate" {
		t.Fatalf("import breaking the rule: %+v %v", res, err)
	}
	// PUT without validate switches it off.
	f, err = e.srv.UpdateForwarder(ctx, fs[len(fs)-1].ID, ForwarderInput{Domains: fs[len(fs)-1].Domains,
		Upstreams: fs[len(fs)-1].Upstreams, Enabled: true})
	if err != nil || f.Validate {
		t.Fatalf("PUT: %+v %v", f, err)
	}
}

func TestValidatingForwardersRegistered(t *testing.T) {
	var got [][]string
	e := newEnv(t, nil)
	e.srv.d.ValidatingForwarders = func(t [][]string) { got = t }
	ctx := context.Background()
	if _, err := e.srv.CreateForwarder(ctx, ForwarderInput{Domains: []string{"a.example"}, Upstreams: []string{"192.0.2.1"},
		Enabled: true, Validate: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.srv.CreateForwarder(ctx, ForwarderInput{Domains: []string{"b.example"}, Upstreams: []string{"192.0.2.2"},
		Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(got[0]) != 1 || got[0][0] != "192.0.2.1" {
		t.Fatalf("registered %v", got)
	}
}

func TestSyncCarriesValidate(t *testing.T) {
	e := newEnv(t, nil)
	synced, err := e.srv.ValidateSync(nil, []Forwarder{{ID: 7, Domain: "public.example", Domains: []string{"public.example"},
		Upstreams: []string{"192.0.2.1"}, Enabled: true, Validate: true}})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.srv.d.DB.Tx(context.Background(), func(tx *sql.Tx) error { return ReplaceSynced(context.Background(), tx, synced) }); err != nil {
		t.Fatal(err)
	}
	fs, err := queryForwarders(context.Background(), e.srv.d.DB.R, 7)
	if err != nil || len(fs) != 1 || !fs[0].Validate {
		t.Fatalf("synced %+v %v", fs, err)
	}
	// A 0.16 primary's forwarder (no validate) → false; a synced forwarder
	// that breaks the rule is refused.
	if _, err := e.srv.ValidateSync(nil, []Forwarder{{ID: 8, Domain: "box.lan", Domains: []string{"box.lan"},
		Upstreams: []string{"192.0.2.1"}, Enabled: true, Validate: true}}); err == nil {
		t.Fatal("a synced forwarder breaking the rule was accepted")
	}
}

func TestDNSSchemaV4(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(filepath.Join(t.TempDir(), "picache.db"), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.Migrate(ctx, "clients", clients.Migrations()); err != nil {
		t.Fatal(err)
	}
	// dns v3 with a forwarder, then v4.
	if err := d.Migrate(ctx, "dns", migrations[:3]); err != nil {
		t.Fatal(err)
	}
	if _, err := d.W.ExecContext(ctx, `INSERT INTO dns_forwarders (domain, upstreams, enabled, comment, created_at, updated_at)
		VALUES ('old.example', '["192.0.2.1"]', 1, '', 0, 0)`); err != nil {
		t.Fatal(err)
	}
	if err := d.Migrate(ctx, "dns", migrations); err != nil {
		t.Fatal(err)
	}
	fs, err := queryForwarders(ctx, d.R, 0)
	if err != nil || len(fs) != 1 || fs[0].Validate {
		t.Fatalf("after v4: %+v %v", fs, err)
	}
	var v int
	if err := d.R.QueryRow(`SELECT MAX(version) FROM schema_migrations WHERE component = 'dns'`).Scan(&v); err != nil || v != 4 {
		t.Fatalf("dns v%d %v", v, err)
	}
}

// TestChainClientKey: the share of the DNSSEC chain exchanges a query
// draws on is keyed by the source's device: the MAC the neighbour table
// knows for the source's own identity (every address of a device, IPv6
// privacy addresses included, shares it), else its DNS rate-limit key.
func TestChainClientKey(t *testing.T) {
	set := &settings.All{}
	set.DNS.RateLimitIPv4Prefix, set.DNS.RateLimitIPv6Prefix = 24, 56
	const mac = "aa:bb:cc:dd:ee:ff"
	key := func(ip string, id *clients.Identity, derived, ednsMAC bool) string {
		return chainClientKey(&qctx{set: set, id: id, derived: derived, ednsMAC: ednsMAC}, netip.MustParseAddr(ip))
	}
	for _, ip := range []string{"fd00::1:2", "fd00::9:9", "2001:db8:1:2::77", "192.168.1.20"} {
		if k := key(ip, &clients.Identity{MAC: mac}, false, false); k != "mac "+mac {
			t.Errorf("%s with a known MAC: %q", ip, k)
		}
	}
	for _, tc := range []struct {
		ip               string
		id               *clients.Identity
		derived, ednsMAC bool
		want             string
	}{
		{"fd00::1", &clients.Identity{}, false, false, "fd00::1/128"}, // LAN addresses: one each
		{"192.168.1.9", &clients.Identity{}, false, false, "192.168.1.9/32"},
		{"203.0.113.9", &clients.Identity{}, false, false, "203.0.113.0/24"}, // public: the network
		{"2001:db8:9::1", nil, false, false, "2001:db8:9::/56"},
		// A MAC or address a trusted forwarder named in EDNS is not the
		// source's own.
		{"192.168.1.9", &clients.Identity{MAC: mac}, true, false, "192.168.1.9/32"},
		{"192.168.1.9", &clients.Identity{MAC: mac}, false, true, "192.168.1.9/32"},
	} {
		if k := key(tc.ip, tc.id, tc.derived, tc.ednsMAC); k != tc.want {
			t.Errorf("%s: %q, want %q", tc.ip, k, tc.want)
		}
	}
}
