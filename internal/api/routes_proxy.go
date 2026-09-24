package api

import (
	"net/http"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/netutil"
)

// registerProxyRoutes registers the proxy endpoints (docs/API.md).
func (s *Server) registerProxyRoutes() {
	s.route("GET /api/v1/cache/live", permRead, s.handleProxyLive)
	s.route("GET /api/v1/cache/active", permRead, s.handleProxyActive)
	s.route("GET /api/v1/cache/proxy/stats", permRead, s.handleProxyStats)
	s.route("GET /api/v1/cache/noslice", permRead, s.handleNoSliceList)
	s.route("DELETE /api/v1/cache/noslice/{host}", permAdmin, s.handleNoSliceReset)
}

// errProxyMissing is returned when the proxy is not wired (never in the
// running app).
var errProxyMissing = apperr.Unavailable("the cache proxy is not available")

// handleProxyLive returns live downloads aggregated per client and content.
func (s *Server) handleProxyLive(w http.ResponseWriter, r *http.Request) error {
	if s.d.Proxy == nil {
		return errProxyMissing
	}
	return ok(w, s.d.Proxy.ActiveDownloads())
}

// handleProxyActive returns the individual live requests.
func (s *Server) handleProxyActive(w http.ResponseWriter, r *http.Request) error {
	if s.d.Proxy == nil {
		return errProxyMissing
	}
	return ok(w, s.d.Proxy.Active())
}

// handleProxyStats returns the proxy's live counters.
func (s *Server) handleProxyStats(w http.ResponseWriter, r *http.Request) error {
	if s.d.Proxy == nil {
		return errProxyMissing
	}
	return ok(w, s.d.Proxy.Stats())
}

// handleNoSliceList lists hosts detected without range support.
func (s *Server) handleNoSliceList(w http.ResponseWriter, r *http.Request) error {
	if s.d.Proxy == nil {
		return errProxyMissing
	}
	hosts, err := s.d.Proxy.NoSliceHosts(r.Context())
	if err != nil {
		return err
	}
	return ok(w, hosts)
}

// handleNoSliceReset clears the no-slice state of a host.
func (s *Server) handleNoSliceReset(w http.ResponseWriter, r *http.Request) error {
	if s.d.Proxy == nil {
		return errProxyMissing
	}
	host, isIP, valid := netutil.NormalizeHost(r.PathValue("host"))
	if !valid || isIP {
		return apperr.Invalid("host", "invalid host name")
	}
	if err := s.d.Proxy.ResetNoSlice(r.Context(), host); err != nil {
		return err
	}
	s.audit(r, "cache.noslice.reset", host, nil)
	return noContent(w)
}
