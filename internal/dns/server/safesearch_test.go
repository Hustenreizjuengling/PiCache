package dnsserver

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/clients"
	"github.com/hustenreizjuengling/picache/internal/dns/filter"
	"github.com/hustenreizjuengling/picache/internal/dns/parental"
	"github.com/hustenreizjuengling/picache/internal/dns/upstream"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

var localhost = netip.MustParseAddr("127.0.0.1")

// safeEnv is the pipeline environment with the real parental engine: the
// group Kids has every safe search engine on (YouTube strict) and the
// test client 127.0.0.1 is in Default and Kids.
func safeEnv(t *testing.T, mutate func(*settings.All)) (*testEnv, *parental.Engine, int64) {
	t.Helper()
	e := newEnv(t, mutate)
	ctx := context.Background()
	reg, err := clients.New(ctx, e.srv.d.DB, nil, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	p, err := parental.New(ctx, e.srv.d.DB, reg, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	kids, err := reg.CreateGroup(ctx, clients.GroupInput{Name: "Kids", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	on, strict := true, parental.YouTubeStrict
	if _, err := p.Update(ctx, kids.ID, parental.UpdateInput{SafeSearch: &parental.SafeSearchInput{Google: &on, YouTube: &strict,
		Bing: &on, DuckDuckGo: &on, Ecosia: &on, Yandex: &on, Pixabay: &on}}); err != nil {
		t.Fatal(err)
	}
	e.srv.d.Parental = p
	e.cl.set(&clients.Identity{IP: localhost, ClientID: 7, Name: "tablet", GroupIDs: []int64{clients.DefaultGroupID, kids.ID}})
	return e.serve(), p, kids.ID
}

// lookup runs Lookup as the test client.
func (e *testEnv) lookup(name, typ string) LookupResult {
	e.t.Helper()
	res, err := e.srv.Lookup(context.Background(), LookupRequest{Name: name, Type: typ}, localhost)
	if err != nil {
		e.t.Fatal(err)
	}
	return res
}

// wantCNAME checks that r starts with the CNAME name → target.
func wantCNAME(t *testing.T, r *dns.Msg, name, target string) *dns.CNAME {
	t.Helper()
	if len(r.Answer) == 0 {
		t.Fatalf("%s: no answer: %v", name, r)
	}
	c, ok := r.Answer[0].(*dns.CNAME)
	if !ok || !strings.EqualFold(c.Hdr.Name, name+".") || c.Target != target+"." {
		t.Fatalf("%s: first record %v, want CNAME to %s", name, r.Answer[0], target)
	}
	return c
}

// Every engine rewrites A, AAAA and CNAME; HTTPS, SVCB and ANY get NODATA;
// other types (MX, TXT, DS) take the normal path.
func TestSafeSearchRewrites(t *testing.T) {
	e, _, _ := safeEnv(t, func(a *settings.All) { a.DNS.RefuseANY = false })
	for _, tc := range []struct{ name, target, label string }{
		{"www.google.de", "forcesafesearch.google.com", "Google safe search"},
		{"google.com", "forcesafesearch.google.com", "Google safe search"},
		{"www.youtube.com", "restrict.youtube.com", "YouTube restricted mode (strict)"},
		{"www.bing.com", "strict.bing.com", "Bing safe search"},
		{"duckduckgo.com", "safe.duckduckgo.com", "DuckDuckGo safe search"},
		{"www.ecosia.org", "strict-safe-search.ecosia.org", "Ecosia safe search"},
		{"yandex.ru", "familysearch.yandex.ru", "Yandex family search"},
		{"pixabay.com", "safesearch.pixabay.com", "Pixabay safe search"},
	} {
		for i, qt := range []uint16{dns.TypeA, dns.TypeAAAA, dns.TypeCNAME} {
			r := e.query("udp", tc.name, qt, withEDNS(1232, false))
			c := wantCNAME(t, r, tc.name, tc.target)
			switch qt {
			case dns.TypeCNAME:
				if len(r.Answer) != 1 || c.Hdr.Ttl != safeSearchTTL {
					t.Errorf("%s CNAME: %v", tc.name, r.Answer)
				}
			default:
				want := "198.51.100.7"
				if qt == dns.TypeAAAA {
					want = "2001:db8::7"
				}
				if ips := answerIPs(r.Answer); !slices.Equal(ips, []string{want}) || r.Answer[1].Header().Name != tc.target+"." {
					t.Errorf("%s %s: %v", tc.name, dns.TypeToString[qt], r.Answer)
				}
			}
			ev := e.logs.waitEvent(t, tc.name, i)
			if ev.Status != StatusSafeSearch || ev.Reason != "Kids: "+tc.label || ev.Purpose != PurposeSafeSearch ||
				(qt != dns.TypeCNAME && ev.Upstream != "fake-upstream") {
				t.Errorf("%s: logged %+v", tc.name, ev)
			}
			if r.IsEdns0() != nil && len(r.IsEdns0().Option) != 0 {
				t.Errorf("%s: safe search is no block (no EDE): %v", tc.name, r.IsEdns0())
			}
		}
		if calls := e.up.callsFor(tc.name); len(calls) != 0 {
			t.Errorf("%s: the original name must never be forwarded: %v", tc.name, calls)
		}
	}
	for _, qt := range []uint16{dns.TypeHTTPS, dns.TypeSVCB, dns.TypeANY} {
		r := e.query("udp", "www.google.de", qt)
		if r.Rcode != dns.RcodeSuccess || len(r.Answer) != 0 || len(r.Ns) != 1 || r.Ns[0].(*dns.SOA).Minttl != safeSearchTTL {
			t.Errorf("%s: want NODATA with the synthetic SOA, got %v", dns.TypeToString[qt], r)
		}
	}
	for _, qt := range []uint16{dns.TypeMX, dns.TypeTXT, dns.TypeDS} {
		before := len(e.up.callsFor("google.com"))
		r := e.query("udp", "google.com", qt)
		if calls := e.up.callsFor("google.com"); len(calls) != before+1 || calls[len(calls)-1].qtype != qt || r.Rcode != dns.RcodeSuccess {
			t.Errorf("%s at google.com must take the normal path: %v", dns.TypeToString[qt], calls)
		}
	}
	res := e.lookup("www.youtube.com", "A")
	if res.Status != StatusSafeSearch || !slices.Contains(res.Steps, "safe search: www.youtube.com → CNAME restrict.youtube.com (Kids)") ||
		!slices.ContainsFunc(res.Steps, func(s string) bool { return strings.HasPrefix(s, "answered via upstreams") }) {
		t.Errorf("lookup: %s %q", res.Status, res.Steps)
	}
	// Names outside the table and other groups are untouched.
	if r := e.query("udp", "mail.google.com", dns.TypeA); len(r.Answer) != 1 || r.Answer[0].Header().Rrtype != dns.TypeA {
		t.Errorf("mail.google.com: %v", r.Answer)
	}
	e.cl.set(&clients.Identity{IP: localhost, GroupIDs: []int64{clients.DefaultGroupID}})
	if r := e.query("udp", "www.google.de", dns.TypeA); len(r.Answer) != 1 || r.Answer[0].Header().Rrtype != dns.TypeA {
		t.Errorf("Default group: %v", r.Answer)
	}
}

// A block for the client wins over safe search; allow rules do not lift it.
func TestSafeSearchGuard(t *testing.T) {
	e, p, kids := safeEnv(t, func(a *settings.All) { a.DNS.DroppedDomains = []string{"duckduckgo.com"} })
	e.flt.check["www.bing.com"] = filter.Decision{Action: filter.ActionBlock, Source: "rule", Kind: "subtree", RuleID: 3, Name: "bing.com"}
	e.flt.check["www.google.com"] = listBlock("Search engines")
	e.flt.rules["pixabay.com"] = filter.Decision{Action: filter.ActionAllow, Source: "rule", Kind: "exact", RuleID: 4, Name: "pixabay.com"}
	e.flt.check["pixabay.com"] = e.flt.rules["pixabay.com"]
	if _, err := p.Update(context.Background(), kids, parental.UpdateInput{BlockedServices: []string{"youtube"}}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, status string }{
		{"www.bing.com", StatusBlockedRule},
		{"www.google.com", StatusBlockedList},
		{"www.youtube.com", StatusBlockedService},
		{"pixabay.com", StatusSafeSearch},
		{"duckduckgo.com", StatusDropped},
	} {
		res := e.lookup(tc.name, "A")
		if res.Status != tc.status {
			t.Errorf("%s: status %s, want %s (%q)", tc.name, res.Status, tc.status, res.Steps)
		}
	}
	if res := e.lookup("www.bing.com", "A"); !slices.Contains(res.Steps, "safe search: not applied, www.bing.com is blocked for the client") ||
		!slices.Equal(res.Answers, []string{"www.bing.com.\t10\tIN\tA\t0.0.0.0"}) {
		t.Errorf("guard trace %q answers %q", res.Steps, res.Answers)
	}
	// While blocking is paused the guard does not run: safe search applies.
	if _, err := e.srv.SetBlocking(context.Background(), false, time.Hour); err != nil {
		t.Fatal(err)
	}
	if res := e.lookup("www.bing.com", "A"); res.Status != StatusSafeSearch {
		t.Errorf("paused: %s", res.Status)
	}
}

// Safe search is content protection: blocking paused or disabled, the
// group's filtering paused and the allow override leave it on.
func TestSafeSearchIndependentOfBlocking(t *testing.T) {
	e, p, kids := safeEnv(t, nil)
	ctx := context.Background()
	check := func(what string) {
		t.Helper()
		if r := e.query("udp", "www.google.de", dns.TypeA); len(r.Answer) == 0 || r.Answer[0].Header().Rrtype != dns.TypeCNAME {
			t.Errorf("%s: safe search must stay on, got %v", what, r.Answer)
		}
	}
	if _, err := e.srv.SetBlocking(ctx, false, time.Hour); err != nil {
		t.Fatal(err)
	}
	check("global pause")
	if _, err := e.srv.SetBlocking(ctx, false, 0); err != nil {
		t.Fatal(err)
	}
	check("blocking disabled")
	if _, err := e.srv.SetBlocking(ctx, true, 0); err != nil {
		t.Fatal(err)
	}
	minutes := 30
	if _, err := p.SetPause(ctx, kids, parental.PauseInput{Minutes: &minutes}); err != nil {
		t.Fatal(err)
	}
	check("group paused")
	if _, err := p.SetOverride(ctx, kids, parental.OverrideInput{Mode: parental.OverrideAllow, Minutes: &minutes}); err != nil {
		t.Fatal(err)
	}
	check("allow override")
	e.flt.rules["www.google.de"] = filter.Decision{Action: filter.ActionAllow, Source: "rule", Kind: "exact", RuleID: 1, Name: "www.google.de"}
	check("user allow rule")
}

// The target's answer runs the forwarding steps with the client's context.
func TestSafeSearchTarget(t *testing.T) {
	var targetAnswer func(req *dns.Msg) (*dns.Msg, upstream.Info, error)
	setUp := func(e *testEnv) {
		e.up.setAnswer(func(req *dns.Msg, via []string) (*dns.Msg, upstream.Info, error) {
			if targetAnswer != nil && normalizeName(req.Question[0].Name) == "forcesafesearch.google.com" {
				return targetAnswer(req)
			}
			return upAnswer(req), upstream.Info{Upstream: "fake-upstream"}, nil
		})
	}

	t.Run("ttl and AD", func(t *testing.T) {
		e, _, _ := safeEnv(t, nil)
		setUp(e)
		for _, tc := range []struct{ ttl, want uint32 }{{30, 30}, {3600, 300}} {
			targetAnswer = func(req *dns.Msg) (*dns.Msg, upstream.Info, error) {
				m := upAnswer(req)
				m.Answer[0].Header().Ttl = tc.ttl
				m.AuthenticatedData = true
				return m, upstream.Info{Upstream: "fake-upstream"}, nil
			}
			r := e.query("udp", "www.google.de", dns.TypeA, withEDNS(1232, true))
			if c := wantCNAME(t, r, "www.google.de", "forcesafesearch.google.com"); c.Hdr.Ttl != tc.want || r.AuthenticatedData {
				t.Errorf("ttl %d: CNAME TTL %d, AD %v", tc.ttl, c.Hdr.Ttl, r.AuthenticatedData)
			}
		}
		// NXDOMAIN keeps the target's SOA and its negative TTL.
		targetAnswer = func(req *dns.Msg) (*dns.Msg, upstream.Info, error) {
			m := new(dns.Msg)
			m.SetRcode(req, dns.RcodeNameError)
			m.Ns = []dns.RR{syntheticSOA("google.com.", 60)}
			return m, upstream.Info{Upstream: "fake-upstream"}, nil
		}
		r := e.query("udp", "www.google.de", dns.TypeA)
		if c := wantCNAME(t, r, "www.google.de", "forcesafesearch.google.com"); r.Rcode != dns.RcodeNameError || len(r.Ns) != 1 || c.Hdr.Ttl != 60 {
			t.Errorf("NXDOMAIN target: %v", r)
		}
	})

	t.Run("failure", func(t *testing.T) {
		e, _, _ := safeEnv(t, nil)
		setUp(e)
		targetAnswer = func(req *dns.Msg) (*dns.Msg, upstream.Info, error) {
			return nil, upstream.Info{}, errors.New("all upstreams failed")
		}
		r := e.query("udp", "www.google.de", dns.TypeA)
		if r.Rcode != dns.RcodeServerFailure || len(r.Answer) != 0 {
			t.Errorf("failure: %v", r)
		}
		ev := e.logs.waitEvent(t, "www.google.de", 0)
		if ev.Status != StatusError || !strings.HasPrefix(ev.Reason, "safe search: ") {
			t.Errorf("logged %+v", ev)
		}
		targetAnswer = func(req *dns.Msg) (*dns.Msg, upstream.Info, error) {
			m := new(dns.Msg)
			m.SetRcode(req, dns.RcodeServerFailure)
			return m, upstream.Info{Upstream: "fake-upstream"}, nil
		}
		if r := e.query("udp", "www.google.de", dns.TypeA); r.Rcode != dns.RcodeServerFailure || len(r.Answer) != 0 {
			t.Errorf("SERVFAIL of the target: %v", r)
		}
	})

	t.Run("rebind", func(t *testing.T) {
		e, _, _ := safeEnv(t, nil)
		setUp(e)
		targetAnswer = func(req *dns.Msg) (*dns.Msg, upstream.Info, error) {
			m := new(dns.Msg)
			m.SetReply(req)
			m.Answer = []dns.RR{&dns.A{Hdr: rrHeader(req.Question[0].Name, dns.TypeA, 300), A: net.ParseIP("10.0.0.1").To4()}}
			return m, upstream.Info{Upstream: "fake-upstream"}, nil
		}
		r := e.query("udp", "www.google.de", dns.TypeA)
		if ips := answerIPs(r.Answer); !slices.Equal(ips, []string{"0.0.0.0"}) || r.Answer[0].Header().Name != "www.google.de." ||
			r.Question[0].Name != "www.google.de." {
			t.Errorf("rebind: %v", r)
		}
		if ev := e.logs.waitEvent(t, "www.google.de", 0); ev.Status != StatusBlockedRebind || ev.Purpose != PurposeRebind {
			t.Errorf("logged %+v", ev)
		}
	})

	t.Run("disableAAAA", func(t *testing.T) {
		e, _, _ := safeEnv(t, func(a *settings.All) { a.DNS.DisableAAAA = true })
		r := e.query("udp", "www.google.de", dns.TypeAAAA)
		c := wantCNAME(t, r, "www.google.de", "forcesafesearch.google.com")
		if len(r.Answer) != 1 || len(r.Ns) != 1 || r.Rcode != dns.RcodeSuccess || c.Hdr.Ttl != specialTTL {
			t.Errorf("disableAAAA: %v", r)
		}
		if calls := e.up.callsFor("forcesafesearch.google.com"); len(calls) != 0 {
			t.Errorf("AAAA must not be forwarded: %v", calls)
		}
	})

	t.Run("dns64", func(t *testing.T) {
		e, _, _ := safeEnv(t, func(a *settings.All) { a.DNS.DNS64.Enabled = true })
		setUp(e)
		targetAnswer = func(req *dns.Msg) (*dns.Msg, upstream.Info, error) {
			if req.Question[0].Qtype == dns.TypeAAAA {
				m := new(dns.Msg)
				m.SetReply(req)
				m.Ns = []dns.RR{syntheticSOA("google.com.", 300)}
				return m, upstream.Info{Upstream: "fake-upstream"}, nil
			}
			return upAnswer(req), upstream.Info{Upstream: "fake-upstream"}, nil
		}
		r := e.query("udp", "www.google.de", dns.TypeAAAA)
		wantCNAME(t, r, "www.google.de", "forcesafesearch.google.com")
		if ips := answerIPs(r.Answer); !slices.Equal(ips, []string{"64:ff9b::c633:6407"}) {
			t.Errorf("dns64: %v", r.Answer)
		}
	})

	t.Run("forwarder", func(t *testing.T) {
		e, _, _ := safeEnv(t, nil)
		if _, err := e.srv.CreateForwarder(context.Background(), ForwarderInput{Domain: "youtube.com", Upstreams: []string{"10.9.9.8"},
			Enabled: true}); err != nil {
			t.Fatal(err)
		}
		wantCNAME(t, e.query("udp", "m.youtube.com", dns.TypeA), "m.youtube.com", "restrict.youtube.com")
		if c := e.up.callsFor("restrict.youtube.com"); len(c) == 0 || !slices.Equal(c[len(c)-1].via, []string{"10.9.9.8"}) {
			t.Errorf("the target is matched against the forwarders: %v", c)
		}
	})
}

// Protection lists are enforced at 7a: independent of the blocking switch
// and the group's pause, not lifted by the allow override or an
// allowlist, lifted by a user allow rule.
func TestProtectionLists(t *testing.T) {
	e, p, kids := safeEnv(t, nil)
	ctx := context.Background()
	e.flt.protect["porn.example"] = filter.Decision{Action: filter.ActionBlock, Source: "list", Kind: "subtree", ListID: 5,
		Name: "OISD NSFW", Category: filter.CategoryAdult}
	e.flt.check["porn.example"] = filter.Decision{Action: filter.ActionAllow, Source: "list", Kind: "exact", ListID: 6, Name: "Fixes"}
	blocked := func(what string) {
		t.Helper()
		if r := e.query("udp", "porn.example", dns.TypeA); !slices.Equal(answerIPs(r.Answer), []string{"0.0.0.0"}) {
			t.Errorf("%s: protection list must block, got %v", what, r.Answer)
		}
	}
	blocked("an allowlist of the client")
	ev := e.logs.waitEvent(t, "porn.example", 0)
	if ev.Status != StatusBlockedList || ev.Reason != "OISD NSFW" || ev.ListID != 5 || ev.Purpose != filter.CategoryAdult {
		t.Errorf("logged %+v", ev)
	}
	if res := e.lookup("porn.example", "A"); !slices.Contains(res.Steps, `parental: blocked by list "OISD NSFW" (adult): null reply`) {
		t.Errorf("trace %q", res.Steps)
	}
	if _, err := e.srv.SetBlocking(ctx, false, time.Hour); err != nil {
		t.Fatal(err)
	}
	blocked("global pause")
	if _, err := e.srv.SetBlocking(ctx, false, 0); err != nil {
		t.Fatal(err)
	}
	blocked("blocking disabled")
	minutes := 60
	if _, err := p.SetPause(ctx, kids, parental.PauseInput{Minutes: &minutes}); err != nil {
		t.Fatal(err)
	}
	blocked("group paused")
	if _, err := p.SetOverride(ctx, kids, parental.OverrideInput{Mode: parental.OverrideAllow, Minutes: &minutes}); err != nil {
		t.Fatal(err)
	}
	blocked("allow override")
	e.flt.rules["porn.example"] = filter.Decision{Action: filter.ActionAllow, Source: "rule", Kind: "exact", RuleID: 2, Name: "porn.example"}
	if r := e.query("udp", "porn.example", dns.TypeA); !slices.Equal(answerIPs(r.Answer), []string{"198.51.100.7"}) {
		t.Errorf("a user allow rule lifts it: %v", r.Answer)
	}
	if res := e.lookup("porn.example", "A"); !slices.Contains(res.Steps, `parental block lifted by allow rule "porn.example" (OISD NSFW)`) {
		t.Errorf("lift trace %q", res.Steps)
	}
}

// The group pause: the paused group's lists and rules stop (steps 8, 11,
// 14, the 14b check); the client's other groups still apply; all groups
// paused = blocking inactive.
func TestGroupPause(t *testing.T) {
	e := pipelineEnv(t)
	until := time.Now().Add(time.Hour)
	p := &fakeParental{block: map[string]parental.Decision{},
		paused: map[int64]parental.GroupPause{2: {GroupID: 2, Group: "Kids", Until: until}}}
	e.srv.d.Parental = p
	e.cl.set(&clients.Identity{IP: localhost, GroupIDs: []int64{1, 2}})
	e.flt.scoped["ads.example.com"] = 2     // a list of Kids only
	e.flt.scoped["tracker.example.net"] = 2 // CNAME target blocked for Kids only
	e.flt.scoped["blocked.steamcontent.com"] = 2
	e.flt.check["mixed.example"] = listBlock("Default list")
	e.flt.scoped["mixed.example"] = 1 // a list of Default

	for _, tc := range []struct {
		name string
		ips  []string
	}{
		{"ads.example.com", []string{"198.51.100.7"}},          // 11: Kids' list paused
		{"www.cnamed.example", []string{"198.51.100.7"}},       // 14: the target is checked with the filtering groups
		{"blocked.steamcontent.com", []string{"192.168.1.10"}}, // 8: Kids' user rule paused, the download cache answers
		{"mixed.example", []string{"0.0.0.0"}},                 // mixed membership: Default still applies
	} {
		if r := e.query("udp", tc.name, dns.TypeA); !slices.Equal(answerIPs(r.Answer), tc.ips) {
			t.Errorf("%s: %v, want %v", tc.name, answerIPs(r.Answer), tc.ips)
		}
	}
	if got := e.flt.lastChecked("ads.example.com"); !slices.Equal(got, []int64{1}) {
		t.Errorf("Check got groups %v, want the filtering groups", got)
	}
	res := e.lookup("ads.example.com", "A")
	if !slices.Contains(res.Steps, "filtering paused for group Kids until "+until.UTC().Format(time.RFC3339)) ||
		!slices.Contains(res.Steps, "blocking is active (mode null)") {
		t.Errorf("trace %q", res.Steps)
	}
	// Without the pause Kids' entries apply again.
	p.mu.Lock()
	p.paused = nil
	p.mu.Unlock()
	if r := e.query("udp", "ads.example.com", dns.TypeA); !slices.Equal(answerIPs(r.Answer), []string{"0.0.0.0"}) {
		t.Errorf("unpaused: %v", r.Answer)
	}
	if r := e.query("udp", "www.cnamed.example", dns.TypeA); !slices.Equal(answerIPs(r.Answer), []string{"0.0.0.0"}) {
		t.Errorf("unpaused CNAME inspection: %v", r.Answer)
	}
	// Every group of the client paused: blocking is inactive for it.
	p.mu.Lock()
	p.paused = map[int64]parental.GroupPause{1: {GroupID: 1, Group: "Default", Until: until}, 2: {GroupID: 2, Group: "Kids", Until: until}}
	p.mu.Unlock()
	if r := e.query("udp", "mixed.example", dns.TypeA); !slices.Equal(answerIPs(r.Answer), []string{"198.51.100.7"}) {
		t.Errorf("all groups paused: %v", r.Answer)
	}
	if res := e.lookup("mixed.example", "A"); !slices.Contains(res.Steps, "blocking is inactive: filtering is paused for every group of the client") {
		t.Errorf("trace %q", res.Steps)
	}
	// The special domains stay blocked (their own settings decide).
	if r := e.query("udp", "use-application-dns.net", dns.TypeA); r.Rcode != dns.RcodeNameError {
		t.Errorf("canary during the group pause: %s", dns.RcodeToString[r.Rcode])
	}
}

// An allowlisted special domain is left alone (Filter.Check with all
// groups, computed for these names only).
func TestSpecialDomainAllowlisted(t *testing.T) {
	e := pipelineEnv(t)
	e.flt.check["mask.icloud.com"] = filter.Decision{Action: filter.ActionAllow, Source: "rule", Kind: "exact", RuleID: 1, Name: "mask.icloud.com"}
	if _, err := e.srv.SetBlocking(context.Background(), false, 0); err != nil {
		t.Fatal(err)
	}
	if r := e.query("udp", "mask.icloud.com", dns.TypeA); r.Rcode != dns.RcodeSuccess || len(r.Answer) == 0 {
		t.Errorf("allowlisted special domain: %v", r)
	}
	e.query("udp", "www.example.com", dns.TypeA)
	if calls := e.flt.lastChecked("www.example.com"); calls != nil {
		t.Error("Check must not run for other names while blocking is off")
	}
}

// The purpose of a logged query follows the mechanism that decided it.
func TestQueryPurpose(t *testing.T) {
	e, _ := parentalEnv(t)
	e.flt.check["sec.example"] = filter.Decision{Action: filter.ActionBlock, Source: "list", Kind: "subtree", ListID: 8, Name: "TIF",
		Category: filter.CategorySecurity}
	e.flt.check["re.example"] = filter.Decision{Action: filter.ActionBlock, Source: "rule", Kind: "regex", RuleID: 3, Name: "/re/"}
	e.flt.check["rule.example"] = ruleBlock("rule.example")
	for _, tc := range []struct{ name, status, purpose string }{
		{"ads.example.com", StatusBlockedList, filter.CategoryOther},
		{"sec.example", StatusBlockedList, filter.CategorySecurity},
		{"re.example", StatusBlockedRegex, PurposeRule},
		{"rule.example", StatusBlockedRule, PurposeRule},
		{"www.cnamed.example", StatusBlockedCNAME, filter.CategoryOther},
		{"www.youtube.com", StatusBlockedService, PurposeService},
		{"late.example", StatusBlockedSchedule, PurposeSchedule},
		{"use-application-dns.net", StatusBlockedSpecial, PurposeSpecial},
		{"www.example.com", StatusForwarded, ""},
	} {
		e.query("udp", tc.name, dns.TypeA)
		if ev := e.logs.waitEvent(t, tc.name, 0); ev.Status != tc.status || ev.Purpose != tc.purpose {
			t.Errorf("%s: %s/%q, want %s/%q", tc.name, ev.Status, ev.Purpose, tc.status, tc.purpose)
		}
	}
	for status, want := range map[string]string{StatusBlockedUpstream: PurposeUpstream, StatusBlockedRebind: PurposeRebind,
		StatusSafeSearch: PurposeSafeSearch, StatusCached: "", StatusError: ""} {
		if got := purposeOf(result{status: status}); got != want {
			t.Errorf("%s: %q, want %q", status, got, want)
		}
	}
}

func TestBlockingStatusTimeZone(t *testing.T) {
	e := newEnv(t, nil)
	zone, offset := time.Now().Zone()
	if st := e.srv.Blocking(); st.TimeZone != zone || st.UTCOffsetMinutes != offset/60 {
		t.Errorf("status %+v, want %s %d", st, zone, offset/60)
	}
}
