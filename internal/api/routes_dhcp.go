package api

import (
	"context"
	"net/http"
	"net/netip"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/dhcp"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// DHCP is implemented by *dhcp.Service: the optional DHCP server
// (docs/ARCHITECTURE.md 18).
type DHCP interface {
	Status() dhcp.Status
	Interfaces() []dhcp.Interface
	// Probe looks for other DHCP servers for 5 s: apperr.Unavailable when
	// DHCP is unavailable, apperr.TooMany within 10 s of the previous probe.
	Probe(ctx context.Context) (dhcp.ProbeResult, error)
	Leases() []dhcp.Lease
	DeleteLease(ctx context.Context, mac string) error
	Statics() []dhcp.StaticLease
	CreateStatic(ctx context.Context, in dhcp.StaticInput) (dhcp.StaticLease, error)
	UpdateStatic(ctx context.Context, mac string, in dhcp.StaticUpdate) (dhcp.StaticLease, error)
	DeleteStatic(ctx context.Context, mac string) error
	// CheckSettings checks enabled DHCP settings against the live
	// interface (fields dhcp.interface, dhcp.rangeStart, dhcp.rangeEnd,
	// dhcp.router).
	CheckSettings(a *settings.All) error
}

// registerDHCPRoutes registers the DHCP endpoints (docs/API.md).
func (s *Server) registerDHCPRoutes() {
	s.route("GET /api/v1/dhcp", permRead, s.dhcpStatus)
	s.route("GET /api/v1/dhcp/interfaces", permRead, s.dhcpInterfaces)
	s.route("POST /api/v1/dhcp/probe", permAdmin, s.dhcpProbe)
	s.route("GET /api/v1/dhcp/leases", permRead, s.dhcpLeases)
	s.route("DELETE /api/v1/dhcp/leases/{mac}", permAdmin, s.dhcpLeaseDelete)
	s.route("GET /api/v1/dhcp/static", permRead, s.dhcpStatics)
	s.route("POST /api/v1/dhcp/static", permAdmin, s.dhcpStaticCreate)
	s.route("PUT /api/v1/dhcp/static/{mac}", permAdmin, s.dhcpStaticUpdate)
	s.route("DELETE /api/v1/dhcp/static/{mac}", permAdmin, s.dhcpStaticDelete)
}

var errNoDHCP = apperr.Unavailable("the DHCP server is not available")

func (s *Server) dhcpStatus(w http.ResponseWriter, r *http.Request) error {
	if s.d.DHCP == nil {
		return errNoDHCP
	}
	return ok(w, s.d.DHCP.Status())
}

func (s *Server) dhcpInterfaces(w http.ResponseWriter, r *http.Request) error {
	if s.d.DHCP == nil {
		return errNoDHCP
	}
	return ok(w, s.d.DHCP.Interfaces())
}

func (s *Server) dhcpProbe(w http.ResponseWriter, r *http.Request) error {
	if s.d.DHCP == nil {
		return errNoDHCP
	}
	res, err := s.d.DHCP.Probe(r.Context())
	if err != nil {
		return err
	}
	s.audit(r, "dhcp.probe", "", map[string]int{"servers": len(res.Servers)})
	return ok(w, res)
}

// dhcpLeases lists the leases with the configured client of each address
// or MAC.
func (s *Server) dhcpLeases(w http.ResponseWriter, r *http.Request) error {
	if s.d.DHCP == nil {
		return errNoDHCP
	}
	leases := s.d.DHCP.Leases()
	if s.d.Clients != nil {
		for i := range leases {
			ip, err := netip.ParseAddr(leases[i].IP)
			if err != nil {
				continue
			}
			if id, name, _ := s.d.Clients.Describe(ip, leases[i].MAC); id != 0 {
				leases[i].ClientName = name
			}
		}
	}
	return ok(w, leases)
}

func (s *Server) dhcpLeaseDelete(w http.ResponseWriter, r *http.Request) error {
	if s.d.DHCP == nil {
		return errNoDHCP
	}
	mac := r.PathValue("mac")
	if err := s.d.DHCP.DeleteLease(r.Context(), mac); err != nil {
		return err
	}
	s.audit(r, "dhcp.lease.delete", normalMAC(mac), nil)
	return noContent(w)
}

func (s *Server) dhcpStatics(w http.ResponseWriter, r *http.Request) error {
	if s.d.DHCP == nil {
		return errNoDHCP
	}
	return ok(w, s.d.DHCP.Statics())
}

func (s *Server) dhcpStaticCreate(w http.ResponseWriter, r *http.Request) error {
	if s.d.DHCP == nil {
		return errNoDHCP
	}
	var in dhcp.StaticInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	st, err := s.d.DHCP.CreateStatic(r.Context(), in)
	if err != nil {
		return err
	}
	s.audit(r, "dhcp.static.create", st.MAC, map[string]string{"ip": st.IP, "hostname": st.Hostname})
	return created(w, st)
}

func (s *Server) dhcpStaticUpdate(w http.ResponseWriter, r *http.Request) error {
	if s.d.DHCP == nil {
		return errNoDHCP
	}
	var in dhcp.StaticUpdate
	if err := decode(w, r, &in); err != nil {
		return err
	}
	st, err := s.d.DHCP.UpdateStatic(r.Context(), r.PathValue("mac"), in)
	if err != nil {
		return err
	}
	s.audit(r, "dhcp.static.update", st.MAC, map[string]string{"ip": st.IP, "hostname": st.Hostname})
	return ok(w, st)
}

func (s *Server) dhcpStaticDelete(w http.ResponseWriter, r *http.Request) error {
	if s.d.DHCP == nil {
		return errNoDHCP
	}
	mac := r.PathValue("mac")
	if err := s.d.DHCP.DeleteStatic(r.Context(), mac); err != nil {
		return err
	}
	s.audit(r, "dhcp.static.delete", normalMAC(mac), nil)
	return noContent(w)
}

// normalMAC returns the normalised form of a MAC path value for the audit
// log (the raw value was validated by the DHCP service already).
func normalMAC(s string) string {
	if m, ok := dhcp.NormalizeMAC(s); ok {
		return m
	}
	return ""
}

// checkDHCP checks changed and enabled DHCP settings against the live
// interface; unchanged settings are not checked again.
func (s *Server) checkDHCP(old, next *settings.All) error {
	if s.d.DHCP == nil || !next.DHCP.Enabled || (old.DHCP == next.DHCP && old.DNS.LocalDomain == next.DNS.LocalDomain) {
		return nil
	}
	return s.d.DHCP.CheckSettings(next)
}
