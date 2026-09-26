package api

import (
	"context"
	"errors"
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
	imported []dhcp.ImportInput
	resets   int
	resetErr error // returned after the settings were reset
	limit    int
}

func (f *fakeDHCP) ExportStatics(format string) ([]byte, error) {
	switch format {
	case dhcp.FormatCSV:
		return []byte("mac,ip,hostname,comment,clientId,leaseSeconds\n"), nil
	case dhcp.FormatHosts:
		return []byte("# PiCache reserved addresses\n"), nil
	}
	return nil, apperr.Invalid("format", "must be csv or hosts")
}

func (f *fakeDHCP) ImportStatics(_ context.Context, in dhcp.ImportInput) (dhcp.ImportResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if in.Format != dhcp.FormatCSV && in.Format != dhcp.FormatHosts && in.Format != dhcp.FormatLines {
		return dhcp.ImportResult{}, apperr.Invalid("format", "must be csv, hosts or lines")
	}
	f.imported = append(f.imported, in)
	if strings.Contains(in.Text, "bad") {
		return dhcp.ImportResult{Added: 1, Errors: []dhcp.ImportError{{Line: 2, Field: "ip", Message: "must be an IPv4 address"}}}, nil
	}
	return dhcp.ImportResult{Applied: !in.DryRun, Added: 2, Updated: 1, Unchanged: 3, Removed: 4, Errors: []dhcp.ImportError{}}, nil
}

func (f *fakeDHCP) DeleteLeases(context.Context) (int, error) { return 7, nil }

func (f *fakeDHCP) Reset(context.Context) (dhcp.ResetResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resets++
	if f.resetErr != nil {
		return dhcp.ResetResult{Settings: true}, f.resetErr
	}
	return dhcp.ResetResult{Settings: true, Leases: 3, Statics: 2}, nil
}

func (f *fakeDHCP) Log(limit int) []dhcp.LogEntry {
	f.mu.Lock()
	f.limit = limit
	f.mu.Unlock()
	return []dhcp.LogEntry{{Time: dhcpAt, Kind: dhcp.LogDHCPv4, MAC: "02:00:00:00:00:01", Address: "192.168.1.100", In: "DISCOVER",
		Result: dhcp.ResultIgnored, Reason: dhcp.LogReasonNotReserved}}
}

var dhcpAt = time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)

func (f *fakeDHCP) Status() dhcp.Status {
	return dhcp.Status{
		Available: true, State: dhcp.StateBlocked, Blockers: []string{dhcp.BlockerOtherServer}, Deployment: dhcp.DeploymentSystemd,
		Interface: &dhcp.StatusInterface{Name: "eth0", MAC: "02:aa:00:00:00:10", IPv4: "192.168.1.10", PrefixLen: 24},
		Pool:      &dhcp.StatusPool{Start: "192.168.1.100", End: "192.168.1.199", Size: 100, Used: 1, Static: 0},
		Router:    "192.168.1.1", DNSServer: "192.168.1.10", Domain: "lan",
		OtherServers: []dhcp.OtherServer{{Address: "192.168.1.1", ServerID: "192.168.1.1", Source: dhcp.SourceProbe, LastSeen: dhcpAt}},
		LastProbe:    &dhcp.LastProbe{Time: dhcpAt, Servers: 1},
		IPv6: dhcp.StatusIPv6{
			RouterAdvertisements: dhcp.RAStatus{Enabled: true, Available: true, State: dhcp.StateSending, Blockers: []string{},
				Address: "fd00::10", LastSent: dhcpAt, Sent: 3},
			DHCPv6: dhcp.DHCPv6Status{State: dhcp.StateOff, Blockers: []string{}},
			OtherAnnouncers: []dhcp.Announcer{{Kind: dhcp.AnnouncerRA, Address: "fe80::1", Interface: "eth0", DNS: []string{"fd00::10", "fd00::1"}, OwnDNS: []string{"fd00::10"},
				Managed: new(false), Other: new(true), RouterLifetime: new(1800), FirstSeen: dhcpAt, LastSeen: dhcpAt, Conflict: true}},
			LastSearch: &dhcp.LastSearch{Time: dhcpAt, RA: true, DHCPv6: false},
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
	for _, m := range []string{`"available":true`, `"deployment":"systemd"`, `"state":"blocked"`, `"blockers":["other-server"]`,
		`"otherAnnouncers":[{"kind":"ra","address":"fe80::1","interface":"eth0","dns":["fd00::10","fd00::1"],"ownDns":["fd00::10"],"managed":false,"other":true,"routerLifetime":1800,"firstSeen":"2026-09-25T12:00:00Z","lastSeen":"2026-09-25T12:00:00Z","conflict":true}]`,
		`"lastSearch":{"time":"2026-09-25T12:00:00Z","ra":true,"dhcpv6":false}`,
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
		{"POST", "/api/v1/dhcp/static"}, {"PUT", "/api/v1/dhcp/static/02:00:00:00:00:01"}, {"DELETE", "/api/v1/dhcp/static/02:00:00:00:00:01"},
		{"DELETE", "/api/v1/dhcp/leases"}, {"POST", "/api/v1/dhcp/reset"}, {"POST", "/api/v1/dhcp/static/import"}} {
		body := ""
		if r[0] != "DELETE" && r[1] != "/api/v1/dhcp/probe" && r[1] != "/api/v1/dhcp/reset" {
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

	// Export (every principal), import (admins; only an applied import is
	// audited), resetting the leases and the whole server, the log.
	w = ce.do("GET", "/api/v1/dhcp/static/export?format=csv", "", readTok)
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "text/csv; charset=utf-8" ||
		w.Header().Get("Content-Disposition") != `attachment; filename="picache-reservations.csv"` || !strings.HasPrefix(w.Body.String(), "mac,ip,") {
		t.Fatalf("export csv %d %v %s", w.Code, w.Header(), w.Body)
	}
	w = ce.do("GET", "/api/v1/dhcp/static/export?format=hosts", "", readTok)
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "text/plain; charset=utf-8" ||
		w.Header().Get("Content-Disposition") != `attachment; filename="picache-reservations.hosts"` {
		t.Fatalf("export hosts %d %v", w.Code, w.Header())
	}
	coreWantError(t, ce.do("GET", "/api/v1/dhcp/static/export?format=xml", "", readTok), http.StatusBadRequest, "invalid", "format")
	coreWantError(t, ce.do("GET", "/api/v1/dhcp/static/export", "", readTok), http.StatusBadRequest, "invalid", "format")
	w = ce.do("POST", "/api/v1/dhcp/static/import", `{"format":"csv","text":"x","replace":true,"dryRun":true}`, session)
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != `{"applied":false,"added":2,"updated":1,"unchanged":3,"removed":4,"errors":[]}` {
		t.Fatalf("dry run %d %s", w.Code, w.Body)
	}
	w = ce.do("POST", "/api/v1/dhcp/static/import", `{"format":"lines","text":"bad"}`, session)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"errors":[{"line":2,"field":"ip","message":"must be an IPv4 address"}]`) {
		t.Fatalf("errors %d %s", w.Code, w.Body)
	}
	if slices.Contains(ce.auditActions(t), "dhcp.static.import") {
		t.Fatal("a dry run or failed import was audited")
	}
	w = ce.do("POST", "/api/v1/dhcp/static/import", `{"format":"hosts","text":"ok","replace":true}`, session)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"applied":true`) {
		t.Fatalf("import %d %s", w.Code, w.Body)
	}
	coreWantError(t, ce.do("POST", "/api/v1/dhcp/static/import", `{"format":"xml","text":""}`, session), http.StatusBadRequest, "invalid", "format")
	coreWantError(t, ce.do("POST", "/api/v1/dhcp/static/import", `{"format":"csv","text":"","extra":1}`, session), http.StatusBadRequest, "invalid", "body")
	w = ce.do("DELETE", "/api/v1/dhcp/leases", "", session)
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != `{"deleted":7}` {
		t.Fatalf("delete leases %d %s", w.Code, w.Body)
	}
	if w := ce.do("POST", "/api/v1/dhcp/reset", "", session); w.Code != http.StatusNoContent || fd.resets != 1 {
		t.Fatalf("reset %d %s", w.Code, w.Body)
	}
	// The settings were reset but the tables could not be emptied: 500,
	// and the reset is audited all the same (checked below).
	fd.resetErr = errors.New("database is locked")
	if w := ce.do("POST", "/api/v1/dhcp/reset", "", session); w.Code != http.StatusInternalServerError || fd.resets != 2 {
		t.Fatalf("failed reset %d %s", w.Code, w.Body)
	}
	fd.resetErr = nil
	w = ce.do("GET", "/api/v1/dhcp/log?limit=5", "", readTok)
	if w.Code != http.StatusOK || fd.limit != 5 || !strings.Contains(w.Body.String(),
		`[{"time":"2026-09-25T12:00:00Z","kind":"dhcpv4","mac":"02:00:00:00:00:01","address":"192.168.1.100","in":"DISCOVER","result":"ignored","reason":"not-reserved"}]`) {
		t.Fatalf("log %d %s", w.Code, w.Body)
	}
	if ce.do("GET", "/api/v1/dhcp/log", "", readTok); fd.limit != 200 {
		t.Fatalf("default limit %d", fd.limit)
	}
	for _, q := range []string{"0", "201", "x"} {
		coreWantError(t, ce.do("GET", "/api/v1/dhcp/log?limit="+q, "", readTok), http.StatusBadRequest, "invalid", "limit")
	}

	actions := ce.auditActions(t)
	for _, a := range []string{"dhcp.probe", "dhcp.static.create", "dhcp.static.update", "dhcp.static.delete", "dhcp.lease.delete",
		"dhcp.static.import", "dhcp.leases.reset", "dhcp.reset"} {
		if !slices.Contains(actions, a) {
			t.Errorf("audit lacks %s: %v", a, actions)
		}
	}
	entries, _, err := ce.auth.AuditLog(context.Background(), auth.AuditQuery{Search: "dhcp.static.delete", Limit: 5})
	if err != nil || len(entries) != 1 || entries[0].Target != "02:00:00:00:00:02" {
		t.Fatalf("audit target %+v %v", entries, err)
	}
	entries, _, err = ce.auth.AuditLog(context.Background(), auth.AuditQuery{Search: "dhcp.reset", Limit: 5})
	if err != nil || len(entries) != 2 || !strings.Contains(string(entries[0].Details), `"settings":true`) ||
		!strings.Contains(string(entries[0].Details), "database is locked") || !strings.Contains(string(entries[1].Details), `"statics":2`) {
		t.Errorf("audit dhcp.reset: %+v %v", entries, err)
	}
	for action, want := range map[string]string{
		"dhcp.static.import": `"added":2`, "dhcp.leases.reset": `"leases":7`,
	} {
		entries, _, err := ce.auth.AuditLog(context.Background(), auth.AuditQuery{Search: action, Limit: 5})
		if err != nil || len(entries) != 1 || !strings.Contains(string(entries[0].Details), want) {
			t.Errorf("audit %s: %+v %v", action, entries, err)
		}
	}
	entries, _, _ = ce.auth.AuditLog(context.Background(), auth.AuditQuery{Search: "dhcp.static.import", Limit: 5})
	if len(entries) == 1 && (!strings.Contains(string(entries[0].Details), `"replace":true`) || !strings.Contains(string(entries[0].Details), `"removed":4`)) {
		t.Errorf("import details %s", entries[0].Details)
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
	// The new members: defaults, dotted error fields, the WPAD URL audited
	// with its value.
	w = ce.do("GET", "/api/v1/settings", "", session)
	if !strings.Contains(w.Body.String(), `"generateNames":true`) || !strings.Contains(w.Body.String(),
		`"onlyReserved":false,"rapidCommit":false,"options":{"ntpServers":[],"mtu":0,"wpadUrl":"","extraSearchDomains":[]}`) {
		t.Fatalf("settings %s", w.Body)
	}
	coreWantError(t, ce.do("PATCH", "/api/v1/settings/dhcp", `{"options":{"ntpServers":["10.0.0.1","x"]}}`, session),
		http.StatusBadRequest, "invalid", "dhcp.options.ntpServers[1]")
	coreWantError(t, ce.do("PATCH", "/api/v1/settings/dhcp", `{"options":{"mtu":100}}`, session), http.StatusBadRequest, "invalid", "dhcp.options.mtu")
	if w := ce.do("PATCH", "/api/v1/settings/dhcp", `{"options":{"wpadUrl":"http://proxy.lan/wpad.dat"}}`, session); w.Code != http.StatusOK {
		t.Fatalf("wpad %d %s", w.Code, w.Body)
	}
	entries, _, err := ce.auth.AuditLog(context.Background(), auth.AuditQuery{Search: "settings.update", Limit: 1})
	if err != nil || len(entries) != 1 || !strings.Contains(string(entries[0].Details), `"wpadUrl":["http://proxy.lan/wpad.dat"]`) ||
		!strings.Contains(string(entries[0].Details), `"dhcp.options"`) {
		t.Fatalf("wpad audit %+v %v", entries, err)
	}
}
