package logs

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/settings"
)

// privacyOutcome is what one query and one cache request leave behind.
type privacyOutcome struct {
	queryRows, liveQueries  int
	dnsCounted              bool // count rollups
	kinds                   map[string]bool
	cacheRows, cacheCounted bool
	sniRows, sniCounted     bool
	sessions                bool
}

// runPrivacyCase counts and stores one query of each of two types (A and
// TXT), one cache request and one SNI connection under the log settings
// mod and the producer flags, and reports what was kept.
func runPrivacyCase(t *testing.T, mod func(*settings.Logs), noLog, noStats bool) privacyOutcome {
	t.Helper()
	s, set := newTestStore(t)
	updateLogs(t, set, mod)
	live, cancel, err := subscribe(&s.live, &s.live.queries, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	now := time.Now()
	for _, qt := range []string{"A", "TXT"} {
		q := query(now, "192.168.1.10", "ads.example", "blocked-list")
		q.QType, q.Purpose, q.Upstream = qt, "advertising", "9.9.9.9"
		q.NoLog, q.NoStats = noLog, noStats
		s.w.addQuery(q)
	}
	c := cacheEv(now, "192.168.1.10", "steam", "steam:depot:1", 100, 100, 0, 10)
	c.NoLog, c.NoStats = noLog, noStats
	s.w.addCache(c)
	s.w.addSNI(SNIEvent{Time: now, ClientIP: "192.168.1.10", SNI: "x.example", Service: "steam", BytesUp: 10, NoLog: noLog, NoStats: noStats})
	s.w.flush(now)
	s.w.checkpoint(now)
	var out privacyOutcome
	out.queryRows = count(t, s, "logs_queries")
	out.liveQueries = len(live)
	out.dnsCounted = count(t, s, "logs_dns_minute") > 0
	out.kinds = map[string]bool{}
	rows, err := s.d.R.Query(`SELECT DISTINCT kind FROM logs_dns_top_hourly`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatal(err)
		}
		out.kinds[k] = true
	}
	rows.Close()
	out.cacheRows = count(t, s, "logs_cache_requests") > 0
	var cacheReq, sniConns int
	if err := s.d.R.QueryRow(`SELECT COALESCE(SUM(requests), 0), COALESCE(SUM(sni_conns), 0) FROM logs_cache_minute`).Scan(&cacheReq, &sniConns); err != nil {
		t.Fatal(err)
	}
	out.cacheCounted = cacheReq > 0 && count(t, s, "logs_cache_top_hourly") > 0
	out.sniRows, out.sniCounted = count(t, s, "logs_sni") > 0, sniConns > 0
	out.sessions = count(t, s, "logs_downloads") > 0
	return out
}

func kindSet(kinds ...string) map[string]bool {
	m := map[string]bool{}
	for _, k := range kinds {
		m[k] = true
	}
	return m
}

// Every privacy switch and preset of ARCHITECTURE 11 (table): what is
// stored, published live and counted.
func TestPrivacySwitches(t *testing.T) {
	noDomains := kindSet("client", "upstream", "purpose", "qtype", "unique")
	cacheAll := func(o privacyOutcome) privacyOutcome {
		o.cacheRows, o.cacheCounted, o.sniRows, o.sniCounted, o.sessions = true, true, true, true, true
		return o
	}
	preset := func(queryLog, anon, hide, stats bool) func(*settings.Logs) {
		return func(g *settings.Logs) {
			g.QueryLogEnabled, g.AnonymizeClientIPs, g.HideDomains, g.StatsEnabled = queryLog, anon, hide, stats
		}
	}
	for _, tc := range []struct {
		name           string
		mod            func(*settings.Logs)
		noLog, noStats bool
		want           privacyOutcome
	}{
		{"defaults (full)", func(*settings.Logs) {}, false, false,
			cacheAll(privacyOutcome{queryRows: 2, liveQueries: 2, dnsCounted: true, kinds: kindSet("blocked", "client", "upstream", "purpose", "qtype", "unique")})},
		{"query log off", func(g *settings.Logs) { g.QueryLogEnabled = false }, false, false,
			cacheAll(privacyOutcome{dnsCounted: true, kinds: kindSet("blocked", "client", "upstream", "purpose", "qtype", "unique")})},
		{"statistics off", func(g *settings.Logs) { g.StatsEnabled = false }, false, false,
			cacheAll(privacyOutcome{queryRows: 2, liveQueries: 2, kinds: kindSet()})},
		{"hide domains", func(g *settings.Logs) { g.HideDomains = true }, false, false,
			cacheAll(privacyOutcome{queryRows: 2, liveQueries: 2, dnsCounted: true, kinds: noDomains})},
		{"anonymise", func(g *settings.Logs) { g.AnonymizeClientIPs = true }, false, false,
			cacheAll(privacyOutcome{queryRows: 2, liveQueries: 2, dnsCounted: true, kinds: kindSet("blocked", "client", "upstream", "purpose", "qtype", "unique")})},
		{"only address queries", func(g *settings.Logs) { g.StatsOnlyAddressQueries = true }, false, false,
			cacheAll(privacyOutcome{queryRows: 2, liveQueries: 2, dnsCounted: true, kinds: kindSet("blocked", "client", "upstream", "purpose", "qtype", "unique")})},
		{"client ignoreLogs", func(*settings.Logs) {}, true, false,
			privacyOutcome{dnsCounted: true, kinds: kindSet("blocked", "client", "upstream", "purpose", "qtype", "unique"), cacheCounted: true, sniCounted: true}},
		{"client ignoreStats", func(*settings.Logs) {}, false, true,
			privacyOutcome{queryRows: 2, liveQueries: 2, kinds: kindSet(), cacheRows: true, sniRows: true, sessions: true}},
		{"preset full", preset(true, false, false, true), false, false,
			cacheAll(privacyOutcome{queryRows: 2, liveQueries: 2, dnsCounted: true, kinds: kindSet("blocked", "client", "upstream", "purpose", "qtype", "unique")})},
		{"preset hide-domains", preset(true, false, true, true), false, false,
			cacheAll(privacyOutcome{queryRows: 2, liveQueries: 2, dnsCounted: true, kinds: noDomains})},
		{"preset anonymous", preset(true, true, true, true), false, false,
			cacheAll(privacyOutcome{queryRows: 2, liveQueries: 2, dnsCounted: true, kinds: noDomains})},
		{"preset off", preset(false, true, true, false), false, false,
			cacheAll(privacyOutcome{kinds: kindSet()})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := runPrivacyCase(t, tc.mod, tc.noLog, tc.noStats)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got  %+v\nwant %+v", got, tc.want)
			}
		})
	}
}

// statsOnlyAddressQueries: other types are not in the counts or the top
// kinds, but the query-type chart keeps them.
func TestStatsOnlyAddressQueriesKeepsQTypes(t *testing.T) {
	s, set := newTestStore(t)
	updateLogs(t, set, func(g *settings.Logs) { g.StatsOnlyAddressQueries = true })
	now := time.Now()
	for _, qt := range []string{"A", "AAAA", "HTTPS", "TXT", "PTR", "SRV"} {
		q := query(now, "10.0.0.1", "x-"+strings.ToLower(qt)+".example", "forwarded")
		q.QType = qt
		s.w.addQuery(q)
	}
	s.w.flush(now)
	ctx := context.Background()
	sum, err := s.Summary(ctx, now.Add(-time.Hour), now.Add(time.Minute))
	if err != nil || sum.DNSQueries != 3 || sum.UniqueDomains != 3 {
		t.Fatalf("summary %+v, %v", sum, err)
	}
	qt, err := s.QTypes(ctx, now.Add(-time.Hour), now.Add(time.Minute))
	if err != nil || len(qt.QTypes) != 6 {
		t.Fatalf("qtypes %+v, %v", qt, err)
	}
	top, err := s.Top(ctx, TopDomains, now.Add(-time.Hour), now.Add(time.Minute), 10)
	if err != nil || len(top) != 3 {
		t.Fatalf("top %+v, %v", top, err)
	}
}

// hideDomains: a planted name in every string field of a query event
// survives cleanQuery only where the rule keeps it (the reason of the
// listed statuses, the upstream, the service, the ECS, an EDE text without
// a dot, a colon or the name).
func TestHideDomainsFieldRules(t *testing.T) {
	const secret = "secret.example"
	now := time.Now()
	ev := QueryEvent{Time: now, ClientIP: "10.0.0.1", ClientName: "laptop", QName: secret, QType: "A",
		Status: "forwarded", RCode: "NOERROR", Reason: "CNAME " + secret, Service: "svc", Upstream: "https://dns.quad9.net/dns-query",
		Answer: "CNAME " + secret + ", 192.0.2.1", UpstreamAnswer: "CNAME " + secret, Protocol: "udp",
		UpstreamEDE: &UpstreamEDE{Code: 15, Text: "blocked"}, ECS: "203.0.113.0/24"}
	got := cleanQuery(ev, false, true, now)
	v := reflect.ValueOf(got)
	for i := range v.NumField() {
		if f := v.Field(i); f.Kind() == reflect.String && strings.Contains(f.String(), secret) {
			t.Errorf("%s keeps the domain: %q", v.Type().Field(i).Name, f.String())
		}
	}
	if got.QName != "hidden" || got.Answer != "" || got.UpstreamAnswer != "" || got.Reason != "" ||
		got.Upstream != ev.Upstream || got.Service != "svc" || got.ECS != ev.ECS || got.UpstreamEDE.Text != "blocked" ||
		got.ClientName != "laptop" {
		t.Fatalf("hidden event %+v", got)
	}
	for _, tc := range []struct{ status, reason, want string }{
		{"blocked-list", "Ads list for " + secret, "Ads list for " + secret},
		{"blocked-service", "youtube", "youtube"},
		{"blocked-schedule", "Kids: bedtime", "Kids: bedtime"},
		{"blocked-special", "use-application-dns.net", "use-application-dns.net"},
		{"blocked-upstream", "dns.quad9.net: nxdomain", "dns.quad9.net: nxdomain"},
		{"safesearch", "google", "google"},
		{"blocked-rule", "||" + secret + "^", ""},
		{"blocked-cname", "list (CNAME " + secret + ")", ""},
		{"blocked-rebind", "rebind: 192.168.1.1", ""},
		{"special", "bogus-nxdomain", "bogus-nxdomain"},
		{"forwarded", "fallback", "fallback"},
		{"special", "fe80::1", ""},
	} {
		e := ev
		e.Status, e.Reason = tc.status, tc.reason
		if got := cleanQuery(e, false, true, now).Reason; got != tc.want {
			t.Errorf("%s %q: reason %q, want %q", tc.status, tc.reason, got, tc.want)
		}
	}
	// The EDE text often names the query: removed when it could (a dot, a
	// colon or the single-label name), the code kept; the producer's value
	// is not modified.
	for _, tc := range []struct{ qname, text, want string }{
		{secret, "no SEP matching the DS found for " + secret + ".", ""},
		{secret, "validation failure <" + secret + ". A IN>: no signatures", ""},
		{secret, "[2001:db8::1] timed out", ""},
		{"nas", "NAS not found", ""},
		{"nas", "blocked", "blocked"},
		{secret, "", ""},
	} {
		e := ev
		e.QName = tc.qname
		orig := &UpstreamEDE{Code: 9, Text: tc.text}
		e.UpstreamEDE = orig
		got := cleanQuery(e, false, true, now)
		if got.UpstreamEDE == nil || got.UpstreamEDE.Code != 9 || got.UpstreamEDE.Text != tc.want || orig.Text != tc.text {
			t.Errorf("%s %q: EDE %+v, want text %q", tc.qname, tc.text, got.UpstreamEDE, tc.want)
		}
	}
	// Without hideDomains nothing is removed.
	e := ev
	e.UpstreamEDE = &UpstreamEDE{Code: 9, Text: "no SEP matching the DS found for " + secret + "."}
	if got := cleanQuery(e, false, false, now); got.QName != secret || got.Answer == "" || got.UpstreamAnswer == "" ||
		!strings.Contains(got.UpstreamEDE.Text, secret) {
		t.Fatalf("not hidden: %+v", got)
	}
}

// The unique sketch counts the names although hideDomains replaces them.
func TestHideDomainsCountsUniqueNames(t *testing.T) {
	s, set := newTestStore(t)
	updateLogs(t, set, func(g *settings.Logs) { g.HideDomains = true })
	now := time.Now().Add(-time.Second) // before the default end of the query-log range
	for _, n := range []string{"a.example", "b.example", "c.example", "a.example"} {
		s.w.addQuery(query(now, "10.0.0.1", n, "forwarded"))
	}
	s.w.flush(now)
	ctx := context.Background()
	sum, err := s.Summary(ctx, now.Add(-time.Hour), now.Add(time.Minute))
	if err != nil || sum.UniqueDomains != 3 || !sum.UniqueDomainsEstimated {
		t.Fatalf("summary %+v, %v", sum, err)
	}
	page, err := s.QueryLog(ctx, QueryFilter{})
	if err != nil || len(page.Items) != 4 || page.Items[0].QName != "hidden" {
		t.Fatalf("query log %+v, %v", page, err)
	}
}

// A switch applies from the change on: stored rows are not rewritten.
func TestPrivacySwitchNotRetroactive(t *testing.T) {
	s, set := newTestStore(t)
	now := time.Now().Add(-time.Second) // before the default end of the query-log range
	s.w.addQuery(query(now, "192.168.1.10", "before.example", "forwarded"))
	s.w.flush(now)
	updateLogs(t, set, func(g *settings.Logs) { g.HideDomains, g.AnonymizeClientIPs = true, true })
	s.w.addQuery(query(now, "192.168.1.10", "after.example", "forwarded"))
	s.w.flush(now)
	page, err := s.QueryLog(context.Background(), QueryFilter{})
	if err != nil || len(page.Items) != 2 {
		t.Fatal(page, err)
	}
	if page.Items[1].QName != "before.example" || page.Items[1].ClientIP != "192.168.1.10" ||
		page.Items[0].QName != "hidden" || page.Items[0].ClientIP != "192.168.0.0" {
		t.Fatalf("rows %+v", page.Items)
	}
}

// The upstream answer: control and bidi characters removed, cut at 512
// bytes on a rune boundary, stored and read back.
func TestUpstreamAnswer(t *testing.T) {
	now := time.Now().Add(-time.Second) // before the default end of the query-log range
	long := strings.Repeat("é", 300)    // 600 bytes
	e := cleanQuery(QueryEvent{Time: now, UpstreamAnswer: "a\x00b\u202ec\u2066d\x9fe\n" + long}, false, false, now)
	if !strings.HasPrefix(e.UpstreamAnswer, "abcde") || len(e.UpstreamAnswer) > maxUpstreamAnswerLen ||
		!strings.HasSuffix(e.UpstreamAnswer, "é") {
		t.Fatalf("upstream answer %q (%d bytes)", e.UpstreamAnswer, len(e.UpstreamAnswer))
	}
	s, _ := newTestStore(t)
	q := query(now, "10.0.0.1", "x.example", "blocked-cname")
	q.UpstreamAnswer = "CNAME ads.example, 192.0.2.1"
	s.w.addQuery(q)
	s.w.flush(now)
	page, err := s.QueryLog(context.Background(), QueryFilter{})
	if err != nil || len(page.Items) != 1 || page.Items[0].UpstreamAnswer != q.UpstreamAnswer {
		t.Fatalf("stored %+v, %v", page.Items, err)
	}
}
