package api

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/hustenreizjuengling/picache/internal/auth"
	"github.com/hustenreizjuengling/picache/internal/clients"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/dns/filter"
	dnsserver "github.com/hustenreizjuengling/picache/internal/dns/server"
	"github.com/hustenreizjuengling/picache/internal/dns/upstream"
	"github.com/hustenreizjuengling/picache/internal/secrets"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// pkgiEnv is an API server with a real filter engine, DNS server, clients
// registry and auth service on a temp database (the routes of rules,
// local DNS and upstreams per group). Handlers are called directly as an
// admin browser session.
type pkgiEnv struct {
	srv  *Server
	eng  *filter.Engine
	auth *auth.Service
	reg  *clients.Registry
	dns  *dnsserver.Server
}

// The test addresses: the router, a trusted EDNS forwarder and devices
// (outside the networks a test machine is likely to have).
var (
	pkgiRouter    = netip.MustParseAddr("192.168.51.1")
	pkgiForwarder = netip.MustParseAddr("192.168.51.3")
	pkgiPhone     = netip.MustParseAddr("192.168.51.60")
	pkgiSameMAC   = netip.MustParseAddr("192.168.51.61") // the forwarder's MAC
)

func newPkgIEnv(t *testing.T) *pkgiEnv {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	d, err := db.Open(filepath.Join(dir, "picache.db"), 2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	set, err := settings.Open(ctx, d, log)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := set.Update(ctx, func(a *settings.All) error {
		a.DNS.RouterResolver = pkgiRouter.String()
		a.DNS.EDNSClientTrusted = []string{pkgiForwarder.String()}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	box, err := secrets.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	a, err := auth.New(ctx, d, set, box, filepath.Join(dir, "setup-token"), log)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := clients.New(ctx, d, nil, log)
	if err != nil {
		t.Fatal(err)
	}
	eng, err := filter.New(ctx, d, set, &http.Client{Transport: noNetwork{}}, filepath.Join(dir, "lists"), log)
	if err != nil {
		t.Fatal(err)
	}
	dns, err := dnsserver.New(ctx, dnsserver.Deps{DB: d, Settings: set, Clients: reg, Filter: eng, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{d: Deps{Settings: set, Auth: a, DNS: dns, Filter: eng, Clients: reg, Log: log}, log: log, mux: http.NewServeMux()}
	s.registerFilterRoutes()
	s.registerDNSRoutes()
	s.macOf = func(ip netip.Addr) (string, bool) {
		switch ip {
		case pkgiPhone:
			return "aa:bb:cc:00:00:60", true
		case pkgiForwarder, pkgiSameMAC:
			return "aa:bb:cc:00:00:03", true
		}
		return "", false
	}
	return &pkgiEnv{srv: s, eng: eng, auth: a, reg: reg, dns: dns}
}

// call invokes h like the router does as an admin session from
// 192.168.1.44; pathVals are name, value pairs.
func (e *pkgiEnv) call(h handlerFunc, method, target, body string, pathVals ...string) *httptest.ResponseRecorder {
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

// audits returns the audit entries of action, newest first.
func (e *pkgiEnv) audits(t *testing.T, action string) []auth.AuditEntry {
	t.Helper()
	entries, _, err := e.auth.AuditLog(context.Background(), auth.AuditQuery{Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	var out []auth.AuditEntry
	for _, a := range entries {
		if a.Action == action {
			out = append(out, a)
		}
	}
	return out
}

func wantError(t *testing.T, what string, rec *httptest.ResponseRecorder, status int, field, text string) {
	t.Helper()
	wantStatus(t, what, rec, status, field)
	if text != "" && !strings.Contains(rec.Body.String(), text) {
		t.Errorf("%s: want %q: %s", what, text, rec.Body)
	}
}

func decodeAs[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %s: %v", rec.Body, err)
	}
	return v
}

// The batch body: action, 1–1000 unique positive ids, force only for
// lists; all or nothing; one audit entry with at most 100 ids.
func TestBatchRoutes(t *testing.T) {
	e := newPkgIEnv(t)
	s := e.srv
	ctx := context.Background()
	for body, field := range map[string]string{
		`{"action":"toggle","ids":[1]}`:              "action",
		`{"action":"delete","ids":[]}`:               "ids",
		`{"action":"delete"}`:                        "ids",
		`{"action":"delete","ids":[1,1]}`:            "ids",
		`{"action":"delete","ids":[0]}`:              "ids",
		`{"action":"delete","ids":[-3]}`:             "ids",
		`{"action":"delete","ids":[1],"force":true}`: "force",
	} {
		wantStatus(t, body, e.call(s.filterBatchRules, "POST", "/api/v1/filter/rules/batch", body), http.StatusBadRequest, field)
	}
	many := make([]string, 1001)
	for i := range many {
		many[i] = fmt.Sprint(i + 1)
	}
	wantStatus(t, "1001 ids", e.call(s.filterBatchRules, "POST", "/x", `{"action":"delete","ids":[`+strings.Join(many, ",")+`]}`),
		http.StatusBadRequest, "ids")

	var ids []int64
	for i := range 3 {
		r, err := e.eng.CreateRule(ctx, filter.RuleInput{Action: "block", Type: "exact", Pattern: fmt.Sprintf("r%d.example", i), Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, r.ID)
	}
	body := func(action string, ids ...int64) string {
		b, _ := json.Marshal(map[string]any{"action": action, "ids": ids})
		return string(b)
	}
	if res := decodeAs[batchResult](t, e.call(s.filterBatchRules, "POST", "/x", body("disable", ids[0], ids[1]))); res.Changed != 2 {
		t.Errorf("disable %+v", res)
	}
	if res := decodeAs[batchResult](t, e.call(s.filterBatchRules, "POST", "/x", body("disable", ids...))); res.Changed != 1 {
		t.Errorf("only changed rows count: %+v", res)
	}
	wantError(t, "unknown id", e.call(s.filterBatchRules, "POST", "/x", body("delete", ids[0], 999)), http.StatusNotFound, "", "rule 999 not found")
	if rules, _ := e.eng.Rules(ctx, filter.RuleQuery{}); len(rules) != 3 {
		t.Errorf("a refused batch changed %d rules", 3-len(rules))
	}
	if res := decodeAs[batchResult](t, e.call(s.filterBatchRules, "POST", "/x", body("delete", ids...))); res.Changed != 3 {
		t.Errorf("delete %+v", res)
	}

	// Clients only delete; the Default group is never deleted.
	wantError(t, "clients enable", e.call(s.clientsBatch, "POST", "/x", body("enable", 1)), http.StatusBadRequest, "action", "clients can only be deleted")
	g, err := e.reg.CreateGroup(ctx, clients.GroupInput{Name: "Kids", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	wantError(t, "Default", e.call(s.groupsBatch, "POST", "/x", body("delete", g.ID, 1)), http.StatusForbidden, "", "the Default group cannot be deleted")
	if res := decodeAs[batchResult](t, e.call(s.groupsBatch, "POST", "/x", body("disable", g.ID))); res.Changed != 1 {
		t.Errorf("group disable %+v", res)
	}

	// Lists accept force.
	if res := decodeAs[batchResult](t, e.call(s.filterBatchLists, "POST", "/x", `{"action":"disable","ids":[1],"force":true}`)); res.Changed != 1 {
		t.Errorf("lists %+v", res)
	}

	// One audit entry per call, ids cut to the first 100.
	var ipIDs []int64
	for i := range 120 {
		r, err := e.eng.CreateIPRule(ctx, filter.IPRuleInput{Action: "block", Pattern: fmt.Sprintf("10.%d.0.0/16", i), Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		ipIDs = append(ipIDs, r.ID)
	}
	if res := decodeAs[batchResult](t, e.call(s.filterBatchIPRules, "POST", "/x", body("disable", ipIDs...))); res.Changed != 120 {
		t.Errorf("ip rules %+v", res)
	}
	a := e.audits(t, "filter.ip_rule.batch")
	if len(a) != 1 {
		t.Fatalf("audit %+v", a)
	}
	var details struct {
		Action string  `json:"action"`
		Count  int     `json:"count"`
		IDs    []int64 `json:"ids"`
	}
	if err := json.Unmarshal([]byte(a[0].Details), &details); err != nil || details.Action != "disable" || details.Count != 120 ||
		!slices.Equal(details.IDs, ipIDs[:100]) {
		t.Errorf("audit details %s", a[0].Details)
	}
	for _, action := range []string{"filter.rule.batch", "group.batch", "filter.list.batch"} {
		if len(e.audits(t, action)) == 0 {
			t.Errorf("no %s audit", action)
		}
	}
}

// deviceRule posts an "Only for this device" request.
func (e *pkgiEnv) deviceRule(ip, rule string) *httptest.ResponseRecorder {
	return e.call(e.srv.filterDeviceRule, "POST", "/api/v1/filter/rules/device", `{"clientIp":"`+ip+`","rule":`+rule+`}`)
}

// "Only for this device": refusals first (nothing changed), then a client
// and a group of the device's own; reuse; a conflicting rule undoes the
// clients change.
func TestDeviceRuleRoute(t *testing.T) {
	e := newPkgIEnv(t)
	ctx := context.Background()
	block := `{"action":"block","type":"exact","pattern":"ads.example","enabled":true}`
	for _, c := range []struct {
		ip, rule    string
		status      int
		field, text string
	}{
		{"not-an-ip", block, 400, "clientIp", "must be an IP address"},
		{"127.0.0.1", block, 400, "clientIp", "loopback"},
		{"::", block, 400, "clientIp", "not a device address"},
		{pkgiRouter.String(), block, 400, "clientIp", "is the router; configure it on Clients & groups"},
		{pkgiForwarder.String(), block, 400, "clientIp", "is the trusted EDNS forwarder"},
		{pkgiPhone.String(), `{"action":"block","type":"exact","pattern":"ads.example","groupIds":[1]}`, 400, "rule.groupIds", "chosen by the server"},
		{pkgiPhone.String(), `{"action":"block","type":"exact","pattern":"bad pattern"}`, 400, "rule.pattern", ""},
		{pkgiPhone.String(), `{"action":"block","type":"exact","pattern":"x.example","reply":"custom_ip"}`, 400, "rule.replyIpv4", ""},
	} {
		wantError(t, c.ip+" "+c.rule, e.deviceRule(c.ip, c.rule), c.status, c.field, c.text)
	}
	if _, err := e.srv.d.Settings.Update(ctx, func(a *settings.All) error { a.Logs.AnonymizeClientIPs = true; return nil }); err != nil {
		t.Fatal(err)
	}
	wantError(t, "anonymised", e.deviceRule(pkgiPhone.String(), block), http.StatusConflict, "", "client addresses are anonymised")
	if _, err := e.srv.d.Settings.Update(ctx, func(a *settings.All) error { a.Logs.AnonymizeClientIPs = false; return nil }); err != nil {
		t.Fatal(err)
	}
	if cl, _ := e.reg.Clients(ctx); len(cl) != 0 {
		t.Fatalf("a refusal created clients: %+v", cl)
	}

	// A new client (address and MAC) and a group of its own; Default kept.
	rec := e.deviceRule(pkgiPhone.String(), block)
	wantStatus(t, "device", rec, http.StatusOK, "")
	res := decodeAs[deviceRuleResponse](t, rec)
	if !res.CreatedClient || !res.CreatedGroup || !res.CreatedRule || res.Client.Name != pkgiPhone.String() ||
		!slices.Equal(res.Client.Identifiers, []string{pkgiPhone.String(), "aa:bb:cc:00:00:60"}) ||
		!slices.Equal(res.Client.GroupIDs, []int64{1, res.Group.ID}) || res.Group.DeviceClientID == nil ||
		*res.Group.DeviceClientID != res.Client.ID || !res.Group.Enabled || !slices.Equal(res.Rule.GroupIDs, []int64{res.Group.ID}) {
		t.Fatalf("device %+v", res)
	}
	if !strings.Contains(rec.Body.String(), `"deviceClientId":`) {
		t.Errorf("deviceClientId missing: %s", rec.Body)
	}
	// Again: everything reused; another rule joins the same group.
	again := decodeAs[deviceRuleResponse](t, e.deviceRule(pkgiPhone.String(), block))
	if again.CreatedClient || again.CreatedGroup || again.CreatedRule || again.Group.ID != res.Group.ID || again.Rule.ID != res.Rule.ID {
		t.Errorf("reuse %+v", again)
	}
	other := decodeAs[deviceRuleResponse](t, e.deviceRule(pkgiPhone.String(), `{"action":"allow","type":"subtree","pattern":"shop.example","enabled":true}`))
	if !other.CreatedRule || other.Group.ID != res.Group.ID {
		t.Errorf("second rule %+v", other)
	}
	// An existing identical rule gains the device group.
	shared, err := e.eng.CreateRule(ctx, filter.RuleInput{Action: "block", Type: "exact", Pattern: "shared.example", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	sr := decodeAs[deviceRuleResponse](t, e.deviceRule(pkgiPhone.String(), `{"action":"block","type":"exact","pattern":"shared.example","enabled":true}`))
	if sr.CreatedRule || sr.Rule.ID != shared.ID || !slices.Equal(sr.Rule.GroupIDs, []int64{1, res.Group.ID}) {
		t.Errorf("shared rule %+v", sr.Rule)
	}

	// Other options: 409 and the new client and group are undone.
	if _, err := e.eng.CreateRule(ctx, filter.RuleInput{Action: "block", Type: "exact", Pattern: "typed.example", Enabled: true,
		Qtypes: []string{"AAAA"}}); err != nil {
		t.Fatal(err)
	}
	before, _ := e.reg.Groups(ctx)
	wantError(t, "conflict", e.deviceRule("192.168.51.70", `{"action":"block","type":"exact","pattern":"typed.example","enabled":true}`),
		http.StatusConflict, "", "with other options; edit it instead")
	after, _ := e.reg.Groups(ctx)
	if cl, _ := e.reg.Clients(ctx); len(cl) != 1 || len(after) != len(before) {
		t.Errorf("not undone: %d clients, groups %d → %d", len(cl), len(before), len(after))
	}
	// A disabled rule with the same options: 409 (adding the group would
	// block nothing for the device), undone, the rule's groups unchanged.
	off, err := e.eng.CreateRule(ctx, filter.RuleInput{Action: "block", Type: "exact", Pattern: "off.example", Enabled: false})
	if err != nil {
		t.Fatal(err)
	}
	wantError(t, "disabled", e.deviceRule("192.168.51.71", `{"action":"block","type":"exact","pattern":"off.example","enabled":true}`),
		http.StatusConflict, "", fmt.Sprintf("rule %d) but is disabled; enable or edit it", off.ID))
	if cl, _ := e.reg.Clients(ctx); len(cl) != 1 {
		t.Errorf("not undone: %d clients", len(cl))
	}
	if r, _ := e.eng.Rules(ctx, filter.RuleQuery{}); !slices.ContainsFunc(r, func(r filter.Rule) bool {
		return r.ID == off.ID && slices.Equal(r.GroupIDs, []int64{1}) && !r.Enabled
	}) {
		t.Errorf("the disabled rule changed: %+v", r)
	}

	// The MAC of a trusted forwarder never becomes an identifier.
	pm := decodeAs[deviceRuleResponse](t, e.deviceRule(pkgiSameMAC.String(), block))
	if !slices.Equal(pm.Client.Identifiers, []string{pkgiSameMAC.String()}) {
		t.Errorf("protected MAC %+v", pm.Client)
	}

	// Inside a CIDR client: a new client with its groups and flags.
	lab, err := e.reg.CreateGroup(ctx, clients.GroupInput{Name: "Lab", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.reg.CreateClient(ctx, clients.ClientInput{Name: "Lab net", Identifiers: []string{"192.168.52.0/24"},
		GroupIDs: []int64{lab.ID}, IgnoreLogs: true}); err != nil {
		t.Fatal(err)
	}
	lr := decodeAs[deviceRuleResponse](t, e.deviceRule("192.168.52.5", block))
	if !lr.CreatedClient || !slices.Equal(lr.Client.GroupIDs, []int64{lab.ID, lr.Group.ID}) || !lr.Client.IgnoreLogs || !lr.Client.IgnoreStats {
		t.Errorf("CIDR client %+v", lr.Client)
	}
	if _, err := e.reg.CreateClient(ctx, clients.ClientInput{Name: "Mixed", Identifiers: []string{"192.168.53.0/24", "192.168.54.9"}}); err != nil {
		t.Fatal(err)
	}
	wantError(t, "mixed", e.deviceRule("192.168.54.9", block), http.StatusConflict, "", "the address belongs to client Mixed, which also covers a network")

	a := e.audits(t, "filter.rule.device")
	if len(a) != 6 || !strings.Contains(a[len(a)-1].Details, `"createdClient":true`) {
		t.Errorf("audit %+v", a)
	}
}

// Concurrent "Only for this device" requests for a new device: the undo of
// a refused one never deletes the client or group a successful one reused.
func TestDeviceRuleConcurrent(t *testing.T) {
	e := newPkgIEnv(t)
	ctx := context.Background()
	if _, err := e.eng.CreateRule(ctx, filter.RuleInput{Action: "block", Type: "exact", Pattern: "typed.example", Enabled: true,
		Qtypes: []string{"AAAA"}}); err != nil {
		t.Fatal(err)
	}
	for i := range 20 {
		ip := fmt.Sprintf("192.168.55.%d", 10+i)
		var wg sync.WaitGroup
		var good *httptest.ResponseRecorder
		wg.Go(func() {
			good = e.deviceRule(ip, fmt.Sprintf(`{"action":"block","type":"exact","pattern":"ok%d.example","enabled":true}`, i))
		})
		wg.Go(func() { e.deviceRule(ip, `{"action":"block","type":"exact","pattern":"typed.example","enabled":true}`) })
		wg.Wait()
		wantStatus(t, ip, good, http.StatusOK, "")
		res := decodeAs[deviceRuleResponse](t, good)
		groups, _ := e.reg.Groups(ctx)
		cl, _ := e.reg.Clients(ctx)
		if !slices.ContainsFunc(groups, func(g clients.Group) bool { return g.ID == res.Group.ID }) ||
			!slices.ContainsFunc(cl, func(c clients.Client) bool { return c.ID == res.Client.ID }) {
			t.Fatalf("%s: the device group or client of a successful request is gone: %+v", ip, res)
		}
		rules, _ := e.eng.Rules(ctx, filter.RuleQuery{})
		if !slices.ContainsFunc(rules, func(r filter.Rule) bool { return r.ID == res.Rule.ID && slices.Contains(r.GroupIDs, res.Group.ID) }) {
			t.Fatalf("%s: the rule lost its device group", ip)
		}
	}
}

// The upstreams of a group: presets, the dedicated route, the settings
// cross-checks both ways, GET /dns/upstreams groups.
func TestGroupUpstreamRoutes(t *testing.T) {
	e := newPkgIEnv(t)
	s := e.srv
	ctx := context.Background()
	rec := e.call(s.groupUpstreamPresets, "GET", "/api/v1/groups/upstream-presets", "")
	presets := decodeAs[[]settings.UpstreamPreset](t, rec)
	if len(presets) != 3 || presets[0].Key != "cloudflare-family" || !strings.Contains(rec.Body.String(), `"plain":[`) {
		t.Errorf("presets %s", rec.Body)
	}
	g, err := e.reg.CreateGroup(ctx, clients.GroupInput{Name: "Kids", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	id := fmt.Sprint(g.ID)
	put := func(gid, body string) *httptest.ResponseRecorder {
		return e.call(s.groupSetUpstreams, "PUT", "/api/v1/groups/"+gid+"/upstreams", body, "id", gid)
	}
	for _, c := range []struct {
		body, field, text string
	}{
		{`{"upstreamPreset":""}`, "upstreams", "required"},
		{`{"upstreams":[]}`, "upstreamPreset", "required"},
		{`{"upstreams":["9.9.9.9"],"upstreamPreset":"cloudflare-family"}`, "upstreamPreset", "not both"},
		{`{"upstreams":[],"upstreamPreset":"nope"}`, "upstreamPreset", ""},
		{`{"upstreams":["dns.lan"],"upstreamPreset":""}`, "upstreams[0]", ""},
		{`{"upstreams":["not a url ::"],"upstreamPreset":""}`, "upstreams[0]", ""},
	} {
		wantError(t, c.body, put(id, c.body), http.StatusBadRequest, c.field, c.text)
	}
	wantError(t, "Default", put("1", `{"upstreams":["9.9.9.9"],"upstreamPreset":""}`), http.StatusBadRequest, "upstreams", "DNS settings")
	wantStatus(t, "unknown", put("99", `{"upstreams":["9.9.9.9"],"upstreamPreset":""}`), http.StatusNotFound, "")
	rec = put(id, `{"upstreams":[],"upstreamPreset":"cloudflare-family"}`)
	wantStatus(t, "preset", rec, http.StatusOK, "")
	if got := decodeAs[clients.Group](t, rec); got.UpstreamPreset != "cloudflare-family" || len(got.Upstreams) != 0 || got.Name != "Kids" {
		t.Errorf("preset %+v", got)
	}
	a := e.audits(t, "group.upstreams")
	if len(a) != 1 || a[0].Details != `{"upstreamPreset":"cloudflare-family","upstreams":0}` {
		t.Errorf("audit %+v", a)
	}
	// A PUT /groups without the members keeps them.
	rec = e.call(s.groupUpdate, "PUT", "/api/v1/groups/"+id, `{"name":"Kids","enabled":true}`, "id", id)
	if got := decodeAs[clients.Group](t, rec); got.UpstreamPreset != "cloudflare-family" {
		t.Errorf("0.12 PUT %s", rec.Body)
	}
	// The settings cannot remove what the group needs.
	patch := func(body string) *httptest.ResponseRecorder {
		return e.call(s.settingsPatch, "PATCH", "/api/v1/settings/dns", body, "section", "dns")
	}
	wantError(t, "bootstrap", patch(`{"bootstrap":[]}`), http.StatusBadRequest, "dns.bootstrap", "group Kids")
	wantStatus(t, "custom", put(id, `{"upstreams":["dns.example.net","https://dns.example.org/dns-query"],"upstreamPreset":""}`), http.StatusOK, "")
	wantError(t, "domain", patch(`{"localDomain":"example.net"}`), http.StatusBadRequest, "dns.localDomain", "dns.example.net")
	wantStatus(t, "unrelated domain", patch(`{"localDomain":"home.arpa"}`), http.StatusOK, "")
	// POST /groups checks the same.
	wantStatus(t, "create", e.call(s.groupCreate, "POST", "/api/v1/groups", `{"name":"Teens","enabled":true,"upstreams":["dns.home.arpa"]}`),
		http.StatusBadRequest, "upstreams[0]")
	// group.create and group.update record the upstreams only as their
	// number (a URL can carry a token or a profile ID).
	rec = e.call(s.groupCreate, "POST", "/api/v1/groups", `{"name":"Teens","enabled":true,"upstreams":["https://doh.example.org/dns-query?token=SECRET1"]}`)
	wantStatus(t, "create with upstreams", rec, http.StatusCreated, "")
	teens := fmt.Sprint(decodeAs[clients.Group](t, rec).ID)
	wantStatus(t, "update with upstreams", e.call(s.groupUpdate, "PUT", "/api/v1/groups/"+teens,
		`{"name":"Teens","enabled":true,"upstreams":["tls://abc123.dns.example.org"]}`, "id", teens), http.StatusOK, "")
	for _, action := range []string{"group.create", "group.update"} {
		a := e.audits(t, action)
		if len(a) == 0 || strings.Contains(a[0].Details, "SECRET1") || strings.Contains(a[0].Details, "abc123") ||
			!strings.Contains(a[0].Details, `"upstreams":1`) || !strings.Contains(a[0].Details, `"name":"Teens"`) {
			t.Errorf("%s audit %+v", action, a)
		}
	}

	// GET /dns/upstreams reports the group sets (no names).
	us, up := newUpstreamTestServer(t)
	up.SetGroupUpstreams([]upstream.GroupUpstreams{{ID: 2, Name: "Kids", Upstreams: []string{"192.0.2.7"}}})
	rec = callUpstream(us, us.handleUpstreamList, http.MethodGet, "")
	view := decodeAs[upstreamsView](t, rec)
	if len(view.Groups) != 1 || !slices.Equal(view.Groups[0].GroupIDs, []int64{2}) || len(view.Groups[0].Upstreams) != 1 ||
		strings.Contains(rec.Body.String(), "Kids") {
		t.Errorf("groups %s", rec.Body)
	}
}

// IP rules over the routes: 201, 409 duplicate, 400 fields, 404, list
// filters.
func TestIPRuleRoutes(t *testing.T) {
	e := newPkgIEnv(t)
	s := e.srv
	rec := e.call(s.filterCreateIPRule, "POST", "/api/v1/filter/ip-rules", `{"action":"block","pattern":"198.51.100.0/24","enabled":true,"comment":"bad net"}`)
	wantStatus(t, "create", rec, http.StatusCreated, "")
	r := decodeAs[filter.IPRule](t, rec)
	if r.Pattern != "198.51.100.0/24" || !slices.Equal(r.GroupIDs, []int64{1}) {
		t.Errorf("created %+v", r)
	}
	wantError(t, "duplicate", e.call(s.filterCreateIPRule, "POST", "/x", `{"action":"block","pattern":"198.51.100.9/24","enabled":true}`),
		http.StatusConflict, "", "this rule already exists")
	for body, field := range map[string]string{
		`{"action":"deny","pattern":"198.51.100.1"}`:               "action",
		`{"action":"block","pattern":"10.0.0.0/4"}`:                "pattern",
		`{"action":"block","pattern":"example.com"}`:               "pattern",
		`{"action":"block","pattern":"2001:db8::/16"}`:             "pattern",
		`{"action":"block","pattern":"192.0.2.1","groupIds":[42]}`: "groupIds",
	} {
		wantStatus(t, body, e.call(s.filterCreateIPRule, "POST", "/x", body), http.StatusBadRequest, field)
	}
	id := fmt.Sprint(r.ID)
	rec = e.call(s.filterUpdateIPRule, "PUT", "/x", `{"action":"allow","pattern":"198.51.100.7","enabled":true}`, "id", id)
	if u := decodeAs[filter.IPRule](t, rec); u.Action != "allow" || u.Pattern != "198.51.100.7" {
		t.Errorf("update %s", rec.Body)
	}
	wantStatus(t, "404", e.call(s.filterUpdateIPRule, "PUT", "/x", `{"action":"allow","pattern":"192.0.2.1"}`, "id", "99"), http.StatusNotFound, "")
	wantStatus(t, "action filter", e.call(s.filterIPRules, "GET", "/api/v1/filter/ip-rules?action=nope", ""), http.StatusBadRequest, "action")
	wantStatus(t, "search", e.call(s.filterIPRules, "GET", "/api/v1/filter/ip-rules?search="+strings.Repeat("a", 257), ""), http.StatusBadRequest, "search")
	if got := decodeAs[[]filter.IPRule](t, e.call(s.filterIPRules, "GET", "/api/v1/filter/ip-rules?action=allow&search=100.7", "")); len(got) != 1 {
		t.Errorf("filtered %+v", got)
	}
	wantStatus(t, "delete", e.call(s.filterDeleteIPRule, "DELETE", "/x", "", "id", id), http.StatusNoContent, "")
	for _, action := range []string{"filter.ip_rule.create", "filter.ip_rule.update", "filter.ip_rule.delete"} {
		if len(e.audits(t, action)) != 1 {
			t.Errorf("no %s audit", action)
		}
	}
	st := e.call(s.filterStats, "GET", "/api/v1/filter/stats", "")
	for _, member := range []string{`"ipEntries":`, `"ipRules":`, `"modifiedEntries":`, `"modifiedDropped":`, `"ipGuardLists":`} {
		if !strings.Contains(st.Body.String(), member) {
			t.Errorf("stats lack %s: %s", member, st.Body)
		}
	}
}

// Rules over the routes: the new members, import (dry run unaudited),
// export, explain with qtype, search bounds, lists with format.
func TestRuleRoutesPkgI(t *testing.T) {
	e := newPkgIEnv(t)
	s := e.srv
	rec := e.call(s.filterCreateRule, "POST", "/api/v1/filter/rules",
		`{"action":"block","type":"subtree","pattern":"ads.example","enabled":true,"qtypes":["AAAA"],"reply":"nxdomain","denyallow":["ok.ads.example"]}`)
	wantStatus(t, "create", rec, http.StatusCreated, "")
	r := decodeAs[filter.Rule](t, rec)
	for _, bad := range []struct{ body, field string }{
		{`{"action":"block","type":"exact","pattern":"x.example","qtypes":["BOGUS"]}`, "qtypes[0]"},
		{`{"action":"block","type":"exact","pattern":"x.example","reply":"custom_ip","replyIpv4":"::1"}`, "replyIpv4"},
		{`{"action":"allow","type":"exact","pattern":"x.example","reply":"null"}`, "reply"},
		{`{"action":"block","type":"exact","pattern":"x.example","invert":true}`, "invert"},
	} {
		wantStatus(t, bad.body, e.call(s.filterCreateRule, "POST", "/x", bad.body), http.StatusBadRequest, bad.field)
	}
	// A 0.12-shaped PUT keeps the new members.
	id := fmt.Sprint(r.ID)
	rec = e.call(s.filterUpdateRule, "PUT", "/x", `{"action":"block","type":"subtree","pattern":"ads.example","enabled":true,"comment":"ads"}`, "id", id)
	if u := decodeAs[filter.Rule](t, rec); !slices.Equal(u.Qtypes, []string{"AAAA"}) || u.Reply != "nxdomain" || len(u.Denyallow) != 1 || u.Comment != "ads" {
		t.Errorf("0.12 PUT %s", rec.Body)
	}

	// Import: a dry run changes and audits nothing.
	text := `"||one.example^\n@@||two.example^$dnstype=A\n/^three[0-9]\\.example$/\n"`
	rec = e.call(s.filterImportRules, "POST", "/api/v1/filter/rules/import", `{"text":`+text+`,"dryRun":true}`)
	if res := decodeAs[filter.RuleImportResult](t, rec); res.Applied || res.Added != 3 || res.ErrorCount != 0 {
		t.Errorf("dry run %s", rec.Body)
	}
	if len(e.audits(t, "filter.rule.import")) != 0 {
		t.Error("a dry run was audited")
	}
	rec = e.call(s.filterImportRules, "POST", "/x", `{"text":`+text+`}`)
	if res := decodeAs[filter.RuleImportResult](t, rec); !res.Applied || res.Added != 3 {
		t.Errorf("import %s", rec.Body)
	}
	rec = e.call(s.filterImportRules, "POST", "/x", `{"text":"||four.example^$client=1.2.3.4\n||five.example^"}`)
	if res := decodeAs[filter.RuleImportResult](t, rec); res.Applied || res.ErrorCount != 1 || res.Errors[0].Line != 1 {
		t.Errorf("errors %s", rec.Body)
	}
	if a := e.audits(t, "filter.rule.import"); len(a) != 1 || a[0].Details != `{"added":3}` {
		t.Errorf("audit %+v", a)
	}
	wantStatus(t, "import groups", e.call(s.filterImportRules, "POST", "/x", `{"text":"||x.example^","groupIds":[42]}`), http.StatusBadRequest, "groupIds")

	// Export: text that names the new modifiers.
	rec = e.call(s.filterExportRules, "GET", "/api/v1/filter/rules/export?action=block", "")
	out := rec.Body.String()
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") ||
		!strings.Contains(out, "! ads\n||ads.example^$dnstype=AAAA,denyallow=ok.ads.example,reply=nxdomain\n") ||
		strings.Contains(out, "two.example") {
		t.Errorf("export %d %q", rec.Code, out)
	}

	// Explain with a query type.
	rec = e.call(s.filterExplain, "POST", "/api/v1/filter/explain", `{"domain":"x.ads.example","qtype":"aaaa","clientIp":"192.168.1.9"}`)
	if !strings.Contains(rec.Body.String(), `"qtype":"AAAA"`) || !strings.Contains(rec.Body.String(), `"action":"block"`) {
		t.Errorf("explain %s", rec.Body)
	}
	rec = e.call(s.filterExplain, "POST", "/x", `{"domain":"x.ads.example","clientIp":"192.168.1.9"}`)
	if !strings.Contains(rec.Body.String(), `"qtype":"A"`) || !strings.Contains(rec.Body.String(), `"skipped":"qtype"`) {
		t.Errorf("explain A %s", rec.Body)
	}
	wantStatus(t, "qtype", e.call(s.filterExplain, "POST", "/x", `{"domain":"x.example","qtype":"BOGUS"}`), http.StatusBadRequest, "qtype")

	// Search bounds and results.
	for target, field := range map[string]string{
		"/api/v1/filter/search?q=ab":                          "q",
		"/api/v1/filter/search?q=" + strings.Repeat("a", 254): "q",
		"/api/v1/filter/search?q=ads&limit=0":                 "limit",
		"/api/v1/filter/search?q=ads&limit=201":               "limit",
		"/api/v1/filter/search?q=ads&clientIp=nope":           "clientIp",
	} {
		wantStatus(t, target, e.call(s.filterSearch, "GET", target, ""), http.StatusBadRequest, field)
	}
	rec = e.call(s.filterSearch, "GET", "/api/v1/filter/search?q=Example&limit=2&clientIp=192.168.1.9", "")
	res := decodeAs[filter.SearchResult](t, rec)
	if res.Q != "example" || len(res.Items) != 2 || !res.Truncated || res.Items[0].Applies == nil {
		t.Errorf("search %s", rec.Body)
	}

	// Lists: format and the category of answer-address lists.
	for _, c := range []struct{ body, field string }{
		{`{"url":"https://lists.example/a.txt","format":"csv","enabled":false}`, "format"},
		{`{"url":"https://lists.example/a.txt","format":"ips","category":"ads","enabled":false}`, "category"},
	} {
		wantStatus(t, c.body, e.call(s.filterCreateList, "POST", "/x", c.body), http.StatusBadRequest, c.field)
	}
	rec = e.call(s.filterCreateList, "POST", "/x", `{"url":"https://lists.example/ips.txt","format":"ips","category":"security","enabled":false}`)
	wantStatus(t, "ips list", rec, http.StatusCreated, "")
	l := decodeAs[filter.List](t, rec)
	if l.Format != filter.FormatIPs || !l.NameAuto || !strings.Contains(rec.Body.String(), `"ipBlocksIgnored":0`) {
		t.Errorf("list %s", rec.Body)
	}
	lid := fmt.Sprint(l.ID)
	rec = e.call(s.filterUpdateList, "PUT", "/x", `{"url":"https://lists.example/ips.txt","name":"Bad hosts","category":"security","enabled":false}`, "id", lid)
	if u := decodeAs[filter.List](t, rec); u.Format != filter.FormatIPs || u.NameAuto || u.Name != "Bad hosts" {
		t.Errorf("0.12 list PUT %s", rec.Body)
	}
}

// Records over the routes: typed data, scopes, the import (dry run
// unaudited), batch.
func TestRecordRoutesPkgI(t *testing.T) {
	e := newPkgIEnv(t)
	s := e.srv
	ctx := context.Background()
	g, err := e.reg.CreateGroup(ctx, clients.GroupInput{Name: "Kids", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	gid := fmt.Sprint(g.ID)
	rec := e.call(s.dnsRecordCreate, "POST", "/api/v1/dns/records",
		`{"name":"_sip._tcp.lan","type":"SRV","data":{"priority":10,"weight":5,"port":5060,"target":"pbx.lan"},"enabled":true,"scope":"groups","groupIds":[`+gid+`]}`)
	wantStatus(t, "srv", rec, http.StatusCreated, "")
	r := decodeAs[dnsserver.Record](t, rec)
	if r.Scope != "groups" || !slices.Equal(r.GroupIDs, []int64{g.ID}) || !strings.Contains(rec.Body.String(), `"port":5060`) {
		t.Errorf("created %s", rec.Body)
	}
	for _, c := range []struct{ body, field string }{
		{`{"name":"x.lan","type":"MX","data":{"preference":10,"host":"."}}`, "data.host"},
		{`{"name":"x.lan","type":"SRV","value":"1 1 5060 a.lan","otherFamily":"forward"}`, "otherFamily"},
		{`{"name":"x.lan","type":"A","value":"192.0.2.1","scope":"all","groupIds":[` + gid + `]}`, "groupIds"},
		{`{"name":"x.lan","type":"A","value":"192.0.2.1","scope":"groups","groupIds":[42]}`, "groupIds"},
		{`{"name":"x.lan","type":"A","value":"192.0.2.1","scope":"some"}`, "scope"},
		{`{"name":"x.lan","type":"A","value":"192.0.2.1","otherFamily":"maybe"}`, "otherFamily"},
	} {
		wantStatus(t, c.body, e.call(s.dnsRecordCreate, "POST", "/x", c.body), http.StatusBadRequest, c.field)
	}
	// A 0.12-shaped PUT keeps scope, groups and otherFamily (A record).
	a, err := e.dns.CreateRecord(ctx, dnsserver.RecordInput{Name: "nas.lan", Type: "A", Value: "192.168.1.5", Enabled: true,
		Scope: new("groups"), GroupIDs: []int64{g.ID}, OtherFamily: new("forward")})
	if err != nil {
		t.Fatal(err)
	}
	aid := fmt.Sprint(a.ID)
	rec = e.call(s.dnsRecordUpdate, "PUT", "/x", `{"name":"nas.lan","type":"A","value":"192.168.1.6","enabled":true}`, "id", aid)
	if u := decodeAs[dnsserver.Record](t, rec); u.Scope != "groups" || !slices.Equal(u.GroupIDs, []int64{g.ID}) || u.OtherFamily != "forward" {
		t.Errorf("0.12 PUT %s", rec.Body)
	}

	text := `"# header\n127.0.0.1 localhost\n192.168.1.20 printer printer.lan\n0.0.0.0 ads.example\n"`
	rec = e.call(s.dnsRecordsImport, "POST", "/api/v1/dns/records/import", `{"format":"hosts","text":`+text+`,"dryRun":true}`)
	if res := decodeAs[dnsserver.RecordImportResult](t, rec); res.ErrorCount != 1 || res.Errors[0].Line != 4 {
		t.Errorf("dry run %s", rec.Body)
	}
	text = `"192.168.1.20 printer printer.lan\n192.168.1.21 scanner\n"`
	rec = e.call(s.dnsRecordsImport, "POST", "/x", `{"format":"hosts","text":`+text+`}`)
	if res := decodeAs[dnsserver.RecordImportResult](t, rec); res.Added != 3 {
		t.Errorf("import %s", rec.Body)
	}
	if a := e.audits(t, "dns.record.import"); len(a) != 1 {
		t.Errorf("import audits %+v", a)
	}
	wantStatus(t, "format", e.call(s.dnsRecordsImport, "POST", "/x", `{"format":"csv","text":"x"}`), http.StatusBadRequest, "format")

	rec = e.call(s.dnsRecordsBatch, "POST", "/api/v1/dns/records/batch", fmt.Sprintf(`{"action":"disable","ids":[%d,%d]}`, r.ID, a.ID))
	if res := decodeAs[batchResult](t, rec); res.Changed != 2 {
		t.Errorf("records batch %s", rec.Body)
	}
	fw, err := e.dns.CreateForwarder(ctx, dnsserver.ForwarderInput{Domain: "corp.example", Upstreams: []string{"10.9.9.9"}, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	rec = e.call(s.dnsForwardersBatch, "POST", "/api/v1/dns/forwarders/batch", fmt.Sprintf(`{"action":"delete","ids":[%d]}`, fw.ID))
	if res := decodeAs[batchResult](t, rec); res.Changed != 1 {
		t.Errorf("forwarders batch %s", rec.Body)
	}
	for _, action := range []string{"dns.record.batch", "dns.forwarder.batch"} {
		if len(e.audits(t, action)) != 1 {
			t.Errorf("no %s audit", action)
		}
	}
}
