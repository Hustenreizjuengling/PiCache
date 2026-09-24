package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/clients"
	dnsserver "github.com/hustenreizjuengling/picache/internal/dns/server"
	"github.com/hustenreizjuengling/picache/internal/netutil"
)

// registerDNSRoutes registers the dns endpoints (docs/API.md).
func (s *Server) registerDNSRoutes() {
	s.route("GET /api/v1/dns/blocking", permRead, s.dnsBlockingGet)
	s.route("POST /api/v1/dns/blocking", permAdmin, s.dnsBlockingSet)
	s.route("POST /api/v1/dns/lookup", permRead, s.dnsLookup)
	s.route("GET /api/v1/dns/stats", permRead, s.dnsStats)
	s.route("GET /api/v1/dns/cache-ips", permRead, s.dnsCacheIPs)
	s.route("GET /api/v1/dns/router", permRead, s.dnsRouter)

	s.route("GET /api/v1/dns/records", permRead, s.dnsRecordsList)
	s.route("POST /api/v1/dns/records", permAdmin, s.dnsRecordCreate)
	s.route("PUT /api/v1/dns/records/{id}", permAdmin, s.dnsRecordUpdate)
	s.route("DELETE /api/v1/dns/records/{id}", permAdmin, s.dnsRecordDelete)

	s.route("GET /api/v1/dns/forwarders", permRead, s.dnsForwardersList)
	s.route("POST /api/v1/dns/forwarders", permAdmin, s.dnsForwarderCreate)
	s.route("PUT /api/v1/dns/forwarders/{id}", permAdmin, s.dnsForwarderUpdate)
	s.route("DELETE /api/v1/dns/forwarders/{id}", permAdmin, s.dnsForwarderDelete)

	s.route("GET /api/v1/clients", permRead, s.clientsList)
	s.route("POST /api/v1/clients", permAdmin, s.clientCreate)
	s.route("GET /api/v1/clients/known", permRead, s.clientsKnown)
	s.route("PUT /api/v1/clients/{id}", permAdmin, s.clientUpdate)
	s.route("DELETE /api/v1/clients/{id}", permAdmin, s.clientDelete)

	s.route("GET /api/v1/groups", permRead, s.groupsList)
	s.route("POST /api/v1/groups", permAdmin, s.groupCreate)
	s.route("PUT /api/v1/groups/{id}", permAdmin, s.groupUpdate)
	s.route("DELETE /api/v1/groups/{id}", permAdmin, s.groupDelete)
}

// --- blocking, lookup, status ---

func (s *Server) dnsBlockingGet(w http.ResponseWriter, r *http.Request) error {
	return ok(w, s.d.DNS.Blocking())
}

func (s *Server) dnsBlockingSet(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Enabled      *bool `json:"enabled"`
		PauseSeconds int   `json:"pauseSeconds"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	if in.Enabled == nil {
		return apperr.Invalid("enabled", "required")
	}
	if in.PauseSeconds < 0 {
		return apperr.Invalid("pauseSeconds", "must not be negative")
	}
	st, err := s.d.DNS.SetBlocking(r.Context(), *in.Enabled, time.Duration(in.PauseSeconds)*time.Second)
	if err != nil {
		return err
	}
	s.audit(r, "dns.blocking.set", "", map[string]any{"enabled": *in.Enabled, "pauseSeconds": in.PauseSeconds})
	return ok(w, st)
}

func (s *Server) dnsLookup(w http.ResponseWriter, r *http.Request) error {
	var in dnsserver.LookupRequest
	if err := decode(w, r, &in); err != nil {
		return err
	}
	res, err := s.d.DNS.Lookup(r.Context(), in, netutil.AddrFromRemote(r.RemoteAddr))
	if err != nil {
		return err
	}
	return ok(w, res)
}

func (s *Server) dnsStats(w http.ResponseWriter, r *http.Request) error {
	return ok(w, s.d.DNS.Stats())
}

func (s *Server) dnsCacheIPs(w http.ResponseWriter, r *http.Request) error {
	return ok(w, s.d.DNS.CacheIPs())
}

func (s *Server) dnsRouter(w http.ResponseWriter, r *http.Request) error {
	return ok(w, s.d.DNS.Router())
}

// --- local records ---

func (s *Server) dnsRecordsList(w http.ResponseWriter, r *http.Request) error {
	recs, err := s.d.DNS.Records(r.Context())
	if err != nil {
		return err
	}
	return ok(w, recs)
}

func (s *Server) dnsRecordCreate(w http.ResponseWriter, r *http.Request) error {
	var in dnsserver.RecordInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	rec, err := s.d.DNS.CreateRecord(r.Context(), in)
	if err != nil {
		return err
	}
	s.audit(r, "dns.record.create", strconv.FormatInt(rec.ID, 10), rec)
	return created(w, rec)
}

func (s *Server) dnsRecordUpdate(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in dnsserver.RecordInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	rec, err := s.d.DNS.UpdateRecord(r.Context(), id, in)
	if err != nil {
		return err
	}
	s.audit(r, "dns.record.update", strconv.FormatInt(id, 10), rec)
	return ok(w, rec)
}

func (s *Server) dnsRecordDelete(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	if err := s.d.DNS.DeleteRecord(r.Context(), id); err != nil {
		return err
	}
	s.audit(r, "dns.record.delete", strconv.FormatInt(id, 10), nil)
	return noContent(w)
}

// --- conditional forwarders ---

func (s *Server) dnsForwardersList(w http.ResponseWriter, r *http.Request) error {
	fwds, err := s.d.DNS.Forwarders(r.Context())
	if err != nil {
		return err
	}
	return ok(w, fwds)
}

func (s *Server) dnsForwarderCreate(w http.ResponseWriter, r *http.Request) error {
	var in dnsserver.ForwarderInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	f, err := s.d.DNS.CreateForwarder(r.Context(), in)
	if err != nil {
		return err
	}
	s.audit(r, "dns.forwarder.create", strconv.FormatInt(f.ID, 10), f)
	return created(w, f)
}

func (s *Server) dnsForwarderUpdate(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in dnsserver.ForwarderInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	f, err := s.d.DNS.UpdateForwarder(r.Context(), id, in)
	if err != nil {
		return err
	}
	s.audit(r, "dns.forwarder.update", strconv.FormatInt(id, 10), f)
	return ok(w, f)
}

func (s *Server) dnsForwarderDelete(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	if err := s.d.DNS.DeleteForwarder(r.Context(), id); err != nil {
		return err
	}
	s.audit(r, "dns.forwarder.delete", strconv.FormatInt(id, 10), nil)
	return noContent(w)
}

// --- clients ---

func (s *Server) clientsList(w http.ResponseWriter, r *http.Request) error {
	cl, err := s.d.Clients.Clients(r.Context())
	if err != nil {
		return err
	}
	return ok(w, cl)
}

func (s *Server) clientCreate(w http.ResponseWriter, r *http.Request) error {
	var in clients.ClientInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	c, err := s.d.Clients.CreateClient(r.Context(), in)
	if err != nil {
		return err
	}
	s.audit(r, "client.create", strconv.FormatInt(c.ID, 10), c)
	return created(w, c)
}

func (s *Server) clientUpdate(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in clients.ClientInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	c, err := s.d.Clients.UpdateClient(r.Context(), id, in)
	if err != nil {
		return err
	}
	s.audit(r, "client.update", strconv.FormatInt(id, 10), c)
	return ok(w, c)
}

func (s *Server) clientDelete(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	if err := s.d.Clients.DeleteClient(r.Context(), id); err != nil {
		return err
	}
	s.audit(r, "client.delete", strconv.FormatInt(id, 10), nil)
	return noContent(w)
}

// clientsKnown lists recently seen addresses; ?within=30d|24h|… (default and
// maximum 30 days).
func (s *Server) clientsKnown(w http.ResponseWriter, r *http.Request) error {
	var within time.Duration
	if v := qString(r, "within"); v != "" {
		d, err := parseRange(v)
		if err != nil {
			return apperr.Invalid("within", "invalid duration %q (e.g. 24h or 30d)", v)
		}
		within = d
	}
	known, err := s.d.Clients.Known(r.Context(), within)
	if err != nil {
		return err
	}
	return ok(w, known)
}

// --- groups ---

func (s *Server) groupsList(w http.ResponseWriter, r *http.Request) error {
	gs, err := s.d.Clients.Groups(r.Context())
	if err != nil {
		return err
	}
	return ok(w, gs)
}

func (s *Server) groupCreate(w http.ResponseWriter, r *http.Request) error {
	var in clients.GroupInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	g, err := s.d.Clients.CreateGroup(r.Context(), in)
	if err != nil {
		return err
	}
	s.audit(r, "group.create", strconv.FormatInt(g.ID, 10), g)
	return created(w, g)
}

func (s *Server) groupUpdate(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in clients.GroupInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	g, err := s.d.Clients.UpdateGroup(r.Context(), id, in)
	if err != nil {
		return err
	}
	s.audit(r, "group.update", strconv.FormatInt(id, 10), g)
	return ok(w, g)
}

func (s *Server) groupDelete(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	if err := s.d.Clients.DeleteGroup(r.Context(), id); err != nil {
		return err
	}
	s.audit(r, "group.delete", strconv.FormatInt(id, 10), nil)
	return noContent(w)
}
