package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/settings"
	"github.com/hustenreizjuengling/picache/internal/update"
)

// GET /network/check describes the effective client of every request:
// the TCP peer, or behind a trusted proxy the address it forwarded (the
// app computes the requester for every call, not from its cached check).
func TestNetworkCheckRequesterRoute(t *testing.T) {
	e := newStorageTestEnv(t)
	e.srv.d.Network = &fakeNetwork{}
	e.srv.d.Config.WebListen = []string{":8080"}
	ce := &coreEnv{srv: New(e.srv.d), auth: e.auth, set: e.srv.d.Settings}
	session := ce.provisionAndLogin(t)
	readTok := ce.createToken(t, session, "read")
	ce.web(t, func(w *settings.Web) { w.TrustedProxies = []string{testProxy} })

	w := ce.req("GET", "/api/v1/network/check", "", readTok, from("192.168.1.20:40000"))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"requester":{"address":"192.168.1.20","local":false}`) {
		t.Fatalf("peer: %d %s", w.Code, w.Body)
	}
	w = ce.req("GET", "/api/v1/network/check", "", readTok, from(testProxy+":40000"), header("X-Forwarded-For", "192.168.1.30"))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"requester":{"address":"192.168.1.30","local":false}`) {
		t.Fatalf("trusted proxy: %d %s", w.Code, w.Body)
	}
	w = ce.req("GET", "/api/v1/network/check", "", readTok, from("127.0.0.1:40000"))
	if !strings.Contains(w.Body.String(), `"requester":{"address":"127.0.0.1","local":true}`) {
		t.Fatalf("loopback: %s", w.Body)
	}
	coreWantError(t, ce.do("GET", "/api/v1/network/check", "", ""), http.StatusUnauthorized, "unauthorized", "")
}

// Mode package: GET /system/update names the .deb, and the app's 409 for
// queueing reaches the client as it is.
func TestSystemUpdatePackageMode(t *testing.T) {
	e := newCoreEnv(t)
	session := e.provisionAndLogin(t)
	readTok := e.createToken(t, session, "read")
	rel := &update.Release{Version: "v0.16.1", URL: "https://github.com/" + update.Repository + "/releases/tag/v0.16.1"}
	o := update.NewOverview("v0.16.0", update.ModePackage, true, update.ChannelStable, update.CheckResult{Latest: rel}, nil)
	o.Package = update.NewPackageInfo("armhf", o.Latest)
	e.upd.overview = o
	w := e.do("GET", "/api/v1/system/update", "", readTok)
	for _, m := range []string{`"mode":"package"`, `"updateAvailable":true`,
		`"package":{"format":"deb","arch":"armhf","file":"picache_0.16.1_armhf.deb","url":"https://github.com/Hustenreizjuengling/PiCache/releases/download/v0.16.1/picache_0.16.1_armhf.deb"}`} {
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), m) {
			t.Fatalf("overview lacks %s: %d %s", m, w.Code, w.Body)
		}
	}
	e.upd.queueErr = apperr.Conflict("PiCache was installed as a Debian package: update it with apt (System → Updates shows the steps)")
	w = e.do("POST", "/api/v1/system/update/apply", `{"version":"v0.16.1","currentPassword":"`+corePassword+`"}`, session)
	coreWantError(t, w, http.StatusConflict, "conflict", "")
	if !strings.Contains(w.Body.String(), "PiCache was installed as a Debian package: update it with apt (System → Updates shows the steps)") {
		t.Fatalf("409 %s", w.Body)
	}
	if len(e.upd.queued) != 0 {
		t.Fatalf("queued %v", e.upd.queued)
	}
}

// PATCH /settings/web with onboardingDone alone changes only that member;
// the language takes the ids of settings.Languages.
func TestWebSettingsK2(t *testing.T) {
	e := newCoreEnv(t)
	session := e.provisionAndLogin(t)
	e.web(t, func(w *settings.Web) { w.Language = "de"; w.OnboardingDone = false })
	w := e.do("PATCH", "/api/v1/settings/web", `{"onboardingDone":true}`, session)
	if w.Code != http.StatusOK || !e.set.Get().Web.OnboardingDone || e.set.Get().Web.Language != "de" {
		t.Fatalf("hide checklist: %d %s", w.Code, w.Body)
	}
	if w := e.do("PATCH", "/api/v1/settings/web", `{"onboardingDone":false}`, session); w.Code != http.StatusOK || e.set.Get().Web.OnboardingDone {
		t.Fatalf("show checklist: %d %s", w.Code, w.Body)
	}
	for _, l := range []string{"en", "de", ""} {
		if w := e.do("PATCH", "/api/v1/settings/web", `{"language":"`+l+`"}`, session); w.Code != http.StatusOK || e.set.Get().Web.Language != l {
			t.Fatalf("language %q: %d %s", l, w.Code, w.Body)
		}
	}
	w = e.do("PATCH", "/api/v1/settings/web", `{"language":"fr"}`, session)
	coreWantError(t, w, http.StatusBadRequest, "invalid", "web.language")
	if !strings.Contains(w.Body.String(), "must be empty or one of en, de") {
		t.Fatalf("message %s", w.Body)
	}
}
