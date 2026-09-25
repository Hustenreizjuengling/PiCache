package api

import (
	"encoding/json/v2"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/hustenreizjuengling/picache/internal/auth"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

func TestSettingsRoutes(t *testing.T) {
	e := newCoreEnv(t)
	session := e.provisionAndLogin(t)
	readTok := e.createToken(t, session, "read")

	var got settings.All
	w := e.do("GET", "/api/v1/settings", "", readTok)
	coreDecode(t, w, &got)
	if w.Code != http.StatusOK || got.Cache.ActiveStoreID != "local" {
		t.Fatalf("get: %d %+v", w.Code, got.Cache)
	}
	var defs settings.All
	coreDecode(t, e.do("GET", "/api/v1/settings/defaults", "", readTok), &defs)
	if defs.Web.SessionIdleMinutes != 60 {
		t.Fatalf("defaults = %+v", defs.Web)
	}

	// PUT the full document with one change.
	got.Web.Language = "de"
	got.DNS.RateLimitQPS = 77
	body, _ := json.Marshal(got)
	coreWantError(t, e.do("PUT", "/api/v1/settings", string(body), readTok), http.StatusForbidden, "forbidden", "")
	w = e.do("PUT", "/api/v1/settings", string(body), session)
	if w.Code != http.StatusOK || e.set.Get().Web.Language != "de" || e.set.Get().DNS.RateLimitQPS != 77 {
		t.Fatalf("put: %d %s", w.Code, w.Body)
	}
	entries, _, err := e.auth.AuditLog(t.Context(), auth.AuditQuery{Search: "settings.update"})
	if err != nil || len(entries) != 1 || entries[0].Details != `{"changed":["dns.rateLimitQps","web.language"]}` {
		t.Fatalf("settings audit = %+v, %v", entries, err)
	}

	// The active store cannot be switched here.
	got.Cache.ActiveStoreID = "0123456789abcdef0123456789abcdef"
	body, _ = json.Marshal(got)
	coreWantError(t, e.do("PUT", "/api/v1/settings", string(body), session), http.StatusBadRequest, "invalid", "cache.activeStoreId")
	coreWantError(t, e.do("PATCH", "/api/v1/settings/cache", `{"activeStoreId":"other"}`, session),
		http.StatusBadRequest, "invalid", "cache.activeStoreId")
	if e.set.Get().Cache.ActiveStoreID != "local" {
		t.Fatal("active store changed")
	}

	for _, tc := range []struct {
		name, section, body string
		status              int
		field               string
	}{
		{"unknown section", "nope", `{}`, http.StatusBadRequest, "section"},
		{"unknown member", "web", `{"bogus":1}`, http.StatusBadRequest, "body"},
		{"validation error", "web", `{"sessionIdleMinutes":1}`, http.StatusBadRequest, "web.sessionIdleMinutes"},
		{"wrong type", "logs", `{"queryLogEnabled":"yes"}`, http.StatusBadRequest, "body"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			coreWantError(t, e.do("PATCH", "/api/v1/settings/"+tc.section, tc.body, session), tc.status, "invalid", tc.field)
		})
	}

	// PATCH keeps omitted members and replaces arrays.
	w = e.do("PATCH", "/api/v1/settings/web", `{"allowedHosts":["nas.lan"],"metricsEnabled":true}`, session)
	if w.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", w.Code, w.Body)
	}
	web := e.set.Get().Web
	if !web.MetricsEnabled || !slices.Equal(web.AllowedHosts, []string{"nas.lan"}) || web.Language != "de" {
		t.Fatalf("web after patch = %+v", web)
	}
	// The new allowed host takes effect immediately.
	r := coreRequest("GET", "/api/v1/auth/status", "")
	r.Host = "nas.lan:8080"
	if w := e.serve(r); w.Code != http.StatusOK {
		t.Fatalf("allowed host after patch: %d", w.Code)
	}
	if !strings.Contains(e.do("PATCH", "/api/v1/settings/cache", `{"maxAgeDays":30}`, session).Body.String(), `"maxAgeDays":30`) {
		t.Fatal("patch cache section without activeStoreId must succeed")
	}
	// The updates section (release checks).
	w = e.do("PATCH", "/api/v1/settings/updates", `{"includePrereleases":true}`, session)
	if u := e.set.Get().Updates; w.Code != http.StatusOK || !u.CheckEnabled || !u.IncludePrereleases {
		t.Fatalf("patch updates: %d %+v", w.Code, u)
	}
	coreWantError(t, e.do("PATCH", "/api/v1/settings/updates", `{"checkEnabled":"no"}`, session), http.StatusBadRequest, "invalid", "body")
}

func TestChangedSettings(t *testing.T) {
	a := settings.Defaults()
	b := a.Clone()
	if got := changedSettings(&a, b); len(got) != 0 {
		t.Fatalf("identical documents: %v", got)
	}
	b.DNS.Upstreams = append([]string{"9.9.9.9"}, b.DNS.Upstreams...)
	b.Filter.BlockedTTL = 1
	if got := changedSettings(&a, b); !slices.Equal(got, []string{"dns.upstreams", "filter.blockedTtl"}) {
		t.Fatalf("changed = %v", got)
	}
}
