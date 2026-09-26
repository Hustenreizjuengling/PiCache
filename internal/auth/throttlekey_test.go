package auth

import (
	"context"
	"testing"

	"github.com/hustenreizjuengling/picache/internal/settings"
)

// The DNS rate-limit prefixes (dns.rateLimitIpv4Prefix/Ipv6Prefix) apply
// to the DNS rate limiter only: the login throttle keeps one key per IPv4
// address and per public IPv6 /64.
func TestThrottleKeyIgnoresDNSRateLimitPrefixes(t *testing.T) {
	e := newEnv(t)
	if _, err := e.set.Update(context.Background(), func(a *settings.All) error {
		a.DNS.RateLimitIPv4Prefix, a.DNS.RateLimitIPv6Prefix = 8, 32
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for in, want := range map[string]string{
		"8.8.8.8:1234":              "c:8.8.8.8/32",
		"[2a00:1450:4001:81c::5]:1": "c:2a00:1450:4001:81c::/64",
	} {
		if got := clientThrottleKey(in); got != want {
			t.Errorf("clientThrottleKey(%s) = %s, want %s", in, got, want)
		}
	}
}
