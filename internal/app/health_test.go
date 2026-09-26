package app

import (
	"fmt"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/dns/filter"
	"github.com/hustenreizjuengling/picache/internal/dns/upstream"
)

// The health check "upstreams" with fallbacks (first match): clock guard
// warns; no healthy default upstream and no healthy fallback fails; no
// healthy default upstream, or a fallback that answered within 5 minutes,
// warns.
func TestUpstreamHealth(t *testing.T) {
	now := time.Now()
	up := []upstream.UpstreamStat{{Upstream: "a", Healthy: true}}
	down := []upstream.UpstreamStat{{Upstream: "a", Healthy: false}}
	fbUp := []upstream.UpstreamStat{{Upstream: "f", Healthy: true}}
	fbDown := []upstream.UpstreamStat{{Upstream: "f", Healthy: false}}
	const fallbackMsg = "fallback DNS in use: the upstream DNS servers are not answering"
	for _, tc := range []struct {
		name      string
		guard     bool
		stats, fb []upstream.UpstreamStat
		last      time.Time
		status    string
		msg       string
	}{
		{"all fine", false, up, fbUp, time.Time{}, "ok", ""},
		{"clock guard", true, down, nil, time.Time{}, "warn", "system clock is not set; using unencrypted DNS to the bootstrap servers"},
		{"down without fallbacks", false, down, []upstream.UpstreamStat{}, time.Time{}, "fail", "no upstream DNS server is answering"},
		{"down, fallbacks down", false, down, fbDown, now, "fail", "no upstream DNS server is answering"},
		{"down, fallback healthy", false, down, fbUp, time.Time{}, "warn", fallbackMsg},
		{"fallback used recently", false, up, fbUp, now.Add(-4 * time.Minute), "warn", fallbackMsg},
		{"fallback used long ago", false, up, fbUp, now.Add(-6 * time.Minute), "ok", ""},
	} {
		st, msg, hint := upstreamHealth(tc.guard, tc.stats, tc.fb, tc.last, now)
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
		{true, filter.Stats{Entries: filter.EntryBudget}, "ok", ""},
		{true, filter.Stats{Entries: filter.EntryBudget + 1}, "warn",
			fmt.Sprintf("the blocklists hold %d entries; a small host may run short of memory", filter.EntryBudget+1)},
		{true, filter.Stats{Entries: 100, TLDGuardLists: 1}, "warn",
			"1 own list(s) contain entries that would block a whole top-level domain; they are ignored"},
	} {
		st, msg, _ := blocklistsHealth(tc.enabled, tc.stats)
		if st != tc.status || msg != tc.msg {
			t.Errorf("%+v: %s %q, want %s %q", tc.stats, st, msg, tc.status, tc.msg)
		}
	}
}
