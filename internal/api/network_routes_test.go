package api

import (
	"context"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/auth"
	"github.com/hustenreizjuengling/picache/internal/listing"
	"github.com/hustenreizjuengling/picache/internal/portcheck"
)

// fakeNetwork is a Network with a fixed check; Scan answers scanErr or
// starts a scan of 253 addresses.
type fakeNetwork struct {
	mu      sync.Mutex
	scanErr error
	scans   int
}

func (f *fakeNetwork) Check(_ context.Context, client netip.Addr) NetworkCheck {
	at := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	var req *NetworkRequester
	if client.IsValid() {
		req = &NetworkRequester{Address: client.String(), Local: client.IsLoopback()}
	}
	return NetworkCheck{
		Requester: req,
		CheckedAt: at, Mode: "host", StatsAvailable: true,
		Router:     &NetworkRouter{IPv4: "192.168.178.1", IPv6: []string{"fe80::1"}, MAC: "3c:a6:2f:00:00:01", Name: "fritz.box", Kind: "fritzbox"},
		Self:       NetworkSelf{IPv4: []string{"192.168.178.10"}, ULA: []string{}, Global: []string{}, DNSIPv6: true},
		Queries24h: NetworkQueries{Total: 300, IPv4: 300, FromRouter: 290},
		Checks: []NetworkItem{
			{ID: "router-forwarding", Status: "warn", Data: NetworkForwarding{RouterQueries: 290, TotalQueries: 300, Share: 0.967,
				RouterAddresses: []string{"192.168.178.1"}}},
			{ID: "ipv6-dns", Status: "warn", Data: NetworkIPv6DNS{LANHasIPv6: true, ULA: []string{}, Global: []string{}, HostIgnoresRA: true}},
			{ID: "refused", Status: "warn", Data: NetworkRefused{Sources: []NetworkRefusedSource{
				{Address: "2001:db8:1::5", Count: 2, Last: at, OnLink: true}}, Since: at}},
			{ID: "devices", Status: "info", Data: NetworkDeviceCounts{Total: 1, Never: 1}},
		},
		Devices: []NetworkDevice{{MAC: "aa:00:00:00:00:22", IPs: []string{"192.168.178.22"}, Status: "never"}},
		Scan:    NetworkScan{Running: true, StartedAt: at, Addresses: 253},
	}
}

func (f *fakeNetwork) Interfaces(context.Context) NetworkInterfaces {
	return NetworkInterfaces{Mode: "host", Interfaces: []NetworkInterface{{Name: "eth0", Index: 2, MAC: "dc:a6:32:00:00:01", Up: true,
		OperState: "up", MTU: 1500, SpeedMbps: 1000, Duplex: "full", Addresses: []string{"192.168.178.10/24"},
		Networks: []string{"192.168.178.0/24"}, DefaultGateways: []NetworkInterfaceGate{{Family: "ipv4", Gateway: "192.168.178.1"}}}}}
}

func (f *fakeNetwork) Scan() (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.scanErr != nil {
		return 0, f.scanErr
	}
	f.scans++
	return 253, nil
}

func TestNetworkRoutes(t *testing.T) {
	e := newStorageTestEnv(t)
	fn := &fakeNetwork{}
	e.srv.d.Network = fn
	e.srv.d.Config.WebListen = []string{":8080"}
	ce := &coreEnv{srv: New(e.srv.d), auth: e.auth, set: e.srv.d.Settings}
	session := ce.provisionAndLogin(t)
	readTok := ce.createToken(t, session, "read")

	w := ce.do("GET", "/api/v1/network/check", "", readTok)
	if w.Code != http.StatusOK {
		t.Fatalf("check %d %s", w.Code, w.Body)
	}
	// The JSON members the UI reads.
	for _, m := range []string{`"checkedAt":"2026-09-25T12:00:00Z"`, `"mode":"host"`, `"statsAvailable":true`,
		`"router":{"ipv4":"192.168.178.1","ipv6":["fe80::1"],"mac":"3c:a6:2f:00:00:01","name":"fritz.box","kind":"fritzbox"}`,
		`"self":{"ipv4":["192.168.178.10"],"ula":[],"global":[],"dnsIpv6":true}`,
		`"queries24h":{"total":300,"ipv4":300,"ipv6":0,"fromRouter":290}`,
		`{"id":"router-forwarding","status":"warn","data":{"routerQueries":290,"totalQueries":300,"share":0.967,"routerAddresses":["192.168.178.1"]}}`,
		`"data":{"lanHasIPv6":true,"ipv6Queries":0,"ipv6Clients":0,"ula":[],"global":[],"hostIgnoresRA":true}`,
		`"data":{"sources":[{"address":"2001:db8:1::5","count":2,"last":"2026-09-25T12:00:00Z","onLink":true}],"since":"2026-09-25T12:00:00Z","trustConnectedNetworks":false}`,
		`"data":{"total":1,"active":0,"inactive":0,"never":1}`,
		`"devices":[{"mac":"aa:00:00:00:00:22","ips":["192.168.178.22"],"queries24h":0,"status":"never"}]`,
		`"scan":{"running":true,"startedAt":"2026-09-25T12:00:00Z","addresses":253}`,
	} {
		if !strings.Contains(w.Body.String(), m) {
			t.Errorf("check JSON lacks %s:\n%s", m, w.Body)
		}
	}

	coreWantError(t, ce.do("POST", "/api/v1/network/scan", "", readTok), http.StatusForbidden, "forbidden", "")
	w = ce.do("POST", "/api/v1/network/scan", "", session)
	if w.Code != http.StatusAccepted || strings.TrimSpace(w.Body.String()) != `{"started":true,"addresses":253}` {
		t.Fatalf("scan %d %s", w.Code, w.Body)
	}
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{apperr.Conflict("a network scan is already running"), http.StatusConflict, "conflict"},
		{apperr.TooMany("wait"), http.StatusTooManyRequests, "too_many_requests"},
		{apperr.Unavailable("bridge"), http.StatusServiceUnavailable, "unavailable"},
	} {
		fn.mu.Lock()
		fn.scanErr = tc.err
		fn.mu.Unlock()
		coreWantError(t, ce.do("POST", "/api/v1/network/scan", "", session), tc.status, tc.code, "")
	}
	entries, _, err := e.auth.AuditLog(context.Background(), auth.AuditQuery{Search: "network.scan", Limit: 10})
	if err != nil || len(entries) != 1 || entries[0].Details != `{"addresses":253}` {
		t.Fatalf("audit %+v %v", entries, err)
	}
}

// fakePorts is a PortChecks with a fixed state; Run answers runErr.
type fakePorts struct {
	mu      sync.Mutex
	runErr  error
	runs    int
	forgot  []string
	lastQry [3]any
}

func (f *fakePorts) State(context.Context) (portcheck.State, error) {
	at := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	return portcheck.State{Enabled: true, LastRun: at, Ports: []portcheck.Port{{Port: 22, Service: "SSH"}},
		Devices: []portcheck.Device{{Device: "mac:aa:00:00:00:00:22", MAC: "aa:00:00:00:00:22", IP: "192.168.178.22",
			FirstCheck: at, LastCheck: at, LastAnswer: at, Open: []portcheck.OpenPort{{Port: 22, Service: "SSH", Since: at}}}}}, nil
}

func (f *fakePorts) Changes(_ context.Context, device string, limit, offset int) (listing.Page[portcheck.Change], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastQry = [3]any{device, limit, offset}
	return listing.Page[portcheck.Change]{Items: []portcheck.Change{}}, nil
}

func (f *fakePorts) Run(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs++
	return f.runErr
}

func (f *fakePorts) Forget(_ context.Context, device string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if device != "mac:aa:00:00:00:00:22" {
		return apperr.NotFound("device", device)
	}
	f.forgot = append(f.forgot, device)
	return nil
}

func TestNetworkPortRoutes(t *testing.T) {
	e := newStorageTestEnv(t)
	ce := &coreEnv{srv: New(e.srv.d), auth: e.auth, set: e.srv.d.Settings}
	session := ce.provisionAndLogin(t)
	readTok := ce.createToken(t, session, "read")
	// Without logs.db: 503.
	coreWantError(t, ce.do("GET", "/api/v1/network/ports", "", readTok), http.StatusServiceUnavailable, "unavailable", "")

	fp := &fakePorts{}
	e.srv.d.Ports = fp
	ce = &coreEnv{srv: New(e.srv.d), auth: e.auth, set: e.srv.d.Settings}
	w := ce.do("GET", "/api/v1/network/ports", "", readTok)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(),
		`"devices":[{"device":"mac:aa:00:00:00:00:22","mac":"aa:00:00:00:00:22","ip":"192.168.178.22","firstCheck":"2026-10-20T12:00:00Z","lastCheck":"2026-10-20T12:00:00Z","lastAnswer":"2026-10-20T12:00:00Z","open":[{"port":22,"service":"SSH","since":"2026-10-20T12:00:00Z"}]}]`) {
		t.Fatalf("state %d %s", w.Code, w.Body)
	}
	if w := ce.do("GET", "/api/v1/network/ports/changes?device=mac:aa:00:00:00:00:22&limit=5&offset=10", "", readTok); w.Code != http.StatusOK ||
		fp.lastQry != [3]any{"mac:aa:00:00:00:00:22", 5, 10} {
		t.Fatalf("changes %d %s %v", w.Code, w.Body, fp.lastQry)
	}
	coreWantError(t, ce.do("GET", "/api/v1/network/ports/changes?limit=501", "", readTok), http.StatusBadRequest, "invalid", "limit")
	coreWantError(t, ce.do("GET", "/api/v1/network/ports/changes?offset=-1", "", readTok), http.StatusBadRequest, "invalid", "offset")

	coreWantError(t, ce.do("POST", "/api/v1/network/ports/run", "", readTok), http.StatusForbidden, "forbidden", "")
	if w := ce.do("POST", "/api/v1/network/ports/run", "", session); w.Code != http.StatusAccepted ||
		strings.TrimSpace(w.Body.String()) != `{"started":true}` {
		t.Fatalf("run %d %s", w.Code, w.Body)
	}
	fp.mu.Lock()
	fp.runErr = apperr.Conflict("a port check is already running")
	fp.mu.Unlock()
	coreWantError(t, ce.do("POST", "/api/v1/network/ports/run", "", session), http.StatusConflict, "conflict", "")

	coreWantError(t, ce.do("DELETE", "/api/v1/network/ports/device", "", session), http.StatusBadRequest, "invalid", "device")
	coreWantError(t, ce.do("DELETE", "/api/v1/network/ports/device?device=ip:192.168.178.9", "", session), http.StatusNotFound, "not_found", "")
	if w := ce.do("DELETE", "/api/v1/network/ports/device?device=mac:aa:00:00:00:00:22", "", session); w.Code != http.StatusNoContent {
		t.Fatalf("forget %d %s", w.Code, w.Body)
	}
	for action, target := range map[string]string{"network.ports.run": "", "network.ports.forget": "mac:aa:00:00:00:00:22"} {
		entries, _, err := e.auth.AuditLog(context.Background(), auth.AuditQuery{Search: action, Limit: 10})
		if err != nil || len(entries) != 1 || entries[0].Target != target {
			t.Errorf("audit %s: %+v %v", action, entries, err)
		}
	}
}
