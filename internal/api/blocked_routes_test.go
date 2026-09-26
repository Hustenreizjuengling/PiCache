package api

import (
	"context"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"testing"

	dnsserver "github.com/hustenreizjuengling/picache/internal/dns/server"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// POST/DELETE /dns/blocked-clients: IP, CIDR or MAC; device:true stores the
// device's MAC; an entry that already matches is reported (added false);
// lockouts, the 256 bound and unknown entries are refused; only real
// changes are audited.
func TestBlockedClientRoutes(t *testing.T) {
	e := newDNSTestEnv(t)
	s := e.srv
	s.registerSettingsRoutes()
	s.macOf = func(ip netip.Addr) (string, bool) {
		switch ip {
		case netip.MustParseAddr("192.168.2.9"), netip.MustParseAddr("192.168.2.10"):
			return "AA:BB:CC:DD:EE:02", true
		case netip.MustParseAddr("192.168.3.3"): // the trusted forwarder below
			return "aa:bb:cc:dd:ee:33", true
		}
		return "", false
	}
	post := func(body string) (int, blockClientResult, errorBody) {
		t.Helper()
		rec := e.call(t, s.dnsBlockClient, "POST", "/api/v1/dns/blocked-clients", body)
		var res blockClientResult
		var eb errorBody
		if rec.Code == http.StatusOK {
			res = dnsDecode[blockClientResult](t, rec)
		} else {
			eb = dnsDecode[errorBody](t, rec)
		}
		return rec.Code, res, eb
	}
	for _, tc := range []struct {
		body  string
		entry string
		added bool
	}{
		{`{"client":"192.168.1.77"}`, "192.168.1.77", true},
		{`{"client":"::ffff:192.168.1.77"}`, "192.168.1.77", false},
		{`{"client":"192.168.1.9/24"}`, "192.168.1.0/24", true},
		{`{"client":"192.168.1.5"}`, "192.168.1.0/24", false}, // inside the CIDR
		{`{"client":"192.168.1.128/25"}`, "192.168.1.0/24", false},
		{`{"client":"192.168.2.9","device":true}`, "aa:bb:cc:dd:ee:02", true},
		{`{"client":"192.168.2.10","device":true}`, "aa:bb:cc:dd:ee:02", false}, // the same device
		{`{"client":"192.168.2.11","device":true}`, "192.168.2.11", true},       // MAC unknown: the address
		{`{"client":"AA-BB-CC-DD-EE-03"}`, "aa:bb:cc:dd:ee:03", true},
	} {
		code, res, eb := post(tc.body)
		if code != http.StatusOK || res.Entry != tc.entry || res.Added != tc.added || !slices.Contains(res.BlockedClients, tc.entry) {
			t.Errorf("%s: %d %+v %+v", tc.body, code, res, eb)
		}
	}
	if got := s.d.Settings.Get().DNS.BlockedClients; !slices.Equal(got, []string{"192.168.1.77", "192.168.1.0/24", "aa:bb:cc:dd:ee:02", "192.168.2.11", "aa:bb:cc:dd:ee:03"}) {
		t.Errorf("stored %v", got)
	}
	// Syntax and lockouts: field client.
	if _, err := s.d.Settings.Update(context.Background(), func(a *settings.All) error {
		a.DNS.EDNSClientTrusted = []string{"192.168.3.3"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for body, msg := range map[string]string{
		`{"client":"nope"}`:           "must be an IP address",
		`{"client":"10.0.0.0/7"}`:     "must be an IP address",
		`{"client":"fe80::1%eth0"}`:   "must be an IP address",
		`{"client":"127.0.0.1"}`:      "127.0.0.1 is a loopback address",
		`{"client":"192.168.3.0/24"}`: "192.168.3.0/24 contains the trusted EDNS forwarder 192.168.3.3",
		// device:true checks the address before taking its MAC, and the
		// forwarder's MAC is protected too.
		`{"client":"192.168.3.3","device":true}`: "192.168.3.3 is the trusted EDNS forwarder",
		`{"client":"AA:BB:CC:DD:EE:33"}`:         "aa:bb:cc:dd:ee:33 is the MAC address of the trusted EDNS forwarder 192.168.3.3",
	} {
		code, _, eb := post(body)
		if code != http.StatusBadRequest || eb.Error.Field != "client" || !strings.Contains(eb.Error.Message, msg) {
			t.Errorf("%s: %d %+v", body, code, eb)
		}
	}
	// 256 entries: a new one is refused, a matching one still reported.
	if _, err := s.d.Settings.Update(context.Background(), func(a *settings.All) error {
		for i := len(a.DNS.BlockedClients); i < settings.MaxBlockedClients; i++ {
			a.DNS.BlockedClients = append(a.DNS.BlockedClients, "10.0."+strconv.Itoa(i/250)+"."+strconv.Itoa(i%250))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := post(`{"client":"172.16.0.1"}`); code != http.StatusConflict {
		t.Errorf("257th entry: %d", code)
	}
	if code, res, _ := post(`{"client":"192.168.1.8"}`); code != http.StatusOK || res.Added {
		t.Errorf("matching entry when full: %d %+v", code, res)
	}
	// DELETE by query parameter (CIDRs contain "/").
	rec := e.call(t, s.dnsUnblockClient, "DELETE", "/api/v1/dns/blocked-clients?entry=192.168.1.9%2F24", "")
	dnsExpect(t, rec, http.StatusOK)
	if got := dnsDecode[blockedClientsResult](t, rec); slices.Contains(got.BlockedClients, "192.168.1.0/24") || len(got.BlockedClients) != 255 {
		t.Errorf("after delete: %d entries", len(got.BlockedClients))
	}
	dnsExpect(t, e.call(t, s.dnsUnblockClient, "DELETE", "/api/v1/dns/blocked-clients?entry=192.168.1.0%2F24", ""), http.StatusNotFound)
	rec = e.call(t, s.dnsUnblockClient, "DELETE", "/api/v1/dns/blocked-clients?entry=bad", "")
	dnsExpect(t, rec, http.StatusBadRequest)
	if eb := dnsDecode[errorBody](t, rec); eb.Error.Field != "entry" {
		t.Errorf("error %+v", eb)
	}
	if acts := e.auditActions(t); acts != nil {
		block, unblock := 0, 0
		for _, a := range acts {
			switch a {
			case "dns.client.block":
				block++
			case "dns.client.unblock":
				unblock++
			}
		}
		if block != 5 || unblock != 1 {
			t.Errorf("audit: %d blocks, %d unblocks (%v)", block, unblock, acts)
		}
	}
}

// PATCH /settings/dns checks a changed dns.blockedClients against the live
// system (the field names the normalised index) and a changed ECS subnet
// for being public; GET /clients/known shows the matching entry.
func TestBlockedClientsSettingsAndKnown(t *testing.T) {
	e := newDNSTestEnv(t)
	s := e.srv
	patch := func(body string) (int, errorBody) {
		t.Helper()
		rec := e.call(t, s.settingsPatch, "PATCH", "/api/v1/settings/dns", body, "section", "dns")
		var eb errorBody
		if rec.Code != http.StatusOK {
			eb = dnsDecode[errorBody](t, rec)
		}
		return rec.Code, eb
	}
	if code, eb := patch(`{"blockedClients":["10.1.1.1"," ","10.1.1.1","127.0.0.0/8"]}`); code != http.StatusBadRequest ||
		eb.Error.Field != "dns.blockedClients[1]" || eb.Error.Message != "127.0.0.0/8 contains loopback addresses (127.0.0.0/8)" {
		t.Errorf("loopback: %d %+v", code, eb)
	}
	if code, eb := patch(`{"blockedClients":["10.1.1.0/24"],"ednsClientTrusted":["10.1.1.53"]}`); code != http.StatusBadRequest ||
		eb.Error.Field != "dns.blockedClients[0]" || !strings.Contains(eb.Error.Message, "trusted EDNS forwarder 10.1.1.53") {
		t.Errorf("trusted: %d %+v", code, eb)
	}
	if code, eb := patch(`{"blockedClients":["192.168.1.0/24","aa:bb:cc:dd:ee:07"]}`); code != http.StatusOK {
		t.Fatalf("valid list: %d %+v", code, eb)
	}
	// An unchanged list is not checked again (e.g. after the router moved).
	if code, _ := patch(`{"rateLimitQps":60,"rateLimitBurst":240}`); code != http.StatusOK {
		t.Errorf("unrelated change refused")
	}
	for body, want := range map[string]int{
		`{"ecs":{"mode":"custom","customSubnet":"10.0.0.0/8"}}`:      http.StatusBadRequest,
		`{"ecs":{"mode":"custom","customSubnet":"100.64.0.0/16"}}`:   http.StatusBadRequest,
		`{"ecs":{"mode":"custom","customSubnet":"2002:c0a8::/32"}}`:  http.StatusBadRequest, // 6to4 of 192.168.0.0
		`{"ecs":{"mode":"custom","customSubnet":"198.51.100.0/24"}}`: http.StatusBadRequest, // documentation
		`{"ecs":{"mode":"custom","customSubnet":"8.8.4.0/24"}}`:      http.StatusOK,
		`{"ecs":{"mode":"client","customSubnet":""}}`:                http.StatusOK,
	} {
		code, eb := patch(body)
		if code != want || (want != http.StatusOK && eb.Error.Field != "dns.ecs.customSubnet") {
			t.Errorf("%s: %d %+v", body, code, eb)
		}
	}
	s.d.Clients.Seen(netip.MustParseAddr("192.168.1.31"))
	s.d.Clients.Seen(netip.MustParseAddr("192.168.2.31"))
	rec := e.call(t, s.clientsKnown, "GET", "/api/v1/clients/known", "")
	dnsExpect(t, rec, http.StatusOK)
	rows := dnsDecode[[]knownView](t, rec)
	if len(rows) != 2 {
		t.Fatalf("known %+v", rows)
	}
	for _, k := range rows {
		want := ""
		if k.IP == "192.168.1.31" {
			want = "192.168.1.0/24"
		}
		if k.BlockedBy != want {
			t.Errorf("%s blockedBy %q, want %q", k.IP, k.BlockedBy, want)
		}
	}
	if !strings.Contains(rec.Body.String(), `"blockedBy":"192.168.1.0/24"`) || strings.Count(rec.Body.String(), "blockedBy") != 1 {
		t.Errorf("body %s", rec.Body)
	}
}

// POST /dns/forwarders/import: dry runs and failed imports are not
// audited; an applied import is.
func TestForwarderImportRoute(t *testing.T) {
	e := newDNSTestEnv(t)
	s := e.srv
	body := `{"text":"[/corp.example/]10.9.9.9\n[//]10.8.8.8\n","dryRun":true}`
	rec := e.call(t, s.dnsForwardersImport, "POST", "/api/v1/dns/forwarders/import", body)
	dnsExpect(t, rec, http.StatusOK)
	res := dnsDecode[dnsserver.ForwarderImportResult](t, rec)
	if res.Applied || res.Added != 2 || res.Errors == nil || !strings.Contains(rec.Body.String(), `"errors":[]`) {
		t.Fatalf("dry run %+v %s", res, rec.Body)
	}
	rec = e.call(t, s.dnsForwardersImport, "POST", "/api/v1/dns/forwarders/import", `{"text":"bad line","dryRun":false}`)
	dnsExpect(t, rec, http.StatusOK)
	if res := dnsDecode[dnsserver.ForwarderImportResult](t, rec); res.Applied || len(res.Errors) != 1 || res.Errors[0].Field != "syntax" {
		t.Fatalf("bad import %+v", res)
	}
	rec = e.call(t, s.dnsForwardersImport, "POST", "/api/v1/dns/forwarders/import", strings.Replace(body, "true", "false", 1))
	if res := dnsDecode[dnsserver.ForwarderImportResult](t, rec); !res.Applied || res.Added != 2 {
		t.Fatalf("import %+v", res)
	}
	dnsExpect(t, e.call(t, s.dnsForwardersImport, "POST", "/api/v1/dns/forwarders/import", `{"text":"`+strings.Repeat(`\n`, 1025)+`"}`), http.StatusBadRequest)
	rec = e.call(t, s.dnsForwardersList, "GET", "/api/v1/dns/forwarders", "")
	list := dnsDecode[[]dnsserver.Forwarder](t, rec)
	if len(list) != 2 || list[0].Domain != "(unqualified)" || !slices.Equal(list[1].Domains, []string{"corp.example"}) {
		t.Errorf("list %+v", list)
	}
	if acts := e.auditActions(t); acts != nil && strings.Count(strings.Join(acts, ","), "dns.forwarder.import") != 1 {
		t.Errorf("audit %v", acts)
	}
	rec = e.call(t, s.dnsStats, "GET", "/api/v1/dns/stats", "")
	if !strings.Contains(rec.Body.String(), `"blockedClients":0`) || !strings.Contains(rec.Body.String(), `"dropped":0`) {
		t.Errorf("stats %s", rec.Body)
	}
}

// The new routes need admin rights.
func TestBlockedClientRoutesNeedAdmin(t *testing.T) {
	e := newCoreEnv(t)
	session := e.provisionAndLogin(t)
	readTok := e.createToken(t, session, "read")
	for _, rt := range []struct{ method, target, body string }{
		{"POST", "/api/v1/dns/blocked-clients", `{"client":"10.1.1.1"}`},
		{"DELETE", "/api/v1/dns/blocked-clients?entry=10.1.1.1", ""},
		{"POST", "/api/v1/dns/forwarders/import", `{"text":"","dryRun":true}`},
	} {
		coreWantError(t, e.do(rt.method, rt.target, rt.body, readTok), http.StatusForbidden, "forbidden", "")
	}
}
