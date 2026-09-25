package api

import (
	"context"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/auth"
	"github.com/hustenreizjuengling/picache/internal/settings"
	"github.com/hustenreizjuengling/picache/internal/storage"
)

const testBackupName = "picache-backup-picache-0123456789ab-20260925T013000Z.db"

// fakeBackups is a ScheduledBackups with one stored backup.
type fakeBackups struct {
	mu      sync.Mutex
	running bool
	runs    int
	deleted []string
}

func (f *fakeBackups) Overview(context.Context) ScheduledBackupsOverview {
	f.mu.Lock()
	defer f.mu.Unlock()
	return ScheduledBackupsOverview{
		Settings: settings.Defaults().Backups,
		Last: &ScheduledBackupRun{Time: time.Date(2026, 9, 25, 1, 30, 0, 0, time.UTC), OK: true, File: testBackupName,
			SizeBytes: 5, Destination: "local"},
		Running:         f.running,
		TimeZone:        "CEST",
		DestinationPath: "/var/lib/picache/backups/scheduled",
		Files:           []ScheduledBackupFile{{Name: testBackupName, SizeBytes: 5, Time: time.Date(2026, 9, 25, 1, 30, 0, 0, time.UTC)}},
	}
}

func (f *fakeBackups) RunNow() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.running {
		return apperr.Conflict("a backup is already running")
	}
	f.running = true
	f.runs++
	return nil
}

func (f *fakeBackups) Open(_ context.Context, name string) (io.ReadCloser, int64, error) {
	if name != testBackupName {
		return nil, 0, apperr.NotFound("scheduled backup", name)
	}
	return io.NopCloser(strings.NewReader("SQLit")), 5, nil
}

func (f *fakeBackups) Delete(_ context.Context, name string) error {
	if name != testBackupName {
		return apperr.NotFound("scheduled backup", name)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, name)
	return nil
}

// Scheduled backups: the overview for read tokens, everything else admin
// only; 202/409 for runs, download with attachment headers, delete; runs,
// downloads and deletes are audited.
func TestBackupRoutes(t *testing.T) {
	ce, e, fb, session, readTok := newNotifyEnv(t)
	const base = "/api/v1/system/backups/scheduled"
	for _, rc := range []struct{ method, path string }{
		{"POST", base + "/run"}, {"GET", base + "/files/" + testBackupName}, {"DELETE", base + "/files/" + testBackupName},
	} {
		coreWantError(t, ce.do(rc.method, rc.path, "", readTok), http.StatusForbidden, "forbidden", "")
	}
	coreWantError(t, ce.do("GET", base, "", ""), http.StatusUnauthorized, "unauthorized", "")

	w := ce.do("GET", base, "", readTok)
	want := `{"settings":{"enabled":false,"schedule":"daily","time":"03:30","weekday":0,"keep":7,"destination":"local","includeSecrets":false},` +
		`"last":{"time":"2026-09-25T01:30:00Z","ok":true,"file":"` + testBackupName + `","sizeBytes":5,"destination":"local"},` +
		`"running":false,"timeZone":"CEST","destinationPath":"/var/lib/picache/backups/scheduled",` +
		`"files":[{"name":"` + testBackupName + `","sizeBytes":5,"time":"2026-09-25T01:30:00Z"}]}`
	if w.Code != http.StatusOK || w.Body.String() != want {
		t.Fatalf("overview: %d\n%s\nwant\n%s", w.Code, w.Body, want)
	}

	w = ce.do("POST", base+"/run", "", session)
	if w.Code != http.StatusAccepted || w.Body.String() != `{"started":true}` {
		t.Fatalf("run: %d %s", w.Code, w.Body)
	}
	coreWantError(t, ce.do("POST", base+"/run", "", session), http.StatusConflict, "conflict", "")

	w = ce.do("GET", base+"/files/"+testBackupName, "", session)
	if w.Code != http.StatusOK || w.Body.String() != "SQLit" || w.Header().Get("Content-Type") != "application/octet-stream" ||
		w.Header().Get("Content-Length") != "5" ||
		w.Header().Get("Content-Disposition") != `attachment; filename=`+testBackupName {
		t.Fatalf("download: %d %v %q", w.Code, w.Header(), w.Body)
	}
	coreWantError(t, ce.do("GET", base+"/files/picache-backup-x.db", "", session), http.StatusNotFound, "not_found", "")
	coreWantError(t, ce.do("GET", base+"/files/..%2Fpicache.db", "", session), http.StatusBadRequest, "invalid", "name")
	coreWantError(t, ce.do("DELETE", base+"/files/a%20b", "", session), http.StatusBadRequest, "invalid", "name")

	if w := ce.do("DELETE", base+"/files/"+testBackupName, "", session); w.Code != http.StatusNoContent || len(fb.deleted) != 1 {
		t.Fatalf("delete: %d %v", w.Code, fb.deleted)
	}

	entries, _, err := e.auth.AuditLog(context.Background(), auth.AuditQuery{Search: "system.backup", Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	for _, a := range entries {
		actions = append(actions, a.Action+" "+a.Target)
	}
	if got := strings.Join(actions, ","); got != "system.backup_delete "+testBackupName+",system.backup_download "+testBackupName+
		",system.backup_scheduled_run " {
		t.Fatalf("audit %s", got)
	}
}

// PATCH /settings/backups validates every member; the destination must be
// "local" or an existing storage target, which cannot be deleted then.
func TestBackupSettingsPatch(t *testing.T) {
	ce, e, _, session, readTok := newNotifyEnv(t)
	coreWantError(t, ce.do("PATCH", "/api/v1/settings/backups", `{"enabled":true}`, readTok), http.StatusForbidden, "forbidden", "")
	for _, tc := range []struct{ body, field string }{
		{`{"time":"3:30"}`, "backups.time"},
		{`{"time":"24:00"}`, "backups.time"},
		{`{"weekday":7}`, "backups.weekday"},
		{`{"keep":0}`, "backups.keep"},
		{`{"keep":91}`, "backups.keep"},
		{`{"schedule":"monthly"}`, "backups.schedule"},
		{`{"destination":"../../etc"}`, "backups.destination"},
		{`{"destination":"ffffffffffffffffffffffffffffffff"}`, "backups.destination"},
		{`{"retention":3}`, "body"},
	} {
		coreWantError(t, ce.do("PATCH", "/api/v1/settings/backups", tc.body, session), http.StatusBadRequest, "invalid", tc.field)
	}
	w := ce.do("PATCH", "/api/v1/settings/backups", `{"enabled":true,"schedule":"weekly","time":"22:15","weekday":3,"keep":14}`, session)
	if b := ce.set.Get().Backups; w.Code != http.StatusOK || !b.Enabled || b.Schedule != "weekly" || b.Time != "22:15" ||
		b.Weekday != 3 || b.Keep != 14 || b.Destination != "local" {
		t.Fatalf("patch: %d %s", w.Code, w.Body)
	}

	target := e.call(e.srv.storageCreate, "POST", "", `{"name":"Disk","kind":"local","path":"`+jsonPath(filepath.Join(e.cfg.MountRoot, "disk"))+`"}`)
	storageWant(t, "create target", target, http.StatusCreated, "")
	id := storageDecode[storage.Target](t, target).ID
	w = ce.do("PATCH", "/api/v1/settings/backups", `{"destination":"`+strings.ToUpper(id)+`"}`, session)
	if w.Code != http.StatusOK || ce.set.Get().Backups.Destination != id {
		t.Fatalf("destination: %d %s", w.Code, w.Body)
	}
	storageWant(t, "delete destination", e.call(e.srv.storageDelete, "DELETE", id, ""), http.StatusConflict, "")
	// The full document keeps an unchanged destination.
	if w := ce.do("PATCH", "/api/v1/settings/backups", `{"keep":5}`, session); w.Code != http.StatusOK {
		t.Fatalf("unchanged destination: %d %s", w.Code, w.Body)
	}
	if w := ce.do("PATCH", "/api/v1/settings/backups", `{"destination":"local"}`, session); w.Code != http.StatusOK {
		t.Fatalf("back to local: %d %s", w.Code, w.Body)
	}
	storageWant(t, "delete target", e.call(e.srv.storageDelete, "DELETE", id, ""), http.StatusNoContent, "")

	entries, _, err := e.auth.AuditLog(context.Background(), auth.AuditQuery{Search: "backups.keep"})
	if err != nil || len(entries) != 2 || !strings.Contains(entries[1].Details, "backups.enabled") {
		t.Fatalf("audit %+v %v", entries, err)
	}
}
