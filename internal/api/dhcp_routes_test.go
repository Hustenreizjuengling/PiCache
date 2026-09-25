package api

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/auth"
	"github.com/hustenreizjuengling/picache/internal/dhcp"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// fakeDHCP is a DHCP server with fixed answers that records the calls.
type fakeDHCP struct {
	mu       sync.Mutex
	probeErr error
	checkErr error
	checked  int
	statics  map[string]dhcp.StaticLease
}

var dhcpAt = time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)

func (f *fakeDHCP) Status() dhcp.Status {
	return dhcp.Status{
		Available: true, State: dhcp.StateBlocked, Blockers: []string{dhcp.BlockerOtherServer},
		Interface: &dhcp.StatusInterface{Name: "eth0", MAC: "02:aa:00:00:00:10", IPv4: "192.168.1.10", PrefixLen: 24},
		Pool:      &dhcp.StatusPool{Start: "192.168.1.100", End: "192.168.1.199", Size: 100, Used: 1, Static: 0},
		Router:    "192.168.1.1", DNSServer: "192.168.1.10", Domain: "lan",
		OtherServers: []dhcp.OtherServer{{Address: "192.168.1.1", ServerID: "192.168.1.1", Source: dhcp.SourceProbe, LastSeen: dhcpAt}},
		LastProbe:    &dhcp.LastProbe{Time: dhcpAt, Servers: 1},
		IPv6: dhcp.StatusIPv6{
			RouterAdvertisements: dhcp.RAStatus{Enabled: true, Available: true, State: dhcp.StateSending, Blockers: []string{},
				Address: "fd00::10", LastSent: dhcpAt, Sent: 3},
			DHCPv6: dhcp.DHCPv6Status{State: dhcp.StateOff, Blockers: []string{}},
		},
	}
}

func (f *fakeDHCP) Interfaces() []dhcp.Interface {
	return []dhcp.Interface{{Name: "eth0", MAC: "02:aa:00:00:00:10", IPv4: []string{"192.168.1.10/24"},
		IPv6: []dhcp.InterfaceIPv6{{Address: "fd00::10", Kind: "ula"}}}}
}

func (f *fakeDHCP) Probe(context.Context) (dhcp.ProbeResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.probeErr != nil {
		return dhcp.ProbeResult{}, f.probeErr
	}
	return dhcp.ProbeResult{Servers: []dhcp.ProbeServer{{Address: "192.168.1.1", ServerID: "192.168.1.1", Offer: "192.168.1.50"}}, DurationMs: 3000}, nil
}

func (f *fakeDHCP) Leases() []dhcp.Lease {
	return []dhcp.Lease{{MAC: "02:00:00:00:00:01", IP: "192.168.1.100", Hostname: "laptop", Expires: dhcpAt, Active: true, DNSName: "laptop.lan"}}
}

func (f *fakeDHCP) DeleteLease(_ context.Context, mac string) error {
	if m, _ := dhcp.NormalizeMAC(mac); m != "02:00:00:00:00:01" {
		return apperr.NotFound("lease", mac)
	}
	return nil
}

func (f *fakeDHCP) Statics() []dhcp.StaticLease {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []dhcp.StaticLease{}
	for _, s := range f.statics {
		out = append(out, s)
	}
	return out
}

func (f *fakeDHCP) CreateStatic(_ context.Context, in dhcp.StaticInput) (dhcp.StaticLease, error) {
	mac, ok := dhcp.NormalizeMAC(in.MAC)
	if !ok {
		return dhcp.StaticLease{}, apperr.Invalid("mac", "bad")
	}
	st := dhcp.StaticLease{MAC: mac, IP: in.IP, Hostname: in.Hostname, Comment: in.Comment, CreatedAt: dhcpAt, UpdatedAt: dhcpAt}
	f.mu.Lock()
	f.statics[mac] = st
	f.mu.Unlock()
	return st, nil
}

func (f *fakeDHCP) UpdateStatic(_ context.Context, mac string, in dhcp.StaticUpdate) (dhcp.StaticLease, error) {
	m, _ := dhcp.NormalizeMAC(mac)
	f.mu.Lock()
	defer f.mu.Unlock()
	st, ok := f.statics[m]
	if !ok {
		return st, apperr.NotFound("static lease", mac)
	}
	st.IP, st.Hostname = in.IP, in.Hostname
	f.statics[m] = st
	return st, nil
}

func (f *fakeDHCP) DeleteStatic(_ context.Context, mac string) error {
	m, _ := dhcp.NormalizeMAC(mac)
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.statics[m]; !ok {
		return apperr.NotFound("static lease", mac)
	}
	delete(f.statics, m)
	return nil
}

func (f *fakeDHCP) CheckSettings(*settings.All) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checked++
	return f.checkErr
}

func TestDHCPRoutes(t *testing.T) {
	ce := newCoreEnv(t)
	fd := &fakeDHCP{statics: map[string]dhcp.StaticLease{}}
	session := ce.provisionAndLogin(t)
	readTok := ce.createToken(t, session, "read")

	// Without a DHCP server every endpoint answers 503.
	coreWantError(t, ce.do("GET", "/api/v1/dhcp", "", readTok), http.StatusServiceUnavailable, "unavailable", "")
	ce.srv.d.DHCP = fd

	w := ce.do("GET", "/api/v1/dhcp", "", readTok)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d %s", w.Code, w.Body)
	}
	for _, m := range []string{`"available":true`, `"state":"blocked"`, `"blockers":["other-server"]`,
		`"interface":{"name":"eth0","mac":"02:aa:00:00:00:10","ipv4":"192.168.1.10","prefixLen":24,"dynamic":false}`,
		`"pool":{"start":"192.168.1.100","end":"192.168.1.199","size":100,"used":1,"static":0}`,
		`"router":"192.168.1.1","dnsServer":"192.168.1.10","domain":"lan"`,
		`"otherServers":[{"address":"192.168.1.1","serverId":"192.168.1.1","source":"probe","lastSeen":"2026-09-25T12:00:00Z"}]`,
		`"lastProbe":{"time":"2026-09-25T12:00:00Z","servers":1}`,
		`"counters":{"received":0,"offers":0,"acks":0,"naks":0,"declines":0,"releases":0,"informs":0,"dropped":0}`,
		`"routerAdvertisements":{"enabled":true,"available":true,"state":"sending","blockers":[],"address":"fd00::10","lastSent":"2026-09-25T12:00:00Z","sent":3,"solicitations":0}`,
		`"dhcpv6":{"enabled":false,"state":"off","blockers":[],"replies":0,"ignored":0}`,
	} {
		if !strings.Contains(w.Body.String(), m) {
			t.Errorf("status JSON lacks %s:\n%s", m, w.Body)
		}
	}
	w = ce.do("GET", "/api/v1/dhcp/interfaces", "", readTok)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(),
		`[{"name":"eth0","mac":"02:aa:00:00:00:10","ipv4":["192.168.1.10/24"],"ipv6":[{"address":"fd00::10","kind":"ula","temporary":false,"deprecated":false}],"dynamic4":false,"virtual":false}]`) {
		t.Fatalf("interfaces %d %s", w.Code, w.Body)
	}
	w = ce.do("GET", "/api/v1/dhcp/leases", "", readTok)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(),
		`[{"mac":"02:00:00:00:00:01","ip":"192.168.1.100","hostname":"laptop","expires":"2026-09-25T12:00:00Z","active":true,"static":false,"dnsName":"laptop.lan"}]`) {
		t.Fatalf("leases %d %s", w.Code, w.Body)
	}

	// Writes need admin rights.
	for _, r := range [][2]string{{"POST", "/api/v1/dhcp/probe"}, {"DELETE", "/api/v1/dhcp/leases/02:00:00:00:00:01"},
		{"POST", "/api/v1/dhcp/static"}, {"PUT", "/api/v1/dhcp/static/02:00:00:00:00:01"}, {"DELETE", "/api/v1/dhcp/static/02:00:00:00:00:01"}} {
		body := ""
		if r[0] != "DELETE" && r[1] != "/api/v1/dhcp/probe" {
			body = `{"ip":"192.168.1.5"}`
		}
		coreWantError(t, ce.do(r[0], r[1], body, readTok), http.StatusForbidden, "forbidden", "")
	}

	w = ce.do("POST", "/api/v1/dhcp/probe", "", session)
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) !=
		`{"servers":[{"address":"192.168.1.1","serverId":"192.168.1.1","offer":"192.168.1.50"}],"durationMs":3000}` {
		t.Fatalf("probe %d %s", w.Code, w.Body)
	}
	fd.probeErr = apperr.TooMany("wait")
	coreWantError(t, ce.do("POST", "/api/v1/dhcp/probe", "", session), http.StatusTooManyRequests, "too_many_requests", "")
	fd.probeErr = apperr.Unavailable("off")
	coreWantError(t, ce.do("POST", "/api/v1/dhcp/probe", "", session), http.StatusServiceUnavailable, "unavailable", "")

	w = ce.do("POST", "/api/v1/dhcp/static", `{"mac":"02-00-00-00-00-02","ip":"192.168.1.50","hostname":"printer","comment":"office"}`, session)
	if w.Code != http.StatusCreated || !strings.Contains(w.Body.String(), `"mac":"02:00:00:00:00:02","ip":"192.168.1.50","hostname":"printer","comment":"office"`) {
		t.Fatalf("create %d %s", w.Code, w.Body)
	}
	coreWantError(t, ce.do("POST", "/api/v1/dhcp/static", `{"mac":"x","ip":"192.168.1.50"}`, session), http.StatusBadRequest, "invalid", "mac")
	coreWantError(t, ce.do("POST", "/api/v1/dhcp/static", `{"mac":"02:00:00:00:00:03","ip":"192.168.1.50","extra":1}`, session), http.StatusBadRequest, "invalid", "body")
	w = ce.do("PUT", "/api/v1/dhcp/static/02:00:00:00:00:02", `{"ip":"192.168.1.51","hostname":"printer2"}`, session)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"ip":"192.168.1.51","hostname":"printer2"`) {
		t.Fatalf("update %d %s", w.Code, w.Body)
	}
	coreWantError(t, ce.do("PUT", "/api/v1/dhcp/static/02:00:00:00:00:09", `{"ip":"192.168.1.51"}`, session), http.StatusNotFound, "not_found", "")
	if w := ce.do("DELETE", "/api/v1/dhcp/static/02-00-00-00-00-02", "", session); w.Code != http.StatusNoContent {
		t.Fatalf("delete static %d %s", w.Code, w.Body)
	}
	if w := ce.do("DELETE", "/api/v1/dhcp/leases/02:00:00:00:00:01", "", session); w.Code != http.StatusNoContent {
		t.Fatalf("delete lease %d %s", w.Code, w.Body)
	}
	coreWantError(t, ce.do("DELETE", "/api/v1/dhcp/leases/02:00:00:00:00:07", "", session), http.StatusNotFound, "not_found", "")

	actions := ce.auditActions(t)
	for _, a := range []string{"dhcp.probe", "dhcp.static.create", "dhcp.static.update", "dhcp.static.delete", "dhcp.lease.delete"} {
		if !slices.Contains(actions, a) {
			t.Errorf("audit lacks %s: %v", a, actions)
		}
	}
	entries, _, err := ce.auth.AuditLog(context.Background(), auth.AuditQuery{Search: "dhcp.static.delete", Limit: 5})
	if err != nil || len(entries) != 1 || entries[0].Target != "02:00:00:00:00:02" {
		t.Fatalf("audit target %+v %v", entries, err)
	}
}

// PATCH /settings/dhcp checks enabled and changed settings against the
// live interface; the checker's field errors reach the client.
func TestDHCPSettingsPatch(t *testing.T) {
	ce := newCoreEnv(t)
	fd := &fakeDHCP{statics: map[string]dhcp.StaticLease{}}
	ce.srv.d.DHCP = fd
	session := ce.provisionAndLogin(t)
	on := `{"enabled":true,"interface":"eth0","rangeStart":"192.168.1.100","rangeEnd":"192.168.1.199","ipv6":{"routerAdvertisements":true}}`
	w := ce.do("PATCH", "/api/v1/settings/dhcp", on, session)
	if w.Code != http.StatusOK || fd.checked != 1 {
		t.Fatalf("patch %d %s (checked %d)", w.Code, w.Body, fd.checked)
	}
	h := ce.set.Get().DHCP
	if !h.Enabled || h.Interface != "eth0" || !h.IPv6.RouterAdvertisements || h.LeaseSeconds != 86400 || !h.RegisterHostnames {
		t.Fatalf("stored %+v", h)
	}
	// Unchanged settings are not checked again.
	ce.do("PATCH", "/api/v1/settings/dhcp", on, session)
	if fd.checked != 1 {
		t.Fatalf("checked %d", fd.checked)
	}
	fd.checkErr = apperr.Invalid("dhcp.rangeStart", "must be an address of 192.168.1.0/24")
	coreWantError(t, ce.do("PATCH", "/api/v1/settings/dhcp", `{"rangeStart":"10.0.0.1"}`, session), http.StatusBadRequest, "invalid", "dhcp.rangeStart")
	if ce.set.Get().DHCP.RangeStart != "192.168.1.100" {
		t.Fatal("refused change stored")
	}
	// Switching off is never checked against the interface.
	fd.checkErr = apperr.Invalid("dhcp.interface", "gone")
	if w := ce.do("PATCH", "/api/v1/settings/dhcp", `{"enabled":false,"interface":"eth9"}`, session); w.Code != http.StatusOK {
		t.Fatalf("switch off %d %s", w.Code, w.Body)
	}
	coreWantError(t, ce.do("PATCH", "/api/v1/settings/dhcp", `{"leaseSeconds":5}`, session), http.StatusBadRequest, "invalid", "dhcp.leaseSeconds")
	if !slices.Contains(ce.auditActions(t), "settings.update") {
		t.Fatal("settings change not audited")
	}
}
