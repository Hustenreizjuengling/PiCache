package api

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/auth"
)

// fakeNetwork is a Network with a fixed check; Scan answers scanErr or
// starts a scan of 253 addresses.
type fakeNetwork struct {
	mu      sync.Mutex
	scanErr error
	scans   int
}

func (f *fakeNetwork) Check(context.Context) NetworkCheck {
	at := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	return NetworkCheck{
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
