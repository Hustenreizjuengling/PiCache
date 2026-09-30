package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/dns/upstream"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// maxUpstreamLen bounds the upstream string accepted by the test endpoint.
const maxUpstreamLen = 512

// upstreamsView is the response of GET /dns/upstreams: the statistics of the
// default upstreams in use and of the fallbacks (never null), when a
// fallback last answered (omitted if never since the start), the group
// sets (upstreams per client group; never null) and, in the DNSSEC mode
// validate, the state of the validation.
type upstreamsView struct {
	Upstreams        []upstream.UpstreamStat      `json:"upstreams"`
	Fallbacks        []upstream.UpstreamStat      `json:"fallbacks"`
	FallbackLastUsed time.Time                    `json:"fallbackLastUsed,omitzero"`
	Cache            upstream.CacheStat           `json:"cache"`
	ClockGuard       bool                         `json:"clockGuard"`
	Groups           []upstream.GroupUpstreamStat `json:"groups"`
	DNSSEC           *dnssecView                  `json:"dnssec,omitempty"`
}

// dnssecView is the DNSSEC state of GET /dns/upstreams (validate mode):
// whether RRSIG validity periods are checked and why not, whether a new
// root key is published, and the validating forwarders with the probe
// states of their targets (never null).
type dnssecView struct {
	TimeChecks string                `json:"timeChecks"` // active | suspended
	TimeReason string                `json:"timeReason,omitempty"`
	NewRootKey bool                  `json:"newRootKey"`
	Forwarders []forwarderDNSSECView `json:"forwarders"`
}

// forwarderDNSSECView is a forwarder that validates DNSSEC.
type forwarderDNSSECView struct {
	ID        int64                   `json:"id"`
	Domains   []string                `json:"domains"`
	Upstreams []upstream.UpstreamStat `json:"upstreams"`
}

// upstreamTestInput is the body of POST /dns/upstreams/test.
type upstreamTestInput struct {
	Upstream string `json:"upstream"`
}

// registerUpstreamRoutes registers the upstream endpoints (docs/API.md).
func (s *Server) registerUpstreamRoutes() {
	s.route("GET /api/v1/dns/upstreams", permRead, s.handleUpstreamList)
	s.route("POST /api/v1/dns/upstreams/test", permAdmin, s.handleUpstreamTest, routeExempt)
	s.route("POST /api/v1/dns/cache/flush", permAdmin, s.handleDNSCacheFlush, routeExempt)
	s.route("POST /api/v1/dns/dnssec/test", permAdmin, s.handleDNSSECTest, routeExempt)
}

func (s *Server) handleUpstreamList(w http.ResponseWriter, r *http.Request) error {
	up := s.d.Upstream
	v := upstreamsView{Upstreams: up.Stats(), Fallbacks: up.FallbackStats(), FallbackLastUsed: up.LastFallback(),
		Cache: up.CacheStats(), ClockGuard: up.ClockGuard(), Groups: up.GroupStats()}
	if s.d.Settings.Get().DNS.Validating() {
		d, err := s.dnssecView(r.Context())
		if err != nil {
			return err
		}
		v.DNSSEC = d
	}
	return ok(w, v)
}

// dnssecView assembles the DNSSEC state of GET /dns/upstreams from the
// upstream package and the forwarders of the DNS server.
func (s *Server) dnssecView(ctx context.Context) (*dnssecView, error) {
	up := s.d.Upstream
	active, reason := up.TimeChecks()
	v := &dnssecView{TimeChecks: "active", NewRootKey: up.NewRootKey(), Forwarders: []forwarderDNSSECView{}}
	if !active {
		v.TimeChecks, v.TimeReason = "suspended", reason
	}
	if s.d.DNS == nil {
		return v, nil
	}
	fwds, err := s.d.DNS.Forwarders(ctx)
	if err != nil {
		return nil, err
	}
	for _, f := range fwds {
		if f.Enabled && f.Validate {
			v.Forwarders = append(v.Forwarders, forwarderDNSSECView{ID: f.ID, Domains: f.Domains, Upstreams: up.ForwarderStats(f.Upstreams)})
		}
	}
	return v, nil
}

// handleDNSSECTest runs the DNSSEC test (docs/API.md): one at a time
// (409), at most one start per 10 s (429); audited with its verdicts.
func (s *Server) handleDNSSECTest(w http.ResponseWriter, r *http.Request) error {
	if s.d.Upstream == nil {
		return apperr.Unavailable("the resolver is not running")
	}
	res, err := s.d.Upstream.TestDNSSEC(r.Context())
	switch {
	case errors.Is(err, upstream.ErrDNSSECTestRunning):
		return apperr.Conflict("a DNSSEC test is running")
	case errors.Is(err, upstream.ErrDNSSECTestTooSoon):
		return apperr.TooMany("wait 10 seconds between DNSSEC tests")
	case err != nil:
		return err
	}
	counts := map[string]int{"passed": 0, "failed": 0, "inconclusive": 0}
	for _, c := range res.Checks {
		switch c.Verdict {
		case upstream.TestPass:
			counts["passed"]++
		case upstream.TestFail:
			counts["failed"]++
		default:
			counts["inconclusive"]++
		}
	}
	s.audit(r, "dns.dnssec.test", "", map[string]any{"passed": counts["passed"], "failed": counts["failed"],
		"inconclusive": counts["inconclusive"]})
	return ok(w, res)
}

// handleUpstreamTest resolves a fixed name through one upstream string. It
// changes no state and is therefore not audited.
func (s *Server) handleUpstreamTest(w http.ResponseWriter, r *http.Request) error {
	var in upstreamTestInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	if len(in.Upstream) > maxUpstreamLen {
		return apperr.Invalid("upstream", "must be at most %d characters", maxUpstreamLen)
	}
	if _, err := settings.ParseUpstream(in.Upstream); err != nil {
		return apperr.Invalid("upstream", "%v", err)
	}
	return ok(w, s.d.Upstream.Test(r.Context(), in.Upstream))
}

func (s *Server) handleDNSCacheFlush(w http.ResponseWriter, r *http.Request) error {
	s.d.Upstream.FlushCache()
	s.audit(r, "dns.cache.flush", "", nil)
	return noContent(w)
}
