package api

import (
	"context"
	"encoding/json/v2"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hustenreizjuengling/picache/internal/auth"
	"github.com/hustenreizjuengling/picache/internal/clients"
	"github.com/hustenreizjuengling/picache/internal/db"
	dnsserver "github.com/hustenreizjuengling/picache/internal/dns/server"
	"github.com/hustenreizjuengling/picache/internal/secrets"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// dnsTestEnv is an API server with a real DNS server, clients registry and
// auth service (for the audit log) on a temp database. Handlers are called
// directly with an admin principal; authentication itself is covered by the
// auth route tests.
type dnsTestEnv struct {
	srv  *Server
	auth *auth.Service
}

func newDNSTestEnv(t *testing.T) *dnsTestEnv {
	t.Helper()
	ctx := context.Background()
	d, err := db.Open(filepath.Join(t.TempDir(), "picache.db"), 2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	set, err := settings.Open(ctx, d, log)
	if err != nil {
		t.Fatal(err)
	}
	box, err := secrets.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	a, err := auth.New(ctx, d, set, box, filepath.Join(t.TempDir(), "setup-token"), log)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := clients.New(ctx, d, nil, log)
	if err != nil {
		t.Fatal(err)
	}
	dns, err := dnsserver.New(ctx, dnsserver.Deps{DB: d, Settings: set, Clients: reg, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{d: Deps{Settings: set, Auth: a, DNS: dns, Clients: reg, Log: log}, log: log, mux: http.NewServeMux()}
	s.registerDNSRoutes() // patterns must register without conflicts
	return &dnsTestEnv{srv: s, auth: a}
}

// call invokes h like the router does (errors written by writeError) as an
// admin browser session from 192.168.1.44.
func (e *dnsTestEnv) call(t *testing.T, h handlerFunc, method, target, body string, pathVals ...string) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, rd)
	req.RemoteAddr = "192.168.1.44:50000"
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(pathVals); i += 2 {
		req.SetPathValue(pathVals[i], pathVals[i+1])
	}
	p := &auth.Principal{UserID: 1, Username: "admin", SessionID: "s1", Scope: auth.ScopeAdmin}
	req = req.WithContext(context.WithValue(req.Context(), principalKey, p))
	rec := httptest.NewRecorder()
	if err := h(rec, req); err != nil {
		writeError(rec, req, e.srv.log, err)
	}
	return rec
}

func dnsDecode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return v
}

func dnsExpect(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status %d, want %d: %s", rec.Code, want, rec.Body.String())
	}
}

// auditActions returns the recorded audit actions (nil if the audit log
// cannot be read).
func (e *dnsTestEnv) auditActions(t *testing.T) []string {
	t.Helper()
	entries, _, err := e.auth.AuditLog(context.Background(), auth.AuditQuery{Limit: 500})
	if err != nil {
		t.Logf("audit log unavailable: %v", err)
		return nil
	}
	var out []string
	for _, a := range entries {
		out = append(out, a.Action)
	}
	return out
}

func TestDNSRoutesRecords(t *testing.T) {
	e := newDNSTestEnv(t)
	s := e.srv

	rec := e.call(t, s.dnsRecordCreate, "POST", "/api/v1/dns/records", `{"name":"NAS.lan.","type":"A","value":"192.168.1.5","enabled":true}`)
	dnsExpect(t, rec, http.StatusCreated)
	r := dnsDecode[dnsserver.Record](t, rec)
	if r.ID == 0 || r.Name != "nas.lan" || r.TTL != 300 {
		t.Fatalf("record %+v", r)
	}
	id := r.ID

	rec = e.call(t, s.dnsRecordCreate, "POST", "/api/v1/dns/records", `{"name":"nas.lan","type":"CNAME","value":"x.lan"}`)
	dnsExpect(t, rec, http.StatusConflict)
	rec = e.call(t, s.dnsRecordCreate, "POST", "/api/v1/dns/records", `{"name":"x.lan","type":"A","value":"not-an-ip"}`)
	dnsExpect(t, rec, http.StatusBadRequest)
	if body := dnsDecode[errorBody](t, rec); body.Error.Field != "value" || body.Error.Code != "invalid" {
		t.Errorf("error body %+v", body)
	}
	rec = e.call(t, s.dnsRecordCreate, "POST", "/api/v1/dns/records", `{"name":"x.lan","type":"A","value":"1.2.3.4","bogus":1}`)
	dnsExpect(t, rec, http.StatusBadRequest)

	rec = e.call(t, s.dnsRecordUpdate, "PUT", "/api/v1/dns/records/x", `{"name":"nas.lan","type":"A","value":"192.168.1.6","ttl":60,"enabled":true}`,
		"id", dnsJSONInt(id))
	dnsExpect(t, rec, http.StatusOK)
	if r := dnsDecode[dnsserver.Record](t, rec); r.Value != "192.168.1.6" || r.TTL != 60 {
		t.Errorf("updated %+v", r)
	}
	rec = e.call(t, s.dnsRecordUpdate, "PUT", "/api/v1/dns/records/x", `{"name":"nas.lan","type":"A","value":"192.168.1.6"}`, "id", "abc")
	dnsExpect(t, rec, http.StatusBadRequest)

	rec = e.call(t, s.dnsRecordsList, "GET", "/api/v1/dns/records", "")
	dnsExpect(t, rec, http.StatusOK)
	if list := dnsDecode[[]dnsserver.Record](t, rec); len(list) != 1 {
		t.Errorf("list %+v", list)
	}

	// The lookup evaluates the caller by default and explains the answer.
	rec = e.call(t, s.dnsLookup, "POST", "/api/v1/dns/lookup", `{"name":"nas.lan"}`)
	dnsExpect(t, rec, http.StatusOK)
	lr := dnsDecode[dnsserver.LookupResult](t, rec)
	if lr.Status != dnsserver.StatusLocal || len(lr.Answers) != 1 || len(lr.Steps) == 0 || !strings.Contains(lr.Steps[0], "192.168.1.44") {
		t.Errorf("lookup %+v", lr)
	}
	rec = e.call(t, s.dnsLookup, "POST", "/api/v1/dns/lookup", `{"name":"nas.lan","type":"BOGUS"}`)
	dnsExpect(t, rec, http.StatusBadRequest)

	rec = e.call(t, s.dnsRecordDelete, "DELETE", "/api/v1/dns/records/x", "", "id", dnsJSONInt(id))
	dnsExpect(t, rec, http.StatusNoContent)
	rec = e.call(t, s.dnsRecordDelete, "DELETE", "/api/v1/dns/records/x", "", "id", dnsJSONInt(id))
	dnsExpect(t, rec, http.StatusNotFound)

	if acts := e.auditActions(t); acts != nil {
		for _, want := range []string{"dns.record.create", "dns.record.update", "dns.record.delete"} {
			if !strings.Contains(strings.Join(acts, ","), want) {
				t.Errorf("audit %v lacks %s", acts, want)
			}
		}
	}
}

func dnsJSONInt(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestDNSRoutesForwarders(t *testing.T) {
	e := newDNSTestEnv(t)
	s := e.srv
	rec := e.call(t, s.dnsForwarderCreate, "POST", "/api/v1/dns/forwarders", `{"domain":"fritz.box","upstreams":["192.168.178.1"],"enabled":true}`)
	dnsExpect(t, rec, http.StatusCreated)
	f := dnsDecode[dnsserver.Forwarder](t, rec)
	rec = e.call(t, s.dnsForwarderUpdate, "PUT", "/api/v1/dns/forwarders/x", `{"domain":"fritz.box","upstreams":["tls://dns.example"],"enabled":false}`, "id", dnsJSONInt(f.ID))
	dnsExpect(t, rec, http.StatusOK)
	rec = e.call(t, s.dnsForwarderCreate, "POST", "/api/v1/dns/forwarders", `{"domain":"x.lan","upstreams":[]}`)
	dnsExpect(t, rec, http.StatusBadRequest)
	rec = e.call(t, s.dnsForwardersList, "GET", "/api/v1/dns/forwarders", "")
	if list := dnsDecode[[]dnsserver.Forwarder](t, rec); len(list) != 1 || list[0].Enabled {
		t.Errorf("list %+v", list)
	}
	rec = e.call(t, s.dnsForwarderDelete, "DELETE", "/api/v1/dns/forwarders/x", "", "id", dnsJSONInt(f.ID))
	dnsExpect(t, rec, http.StatusNoContent)
}

func TestDNSRoutesBlockingAndStatus(t *testing.T) {
	e := newDNSTestEnv(t)
	s := e.srv
	rec := e.call(t, s.dnsBlockingSet, "POST", "/api/v1/dns/blocking", `{"enabled":false,"pauseSeconds":300}`)
	dnsExpect(t, rec, http.StatusOK)
	st := dnsDecode[dnsserver.BlockingStatus](t, rec)
	if st.Enabled || st.Permanent || st.PausedUntil == nil {
		t.Errorf("paused status %+v", st)
	}
	rec = e.call(t, s.dnsBlockingGet, "GET", "/api/v1/dns/blocking", "")
	if st := dnsDecode[dnsserver.BlockingStatus](t, rec); st.PausedUntil == nil {
		t.Errorf("GET blocking %+v", st)
	}
	dnsExpect(t, e.call(t, s.dnsBlockingSet, "POST", "/api/v1/dns/blocking", `{"pauseSeconds":30}`), http.StatusBadRequest)
	dnsExpect(t, e.call(t, s.dnsBlockingSet, "POST", "/api/v1/dns/blocking", `{"enabled":false,"pauseSeconds":-1}`), http.StatusBadRequest)
	rec = e.call(t, s.dnsBlockingSet, "POST", "/api/v1/dns/blocking", `{"enabled":true}`)
	if st := dnsDecode[dnsserver.BlockingStatus](t, rec); !st.Enabled || st.PausedUntil != nil {
		t.Errorf("enabled status %+v", st)
	}

	rec = e.call(t, s.dnsStats, "GET", "/api/v1/dns/stats", "")
	dnsExpect(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), `"topRateLimited":[]`) {
		t.Errorf("stats %s", rec.Body.String())
	}
	rec = e.call(t, s.dnsCacheIPs, "GET", "/api/v1/dns/cache-ips", "")
	if st := dnsDecode[dnsserver.CacheIPStatus](t, rec); st.Ready || st.Reason != "LanCache is disabled" {
		t.Errorf("cache IPs %+v", st)
	}
	rec = e.call(t, s.dnsRouter, "GET", "/api/v1/dns/router", "")
	dnsExpect(t, rec, http.StatusOK)
	if acts := e.auditActions(t); acts != nil && !strings.Contains(strings.Join(acts, ","), "dns.blocking.set") {
		t.Errorf("audit %v", acts)
	}
}

func TestDNSRoutesClientsAndGroups(t *testing.T) {
	e := newDNSTestEnv(t)
	s := e.srv
	rec := e.call(t, s.groupCreate, "POST", "/api/v1/groups", `{"name":"Kids","enabled":true}`)
	dnsExpect(t, rec, http.StatusCreated)
	g := dnsDecode[clients.Group](t, rec)

	rec = e.call(t, s.clientCreate, "POST", "/api/v1/clients", `{"name":"Tablet","identifiers":["AA:BB:CC:DD:EE:FF","192.168.1.30"],"groupIds":[`+dnsJSONInt(g.ID)+`]}`)
	dnsExpect(t, rec, http.StatusCreated)
	c := dnsDecode[clients.Client](t, rec)
	if c.Identifiers[0] != "aa:bb:cc:dd:ee:ff" {
		t.Errorf("client %+v", c)
	}
	rec = e.call(t, s.clientCreate, "POST", "/api/v1/clients", `{"name":"Other","identifiers":["192.168.1.30"]}`)
	dnsExpect(t, rec, http.StatusConflict)
	rec = e.call(t, s.clientUpdate, "PUT", "/api/v1/clients/x", `{"name":"Tablet","identifiers":["192.168.1.31"],"lanCacheBypass":true}`, "id", dnsJSONInt(c.ID))
	dnsExpect(t, rec, http.StatusOK)
	rec = e.call(t, s.clientsList, "GET", "/api/v1/clients", "")
	if list := dnsDecode[[]clients.Client](t, rec); len(list) != 1 || !list[0].LanCacheBypass {
		t.Errorf("clients %+v", list)
	}

	s.d.Clients.Seen(netip.MustParseAddr("192.168.1.31"))
	rec = e.call(t, s.clientsKnown, "GET", "/api/v1/clients/known?within=24h", "")
	dnsExpect(t, rec, http.StatusOK)
	if known := dnsDecode[[]clients.Known](t, rec); len(known) != 1 || known[0].Name != "Tablet" {
		t.Errorf("known %+v", known)
	}
	dnsExpect(t, e.call(t, s.clientsKnown, "GET", "/api/v1/clients/known?within=soon", ""), http.StatusBadRequest)

	rec = e.call(t, s.groupsList, "GET", "/api/v1/groups", "")
	if gs := dnsDecode[[]clients.Group](t, rec); len(gs) != 2 {
		t.Errorf("groups %+v", gs)
	}
	dnsExpect(t, e.call(t, s.groupDelete, "DELETE", "/api/v1/groups/1", "", "id", "1"), http.StatusForbidden)
	dnsExpect(t, e.call(t, s.groupUpdate, "PUT", "/api/v1/groups/x", `{"name":"Children","enabled":false}`, "id", dnsJSONInt(g.ID)), http.StatusOK)
	dnsExpect(t, e.call(t, s.groupDelete, "DELETE", "/api/v1/groups/x", "", "id", dnsJSONInt(g.ID)), http.StatusNoContent)
	dnsExpect(t, e.call(t, s.clientDelete, "DELETE", "/api/v1/clients/x", "", "id", dnsJSONInt(c.ID)), http.StatusNoContent)

	if acts := e.auditActions(t); acts != nil {
		joined := strings.Join(acts, ",")
		for _, want := range []string{"group.create", "group.update", "group.delete", "client.create", "client.update", "client.delete"} {
			if !strings.Contains(joined, want) {
				t.Errorf("audit %v lacks %s", acts, want)
			}
		}
	}
}
