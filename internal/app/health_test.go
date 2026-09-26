package app

import (
	"testing"
	"time"

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
