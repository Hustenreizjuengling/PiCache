package api

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/auth"
	"github.com/hustenreizjuengling/picache/internal/clients"
	"github.com/hustenreizjuengling/picache/internal/config"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/dns/filter"
	"github.com/hustenreizjuengling/picache/internal/dns/parental"
	dnsserver "github.com/hustenreizjuengling/picache/internal/dns/server"
	"github.com/hustenreizjuengling/picache/internal/secrets"
	"github.com/hustenreizjuengling/picache/internal/settings"
	"github.com/hustenreizjuengling/picache/internal/update"
)

// k1Env is a full API server (New) with real settings (with a sealer),
// auth, clients, filter, DNS server and parental controls on a temp
// database, fake listeners and sync managers.
type k1Env struct {
	*coreEnv
	reg       *clients.Registry
	eng       *filter.Engine
	listeners *fakeListeners
	sync      *fakeSync
}

type fakeListeners struct {
	mu    sync.Mutex
	saved map[string][]string
	next  bool
}

func (f *fakeListeners) SavedListeners() (map[string][]string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.saved, f.next
}
func (f *fakeListeners) FailedListeners() *ListenersFailed { return nil }
func (f *fakeListeners) SaveListeners(roles map[string][]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saved, f.next = roles, true
	return nil
}

type fakeSync struct {
	mu   sync.Mutex
	runs int
	err  error
}

func (f *fakeSync) Status() SyncStatus {
	return SyncStatus{Mode: "off", Sections: []string{}, IntervalMinutes: 15}
}
func (f *fakeSync) Run() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs++
	return f.err
}
func (f *fakeSync) ConfigSchema() map[string]int { return map[string]int{"clients": 4, "settings": 6} }

func newK1Env(t *testing.T) *k1Env {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	d, err := db.Open(filepath.Join(dir, "picache.db"), 2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	log := slog.New(slog.DiscardHandler)
	set := openOpenSettings(t, d, log)
	box, err := secrets.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	set.SetSealer(box.Seal, box.Open)
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
	par, err := parental.New(ctx, d, reg, log)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{DataDir: dir, CacheDir: filepath.Join(dir, "cache"), MountRoot: filepath.Join(dir, "mnt"),
		WebListen: []string{":8080"}, WebTLSListen: []string{":8443"}, DNSListen: []string{":53"}, CacheListen: []string{":80"},
		SNIListen: []string{":443"}, DoTListen: []string{":853"}, DestructiveAPI: true, ListenerLock: map[string]string{}}
	rt, upd := &coreRuntime{}, &coreUpdater{}
	fl, fs := &fakeListeners{}, &fakeSync{}
	srv := New(Deps{Config: cfg, Settings: set, Auth: a, Runtime: rt, Updates: upd, Clients: reg, Filter: eng, DNS: dns,
		Parental: par, Listeners: fl, Sync: fs, Log: log})
	srv.localAddrs = func() []netip.Addr {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("::1"), netip.MustParseAddr("192.168.1.2")}
	}
	return &k1Env{coreEnv: &coreEnv{srv: srv, auth: a, set: set, rt: rt, upd: upd, db: d}, reg: reg, eng: eng, listeners: fl, sync: fs}
}

func (e *k1Env) audits(t *testing.T, action string) int {
	t.Helper()
	var n int
	if err := e.db.R.QueryRow(`SELECT COUNT(*) FROM auth_audit WHERE action = ?`, action).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// makeFollower switches this server to follower mode for sections.
func (e *k1Env) makeFollower(t *testing.T, sections ...string) {
	t.Helper()
	tok := "pc_primary"
	if _, err := e.set.Update(context.Background(), func(a *settings.All) error {
		a.Sync = settings.Sync{Mode: settings.SyncFollower, Source: "https://primary.lan:8443", Token: &tok, IntervalMinutes: 15,
			Sections: sections}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// Permission X: an admin session, an admin token or a sync token; a sync
// token reaches nothing else; read tokens and viewers get 403.
func TestSyncTokenAndExportPermission(t *testing.T) {
	e := newK1Env(t)
	session := e.provisionAndLogin(t)
	syncTok := e.createToken(t, session, "sync")
	readTok := e.createToken(t, session, "read")
	adminTok := e.createToken(t, session, "admin")
	_, viewer := e.withViewer(t, session)
	const path = "/api/v1/system/export?sections=dns-settings"
	for name, cred := range map[string]string{"sync token": syncTok, "admin token": adminTok, "session": session} {
		if w := e.do("GET", path, "", cred); w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body)
		}
	}
	for name, cred := range map[string]string{"read token": readTok, "viewer": viewer} {
		coreWantError(t, e.do("GET", path, "", cred), http.StatusForbidden, "forbidden", "")
		_ = name
	}
	for _, p := range []string{"GET /api/v1/settings", "GET /api/v1/system/info", "PATCH /api/v1/settings/web", "GET /api/v1/tokens",
		"GET /api/v1/clients"} {
		method, target, _ := strings.Cut(p, " ")
		w := e.do(method, target, `{}`, syncTok)
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "this token can only read the configuration export") {
			t.Fatalf("sync token on %s: %d %s", p, w.Code, w.Body)
		}
	}
	var st authStatusResponse
	coreDecode(t, e.do("GET", "/api/v1/auth/status", "", syncTok), &st)
	if st.Authenticated || st.User != nil {
		t.Fatalf("auth status of a sync token: %+v", st)
	}
	// A viewer cannot create a sync token.
	w := e.do("POST", "/api/v1/tokens", `{"name":"x","scope":"sync","currentPassword":"`+viewerPassword+`"}`, viewer)
	coreWantError(t, w, http.StatusBadRequest, "invalid", "scope")
}

func TestExportContent(t *testing.T) {
	e := newK1Env(t)
	session := e.provisionAndLogin(t)
	syncTok := e.createToken(t, session, "sync")
	ctx := context.Background()
	kids, err := e.reg.CreateGroup(ctx, clients.GroupInput{Name: "Kids", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.reg.CreateClient(ctx, clients.ClientInput{Name: "Tablet", Identifiers: []string{"192.168.1.40", "host:tablet"},
		GroupIDs: []int64{kids.ID}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.set.Update(ctx, func(a *settings.All) error {
		a.DNS.AllowAllNetworks = true
		a.DNS.Upstreams = []string{"tls://dns.example.net"}
		a.Network.Proxy = settings.Proxy{URL: "http://proxy.lan:3128", Password: new("topsecret")}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	all := "clients-and-groups,lists-and-rules,local-dns,parental,dns-settings"
	w := e.do("GET", "/api/v1/system/export?sections="+all, "", syncTok)
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	var ex ConfigExport
	coreDecode(t, w, &ex)
	if ex.Format != ExportFormat || ex.FormatVersion != 1 || ex.InstanceID == "" || len(ex.ContentSHA256) != 64 ||
		ex.Schema["settings"] != 6 || len(ex.Sections) != 5 {
		t.Fatalf("export header %+v", ex)
	}
	names := map[int64]string{}
	for _, g := range ex.Groups {
		names[g.ID] = g.Name
	}
	if names[1] != "Default" || names[kids.ID] != "Kids" {
		t.Fatalf("groups %v", ex.Groups)
	}
	body := w.Body.String()
	for _, bad := range []string{"topsecret", "proxy.lan", "allowAllNetworks", `"sync"`, "notify", "storage", "passwordSealed", "audit"} {
		if strings.Contains(body, bad) {
			t.Fatalf("the export contains %q", bad)
		}
	}
	var cg ExportClientsGroups
	if err := json.Unmarshal(ex.Sections["clients-and-groups"], &cg); err != nil || len(cg.Clients) != 1 || cg.Clients[0].Name != "Tablet" {
		t.Fatalf("clients %+v %v", cg, err)
	}
	if sha, _ := ExportContentSHA256(ex.Sections); sha != ex.ContentSHA256 {
		t.Fatal("contentSha256 does not match the sections")
	}
	// A sync token polling an unchanged configuration is audited once.
	for range 3 {
		e.do("GET", "/api/v1/system/export?sections="+all, "", syncTok)
	}
	if n := e.audits(t, "system.export"); n != 1 {
		t.Fatalf("%d export audits", n)
	}
	// Section errors.
	for q, msg := range map[string]string{"sections=foo": "unknown section foo", "sections=dhcp": "dhcp cannot be exported",
		"sections=": "choose at least one section", "": "choose at least one section"} {
		w := e.do("GET", "/api/v1/system/export?"+q, "", syncTok)
		coreWantError(t, w, http.StatusBadRequest, "invalid", "sections")
		if !strings.Contains(w.Body.String(), msg) {
			t.Fatalf("%s: %s", q, w.Body)
		}
	}
}

// A list check on the primary changes only runtime state (status, check
// time, error, counts): the export's content hash stays, so followers apply
// nothing and the sync token's export is audited once.
func TestExportHashIgnoresListState(t *testing.T) {
	e := newK1Env(t)
	session := e.provisionAndLogin(t)
	syncTok := e.createToken(t, session, "sync")
	ctx := context.Background()
	l, err := e.eng.CreateList(ctx, filter.ListInput{URL: "https://lists.example/a.txt", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	export := func() string {
		w := e.do("GET", "/api/v1/system/export?sections=lists-and-rules", "", syncTok)
		if w.Code != http.StatusOK {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
		var ex ConfigExport
		coreDecode(t, w, &ex)
		for _, bad := range []string{`"lastChecked"`, `"lastError":"`, `"status":"pending"`} {
			if strings.Contains(string(ex.Sections["lists-and-rules"]), bad) {
				t.Fatalf("runtime member %s in the export: %s", bad, ex.Sections["lists-and-rules"])
			}
		}
		return ex.ContentSHA256
	}
	before := export()
	_, _ = e.eng.RefreshList(ctx, l.ID) // fails without network: a check that changes the runtime state
	lists, err := e.eng.Lists(ctx)
	if i := slices.IndexFunc(lists, func(x filter.List) bool { return x.ID == l.ID }); err != nil || i < 0 ||
		lists[i].LastChecked.IsZero() || lists[i].Status == "pending" {
		t.Fatalf("the refresh did not change the list's state: %+v %v", lists, err)
	}
	if after := export(); after != before {
		t.Fatal("a list check changed the export's content hash")
	}
	if n := e.audits(t, "system.export"); n != 1 {
		t.Fatalf("%d export audits", n)
	}
	// A configuration change does change it.
	if _, err := e.eng.UpdateList(ctx, l.ID, filter.ListInput{URL: l.URL, Enabled: true, Comment: "changed"}); err != nil {
		t.Fatal(err)
	}
	if export() == before {
		t.Fatal("a configuration change must change the hash")
	}
}

// While a section is synced its write routes answer every principal 409;
// settings routes only for the synced members, with the member as field.
func TestSyncedSectionsReadOnly(t *testing.T) {
	e := newK1Env(t)
	session := e.provisionAndLogin(t)
	adminTok := e.createToken(t, session, "admin")
	e.makeFollower(t, settings.ExportSections...)
	for _, cred := range []string{session, adminTok} {
		for _, p := range []string{"POST /api/v1/clients", "POST /api/v1/groups", "PUT /api/v1/groups/1/upstreams", "POST /api/v1/filter/lists",
			"POST /api/v1/filter/rules", "DELETE /api/v1/filter/ip-rules/3", "POST /api/v1/dns/records", "POST /api/v1/dns/forwarders/import",
			"PUT /api/v1/parental/groups/1", "POST /api/v1/dns/blocked-clients", "POST /api/v1/filter/rules/device"} {
			method, target, _ := strings.Cut(p, " ")
			w := e.do(method, target, `{}`, cred)
			coreWantError(t, w, http.StatusConflict, "conflict", "")
			if !strings.Contains(w.Body.String(), "this is synced from https://primary.lan:8443: change it on the primary") {
				t.Fatalf("%s: %s", p, w.Body)
			}
		}
	}
	coreWantError(t, e.do("PATCH", "/api/v1/settings/dns", `{"upstreams":["9.9.9.9"]}`, adminTok), http.StatusConflict, "conflict", "dns.upstreams")
	coreWantError(t, e.do("PATCH", "/api/v1/settings/filter", `{"blockingMode":"nxdomain"}`, session), http.StatusConflict, "conflict",
		"filter.blockingMode")
	for _, body := range []string{`{"allowedNetworks":["100.64.0.0/10"]}`, `{"plainDns":true}`} {
		if w := e.do("PATCH", "/api/v1/settings/dns", body, session); w.Code != http.StatusOK {
			t.Fatalf("a member that is not synced: %d %s", w.Code, w.Body)
		}
	}
	if w := e.do("PATCH", "/api/v1/settings/filter", `{"enabled":false}`, session); w.Code != http.StatusOK {
		t.Fatalf("filter.enabled: %d %s", w.Code, w.Body)
	}
	var st authStatusResponse
	coreDecode(t, e.do("GET", "/api/v1/auth/status", "", session), &st)
	if !slices.Equal(st.SyncedSections, settings.ExportSections) {
		t.Fatalf("syncedSections %v", st.SyncedSections)
	}
	coreDecode(t, e.do("GET", "/api/v1/auth/status", "", ""), &st)
	if len(st.SyncedSections) != 0 || st.SyncedSections == nil {
		t.Fatalf("unauthenticated: %v", st.SyncedSections)
	}
	// Seen data is no synced section.
	if w := e.do("DELETE", "/api/v1/clients/known?ip=192.168.1.9", "", session); w.Code != http.StatusOK {
		t.Fatalf("forget on a follower: %d %s", w.Code, w.Body)
	}
	// Only some sections: the others stay writable.
	e.makeFollower(t, "local-dns")
	if w := e.do("POST", "/api/v1/groups", `{"name":"Guests","enabled":true}`, session); w.Code != http.StatusCreated {
		t.Fatalf("an unsynced section: %d %s", w.Code, w.Body)
	}
	// A device rule writes a rule too: refused while lists and rules are synced.
	e.makeFollower(t, "lists-and-rules")
	w := e.do("POST", "/api/v1/filter/rules/device", `{}`, session)
	coreWantError(t, w, http.StatusConflict, "conflict", "")
	if !strings.Contains(w.Body.String(), "this is synced from https://primary.lan:8443") {
		t.Fatalf("device rule with lists-and-rules synced: %s", w.Body)
	}
}

// Every locked route is tagged with a sync section or listed here as not
// syncable.
func TestLockedRoutesSyncClassification(t *testing.T) {
	notSyncable := []string{
		"PUT /api/v1/download-cache/services/{id}/enabled", "PUT /api/v1/download-cache/services/{id}/domains",
		"POST /api/v1/download-cache/services", "PUT /api/v1/download-cache/services/{id}", "DELETE /api/v1/download-cache/services/{id}",
		"PUT /api/v1/download-cache/labels", "DELETE /api/v1/cache/objects/{id}", "POST /api/v1/cache/objects/{id}/pin",
		"POST /api/v1/cache/groups/pin", "POST /api/v1/cache/evict", "DELETE /api/v1/cache/noslice/{host}",
		"POST /api/v1/cache/groups/delete", "POST /api/v1/cache/services/{service}/purge",
		"DELETE /api/v1/dhcp/leases/{mac}", "DELETE /api/v1/dhcp/leases", "POST /api/v1/dhcp/static", "POST /api/v1/dhcp/static/import",
		"PUT /api/v1/dhcp/static/{mac}", "DELETE /api/v1/dhcp/static/{mac}", "POST /api/v1/dhcp/reset",
		"POST /api/v1/storage/targets", "PUT /api/v1/storage/targets/{id}", "POST /api/v1/storage/targets/{id}/apply",
		"POST /api/v1/storage/targets/{id}/activate", "POST /api/v1/storage/targets/{id}/init", "DELETE /api/v1/storage/targets/{id}",
		"POST /api/v1/notifications/channels", "PUT /api/v1/notifications/channels/{id}", "DELETE /api/v1/notifications/channels/{id}",
		"POST /api/v1/system/update/apply", "POST /api/v1/system/users", "PUT /api/v1/system/users/{id}", "DELETE /api/v1/system/users/{id}",
		"PUT /api/v1/system/tls", "DELETE /api/v1/system/tls", "POST /api/v1/system/tls/local-ca", "POST /api/v1/system/restore",
		"DELETE /api/v1/system/backups/scheduled/files/{name}", "DELETE /api/v1/logs/queries", "DELETE /api/v1/stats",
		"DELETE /api/v1/clients/known", "POST /api/v1/clients/known/flush", "PUT /api/v1/system/listeners",
	}
	e := newK1Env(t)
	for _, r := range e.srv.routes {
		if r.Lock != lockLocked {
			continue
		}
		tagged := r.SyncSection != ""
		listed := slices.Contains(notSyncable, r.Pattern)
		if tagged == listed {
			t.Errorf("%s: tagged %v (%q), listed as not syncable %v", r.Pattern, tagged, r.SyncSection, listed)
		}
		if tagged && !slices.Contains(settings.ExportSections, r.SyncSection) {
			t.Errorf("%s: unknown section %q", r.Pattern, r.SyncSection)
		}
	}
}

func TestSettingsDryRunAndNewSections(t *testing.T) {
	e := newK1Env(t)
	session := e.provisionAndLogin(t)
	before := e.audits(t, "settings.update")
	w := e.do("PATCH", "/api/v1/settings/logs?dryRun=true", `{"seenRetentionDays":8}`, session)
	var got settings.All
	coreDecode(t, w, &got)
	if w.Code != http.StatusOK || got.Logs.SeenRetentionDays != 8 || e.set.Get().Logs.SeenRetentionDays != 30 ||
		e.audits(t, "settings.update") != before {
		t.Fatalf("dry run: %d %+v stored %d", w.Code, got.Logs, e.set.Get().Logs.SeenRetentionDays)
	}
	coreWantError(t, e.do("PATCH", "/api/v1/settings/logs?dryRun=yes", `{}`, session), http.StatusBadRequest, "invalid", "dryRun")
	coreWantError(t, e.do("PUT", "/api/v1/settings?dryRun=true", `{"ntp":{"stratum":1}}`, session), http.StatusBadRequest, "invalid", "ntp.stratum")
	// A dry run seals nothing into the store.
	if w := e.do("PATCH", "/api/v1/settings/network?dryRun=true", `{"proxy":{"url":"http://proxy.lan:3128","password":"x"}}`, session); w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if e.set.Get().Network.Proxy.PasswordSet {
		t.Fatal("a dry run stored the password")
	}
	for body, field := range map[string]string{
		`{"stratum":16}`: "ntp.stratum",
	} {
		coreWantError(t, e.do("PATCH", "/api/v1/settings/ntp", body, session), http.StatusBadRequest, "invalid", field)
	}
	coreWantError(t, e.do("PATCH", "/api/v1/settings/sync", `{"source":"https://192.168.1.2:8443"}`, session), http.StatusBadRequest,
		"invalid", "sync.source")
	coreWantError(t, e.do("PATCH", "/api/v1/settings/network", `{"proxy":{"url":"http://127.0.0.1:8080"}}`, session), http.StatusBadRequest,
		"invalid", "network.proxy.url")
	if w := e.do("PATCH", "/api/v1/settings/clients", `{"nameSources":{"whois":true}}`, session); w.Code != http.StatusOK {
		t.Fatalf("clients: %d %s", w.Code, w.Body)
	}
	w = e.do("PATCH", "/api/v1/settings/nope", `{}`, session)
	coreWantError(t, w, http.StatusBadRequest, "invalid", "section")
	if !strings.Contains(w.Body.String(), "health, clients, sync, network, ntp)") {
		t.Fatalf("%s", w.Body)
	}
	// A secret in the audit details: only the member name.
	if w := e.do("PATCH", "/api/v1/settings/network", `{"proxy":{"url":"http://proxy.lan:3128","password":"hunter2hunter2"}}`, session); w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	var details string
	e.db.R.QueryRow(`SELECT details FROM auth_audit WHERE action = 'settings.update' ORDER BY id DESC LIMIT 1`).Scan(&details)
	if !strings.Contains(details, "network.proxy.password") || strings.Contains(details, "hunter2") {
		t.Fatalf("audit %s", details)
	}
	coreDecode(t, e.do("GET", "/api/v1/settings", "", session), &got)
	if !got.Network.Proxy.PasswordSet || got.Network.Proxy.Password != nil {
		t.Fatalf("GET /settings %+v", got.Network.Proxy)
	}
}

// A proxy on one of PiCache's own TCP listeners is refused with the role
// (dns, webTls), never the name of the bound listener (dns-tcp, web-tls);
// the UDP listeners (dns-udp, ntp) do not count for a proxy.
func TestProxyOwnListenerRoles(t *testing.T) {
	e := newK1Env(t)
	session := e.provisionAndLogin(t)
	e.rt.mu.Lock()
	e.rt.tlsAddr = "[::]:8443"
	e.rt.bound = map[string][]string{"dns-udp": {"[::]:53"}, "dns-tcp": {"[::]:53"}, "ntp": {"[::]:123"}}
	e.rt.mu.Unlock()
	for url, role := range map[string]string{"http://127.0.0.1:53": "dns", "http://127.0.0.1:8443": "webTls"} {
		w := e.do("PATCH", "/api/v1/settings/network", `{"proxy":{"url":"`+url+`"}}`, session)
		coreWantError(t, w, http.StatusBadRequest, "invalid", "network.proxy.url")
		if !strings.Contains(w.Body.String(), "this is PiCache's own "+role+" listener") {
			t.Errorf("%s: %s", url, w.Body)
		}
	}
	if w := e.do("PATCH", "/api/v1/settings/network", `{"proxy":{"url":"http://127.0.0.1:123"}}`, session); w.Code != http.StatusOK {
		t.Errorf("the port of a UDP listener: %d %s", w.Code, w.Body)
	}
}

func TestChannelRoutes(t *testing.T) {
	e := newK1Env(t)
	session := e.provisionAndLogin(t)
	patch := func(body string) *httptest.ResponseRecorder {
		return e.do("PATCH", "/api/v1/settings/updates", body, session)
	}
	if w := patch(`{"includePrereleases":true}`); w.Code != http.StatusOK || e.set.Get().Updates.Channel != settings.ChannelBeta {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if w := patch(`{"channel":"nightly"}`); w.Code != http.StatusOK || !e.set.Get().Updates.IncludePrereleases {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	coreWantError(t, patch(`{"channel":"beta","includePrereleases":false}`), http.StatusBadRequest, "invalid", "updates.channel")
	coreWantError(t, patch(`{"channel":"weekly"}`), http.StatusBadRequest, "invalid", "updates.channel")
	if w := patch(`{"channel":"stable"}`); w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	e.upd.overview = update.Overview{Mode: update.ModeDocker}
	for _, ch := range []string{"nightly", "NIGHTLY", " nightly "} {
		w := patch(`{"channel":"` + ch + `"}`)
		coreWantError(t, w, http.StatusBadRequest, "invalid", "updates.channel")
		if !strings.Contains(w.Body.String(), "nightly builds have no container image") {
			t.Fatalf("%q: %s", ch, w.Body)
		}
	}
	if c := e.set.Get().Updates.Channel; c != settings.ChannelStable {
		t.Fatalf("channel %q", c)
	}
	// The sync source and the proxy are checked as they will be stored.
	for _, body := range []string{`{"source":" https://192.168.1.2:8443"}`, `{"source":"HTTPS://192.168.1.2:8443/"}`} {
		w := e.do("PATCH", "/api/v1/settings/sync", body, session)
		coreWantError(t, w, http.StatusBadRequest, "invalid", "sync.source")
		if !strings.Contains(w.Body.String(), "this is the address of this PiCache") {
			t.Fatalf("%s: %s", body, w.Body)
		}
	}
}

// localRequest sends a request whose TCP connection arrived at local.
func (e *k1Env) localRequest(method, target, body, cred, local string) *httptest.ResponseRecorder {
	r := coreRequest(method, target, body)
	r.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: cred})
	if local != "" {
		r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, net.TCPAddrFromAddrPort(netip.MustParseAddrPort(local))))
	}
	return e.serve(r)
}

func TestListenersRoutes(t *testing.T) {
	e := newK1Env(t)
	session := e.provisionAndLogin(t)
	e.srv.d.Config.ListenerLock = map[string]string{config.RoleDoT: "PICACHE_DOT_LISTEN"}
	var cfg ListenersConfig
	coreDecode(t, e.do("GET", "/api/v1/system/listeners", "", session), &cfg)
	if !cfg.Editable || len(cfg.Roles) != len(config.ListenerRoles) || cfg.Roles[0].Role != "dns" || cfg.Roles[5].Role != "dot" ||
		!cfg.Roles[5].Locked || cfg.Roles[5].LockedBy != "PICACHE_DOT_LISTEN" || cfg.Roles[3].Saved != nil || cfg.Roles[3].Bound[0] != "127.0.0.1:8080" {
		t.Fatalf("GET %+v", cfg)
	}
	put := func(listeners, local string) *httptest.ResponseRecorder {
		return e.localRequest("PUT", "/api/v1/system/listeners", `{"listeners":`+listeners+`,"currentPassword":"`+corePassword+`"}`,
			session, local)
	}
	for _, tc := range []struct{ listeners, field, msg string }{
		{`{"web":["localhost:8080"]}`, "listeners.web[0]", "must be ip:port"},
		{`{"web":[":0"]}`, "listeners.web[0]", "port"},
		{`{"dot":[":8853"]}`, "listeners.dot", "set by PICACHE_DOT_LISTEN on the host"},
		{`{"web":["10.9.9.9:8080"]}`, "listeners.web[0]", "10.9.9.9 is not an address of this machine"},
		{`{"ntp":[":53"]}`, "listeners.ntp", "ntp uses port 53 like dns"},
		{`{"web":["192.168.1.2:80"]}`, "listeners.web", "web uses port 80 like cache"},
		{`{"dns":[]}`, "listeners.dns", "at least one address"},
		{`{"web":[],"webTls":[]}`, "listeners.web", "keep a web listener"},
		// Off only while the saved set itself gives webTls addresses.
		{`{"web":[]}`, "listeners.web", "keep a web listener"},
		{`{"web":["192.168.1.2:8080"],"webTls":["192.168.1.2:8443"]}`, "listeners.web", "all addresses or on loopback"},
	} {
		w := put(tc.listeners, "")
		coreWantError(t, w, http.StatusBadRequest, "invalid", tc.field)
		if !strings.Contains(w.Body.String(), tc.msg) {
			t.Fatalf("%s: %s", tc.listeners, w.Body)
		}
	}
	// The lock-out guard: the connection's local address must stay served.
	w := put(`{"web":["127.0.0.1:8080"],"webTls":["127.0.0.1:8443"]}`, "192.168.1.2:8080")
	coreWantError(t, w, http.StatusBadRequest, "invalid", "listeners.web")
	if !strings.Contains(w.Body.String(), "you are connected through 192.168.1.2") {
		t.Fatalf("%s", w.Body)
	}
	coreWantError(t, e.localRequest("PUT", "/api/v1/system/listeners", `{"listeners":{"ntp":[":123"]},"currentPassword":"wrong"}`, session, ""),
		http.StatusBadRequest, "invalid", "currentPassword")
	if w := put(`{"web":["127.0.0.1:8080","192.168.1.2:8080"],"ntp":["[::]:123"]}`, "192.168.1.2:8080"); w.Code != http.StatusOK {
		t.Fatalf("save: %d %s", w.Code, w.Body)
	}
	coreDecode(t, e.do("GET", "/api/v1/system/listeners", "", session), &cfg)
	if !cfg.RestartRequired || cfg.Roles[7].Saved == nil || (*cfg.Roles[7].Saved)[0] != "[::]:123" || e.audits(t, "system.listeners.update") != 1 ||
		cfg.Roles[3].CanDisable || !cfg.Roles[7].CanDisable {
		t.Fatalf("after save %+v", cfg)
	}
	if w := put(`{"web":[],"webTls":[":8443"]}`, ""); w.Code != http.StatusOK {
		t.Fatalf("web off with a saved webTls: %d %s", w.Code, w.Body)
	}
	coreDecode(t, e.do("GET", "/api/v1/system/listeners", "", session), &cfg)
	if !cfg.Roles[3].CanDisable {
		t.Fatalf("web with a saved webTls %+v", cfg.Roles[3])
	}
	// Docker: read-only.
	e.srv.d.Config.RunAs = "65532:65532"
	coreWantError(t, put(`{}`, ""), http.StatusConflict, "conflict", "")
	coreDecode(t, e.do("GET", "/api/v1/system/listeners", "", session), &cfg)
	if cfg.Editable || cfg.Reason != "docker" {
		t.Fatalf("docker %+v", cfg)
	}
}

func TestRestoreSectionsRoute(t *testing.T) {
	e := newK1Env(t)
	session := e.provisionAndLogin(t)
	restore := func(q string) *httptest.ResponseRecorder {
		r := coreRequest("POST", "/api/v1/system/restore"+q, "")
		r.Body = io.NopCloser(strings.NewReader("backup"))
		r.Header.Set("Content-Type", "application/octet-stream")
		r.Header.Set(restorePasswordHeader, corePassword)
		r.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: session})
		return e.serve(r)
	}
	for q, msg := range map[string]string{
		"?sections=foo":                "unknown section foo",
		"?sections=":                   "choose at least one section",
		"?sections=clients-and-groups": "also restore lists-and-rules, local-dns and parental",
		"?sections=dns-settings":       "dns-settings cannot be restored",
	} {
		w := restore(q)
		coreWantError(t, w, http.StatusBadRequest, "invalid", "sections")
		if !strings.Contains(w.Body.String(), msg) {
			t.Fatalf("%s: %s", q, w.Body)
		}
	}
	w := restore("?sections=local-dns,parental")
	var out struct {
		Sections         []string `json:"sections"`
		WebAccessWarning string   `json:"webAccessWarning"`
	}
	coreDecode(t, w, &out)
	if w.Code != http.StatusAccepted || !slices.Equal(out.Sections, []string{"local-dns", "parental"}) ||
		!slices.Equal(e.rt.restoreSections, []string{"local-dns", "parental"}) {
		t.Fatalf("%d %s %v", w.Code, w.Body, e.rt.restoreSections)
	}
	w = restore("")
	coreDecode(t, w, &out)
	if !slices.Equal(out.Sections, settings.RestoreSections) || e.rt.restoreSections != nil {
		t.Fatalf("full restore: %s", w.Body)
	}
}

func TestPProfRoutes(t *testing.T) {
	e := newK1Env(t)
	session := e.provisionAndLogin(t)
	adminTok := e.createToken(t, session, "admin")
	readTok := e.createToken(t, session, "read")
	get := func(target, cred, remote string) *httptest.ResponseRecorder {
		r := coreRequest("GET", target, "")
		r.RemoteAddr = remote
		switch {
		case strings.HasPrefix(cred, "pc_"):
			r.Header.Set("Authorization", "Bearer "+cred)
		case cred != "":
			r.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: cred})
		}
		return e.serve(r)
	}
	// Off: 404 for everyone (never the UI).
	for _, cred := range []string{"", session, adminTok} {
		if w := get("/debug/pprof/heap", cred, "127.0.0.1:5000"); w.Code != http.StatusNotFound {
			t.Fatalf("off: %d", w.Code)
		}
	}
	e.srv.d.Config.PProf = true
	// From another client: 404, also before the sign-in.
	for _, cred := range []string{"", adminTok} {
		if w := get("/debug/pprof/heap", cred, "192.0.2.7:5000"); w.Code != http.StatusNotFound {
			t.Fatalf("remote: %d", w.Code)
		}
	}
	if w := get("/debug/pprof/heap", "", "127.0.0.1:5000"); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated local: %d", w.Code)
	}
	if w := get("/debug/pprof/heap", readTok, "127.0.0.1:5000"); w.Code != http.StatusForbidden {
		t.Fatalf("read token: %d", w.Code)
	}
	for _, p := range []string{"/debug/pprof/", "/debug/pprof/heap", "/debug/pprof/allocs", "/debug/pprof/goroutine"} {
		for _, remote := range []string{"127.0.0.1:5000", "192.168.1.2:5000"} {
			if w := get(p, adminTok, remote); w.Code != http.StatusOK || w.Body.Len() == 0 {
				t.Fatalf("%s from %s: %d", p, remote, w.Code)
			}
		}
	}
	for _, p := range []string{"/debug/pprof/cmdline", "/debug/pprof/trace", "/debug/pprof/symbol", "/debug/vars"} {
		if w := get(p, adminTok, "127.0.0.1:5000"); w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), "<html") {
			t.Fatalf("%s: %d", p, w.Code)
		}
	}
	coreWantError(t, get("/debug/pprof/profile?seconds=31", session, "127.0.0.1:5000"), http.StatusBadRequest, "invalid", "seconds")
	e.srv.profiling.Store(true)
	coreWantError(t, get("/debug/pprof/heap", session, "127.0.0.1:5000"), http.StatusTooManyRequests, "too_many_requests", "")
	e.srv.profiling.Store(false)
	if w := get("/debug/pprof/profile?seconds=1", session, "::1"); w.Code != http.StatusOK || w.Body.Len() == 0 {
		t.Fatalf("CPU profile: %d", w.Code)
	}
	if e.audits(t, "system.pprof") != 7 { // 3 profiles × 2 clients and the CPU profile
		t.Fatalf("%d pprof audits", e.audits(t, "system.pprof"))
	}
}

// No server uses http.DefaultServeMux or a nil Handler, and net/http/pprof
// is never imported (its init registers on the default mux).
func TestNoDefaultServeMux(t *testing.T) {
	root := filepath.Join("..", "..")
	server := regexp.MustCompile(`http\.Server\{`)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "web" || d.Name() == "node_modules" || strings.HasPrefix(d.Name(), ".")) && path != root {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		src := string(b)
		for _, bad := range []string{`"net/http/pprof"`, "http.DefaultServeMux", "http.HandleFunc(", "http.Handle(", "http.ListenAndServe("} {
			if strings.Contains(src, bad) {
				t.Errorf("%s uses %s", path, bad)
			}
		}
		for _, loc := range server.FindAllStringIndex(src, -1) {
			end := strings.Index(src[loc[1]:], "\n\t}")
			lit := src[loc[1]:]
			if end >= 0 {
				lit = src[loc[1] : loc[1]+end]
			}
			if !strings.Contains(lit, "Handler:") && !strings.Contains(src[max(0, loc[0]-200):loc[0]], ".Handler =") {
				t.Errorf("%s: an http.Server without a Handler", path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestKnownForgetRoutes(t *testing.T) {
	e := newK1Env(t)
	session := e.provisionAndLogin(t)
	e.reg.Seen(netip.MustParseAddr("192.168.1.9"))
	for q, field := range map[string]string{"": "ip", "?ip=1.2.3.4&mac=02:00:00:00:00:01": "ip", "?ip=x": "ip", "?mac=nope": "mac",
		"?ip=fe80::1%25eth0": "ip"} {
		coreWantError(t, e.do("DELETE", "/api/v1/clients/known"+q, "", session), http.StatusBadRequest, "invalid", field)
	}
	var out struct {
		Deleted int `json:"deleted"`
	}
	coreDecode(t, e.do("DELETE", "/api/v1/clients/known?ip=192.168.1.9", "", session), &out)
	if out.Deleted != 1 || e.audits(t, "clients.known.forget") != 1 {
		t.Fatalf("forget %+v", out)
	}
	coreDecode(t, e.do("DELETE", "/api/v1/clients/known?mac=02-00-00-00-00-99", "", session), &out)
	if out.Deleted != 0 {
		t.Fatalf("unknown mac %+v", out)
	}
	e.reg.Seen(netip.MustParseAddr("192.168.1.10"))
	coreDecode(t, e.do("POST", "/api/v1/clients/known/flush", "", session), &out)
	if out.Deleted != 1 || e.audits(t, "clients.known.flush") != 1 {
		t.Fatalf("flush %+v", out)
	}
	e.srv.d.Config.DestructiveAPI = false
	coreWantError(t, e.do("POST", "/api/v1/clients/known/flush", "", session), http.StatusForbidden, "forbidden", "")
}

func TestSyncRoutes(t *testing.T) {
	e := newK1Env(t)
	session := e.provisionAndLogin(t)
	readTok := e.createToken(t, session, "read")
	var st SyncStatus
	coreDecode(t, e.do("GET", "/api/v1/system/sync", "", readTok), &st)
	if st.Mode != "off" || st.Sections == nil {
		t.Fatalf("%+v", st)
	}
	coreWantError(t, e.do("POST", "/api/v1/system/sync/run", "", readTok), http.StatusForbidden, "forbidden", "")
	if w := e.do("POST", "/api/v1/system/sync/run", "", session); w.Code != http.StatusAccepted || e.sync.runs != 1 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

func TestNetworkInterfacesRoute(t *testing.T) {
	ce := newCoreEnv(t)
	session := ce.provisionAndLogin(t)
	coreWantError(t, ce.do("GET", "/api/v1/network/interfaces", "", session), http.StatusServiceUnavailable, "unavailable", "")
	ce.srv.d.Network = &fakeNetwork{}
	w := ce.do("GET", "/api/v1/network/interfaces", "", session)
	var v map[string]jsontext.Value
	coreDecode(t, w, &v)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"speedMbps":1000`) || !strings.Contains(w.Body.String(), `"defaultGateways":[{"family":"ipv4"`) {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	_ = time.Now
}
