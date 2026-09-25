package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/hustenreizjuengling/picache/internal/auth"
	"github.com/hustenreizjuengling/picache/internal/notify"
	"github.com/hustenreizjuengling/picache/internal/secrets"
)

// newNotifyEnv is the storage test environment with a notification
// service and a fake ScheduledBackups behind the real middleware, an admin
// session and a read token.
func newNotifyEnv(t *testing.T) (ce *coreEnv, e *storageTestEnv, fb *fakeBackups, session, readTok string) {
	t.Helper()
	e = newStorageTestEnv(t)
	box, err := secrets.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	n, err := notify.New(context.Background(), e.db, box, notify.Options{InstanceID: "picache-test", Hostname: "pi", Version: "v0.4.0"},
		slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	fb = &fakeBackups{}
	e.srv.d.Notify, e.srv.d.Backups = n, fb
	e.srv.d.Config.WebListen = []string{":8080"}
	ce = &coreEnv{srv: New(e.srv.d), auth: e.auth, set: e.srv.d.Settings}
	session = ce.provisionAndLogin(t)
	readTok = ce.createToken(t, session, "read")
	return ce, e, fb, session, readTok
}

func TestNotifyRoutesRegistered(t *testing.T) {
	s := &Server{mux: http.NewServeMux()}
	s.registerNotifyRoutes()
	s.registerBackupRoutes()
	id := "0123456789abcdef0123456789abcdef"
	name := "picache-backup-picache-0123456789ab-20260925T013000Z.db"
	for _, rc := range []struct{ method, path, pattern string }{
		{"GET", "/api/v1/notifications/channels", "GET /api/v1/notifications/channels"},
		{"POST", "/api/v1/notifications/channels", "POST /api/v1/notifications/channels"},
		{"PUT", "/api/v1/notifications/channels/" + id, "PUT /api/v1/notifications/channels/{id}"},
		{"DELETE", "/api/v1/notifications/channels/" + id, "DELETE /api/v1/notifications/channels/{id}"},
		{"POST", "/api/v1/notifications/channels/" + id + "/test", "POST /api/v1/notifications/channels/{id}/test"},
		{"GET", "/api/v1/notifications/events", "GET /api/v1/notifications/events"},
		{"GET", "/api/v1/notifications/log", "GET /api/v1/notifications/log"},
		{"GET", "/api/v1/system/backups/scheduled", "GET /api/v1/system/backups/scheduled"},
		{"POST", "/api/v1/system/backups/scheduled/run", "POST /api/v1/system/backups/scheduled/run"},
		{"GET", "/api/v1/system/backups/scheduled/files/" + name, "GET /api/v1/system/backups/scheduled/files/{name}"},
		{"DELETE", "/api/v1/system/backups/scheduled/files/" + name, "DELETE /api/v1/system/backups/scheduled/files/{name}"},
	} {
		if _, p := s.mux.Handler(httptest.NewRequest(rc.method, rc.path, nil)); p != rc.pattern {
			t.Errorf("%s %s → %q, want %q", rc.method, rc.path, p, rc.pattern)
		}
	}
}

// Channel CRUD through the middleware: admin only (read tokens see the
// event list), field errors, a write-only secret, audited without it.
func TestNotifyRoutes(t *testing.T) {
	ce, e, _, session, readTok := newNotifyEnv(t)
	var mu sync.Mutex
	var hits []http.Header
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		hits = append(hits, r.Header.Clone())
		mu.Unlock()
	}))
	defer hook.Close()
	const secret = "Bearer s3cret-token-value"

	// Read tokens: event list only.
	var evs []notify.EventInfo
	w := ce.do("GET", "/api/v1/notifications/events", "", readTok)
	coreDecode(t, w, &evs)
	if w.Code != http.StatusOK || len(evs) != 12 || evs[0].Key != "health.failed" || evs[0].Severity != notify.SeverityError ||
		evs[0].Title == "" || evs[0].Description == "" {
		t.Fatalf("events %d %+v", w.Code, evs)
	}
	for _, rc := range []struct{ method, path, body string }{
		{"GET", "/api/v1/notifications/channels", ""},
		{"POST", "/api/v1/notifications/channels", `{"name":"x","kind":"ntfy","url":"https://ntfy.sh/t"}`},
		{"GET", "/api/v1/notifications/log", ""},
		{"DELETE", "/api/v1/notifications/channels/0123456789abcdef0123456789abcdef", ""},
	} {
		coreWantError(t, ce.do(rc.method, rc.path, rc.body, readTok), http.StatusForbidden, "forbidden", "")
	}
	coreWantError(t, ce.do("GET", "/api/v1/notifications/events", "", ""), http.StatusUnauthorized, "unauthorized", "")

	// Field errors.
	for _, tc := range []struct{ body, field string }{
		{`{"name":"","kind":"ntfy","url":"https://ntfy.sh/t"}`, "name"},
		{`{"name":"x","kind":"mail","url":"https://ntfy.sh/t"}`, "kind"},
		{`{"name":"x","kind":"ntfy","url":"ftp://ntfy.sh/t"}`, "url"},
		{`{"name":"x","kind":"ntfy","url":"https://u:p@ntfy.sh/t"}`, "url"},
		{`{"name":"x","kind":"ntfy","url":"https://ntfy.sh/t","minSeverity":"debug"}`, "minSeverity"},
		{`{"name":"x","kind":"ntfy","url":"https://ntfy.sh/t","events":["nope"]}`, "events"},
		{`{"name":"x","kind":"gotify","url":"https://gotify.lan"}`, "secret"},
		{`{"name":"x","kind":"ntfy","url":"https://ntfy.sh/t","token":"x"}`, "body"},
	} {
		coreWantError(t, ce.do("POST", "/api/v1/notifications/channels", tc.body, session), http.StatusBadRequest, "invalid", tc.field)
	}

	body := `{"name":"Home Assistant","kind":"webhook","url":"` + hook.URL + `/api/webhook/picache?x=1","secret":"` + secret +
		`","enabled":true,"minSeverity":"info","events":["backup.failed","health.failed"]}`
	w = ce.do("POST", "/api/v1/notifications/channels", body, session)
	if w.Code != http.StatusCreated || strings.Contains(w.Body.String(), "s3cret") {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	var c notify.Channel
	coreDecode(t, w, &c)
	if !c.HasSecret || !c.Enabled || c.MinSeverity != notify.SeverityInfo || len(c.Events) != 2 || len(c.ID) != 32 {
		t.Fatalf("created %+v", c)
	}
	w = ce.do("GET", "/api/v1/notifications/channels", "", session)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "s3cret") || !strings.Contains(w.Body.String(), `"hasSecret":true`) {
		t.Fatalf("list: %d %s", w.Code, w.Body)
	}

	// Update: the secret is kept when absent (same server) and refused
	// when the server changes without it.
	upd := `{"name":"HA","kind":"webhook","url":"` + hook.URL + `/api/webhook/other","enabled":true}`
	w = ce.do("PUT", "/api/v1/notifications/channels/"+c.ID, upd, session)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"hasSecret":true`) || !strings.Contains(w.Body.String(), `"minSeverity":"warning"`) {
		t.Fatalf("update: %d %s", w.Code, w.Body)
	}
	coreWantError(t, ce.do("PUT", "/api/v1/notifications/channels/"+c.ID,
		`{"name":"HA","kind":"webhook","url":"http://evil.example/hook","enabled":true}`, session), http.StatusBadRequest, "invalid", "secret")
	coreWantError(t, ce.do("PUT", "/api/v1/notifications/channels/nope", upd, session), http.StatusBadRequest, "invalid", "id")
	coreWantError(t, ce.do("PUT", "/api/v1/notifications/channels/ffffffffffffffffffffffffffffffff", upd, session),
		http.StatusNotFound, "not_found", "")

	// Test: synchronous, with the secret, ignoring the filter.
	var res notify.TestResult
	w = ce.do("POST", "/api/v1/notifications/channels/"+c.ID+"/test", "", session)
	coreDecode(t, w, &res)
	if w.Code != http.StatusOK || !res.OK || res.Status != 200 {
		t.Fatalf("test: %d %s", w.Code, w.Body)
	}
	mu.Lock()
	if len(hits) != 1 || hits[0].Get("Authorization") != secret {
		t.Fatalf("hook got %v", hits)
	}
	mu.Unlock()
	coreWantError(t, ce.do("POST", "/api/v1/notifications/channels/ffffffffffffffffffffffffffffffff/test", "", session),
		http.StatusNotFound, "not_found", "")

	// Log: newest first; limit validated.
	var log []notify.LogEntry
	w = ce.do("GET", "/api/v1/notifications/log?limit=5", "", session)
	coreDecode(t, w, &log)
	if w.Code != http.StatusOK || len(log) != 1 || log[0].Event != "notify.test" || !log[0].OK || log[0].ChannelID != c.ID {
		t.Fatalf("log: %d %s", w.Code, w.Body)
	}
	coreWantError(t, ce.do("GET", "/api/v1/notifications/log?limit=201", "", session), http.StatusBadRequest, "invalid", "limit")
	coreWantError(t, ce.do("GET", "/api/v1/notifications/log?limit=x", "", session), http.StatusBadRequest, "invalid", "limit")

	if w := ce.do("DELETE", "/api/v1/notifications/channels/"+c.ID, "", session); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	coreWantError(t, ce.do("DELETE", "/api/v1/notifications/channels/"+c.ID, "", session), http.StatusNotFound, "not_found", "")

	entries, _, err := e.auth.AuditLog(context.Background(), auth.AuditQuery{Search: "notifications.channel", Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	for _, a := range entries {
		actions = append(actions, a.Action)
		if strings.Contains(a.Details, "s3cret") || strings.Contains(a.Details, "x=1") {
			t.Fatalf("audit leaks the secret or the query string: %+v", a)
		}
		if a.Target != c.ID {
			t.Fatalf("audit target %+v", a)
		}
	}
	if got := strings.Join(actions, ","); got != "notifications.channel.delete,notifications.channel.test,notifications.channel.update,notifications.channel.create" {
		t.Fatalf("audit actions %s", got)
	}
}

// Without a notification service the endpoints answer 503 (events excepted).
func TestNotifyRoutesUnavailable(t *testing.T) {
	e := newCoreEnv(t)
	session := e.provisionAndLogin(t)
	coreWantError(t, e.do("GET", "/api/v1/notifications/channels", "", session), http.StatusServiceUnavailable, "unavailable", "")
	coreWantError(t, e.do("GET", "/api/v1/system/backups/scheduled", "", session), http.StatusServiceUnavailable, "unavailable", "")
	if w := e.do("GET", "/api/v1/notifications/events", "", session); w.Code != http.StatusOK {
		t.Fatalf("events: %d", w.Code)
	}
}
