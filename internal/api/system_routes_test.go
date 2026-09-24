package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/hustenreizjuengling/picache/internal/auth"
	"github.com/hustenreizjuengling/picache/internal/dns/upstream"
	"github.com/hustenreizjuengling/picache/internal/settings"
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
		Version: `v1.0 "x"`, DNSQueries: 42, DNSRateLimited: 3, CacheBytesHit: 1 << 30, CacheBytesWAN: 5,
		StoreOnline: true, StoreBytes: 1000, StoreFreeBytes: 2000,
		Upstreams:  []upstream.UpstreamStat{{Upstream: "https://dns.quad9.net/dns-query", AvgRTTMs: 12.5}, {Upstream: "a\\b\nc"}},
		LogDropped: 7,
	})
	want := []string{
		`picache_build_info{version="v1.0 \"x\""} 1`,
		"# TYPE picache_dns_queries_total counter",
		"picache_dns_queries_total 42",
		"picache_dns_rate_limited_total 3",
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
