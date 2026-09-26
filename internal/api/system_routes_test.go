package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/auth"
	"github.com/hustenreizjuengling/picache/internal/dhcp"
	"github.com/hustenreizjuengling/picache/internal/dns/upstream"
	"github.com/hustenreizjuengling/picache/internal/settings"
	"github.com/hustenreizjuengling/picache/internal/update"
	"github.com/hustenreizjuengling/picache/internal/version"
)

func TestSystemInfoAndHealth(t *testing.T) {
	e := newCoreEnv(t)
	session := e.provisionAndLogin(t)
	readTok := e.createToken(t, session, "read")

	var info systemInfoResponse
	w := e.do("GET", "/api/v1/system/info", "", readTok)
	coreDecode(t, w, &info)
	if w.Code != http.StatusOK || info.InstanceID != "0123456789abcdef" || info.UptimeSec < 3599 ||
		info.Version.GoVersion == "" || info.Memory.SysBytes == 0 || info.Goroutines == 0 ||
		len(info.Listeners.Bound["web"]) != 1 || info.MasterKeySource != "memory" {
		t.Fatalf("info: %d %+v", w.Code, info)
	}
	var h Health
	coreDecode(t, e.do("GET", "/api/v1/system/health", "", readTok), &h)
	if !h.OK || len(h.Checks) != 2 {
		t.Fatalf("health = %+v", h)
	}
	coreWantError(t, e.do("GET", "/api/v1/system/info", "", ""), http.StatusUnauthorized, "unauthorized", "")
}

func TestSystemBackup(t *testing.T) {
	e := newCoreEnv(t)
	session := e.provisionAndLogin(t)
	readTok := e.createToken(t, session, "read")
	coreWantError(t, e.do("GET", "/api/v1/system/backup", "", readTok), http.StatusForbidden, "forbidden", "")

	e.rt.backup = []byte("SQLite format 3\x00 backup bytes")
	w := e.do("GET", "/api/v1/system/backup?includeSecrets=true", "", session)
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), e.rt.backup) {
		t.Fatalf("backup: %d %q", w.Code, w.Body)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("content type %q", ct)
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment; filename=picache-backup-") || !strings.HasSuffix(cd, ".db") {
		t.Fatalf("content disposition %q", cd)
	}
	entries, _, err := e.auth.AuditLog(t.Context(), auth.AuditQuery{Search: "system.backup"})
	if err != nil || len(entries) != 1 || entries[0].Details != `{"includeSecrets":true}` {
		t.Fatalf("backup audit = %+v, %v", entries, err)
	}

	// An error before the first byte is still a JSON error, not a download.
	e.rt.backupErr = errCoreBackup
	w = e.do("GET", "/api/v1/system/backup", "", session)
	coreWantError(t, w, http.StatusInternalServerError, "internal", "")
	if w.Header().Get("Content-Disposition") != "" {
		t.Fatal("failed backup must not be offered as a download")
	}
}

func TestSystemRestoreAndRestart(t *testing.T) {
	e := newCoreEnv(t)
	session := e.provisionAndLogin(t)

	w := e.do("POST", "/api/v1/system/restore", `{"not":"a backup"}`, session)
	coreWantError(t, w, http.StatusBadRequest, "invalid", "body")

	w = e.serve(restoreRequest("backup-bytes", session, corePassword))
	if w.Code != http.StatusAccepted || string(e.rt.restored) != "backup-bytes" {
		t.Fatalf("restore: %d %s (restored %q)", w.Code, w.Body, e.rt.restored)
	}
	var out struct {
		Staged  bool   `json:"staged"`
		Message string `json:"message"`
	}
	coreDecode(t, w, &out)
	if !out.Staged || out.Message == "" {
		t.Fatalf("restore response %+v", out)
	}

	w = e.do("POST", "/api/v1/system/restart", "", session)
	if w.Code != http.StatusAccepted || !e.rt.restarted {
		t.Fatalf("restart: %d, restarted %v", w.Code, e.rt.restarted)
	}
	actions := e.auditActions(t)
	if !slices.Contains(actions, "system.restore") || !slices.Contains(actions, "system.restart") {
		t.Fatalf("audit %v", actions)
	}
}

// restoreRequest builds POST /system/restore with a session cookie or an API
// token (cred) and, unless password is "-", the X-PiCache-Password header
// (percent-encoded like the web UI does).
func restoreRequest(body, cred, password string) *http.Request {
	r := httptest.NewRequest("POST", "/api/v1/system/restore", strings.NewReader(body))
	r.Host = coreHost
	r.Header.Set("Content-Type", "application/octet-stream")
	if strings.HasPrefix(cred, "pc_") {
		r.Header.Set("Authorization", "Bearer "+cred)
	} else {
		r.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: cred})
	}
	if password != "-" {
		r.Header.Set("X-PiCache-Password", url.PathEscape(password))
	}
	return r
}

// SEC-01: a restore replaces the whole configuration, so it needs an
// interactive session (never an API token) and the current password again.
func TestSystemRestoreRequiresSessionAndPassword(t *testing.T) {
	e := newCoreEnv(t)
	session := e.provisionAndLogin(t)
	adminTok := e.createToken(t, session, "admin")

	coreWantError(t, e.serve(restoreRequest("x", adminTok, corePassword)), http.StatusForbidden, "forbidden", "")
	coreWantError(t, e.serve(restoreRequest("x", session, "-")), http.StatusUnauthorized, "unauthorized", "password")
	coreWantError(t, e.serve(restoreRequest("x", session, "wrong password")), http.StatusUnauthorized, "unauthorized", "password")
	r := restoreRequest("x", session, "-")
	r.Header.Set("X-PiCache-Password", "%zz") // malformed encoding
	coreWantError(t, e.serve(r), http.StatusUnauthorized, "unauthorized", "password")
	if e.rt.restored != nil {
		t.Fatalf("nothing may be staged without the password (got %q)", e.rt.restored)
	}
	// The session survives a wrong confirmation (the UI must not sign out).
	if w := e.do("GET", "/api/v1/auth/me", "", session); w.Code != http.StatusOK {
		t.Fatalf("session after a wrong confirmation: %d", w.Code)
	}
	// Backups and restarts stay available to admin tokens (automation).
	e.rt.backup = []byte("SQLite format 3\x00")
	if w := e.do("GET", "/api/v1/system/backup", "", adminTok); w.Code != http.StatusOK {
		t.Fatalf("backup with an admin token: %d", w.Code)
	}

	// Non-ASCII passwords are sent percent-encoded.
	const unicodePassword = "pässwörter sind lang"
	w := e.do("POST", "/api/v1/auth/password", `{"currentPassword":"`+corePassword+`","newPassword":"`+unicodePassword+`"}`, session)
	if w.Code != http.StatusNoContent {
		t.Fatalf("change password: %d %s", w.Code, w.Body)
	}
	if w := e.serve(restoreRequest("backup-bytes", session, unicodePassword)); w.Code != http.StatusAccepted {
		t.Fatalf("restore with a percent-encoded password: %d %s", w.Code, w.Body)
	}
	if string(e.rt.restored) != "backup-bytes" {
		t.Fatalf("restored %q", e.rt.restored)
	}
}

func TestSystemAuditListing(t *testing.T) {
	e := newCoreEnv(t)
	session := e.provisionAndLogin(t)
	var page struct {
		Items []struct {
			Action string `json:"action"`
		} `json:"items"`
		Total int `json:"total"`
	}
	coreDecode(t, e.do("GET", "/api/v1/system/audit?search=auth.login&limit=1", "", session), &page)
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].Action != "auth.login" {
		t.Fatalf("audit page = %+v", page)
	}
	coreWantError(t, e.do("GET", "/api/v1/system/audit?limit=x", "", session), http.StatusBadRequest, "invalid", "limit")
}

func TestMetricsEndpointAccess(t *testing.T) {
	e := newCoreEnv(t)
	session := e.provisionAndLogin(t)
	readTok := e.createToken(t, session, "read")

	coreWantError(t, e.do("GET", "/metrics", "", ""), http.StatusNotFound, "not_found", "")
	if _, err := e.set.Update(t.Context(), func(a *settings.All) error { a.Web.MetricsEnabled = true; return nil }); err != nil {
		t.Fatal(err)
	}
	w := e.do("GET", "/metrics", "", "")
	coreWantError(t, w, http.StatusUnauthorized, "unauthorized", "")
	if !strings.HasPrefix(w.Header().Get("WWW-Authenticate"), "Bearer") {
		t.Fatal("401 must carry WWW-Authenticate: Bearer")
	}
	coreWantError(t, e.do("GET", "/metrics", "", session), http.StatusForbidden, "forbidden", "")
	coreWantError(t, e.do("GET", "/metrics", "", readTok), http.StatusForbidden, "forbidden", "")
}

func TestWriteMetrics(t *testing.T) {
	var buf bytes.Buffer
	writeMetrics(&buf, metricsSnapshot{
		Version: `v1.0 "x"`, DNSQueries: 42, DNSRateLimited: 3, DNSBlocked: 4, DNSDropped: 6, CacheBytesHit: 1 << 30, CacheBytesWAN: 5,
		StoreOnline: true, StoreBytes: 1000, StoreFreeBytes: 2000,
		Upstreams:  []upstream.UpstreamStat{{Upstream: "https://dns.quad9.net/dns-query", AvgRTTMs: 12.5}, {Upstream: "a\\b\nc"}},
		LogDropped: 7,
	})
	want := []string{
		`picache_build_info{version="v1.0 \"x\""} 1`,
		"# TYPE picache_dns_queries_total counter",
		"picache_dns_queries_total 42",
		"picache_dns_rate_limited_total 3",
		"# TYPE picache_dns_blocked_clients_total counter",
		"picache_dns_blocked_clients_total 4",
		"# TYPE picache_dns_dropped_total counter",
		"picache_dns_dropped_total 6",
		"picache_cache_bytes_hit_total 1073741824",
		"picache_cache_bytes_wan_total 5",
		"# TYPE picache_cache_store_bytes gauge",
		"picache_cache_store_bytes 1000",
		"picache_cache_store_free_bytes 2000",
		`picache_upstream_rtt_ms{upstream="https://dns.quad9.net/dns-query"} 12.5`,
		`picache_upstream_rtt_ms{upstream="a\\b\nc"} 0`,
		"picache_log_events_dropped_total 7",
	}
	lines := strings.Split(buf.String(), "\n")
	for _, w := range want {
		if !slices.Contains(lines, w) {
			t.Errorf("missing line %q in\n%s", w, buf.String())
		}
	}
	for _, l := range lines {
		if l != "" && !strings.HasPrefix(l, "# ") && !strings.HasPrefix(l, "picache_") {
			t.Errorf("malformed line %q", l)
		}
	}

	buf.Reset()
	writeMetrics(&buf, metricsSnapshot{Version: "dev"})
	if strings.Contains(buf.String(), "picache_cache_store_bytes") || strings.Contains(buf.String(), "picache_upstream_rtt_ms") {
		t.Fatalf("offline store and missing upstreams must be omitted:\n%s", buf.String())
	}
}

// The DNS cache and DHCP families (golden excerpt): the DHCP families only
// while a DHCP service exists.
func TestWriteMetricsCacheAndDHCP(t *testing.T) {
	var buf bytes.Buffer
	writeMetrics(&buf, metricsSnapshot{Version: "dev",
		DNSCache: upstream.CacheStat{Entries: 12, Hits: 100, Misses: 20, StaleHits: 3, Insertions: 25, Evictions: 4, Expired: 9},
		DHCP: &dhcpMetrics{Counters: dhcp.Counters{Received: 50, Offers: 10, Acks: 9, Naks: 1, Declines: 2, Releases: 3, Informs: 4, Dropped: 5},
			ActiveLeases: 7}})
	want := `# HELP picache_dns_cache_entries Answers in the DNS response cache.
# TYPE picache_dns_cache_entries gauge
picache_dns_cache_entries 12
# HELP picache_dns_cache_hits_total DNS answers served from the response cache (stale ones included) since start.
# TYPE picache_dns_cache_hits_total counter
picache_dns_cache_hits_total 100
# HELP picache_dns_cache_misses_total DNS response cache lookups without a usable answer since start.
# TYPE picache_dns_cache_misses_total counter
picache_dns_cache_misses_total 20
# HELP picache_dns_cache_stale_hits_total Stale DNS answers served from the response cache since start.
# TYPE picache_dns_cache_stale_hits_total counter
picache_dns_cache_stale_hits_total 3
# HELP picache_dns_cache_insertions_total Answers stored in the DNS response cache since start.
# TYPE picache_dns_cache_insertions_total counter
picache_dns_cache_insertions_total 25
# HELP picache_dns_cache_evictions_total DNS response cache entries removed for the capacity since start.
# TYPE picache_dns_cache_evictions_total counter
picache_dns_cache_evictions_total 4
# HELP picache_dns_cache_expired_total DNS response cache entries removed after their TTL and the serve-stale window since start.
# TYPE picache_dns_cache_expired_total counter
picache_dns_cache_expired_total 9
`
	if !strings.Contains(buf.String(), want) {
		t.Fatalf("cache families:\n%s", buf.String())
	}
	for _, line := range []string{"picache_dhcp_received_total 50", "picache_dhcp_offers_total 10", "picache_dhcp_acks_total 9",
		"picache_dhcp_naks_total 1", "picache_dhcp_declines_total 2", "picache_dhcp_releases_total 3", "picache_dhcp_informs_total 4",
		"picache_dhcp_dropped_total 5", "# TYPE picache_dhcp_leases_active gauge", "picache_dhcp_leases_active 7"} {
		if !slices.Contains(strings.Split(buf.String(), "\n"), line) {
			t.Errorf("missing %q", line)
		}
	}
	buf.Reset()
	writeMetrics(&buf, metricsSnapshot{Version: "dev"})
	if strings.Contains(buf.String(), "picache_dhcp_") || !strings.Contains(buf.String(), "picache_dns_cache_entries 0") {
		t.Fatalf("without DHCP:\n%s", buf.String())
	}
}

func TestSystemUpdateOverviewAndCheck(t *testing.T) {
	e := newCoreEnv(t)
	session := e.provisionAndLogin(t)
	readTok := e.createToken(t, session, "read")
	adminTok := e.createToken(t, session, "admin")
	e.upd.overview = update.NewOverview("v0.9.0", update.ModeHelper, true, false,
		update.CheckResult{Latest: &update.Release{Version: "v0.9.1", URL: "https://github.com/x", Notes: "n"}}, nil)

	w := e.do("GET", "/api/v1/system/update", "", readTok)
	var got map[string]any
	coreDecode(t, w, &got)
	if w.Code != http.StatusOK || got["mode"] != "helper" || got["updateAvailable"] != true || got["currentIsDevBuild"] != false ||
		got["latest"].(map[string]any)["version"] != "v0.9.1" || got["commands"].(map[string]any)["cli"] != "sudo picache update --version v0.9.1" {
		t.Fatalf("overview: %d %s", w.Code, w.Body)
	}
	for _, member := range []string{"checkedAt", "checkError", "status"} {
		if _, ok := got[member]; ok {
			t.Errorf("empty member %q is sent", member)
		}
	}
	coreWantError(t, e.do("GET", "/api/v1/system/update", "", ""), http.StatusUnauthorized, "unauthorized", "")

	// Checking now is an admin action (API tokens included, for automation).
	coreWantError(t, e.do("POST", "/api/v1/system/update/check", "", readTok), http.StatusForbidden, "forbidden", "")
	if w := e.do("POST", "/api/v1/system/update/check", "", adminTok); w.Code != http.StatusOK || e.upd.checks != 1 {
		t.Fatalf("check: %d %s (checks %d)", w.Code, w.Body, e.upd.checks)
	}
}

// Installing needs an interactive session and the current password (like
// a restore); the version must be the available update (409 otherwise).
func TestSystemUpdateApply(t *testing.T) {
	e := newCoreEnv(t)
	session := e.provisionAndLogin(t)
	adminTok := e.createToken(t, session, "admin")
	body := func(version, pw string) string {
		return `{"version":"` + version + `","currentPassword":"` + pw + `"}`
	}

	coreWantError(t, e.do("POST", "/api/v1/system/update/apply", body("v0.9.1", corePassword), adminTok), http.StatusForbidden, "forbidden", "")
	coreWantError(t, e.do("POST", "/api/v1/system/update/apply", `{"version":"v0.9.1"}`, session), http.StatusBadRequest, "invalid", "currentPassword")
	coreWantError(t, e.do("POST", "/api/v1/system/update/apply", body("v0.9.1", "wrong password"), session), http.StatusBadRequest, "invalid", "currentPassword")
	coreWantError(t, e.do("POST", "/api/v1/system/update/apply", `{"version":"v0.9.1","currentPassword":"x","url":"https://evil"}`, session),
		http.StatusBadRequest, "invalid", "body")
	if len(e.upd.queued) != 0 {
		t.Fatalf("queued without the password: %v", e.upd.queued)
	}
	if !slices.Contains(e.auditActions(t), "auth.login_failed") {
		t.Error("a wrong password confirmation is not audited")
	}
	// The session survives a wrong confirmation.
	if w := e.do("GET", "/api/v1/auth/me", "", session); w.Code != http.StatusOK {
		t.Fatalf("session after a wrong confirmation: %d", w.Code)
	}

	for _, msg := range []string{"an update is already running", "the update helper is not installed", "the requested version is not the available update"} {
		e.upd.queueErr = apperr.Conflict("%s", msg)
		w := e.do("POST", "/api/v1/system/update/apply", body("v0.9.1", corePassword), session)
		coreWantError(t, w, http.StatusConflict, "conflict", "")
		if !strings.Contains(w.Body.String(), msg) {
			t.Errorf("409 body %s", w.Body)
		}
	}
	e.upd.queueErr = nil

	w := e.do("POST", "/api/v1/system/update/apply", body("v0.9.1", corePassword), session)
	var out map[string]bool
	coreDecode(t, w, &out)
	if w.Code != http.StatusAccepted || !out["queued"] || !slices.Equal(e.upd.queued, []string{"v0.9.1 by admin"}) {
		t.Fatalf("apply: %d %s, queued %v", w.Code, w.Body, e.upd.queued)
	}
	entries, _, err := e.auth.AuditLog(t.Context(), auth.AuditQuery{Search: "system.update_queued"})
	if err != nil || len(entries) != 1 || entries[0].Target != "v0.9.1" || entries[0].Details != `{"from":"`+version.Version+`"}` {
		t.Fatalf("audit = %+v, %v", entries, err)
	}
	if strings.Contains(entries[0].Details, corePassword) {
		t.Fatal("the password reached the audit log")
	}
}

func TestSystemUpdateWithoutUpdater(t *testing.T) {
	e := newCoreEnv(t)
	session := e.provisionAndLogin(t)
	e.srv.d.Updates = nil
	coreWantError(t, e.do("GET", "/api/v1/system/update", "", session), http.StatusServiceUnavailable, "unavailable", "")
	coreWantError(t, e.do("POST", "/api/v1/system/update/check", "", session), http.StatusServiceUnavailable, "unavailable", "")
}
