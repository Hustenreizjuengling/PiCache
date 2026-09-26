package filter

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/hustenreizjuengling/picache/internal/apperr"
)

// Lines of a list of answer addresses.
func TestParseIPLine(t *testing.T) {
	cases := []struct {
		line   string
		status lineStatus
		prefix string
		allow  bool
	}{
		{"192.0.2.1", lineOK, "192.0.2.1/32", false},
		{"  203.0.113.7/24  ", lineOK, "203.0.113.0/24", false},
		{"||198.51.100.1^", lineOK, "198.51.100.1/32", false},
		{"@@8.8.8.8", lineOK, "8.8.8.8/32", true},
		{"@@||8.8.4.4^", lineOK, "8.8.4.4/32", true},
		{"::ffff:192.0.2.1", lineOK, "192.0.2.1/32", false},
		{"::ffff:192.0.2.0/120", lineOK, "192.0.2.0/24", false},
		{"2001:db8:1::/48", lineOK, "2001:db8:1::/48", false},
		{"1.2.3.4 # malware host", lineOK, "1.2.3.4/32", false},
		{"# comment", lineSkip, "", false},
		{"! comment", lineSkip, "", false},
		{"", lineSkip, "", false},
		{"fe80::1%eth0", lineInvalid, "", false},
		{"||1.2.3.0/24^", lineInvalid, "", false},
		{"||1.2.3.4", lineInvalid, "", false},
		{"0.0.0.0 ads.example", lineInvalid, "", false},
		{"ads.example", lineInvalid, "", false},
		{"||ads.example^", lineInvalid, "", false},
		{"1.2.3.4$important", lineInvalid, "", false},
		{"1.2.3.999", lineInvalid, "", false},
	}
	for _, c := range cases {
		e, st := parseIPLine(c.line)
		if st != c.status || (st == lineOK && (e.prefix.String() != c.prefix || e.allow != c.allow)) {
			t.Errorf("%q: %d %v %v, want %d %s %v", c.line, st, e.prefix, e.allow, c.status, c.prefix, c.allow)
		}
	}
}

// The IP guard: no block broader than /16 or /32, of private, loopback,
// link-local, multicast or reserved networks, or of an IPv6 prefix that
// embeds IPv4.
func TestIPGuard(t *testing.T) {
	for _, s := range []string{"0.0.0.0/0", "::/0", "10.0.0.0/8", "10.1.2.3/32", "192.168.0.0/16", "192.168.1.5/32", "172.20.0.0/16",
		"100.64.0.1/32", "127.0.0.1/32", "169.254.1.1/32", "224.0.0.1/32", "240.0.0.1/32", "0.1.2.3/32", "8.0.0.0/15",
		"::/128", "::1/128", "fd00::1/128", "fe80::1/128", "ff02::1/128", "2001:db8::/31", "2002:c000:201::/48",
		"64:ff9b::c000:201/128", "64:ff9b:1::1/128", "::c000:201/128", "::ffff:c000:201/128",
		// Blocks around a prefix that embeds IPv4 would block every address it
		// carries (every NAT64/DNS64 answer).
		"64:ff9b::/32", "64:ff9b::/48", "64:ff9b::/64", "64:ff9b::/95", "64:ff9b::/40", "0:0:0:0:0:ffff::/95"} {
		p := netip.MustParsePrefix(s)
		if !ipGuarded(p) {
			t.Errorf("%s passes the guard", s)
		}
	}
	for _, s := range []string{"8.8.0.0/16", "203.0.113.5/32", "1.1.1.1/32", "2001:db8::/32", "2606:4700::1111/128", "2a00:1450::/32",
		"64:ff9a::/32", "64:ff9b:2::/48"} {
		if ipGuarded(netip.MustParsePrefix(s)) {
			t.Errorf("%s is guarded", s)
		}
	}
}

// A hostile list: every broad or private block is ignored and counted; the
// exceptions and the public blocks stay.
func TestIPListHostile(t *testing.T) {
	body := "0.0.0.0/0\n::/0\n10.0.0.0/8\n192.168.0.0/16\n@@10.0.0.0/8\n198.51.100.0/24\n2001:db8:5::/48\n"
	p, err := parseList(context.Background(), strings.NewReader(body), formatOf("block", "exact", CategorySecurity, FormatIPs))
	if err != nil {
		t.Fatal(err)
	}
	if p.broad != 4 || p.invalid != 4 || p.ipEntries != 3 || p.entries != 3 {
		t.Fatalf("broad %d invalid %d ip %d entries %d", p.broad, p.invalid, p.ipEntries, p.entries)
	}
	if !slices.Equal(p.ipBits[ipTierBlock][0], []uint8{24}) || !slices.Equal(p.ipBits[ipTierAllow][0], []uint8{8}) {
		t.Fatalf("bits %v", p.ipBits)
	}
}

// CheckIP: the longest network first, user IP rules before lists, allow
// tiers before block tiers, groups, IPv4-mapped addresses as IPv4.
func TestCheckIP(t *testing.T) {
	block, err := parseList(context.Background(), strings.NewReader("198.51.100.0/24\n198.51.100.7\n2001:db8:bad::/48\n@@198.51.100.8\n"),
		formatOf("block", "exact", CategorySecurity, FormatIPs))
	if err != nil {
		t.Fatal(err)
	}
	allow, err := parseList(context.Background(), strings.NewReader("198.51.100.9\n"), formatOf("allow", "exact", CategoryAllow, FormatIPs))
	if err != nil {
		t.Fatal(err)
	}
	s := &snapshot{
		lists:      buildListMatcher([]int64{1, 2}, []*parsed{block, allow}),
		listNames:  []string{"bad addresses", "good addresses"},
		listCats:   []string{CategorySecurity, CategoryAllow},
		listGroups: [][]int64{{1}, {1}},
		rules:      buildRuleMatcher(nil),
		ipRules: buildIPRuleMatcher([]ipRuleEntry{
			{id: 7, action: ActionAllow, pattern: "198.51.100.10", groups: []int64{1}},
			{id: 8, action: ActionBlock, pattern: "203.0.113.0/24", groups: []int64{2}},
			{id: 9, action: ActionBlock, pattern: "198.51.100.9", groups: []int64{1}},
		}),
	}
	s.hasIP = true
	cases := []struct {
		ip     string
		groups []int64
		want   Decision
	}{
		{"198.51.100.1", []int64{1}, Decision{Action: ActionBlock, Source: "list", Kind: "ip", ListID: 1, Name: "bad addresses", Category: CategorySecurity}},
		{"::ffff:198.51.100.7", []int64{1}, Decision{Action: ActionBlock, Source: "list", Kind: "ip", ListID: 1, Name: "bad addresses", Category: CategorySecurity}},
		{"198.51.100.8", []int64{1}, Decision{Action: ActionAllow, Source: "list", Kind: "ip", ListID: 1, Name: "bad addresses", Category: CategorySecurity}},
		{"198.51.100.9", []int64{1}, Decision{Action: ActionBlock, Source: "ip-rule", Kind: "ip", RuleID: 9, Name: "198.51.100.9"}},
		{"198.51.100.10", []int64{1}, Decision{Action: ActionAllow, Source: "ip-rule", Kind: "ip", RuleID: 7, Name: "198.51.100.10"}},
		{"2001:db8:bad:1::5", []int64{1}, Decision{Action: ActionBlock, Source: "list", Kind: "ip", ListID: 1, Name: "bad addresses", Category: CategorySecurity}},
		{"2001:db8:bad0::5", []int64{1}, Decision{}},
		{"203.0.113.4", []int64{1}, Decision{}},
		{"203.0.113.4", []int64{1, 2}, Decision{Action: ActionBlock, Source: "ip-rule", Kind: "ip", RuleID: 8, Name: "203.0.113.0/24"}},
		{"198.51.100.1", []int64{2}, Decision{}},
		{"198.51.101.1", []int64{1}, Decision{}},
	}
	for _, c := range cases {
		if got := s.checkIP(netip.MustParseAddr(c.ip), c.groups); got != c.want {
			t.Errorf("%s %v: %+v, want %+v", c.ip, c.groups, got, c.want)
		}
	}
	if n := testing.AllocsPerRun(100, func() { s.checkIP(netip.MustParseAddr("2001:db8:bad:1::5"), []int64{1}) }); n != 0 {
		t.Errorf("checkIP allocates %.1f times", n)
	}
	s.hasIP = false
	if d := s.checkIP(netip.MustParseAddr("198.51.100.1"), []int64{1}); d.Action != ActionNone {
		t.Error("without IP entries CheckIP must return at once")
	}
}

// Lists of format ips: validation, counts, the guard in the list and the
// statistics, and a format change that re-parses.
func TestIPListEngine(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	ips := FormatIPs
	_, err := e.CreateList(ctx, ListInput{URL: "https://lists.example/bad.txt", Format: &ips, Category: CategoryAdult})
	wantField(t, "adult address list", err, "category")
	_, err = e.CreateList(ctx, ListInput{URL: "https://lists.example/bad.txt", Format: ptr("words")})
	wantField(t, "unknown format", err, "format")
	_, err = e.CreateList(ctx, ListInput{URL: catalog[0].URL, Format: &ips})
	wantField(t, "catalogue list as addresses", err, "format")
	l := addLocalList(t, e, "addr.txt", "198.51.100.0/24\n10.0.0.0/8\n2001:db8:bad::/48\n@@198.51.100.5\n", ListInput{Format: &ips})
	if l.Format != FormatIPs || l.Category != CategoryOther {
		t.Fatalf("created %+v", l)
	}
	for e.Stats().IPEntries == 0 {
		e.runPending(ctx)
		e.compile()
	}
	got, _ := e.list(l.ID)
	st := e.Stats()
	if got.Entries != 3 || got.Invalid != 1 || got.IPBlocksIgnored != 1 || got.TLDBlocksIgnored != 0 ||
		st.IPEntries != 3 || st.Entries != 3 || st.IPGuardLists != 1 {
		t.Fatalf("list %+v stats %+v", got, st)
	}
	if d := e.CheckIP(netip.MustParseAddr("198.51.100.1"), []int64{1}); !d.Blocked() || d.ListID != l.ID {
		t.Fatalf("CheckIP %+v", d)
	}
	if d := e.CheckIP(netip.MustParseAddr("198.51.100.5"), []int64{1}); d.Action != ActionAllow {
		t.Fatalf("exception %+v", d)
	}
	if e.Check("198.51.100.1", qtypeA, []int64{1}).Blocked() {
		t.Fatal("address entries never match names")
	}
	// A category of a protection list is refused, security is fine.
	_, err = e.UpdateList(ctx, l.ID, ListInput{URL: got.URL, Category: CategoryGambling, Enabled: true})
	wantField(t, "gambling address list", err, "category")
	if u, err := e.UpdateList(ctx, l.ID, ListInput{URL: got.URL, Category: CategorySecurity, Enabled: true}); err != nil || u.Format != FormatIPs {
		t.Fatalf("security %+v %v", u, err)
	}
	// Back to domains: re-parsed, no address entry left.
	if _, err := e.UpdateList(ctx, l.ID, ListInput{URL: got.URL, Format: ptr(FormatDomains), Enabled: true}); err != nil {
		t.Fatal(err)
	}
	for e.Stats().IPEntries != 0 {
		e.runPending(ctx)
		e.compile()
	}
	if d := e.CheckIP(netip.MustParseAddr("198.51.100.1"), []int64{1}); d.Action != ActionNone {
		t.Fatalf("after the format change %+v", d)
	}
}

// User IP rules: canonical patterns, bounds, duplicates, the cap, groups.
func TestIPRules(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	for in, want := range map[string]string{
		"192.0.2.5/32": "192.0.2.5", "10.1.2.3/8": "10.0.0.0/8", "::ffff:11.0.0.0/104": "11.0.0.0/8",
		"2001:DB8::1/128": "2001:db8::1", "2001:db8:ffff::/32": "2001:db8::/32",
	} {
		r, err := e.CreateIPRule(ctx, IPRuleInput{Action: "block", Pattern: in, Enabled: true})
		if err != nil || r.Pattern != want || !slices.Equal(r.GroupIDs, []int64{1}) {
			t.Errorf("%s: %+v %v", in, r, err)
		}
	}
	for _, c := range []struct{ in, field string }{
		{"10.0.0.0/7", "pattern"}, {"2001:db8::/31", "pattern"}, {"fe80::1%eth0", "pattern"}, {"example.com", "pattern"},
	} {
		_, err := e.CreateIPRule(ctx, IPRuleInput{Action: "block", Pattern: c.in})
		wantInvalid(t, c.in, err, c.field)
	}
	_, err := e.CreateIPRule(ctx, IPRuleInput{Action: "maybe", Pattern: "192.0.2.9"})
	wantInvalid(t, "action", err, "action")
	_, err = e.CreateIPRule(ctx, IPRuleInput{Action: "block", Pattern: "192.0.2.9", GroupIDs: []int64{99}})
	wantInvalid(t, "unknown group", err, "groupIds")
	_, err = e.CreateIPRule(ctx, IPRuleInput{Action: "block", Pattern: "192.0.2.5"})
	wantKind(t, "duplicate", err, apperr.KindConflict)
	if d := e.CheckIP(netip.MustParseAddr("10.9.9.9"), []int64{1}); !d.Blocked() || d.Source != "ip-rule" || d.Name != "10.0.0.0/8" {
		t.Fatalf("CheckIP %+v", d)
	}
	allow, err := e.CreateIPRule(ctx, IPRuleInput{Action: "allow", Pattern: "10.9.9.9", Enabled: true, Comment: "the NAS", GroupIDs: []int64{2}})
	if err != nil {
		t.Fatal(err)
	}
	if d := e.CheckIP(netip.MustParseAddr("10.9.9.9"), []int64{1, 2}); d.Action != ActionAllow || d.RuleID != allow.ID {
		t.Fatalf("allow %+v", d)
	}
	list, err := e.IPRules(ctx, IPRuleQuery{Action: "allow"})
	if err != nil || len(list) != 1 || list[0].ID != allow.ID {
		t.Fatalf("filter action %+v %v", list, err)
	}
	if list, _ = e.IPRules(ctx, IPRuleQuery{Search: "nas"}); len(list) != 1 {
		t.Fatalf("search comment %+v", list)
	}
	_, err = e.IPRules(ctx, IPRuleQuery{Action: "x"})
	wantInvalid(t, "query action", err, "action")
	// Updating without groups keeps them; disabling stops the rule.
	u, err := e.UpdateIPRule(ctx, allow.ID, IPRuleInput{Action: "allow", Pattern: "10.9.9.9"})
	if err != nil || u.Enabled || !slices.Equal(u.GroupIDs, []int64{2}) {
		t.Fatalf("update %+v %v", u, err)
	}
	if d := e.CheckIP(netip.MustParseAddr("10.9.9.9"), []int64{1, 2}); !d.Blocked() {
		t.Fatalf("disabled allow %+v", d)
	}
	if err := e.DeleteIPRule(ctx, allow.ID); err != nil {
		t.Fatal(err)
	}
	wantKind(t, "delete twice", e.DeleteIPRule(ctx, allow.ID), apperr.KindNotFound)
	// A deleted group's links cascade; ReloadGroups picks it up.
	g, err := e.CreateIPRule(ctx, IPRuleInput{Action: "block", Pattern: "203.0.113.0/24", Enabled: true, GroupIDs: []int64{3}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.W.Exec(`DELETE FROM client_groups WHERE id = 3`); err != nil {
		t.Fatal(err)
	}
	if err := e.ReloadGroups(ctx); err != nil {
		t.Fatal(err)
	}
	if d := e.CheckIP(netip.MustParseAddr("203.0.113.1"), []int64{3}); d.Action != ActionNone {
		t.Fatalf("rule %d of a deleted group still applies: %+v", g.ID, d)
	}
	if st := e.Stats(); st.IPRules != 6 {
		t.Errorf("enabled IP rules %d", st.IPRules)
	}
}

// At most 1000 IP rules.
func TestIPRuleCap(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	tx, err := e.db.W.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := range maxIPRules {
		if _, err := tx.Exec(`INSERT INTO filter_ip_rules (action, pattern, enabled, comment, created_at, updated_at) VALUES ('block', ?, 0, '', 0, 0)`,
			fmt.Sprintf("198.51.%d.%d", i/250, i%250)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	_, err = e.CreateIPRule(ctx, IPRuleInput{Action: "block", Pattern: "203.0.113.1"})
	wantKind(t, "cap", err, apperr.KindConflict)
	if err == nil || !strings.Contains(err.Error(), "at most 1000 IP rules are supported") {
		t.Fatalf("message %v", err)
	}
}

// The address-list parser handles untrusted content: it never panics, the
// entries are canonical and the guard holds for every block.
func FuzzParseIPLine(f *testing.F) {
	for _, s := range []string{"1.2.3.4", "0.0.0.0/0", "::/0", "@@10.0.0.0/8", "||1.2.3.4^", "::ffff:1.2.3.4", "2002::/16",
		"64:ff9b::1.2.3.4", "fe80::1%eth0", "1.2.3.4 # x", "||1.2.3.0/24^", "2001:db8::/32"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, line string) {
		e, st := parseIPLine(line)
		if st != lineOK {
			return
		}
		p := e.prefix
		if !p.IsValid() || p != p.Masked() || p.Addr().Zone() != "" || p.Addr().Is4In6() {
			t.Fatalf("%q: not canonical: %v", line, p)
		}
		if back, ok := canonicalPrefix(p.String()); !ok || back != p {
			t.Fatalf("%q: %v does not round-trip", line, p)
		}
		if !e.allow && ipGuarded(p) {
			// The list parser counts it as invalid; a guarded block never
			// reaches the matcher.
			res, err := parseList(context.Background(), strings.NewReader(line), formatOf("block", "exact", CategoryOther, FormatIPs))
			if err == nil && res.ipEntries != 0 {
				t.Fatalf("%q: a guarded block reached the entries", line)
			}
		}
	})
}
