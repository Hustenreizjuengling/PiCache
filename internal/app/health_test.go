package app

import (
	"fmt"
	"strings"
	"testing"

	"github.com/hustenreizjuengling/picache/internal/dns/filter"
	"github.com/hustenreizjuengling/picache/internal/dns/upstream"
)

// The health check "upstreams" with fallbacks (first match): clock guard
// warns; no healthy default upstream and no healthy fallback fails; no
// healthy default upstream warns at once; otherwise at least 3 fallback
// answers within 5 minutes warn (one or two did for 5 minutes before).
func TestUpstreamHealth(t *testing.T) {
	up := []upstream.UpstreamStat{{Upstream: "a", Healthy: true}}
	down := []upstream.UpstreamStat{{Upstream: "a", Healthy: false}}
	fbUp := []upstream.UpstreamStat{{Upstream: "f", Healthy: true}}
	fbDown := []upstream.UpstreamStat{{Upstream: "f", Healthy: false}}
	const (
		downMsg   = "fallback DNS in use: the upstream DNS servers are not answering"
		recentMsg = "fallback DNS in use: the upstream DNS servers left several queries of the last 5 minutes unanswered"
	)
	for _, tc := range []struct {
		name      string
		guard     bool
		stats, fb []upstream.UpstreamStat
		recent    int // fallback answers within the last 5 minutes
		status    string
		msg       string
	}{
		{"all fine", false, up, fbUp, 0, "ok", ""},
		{"clock guard", true, down, nil, 0, "warn", "system clock is not set; using unencrypted DNS to the bootstrap servers"},
		{"down without fallbacks", false, down, []upstream.UpstreamStat{}, 0, "fail", "no upstream DNS server is answering"},
		{"down, fallbacks down", false, down, fbDown, 5, "fail", "no upstream DNS server is answering"},
		{"down, fallback healthy", false, down, fbUp, 0, "warn", downMsg}, // at once, before any fallback answer
		{"down, fallback answering", false, down, fbUp, 1, "warn", downMsg},
		// A fallback answer or two (a handshake the provider cut) while the
		// default upstreams are healthy is no reason to warn.
		{"one fallback answer", false, up, fbUp, 1, "ok", ""},
		{"two fallback answers", false, up, fbUp, 2, "ok", ""},
		{"three fallback answers", false, up, fbUp, 3, "warn", recentMsg},
		{"many fallback answers", false, up, fbUp, upstream.FallbackTimesKept, "warn", recentMsg},
	} {
		st, msg, hint := upstreamHealth(tc.guard, tc.stats, tc.fb, tc.recent, nil)
		if st != tc.status || msg != tc.msg || (st != "ok" && hint == "") {
			t.Errorf("%s: %s %q %q", tc.name, st, msg, hint)
		}
	}
	// Group sets, after the existing checks: one that could not be built
	// or has no healthy upstream warns (no fallbacks: SERVFAIL).
	const groupMsg = "the upstreams of group Kids, Teens are not answering: their clients get SERVFAIL"
	for _, tc := range []struct {
		name   string
		groups []upstream.GroupUpstreamStat
		stats  []upstream.UpstreamStat
		status string
		msg    string
	}{
		{"none", []upstream.GroupUpstreamStat{}, up, "ok", ""},
		{"healthy", []upstream.GroupUpstreamStat{{GroupIDs: []int64{2}, Upstreams: up, Names: []string{"Kids"}}}, up, "ok", ""},
		{"down", []upstream.GroupUpstreamStat{{GroupIDs: []int64{2, 3}, Upstreams: down, Names: []string{"Kids", "Teens"}}}, up, "warn", groupMsg},
		{"not built", []upstream.GroupUpstreamStat{{GroupIDs: []int64{2, 3}, Upstreams: []upstream.UpstreamStat{}, Error: "x",
			Names: []string{"Kids", "Teens"}}}, up, "warn", groupMsg},
		{"default down first", []upstream.GroupUpstreamStat{{Upstreams: down, Names: []string{"Kids"}}}, down, "fail", "no upstream DNS server is answering"},
	} {
		st, msg, hint := upstreamHealth(false, tc.stats, nil, 0, tc.groups)
		if st != tc.status || msg != tc.msg || (st != "ok" && hint == "") {
			t.Errorf("%s: %s %q %q", tc.name, st, msg, hint)
		}
	}
}

func TestBlocklistsHealth(t *testing.T) {
	for _, tc := range []struct {
		enabled bool
		stats   filter.Stats
		status  string
		msg     string
	}{
		{true, filter.Stats{Entries: 100}, "ok", ""},
		{true, filter.Stats{FailedLists: 1}, "fail", "no blocklist could be loaded; nothing is blocked"},
		{false, filter.Stats{FailedLists: 1}, "warn", "1 list(s) failed to update"},
		{true, filter.Stats{Entries: 100, StaleLists: 2}, "warn", "2 list(s) not updated for a long time"},
		{true, filter.Stats{Entries: filter.MaxEntryBudget}, "ok", ""},
		{true, filter.Stats{Entries: filter.MaxEntryBudget + 1}, "warn",
			fmt.Sprintf("the blocklists hold %d entries; a small host may run short of memory", filter.MaxEntryBudget+1)},
		{true, filter.Stats{Entries: 100, TLDGuardLists: 1}, "warn",
			"1 own list(s) contain entries that would block a whole top-level domain; they are ignored"},
		{true, filter.Stats{Entries: 100, IPGuardLists: 2}, "warn",
			"2 list(s) of answer addresses contain blocks of broad or private networks; they are ignored"},
	} {
		st, msg, _ := blocklistsHealth(tc.enabled, tc.stats, filter.MaxEntryBudget)
		if st != tc.status || msg != tc.msg {
			t.Errorf("%+v: %s %q, want %s %q", tc.stats, st, msg, tc.status, tc.msg)
		}
	}
	// A smaller host warns earlier and names its budget.
	st, msg, hint := blocklistsHealth(true, filter.Stats{Entries: 2_000_001}, filter.BudgetFor(512<<20))
	if st != "warn" || msg != "the blocklists hold 2000001 entries; a small host may run short of memory" ||
		!strings.Contains(hint, "at most about 2000000 entries in total on this host") {
		t.Errorf("512 MiB host: %s %q %q", st, msg, hint)
	}
	if st, _, _ := blocklistsHealth(true, filter.Stats{Entries: 2_000_000}, 2_000_000); st != "ok" {
		t.Errorf("at the budget: %s", st)
	}
}
