package api

import (
	"net/http"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/dns/upstream"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// maxUpstreamLen bounds the upstream string accepted by the test endpoint.
const maxUpstreamLen = 512

// upstreamsView is the response of GET /dns/upstreams.
type upstreamsView struct {
	Upstreams  []upstream.UpstreamStat `json:"upstreams"`
	Cache      upstream.CacheStat      `json:"cache"`
	ClockGuard bool                    `json:"clockGuard"`
}

// upstreamTestInput is the body of POST /dns/upstreams/test.
type upstreamTestInput struct {
	Upstream string `json:"upstream"`
}

// registerUpstreamRoutes registers the upstream endpoints (docs/API.md).
func (s *Server) registerUpstreamRoutes() {
	s.route("GET /api/v1/dns/upstreams", permRead, s.handleUpstreamList)
	s.route("POST /api/v1/dns/upstreams/test", permAdmin, s.handleUpstreamTest)
	s.route("POST /api/v1/dns/cache/flush", permAdmin, s.handleDNSCacheFlush)
}

func (s *Server) handleUpstreamList(w http.ResponseWriter, r *http.Request) error {
	up := s.d.Upstream
	return ok(w, upstreamsView{Upstreams: up.Stats(), Cache: up.CacheStats(), ClockGuard: up.ClockGuard()})
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
