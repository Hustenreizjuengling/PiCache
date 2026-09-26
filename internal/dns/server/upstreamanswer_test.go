package dnsserver

import (
	"strings"
	"testing"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/dns/filter"
	"github.com/hustenreizjuengling/picache/internal/dns/upstream"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// The query log gets the upstream's answer where the final answer is not
// the upstream's: CNAME, upstream and rebind blocks, bogus NXDOMAIN, DNS64
// and a removed ipv6hint; list and rule blocks and unchanged answers
// (also cache hits) have none.
func TestUpstreamAnswerInQueryLog(t *testing.T) {
	e := newEnv(t, func(a *settings.All) {
		a.DNS.BogusNXDomain = []string{"203.0.113.66"}
		a.DNS.DNS64.Enabled = true
	})
	e.flt.check["tracker.example.net"] = listBlock("Trackers")
	e.flt.check["listed.example"] = listBlock("Ads")
	e.up.setAnswer(func(req *dns.Msg, via []string) (*dns.Msg, upstream.Info, error) {
		q := req.Question[0]
		name := normalizeName(q.Name)
		info := upstream.Info{Upstream: "u"}
		switch {
		case name == "cname.example":
			return answerWith(req, cnameRec(name, "tracker.example.net"), aRec("tracker.example.net", "192.0.2.10")), info, nil
		case name == "upblock.example":
			info.Block = &upstream.BlockInfo{Kind: upstream.BlockNullIP, Host: "u"}
			return answerWith(req, aRec(name, "0.0.0.0")), info, nil
		case name == "bogus.example":
			return answerWith(req, aRec(name, "203.0.113.66")), info, nil
		case name == "rebind.example":
			return answerWith(req, aRec(name, "192.168.1.50")), info, nil
		case name == "v4only.example" && q.Qtype == dns.TypeAAAA:
			return answerWith(req), info, nil
		case name == "v4only.example":
			return answerWith(req, aRec(name, "192.0.2.44")), info, nil
		case name == "https.example":
			return answerWith(req, httpsWithHints(name, []string{"192.0.2.1"}, []string{"2001:db8::1"})), info, nil
		case name == "cachehit.example":
			return upAnswer(req), upstream.Info{Cached: true}, nil
		}
		return upAnswer(req), info, nil
	})
	e.serve()
	for _, tc := range []struct {
		name   string
		qtype  uint16
		status string
		want   string // "" = none
	}{
		{"cname.example", dns.TypeA, StatusBlockedCNAME, "CNAME tracker.example.net, 192.0.2.10"},
		{"upblock.example", dns.TypeA, StatusBlockedUpstream, ""}, // the upstream's 0.0.0.0 is the final answer too
		{"bogus.example", dns.TypeA, StatusSpecial, "203.0.113.66"},
		{"rebind.example", dns.TypeA, StatusBlockedRebind, "192.168.1.50"},
		{"v4only.example", dns.TypeAAAA, StatusForwarded, ""}, // DNS64: the upstream had no AAAA (empty answer)
		{"listed.example", dns.TypeA, StatusBlockedList, ""},  // a list block has none
		{"plain.example", dns.TypeA, StatusForwarded, ""},     // unchanged
		{"cachehit.example", dns.TypeA, StatusCached, ""},     // unchanged cache hit
	} {
		e.query("udp", tc.name, tc.qtype)
		ev := e.logs.waitEvent(t, tc.name, 0)
		if ev.Status != tc.status || ev.UpstreamAnswer != tc.want {
			t.Errorf("%s: status %s, upstream answer %q, want %s %q", tc.name, ev.Status, ev.UpstreamAnswer, tc.status, tc.want)
		}
	}
	if ev := e.logs.waitEvent(t, "v4only.example", 0); ev.Reason != ReasonDNS64 || ev.Answer == "" {
		t.Errorf("dns64 event %+v", ev)
	}

	// A removed ipv6hint: the upstream's answer still has it.
	e.update(func(a *settings.All) { a.DNS.DNS64.Enabled, a.DNS.DisableAAAA = false, true })
	e.query("udp", "https.example", dns.TypeHTTPS)
	ev := e.logs.waitEvent(t, "https.example", 0)
	if ev.UpstreamAnswer == "" || ev.UpstreamAnswer == ev.Answer || !strings.Contains(ev.UpstreamAnswer, "ipv6hint") || strings.Contains(ev.Answer, "ipv6hint") {
		t.Errorf("ipv6hint: answer %q, upstream answer %q", ev.Answer, ev.UpstreamAnswer)
	}
	// A cached upstream answer turned into a CNAME block keeps it too.
	e.flt.check["cdn.cached.example"] = filter.Decision{Action: filter.ActionBlock, Source: "list", Kind: "exact", Name: "Ads"}
	e.up.setAnswer(func(req *dns.Msg, via []string) (*dns.Msg, upstream.Info, error) {
		return answerWith(req, cnameRec(req.Question[0].Name, "cdn.cached.example"), aRec("cdn.cached.example", "192.0.2.9")),
			upstream.Info{Cached: true}, nil
	})
	e.query("udp", "hit.example", dns.TypeA)
	if ev := e.logs.waitEvent(t, "hit.example", 0); ev.Status != StatusBlockedCNAME || ev.UpstreamAnswer != "CNAME cdn.cached.example, 192.0.2.9" {
		t.Errorf("cached CNAME block %+v", ev)
	}
}
