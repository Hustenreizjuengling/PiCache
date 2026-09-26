package api

import (
	"bufio"
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/hustenreizjuengling/picache/internal/applog"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/hostinfo"
	"github.com/hustenreizjuengling/picache/internal/logs"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// fakeDiag is a Diagnostics with a fixed host, databases and bundle.
type fakeDiag struct {
	calls   int
	block   chan struct{} // SupportBundle waits for it (nil: never)
	started chan struct{}
}

func (f *fakeDiag) HostInfo() HostInfo {
	one := int64(42)
	return HostInfo{CPUs: 4, UptimeSec: &one, Temperatures: []hostinfo.Temperature{}, Disks: []hostinfo.Disk{}}
}
func (f *fakeDiag) Databases() DatabaseInfo {
	return DatabaseInfo{PiCache: DatabaseFile{Bytes: 1}, CacheIndexes: []CacheIndexDB{}}
}
func (f *fakeDiag) SupportBundle(ctx context.Context, include bool) ([]byte, error) {
	f.calls++
	if f.started != nil {
		f.started <- struct{}{}
	}
	if f.block != nil {
		<-f.block
	}
	return []byte("PK\x05\x06" + strings.Repeat("\x00", 18)), nil
}

// diagEnv is a coreEnv with a running logs store, the application log and
// diagnostics.
type diagEnv struct {
	*coreEnv
	logs  *logs.Store
	alog  *applog.Log
	slog  *slog.Logger // writes to alog
	diag  *fakeDiag
	admin string
}

func newDiagEnv(t *testing.T) *diagEnv {
	t.Helper()
	e := newCoreEnv(t)
	ldb, err := db.Open(filepath.Join(t.TempDir(), "logs.db"), 2)
	if err != nil {
		t.Fatal(err)
	}
	st, err := logs.New(context.Background(), ldb, e.set, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { st.Start(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done; ldb.Close() })
	h := applog.New(slog.NewTextHandler(io.Discard, nil), slog.LevelInfo)
	d := &diagEnv{coreEnv: e, logs: st, alog: h.Log(), slog: slog.New(h), diag: &fakeDiag{}}
	e.srv.d.Logs, e.srv.d.AppLog, e.srv.d.Diag = st, h.Log(), d.diag
	d.admin = e.provisionAndLogin(t)
	return d
}

// The new routes by principal: viewer session and read token read (R),
// admin token and admin session act (A), only an admin session gets the
// support bundle (S).
func TestDiagRoutePermissions(t *testing.T) {
	e := newDiagEnv(t)
	readTok := e.createToken(t, e.admin, "read")
	adminTok := e.createToken(t, e.admin, "admin")
	_, viewer := e.withViewer(t, e.admin)
	type route struct{ method, path, body, perm string }
	routes := []route{
		{"GET", "/api/v1/logs/queries/export?format=csv", "", "R"},
		{"GET", "/api/v1/stats/qtypes", "", "R"},
		{"GET", "/api/v1/stats/clients/10.0.0.1/series", "", "R"},
		{"GET", "/api/v1/system/host", "", "R"},
		{"GET", "/api/v1/system/databases", "", "R"},
		{"GET", "/api/v1/system/events", "", "R"},
		{"GET", "/api/v1/system/log", "", "A"},
		{"PUT", "/api/v1/system/log/level", `{"level":"debug","minutes":1}`, "A"},
		{"DELETE", "/api/v1/system/log/level", "", "A"},
		{"POST", "/api/v1/system/events/ack-all", "", "A"},
		{"POST", "/api/v1/system/events/1/ack", "", "A"},
		{"PATCH", "/api/v1/settings/health", `{"loadPerCpuMax":3}`, "A"},
		{"DELETE", "/api/v1/logs/queries", "", "A"},
		{"DELETE", "/api/v1/stats", "", "A"},
		{"POST", "/api/v1/system/support-bundle", `{"currentPassword":"` + corePassword + `"}`, "S"},
	}
	for _, r := range routes {
		for _, p := range []struct {
			name, cred  string
			admin, sess bool
		}{{"viewer session", viewer, false, true}, {"read token", readTok, false, false},
			{"admin token", adminTok, true, false}, {"admin session", e.admin, true, true}} {
			w := e.do(r.method, r.path, r.body, p.cred)
			allowed := r.perm == "R" || (r.perm == "A" && p.admin) || (r.perm == "S" && p.admin && p.sess)
			denied := w.Code == http.StatusForbidden || w.Code == http.StatusUnauthorized
			if allowed == denied || (allowed && w.Code >= 500) {
				t.Errorf("%s %s by %s: %d %s", r.method, r.path, p.name, w.Code, w.Body)
			}
		}
	}
}

// Clearing: 200 {deleted}, audited; 503 while logs.db is disabled.
func TestClearRoutes(t *testing.T) {
	e := newDiagEnv(t)
	e.logs.LogQuery(logs.QueryEvent{Time: time.Now().Add(-time.Second), ClientIP: "10.0.0.1", QName: "a.example", QType: "A", Status: "forwarded"})
	deadline := time.Now().Add(10 * time.Second)
	for {
		p, _ := e.logs.QueryLog(context.Background(), logs.QueryFilter{})
		if len(p.Items) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("query not stored")
		}
		time.Sleep(100 * time.Millisecond)
	}
	w := e.do("DELETE", "/api/v1/logs/queries", "", e.admin)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"deleted":1`) {
		t.Fatalf("clear queries %d %s", w.Code, w.Body)
	}
	if w := e.do("DELETE", "/api/v1/stats", "", e.admin); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"deleted":`) {
		t.Fatalf("clear stats %d %s", w.Code, w.Body)
	}
	acts := e.auditActions(t)
	if !slices.Contains(acts, "logs.queries.delete") || !slices.Contains(acts, "logs.stats.delete") {
		t.Fatalf("audit %v", acts)
	}
	e.srv.d.Logs = logs.Discard("broken", nil)
	coreWantError(t, e.do("DELETE", "/api/v1/stats", "", e.admin), http.StatusServiceUnavailable, "unavailable", "")
	coreWantError(t, e.do("DELETE", "/api/v1/logs/queries", "", e.admin), http.StatusServiceUnavailable, "unavailable", "")
}

// H4 key validation and step bounds.
func TestClientSeriesRoute(t *testing.T) {
	e := newDiagEnv(t)
	for _, tc := range []struct {
		key    string
		status int
		field  string
	}{
		{"10.0.0.1", 200, ""}, {"ip:10.0.0.1", 200, ""}, {"2001:db8::1", 200, ""}, {"client:7", 200, ""},
		{"mac:02:00:00:00:00:01", 200, ""},
		{"fe80::1%25eth0", 400, "key"}, {"2001:DB8::1", 400, "key"}, {"2001:db8:0::1", 400, "key"},
		{"::ffff:10.0.0.1", 400, "key"}, {"010.0.0.1", 400, "key"}, {"client:0", 400, "key"}, {"client:-1", 400, "key"},
		{"client:07", 400, "key"}, {"client:x", 400, "key"}, {"mac:02-00-00-00-00-01", 400, "key"},
		{"mac:02:00:00:00:00:0A", 400, "key"}, {"host.example", 400, "key"}, {"ip:", 400, "key"}, {"junk", 400, "key"},
	} {
		w := e.do("GET", "/api/v1/stats/clients/"+tc.key+"/series", "", e.admin)
		if tc.status == 200 {
			if w.Code != 200 {
				t.Errorf("%s: %d %s", tc.key, w.Code, w.Body)
			}
			continue
		}
		coreWantError(t, w, tc.status, "invalid", tc.field)
	}
	var ser logs.ClientSeries
	coreDecode(t, e.do("GET", "/api/v1/stats/clients/10.0.0.1/series", "", e.admin), &ser)
	if ser.Step != 3600 || len(ser.Timestamps) < 168 || fmt.Sprint(ser.Addresses) != "[10.0.0.1]" {
		t.Fatalf("default series: step %d, %d points, %v", ser.Step, len(ser.Timestamps), ser.Addresses)
	}
	coreWantError(t, e.do("GET", "/api/v1/stats/clients/10.0.0.1/series?step=600", "", e.admin), 400, "invalid", "step")
	coreWantError(t, e.do("GET", "/api/v1/stats/clients/10.0.0.1/series?range=90d&step=3600", "", e.admin), 400, "invalid", "step")
	// Device keys cannot be resolved while addresses are anonymised.
	if _, err := e.set.Update(context.Background(), func(a *settings.All) error { a.Logs.AnonymizeClientIPs = true; return nil }); err != nil {
		t.Fatal(err)
	}
	w := e.do("GET", "/api/v1/stats/clients/client:7/series", "", e.admin)
	coreWantError(t, w, 400, "invalid", "key")
	if !strings.Contains(w.Body.String(), "devices cannot be resolved while client addresses are anonymised") {
		t.Fatalf("message %s", w.Body)
	}
	if w := e.do("GET", "/api/v1/stats/clients/10.0.0.0/series", "", e.admin); w.Code != 200 {
		t.Fatalf("masked address key: %d", w.Code)
	}
}

// The application log and the debug level.
func TestSystemLogRoutes(t *testing.T) {
	e := newDiagEnv(t)
	e.slog.With("component", "dns").Info("dns started", slog.String("password", "p-secret"))
	var sl SystemLog
	coreDecode(t, e.do("GET", "/api/v1/system/log?level=info&component=dns", "", e.admin), &sl)
	if len(sl.Records) != 1 || sl.BaseLevel != "info" || sl.Capacity != applog.Capacity || len(sl.Components) != len(applog.Components) ||
		strings.Contains(fmt.Sprint(sl.Records), "p-secret") {
		t.Fatalf("log %+v", sl)
	}
	for _, q := range []struct{ query, field string }{{"level=trace", "level"}, {"component=nope", "component"},
		{"limit=0", "limit"}, {"limit=2001", "limit"}} {
		coreWantError(t, e.do("GET", "/api/v1/system/log?"+q.query, "", e.admin), 400, "invalid", q.field)
	}
	for _, b := range []struct{ body, field string }{
		{`{"level":"info","minutes":5}`, "level"}, {`{"level":"warn","minutes":5}`, "level"},
		{`{"level":"debug","component":"nope","minutes":5}`, "component"},
		{`{"level":"debug","minutes":0}`, "minutes"}, {`{"level":"debug","minutes":241}`, "minutes"},
	} {
		coreWantError(t, e.do("PUT", "/api/v1/system/log/level", b.body, e.admin), 400, "invalid", b.field)
	}
	w := e.do("PUT", "/api/v1/system/log/level", `{"level":"debug","component":"dns","minutes":10}`, e.admin)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"baseLevel":"info"`) || !strings.Contains(w.Body.String(), `"component":"dns"`) {
		t.Fatalf("set %d %s", w.Code, w.Body)
	}
	coreDecode(t, e.do("GET", "/api/v1/system/log", "", e.admin), &sl)
	if sl.Override == nil || sl.Override.Level != "debug" || sl.Records[0].Level != "WARN" {
		t.Fatalf("override %+v", sl)
	}
	if w := e.do("DELETE", "/api/v1/system/log/level", "", e.admin); w.Code != http.StatusNoContent {
		t.Fatalf("clear %d", w.Code)
	}
	if w := e.do("DELETE", "/api/v1/system/log/level", "", e.admin); w.Code != http.StatusNoContent {
		t.Fatalf("clear without one %d", w.Code)
	}
	acts := e.auditActions(t)
	if !slices.Contains(acts, "system.log_level") || !slices.Contains(acts, "system.log_level_clear") {
		t.Fatalf("audit %v", acts)
	}
	e.srv.d.AppLog = nil
	coreWantError(t, e.do("GET", "/api/v1/system/log", "", e.admin), 503, "unavailable", "")
}

// At most 4 application log streams (429 beyond, separate from the 16 query
// and cache streams).
func TestSystemLogStreamLimit(t *testing.T) {
	e := newDiagEnv(t)
	var cancels []func()
	defer func() {
		for _, c := range cancels {
			c()
		}
	}()
	for range applog.MaxSubs {
		_, c, err := e.alog.Subscribe(slog.LevelDebug, "")
		if err != nil {
			t.Fatal(err)
		}
		cancels = append(cancels, c)
	}
	w := e.do("GET", "/api/v1/stream/system-log", "", e.admin)
	coreWantError(t, w, http.StatusTooManyRequests, "too_many_requests", "")
	// The query streams are not affected.
	_, c, err := e.logs.SubscribeCache(nil)
	if err != nil {
		t.Fatalf("query/cache stream: %v", err)
	}
	c()
}

// The warning history: viewers see no security entries and no names, the
// badge counts what the caller may see, the ack routes are audited.
func TestEventRoutes(t *testing.T) {
	e := newDiagEnv(t)
	_, viewer := e.withViewer(t, e.admin)
	e.logs.RecordEvent(logs.EventRecord{Event: "health.warning", Severity: "warning", Title: "Health check warning: host"})
	e.logs.RecordEvent(logs.EventRecord{Event: "security.lockout", Severity: "warning", Title: "Sign-in lockout"})
	e.logs.RecordEvent(logs.EventRecord{Event: "backup.succeeded", Severity: "info", Title: "Scheduled backup written"})
	var page logs.EventPage
	coreDecode(t, e.do("GET", "/api/v1/system/events", "", e.admin), &page)
	if len(page.Items) != 3 {
		t.Fatalf("admin %+v", page)
	}
	coreDecode(t, e.do("GET", "/api/v1/system/events", "", viewer), &page)
	if len(page.Items) != 2 || slices.ContainsFunc(page.Items, func(ev logs.Event) bool { return strings.HasPrefix(ev.Event, "security.") }) {
		t.Fatalf("viewer %+v", page)
	}
	for _, q := range []struct{ query, field string }{{"unacknowledged=maybe", "unacknowledged"}, {"limit=0", "limit"},
		{"limit=201", "limit"}, {"cursor=x", "cursor"}} {
		coreWantError(t, e.do("GET", "/api/v1/system/events?"+q.query, "", e.admin), 400, "invalid", q.field)
	}
	var id int64
	for _, ev := range page.Items {
		if ev.Event == "health.warning" {
			id = ev.ID
		}
	}
	w := e.do("POST", fmt.Sprintf("/api/v1/system/events/%d/ack", id), "", e.admin)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"acknowledgedBy":"admin"`) {
		t.Fatalf("ack %d %s", w.Code, w.Body)
	}
	coreDecode(t, e.do("GET", "/api/v1/system/events?unacknowledged=false", "", viewer), &page)
	if len(page.Items) != 1 || page.Items[0].AcknowledgedAt == nil || page.Items[0].AcknowledgedBy != "" {
		t.Fatalf("viewer sees the acknowledged entry %+v", page.Items)
	}
	coreWantError(t, e.do("POST", "/api/v1/system/events/999/ack", "", e.admin), 404, "not_found", "")
	coreWantError(t, e.do("POST", "/api/v1/system/events/x/ack", "", e.admin), 400, "invalid", "id")
	w = e.do("POST", "/api/v1/system/events/ack-all", "", e.admin)
	if w.Code != 200 || w.Body.String() != `{"acknowledged":2}` {
		t.Fatalf("ack all %d %s", w.Code, w.Body)
	}
	acts := e.auditActions(t)
	if !slices.Contains(acts, "system.event_ack") || !slices.Contains(acts, "system.event_ack_all") {
		t.Fatalf("audit %v", acts)
	}
}

// The support bundle: the password from the body only (a query parameter
// is ignored), throttled and audited like a restore; one at a time.
func TestSupportBundleRoute(t *testing.T) {
	e := newDiagEnv(t)
	w := e.do("POST", "/api/v1/system/support-bundle?currentPassword="+url.QueryEscape(corePassword), "", e.admin)
	coreWantError(t, w, 400, "invalid", "body")
	coreWantError(t, e.do("POST", "/api/v1/system/support-bundle", `{"currentPassword":"wrong"}`, e.admin), 400, "invalid", "currentPassword")
	if !slices.Contains(e.auditActions(t), "auth.login_failed") {
		t.Fatal("a wrong password is not audited")
	}
	w = e.do("POST", "/api/v1/system/support-bundle", `{"currentPassword":"`+corePassword+`","includeClientNames":true}`, e.admin)
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/zip" || w.Header().Get("Content-Length") != "22" ||
		!strings.HasPrefix(w.Header().Get("Content-Disposition"), `attachment; filename="picache-support-`) {
		t.Fatalf("bundle %d %v", w.Code, w.Header())
	}
	entries := e.auditEntries(t)
	if entries[0].Action != "system.support_bundle" || entries[0].Details != `{"includeClientNames":true}` {
		t.Fatalf("audit %+v", entries[0])
	}
	// A second request while one is built: 429.
	e.diag.block, e.diag.started = make(chan struct{}), make(chan struct{})
	first := make(chan int)
	go func() {
		first <- e.do("POST", "/api/v1/system/support-bundle", `{"currentPassword":"`+corePassword+`"}`, e.admin).Code
	}()
	<-e.diag.started
	coreWantError(t, e.do("POST", "/api/v1/system/support-bundle", `{"currentPassword":"`+corePassword+`"}`, e.admin),
		http.StatusTooManyRequests, "too_many_requests", "")
	close(e.diag.block)
	if code := <-first; code != 200 {
		t.Fatalf("first bundle %d", code)
	}
	// Five wrong passwords lock password confirmations of the session.
	for range 5 {
		e.do("POST", "/api/v1/system/support-bundle", `{"currentPassword":"wrong"}`, e.admin)
	}
	if w := e.do("POST", "/api/v1/system/support-bundle", `{"currentPassword":"`+corePassword+`"}`, e.admin); w.Code != http.StatusTooManyRequests {
		t.Fatalf("throttled: %d %s", w.Code, w.Body)
	}
}

// The export: format, headers, CSV quoting and formula prefixes, the
// truncation markers, one export at a time, audit.
func TestExportRoute(t *testing.T) {
	e := newDiagEnv(t)
	now := time.Now().Add(-time.Minute)
	for i, q := range []logs.QueryEvent{
		{QName: "=hyperlink(\"http://evil\").example", ClientName: "+cmd|calc", Reason: "@sum", Answer: "-1"},
		{QName: "bidi\u202ename.example", ClientName: "tab\tname"},
		{QName: "plain.example", RCode: "NXDOMAIN"},
	} {
		q.Time, q.ClientIP, q.QType, q.Status = now.Add(time.Duration(i)*time.Second), "10.0.0.1", "A", "forwarded"
		if q.RCode == "" {
			q.RCode = "NOERROR"
		}
		e.logs.LogQuery(q)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		p, _ := e.logs.QueryLog(context.Background(), logs.QueryFilter{})
		if len(p.Items) == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("queries not stored")
		}
		time.Sleep(100 * time.Millisecond)
	}
	coreWantError(t, e.do("GET", "/api/v1/logs/queries/export", "", e.admin), 400, "invalid", "format")
	coreWantError(t, e.do("GET", "/api/v1/logs/queries/export?format=xml", "", e.admin), 400, "invalid", "format")
	coreWantError(t, e.do("GET", "/api/v1/logs/queries/export?format=csv&dnssec=yes", "", e.admin), 400, "invalid", "dnssec")
	coreWantError(t, e.do("GET", "/api/v1/logs/queries/export?format=csv&rcode=NO%20ERROR", "", e.admin), 400, "invalid", "rcode")

	w := e.do("GET", "/api/v1/logs/queries/export?format=csv&range=1h", "", e.admin)
	if w.Code != 200 || w.Header().Get("Content-Type") != "text/csv; charset=utf-8" ||
		!strings.HasPrefix(w.Header().Get("Content-Disposition"), `attachment; filename="picache-queries-`) ||
		!strings.HasSuffix(w.Header().Get("Content-Disposition"), `.csv"`) {
		t.Fatalf("csv %d %v", w.Code, w.Header())
	}
	recs, err := csv.NewReader(strings.NewReader(w.Body.String())).ReadAll()
	if err != nil || len(recs) != 4 || strings.Join(recs[0], ",") != strings.Join(exportCSVHeader, ",") {
		t.Fatalf("csv %q, %v", w.Body.String(), err)
	}
	body := w.Body.String()
	if strings.Contains(body, "\u202e") || strings.Contains(body, "\t") || strings.Contains(body, "\r") {
		t.Fatalf("control characters in %q", body)
	}
	hostile := recs[3] // oldest last
	if hostile[3] != `'=hyperlink("http://evil").example` || hostile[2] != "'+cmd|calc" || hostile[7] != "'@sum" || hostile[13] != "'-1" {
		t.Fatalf("formula cells %q", hostile)
	}
	if recs[2][3] != "bidiname.example" || recs[2][2] != "tabname" || recs[1][6] != "NXDOMAIN" {
		t.Fatalf("cells %q", recs[1:3])
	}
	// Filters apply.
	w = e.do("GET", "/api/v1/logs/queries/export?format=ndjson&rcode=nxdomain", "", e.admin)
	if lines := strings.Split(strings.TrimSpace(w.Body.String()), "\n"); w.Header().Get("Content-Type") != "application/x-ndjson" ||
		len(lines) != 1 || !strings.Contains(lines[0], `"qname":"plain.example"`) {
		t.Fatalf("ndjson %q", w.Body.String())
	}
	// The row limit: truncation markers.
	e.srv.exportMaxRows = 2
	w = e.do("GET", "/api/v1/logs/queries/export?format=ndjson", "", e.admin)
	lines := strings.Split(strings.TrimSpace(w.Body.String()), "\n")
	if len(lines) != 3 || lines[2] != `{"truncated":true,"reason":"rows"}` {
		t.Fatalf("ndjson rows %q", lines)
	}
	w = e.do("GET", "/api/v1/logs/queries/export?format=csv", "", e.admin)
	if !strings.HasSuffix(w.Body.String(), "# truncated: rows\n") {
		t.Fatalf("csv rows %q", w.Body.String())
	}
	entries := e.auditEntries(t)
	if entries[0].Action != "logs.export" || entries[0].Details != `{"format":"csv","rows":2,"truncated":"rows"}` {
		t.Fatalf("audit %+v", entries[0])
	}
	e.srv.exportMaxRows = 0
	// One export at a time.
	e.srv.exporting.Store(true)
	coreWantError(t, e.do("GET", "/api/v1/logs/queries/export?format=csv", "", e.admin), http.StatusTooManyRequests, "too_many_requests", "")
	e.srv.exporting.Store(false)
}

// The time limit of the export (lowered; synctest).
func TestExportTimeLimit(t *testing.T) {
	dir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		s, st, closeDB := logsTestServer(t, dir)
		defer closeDB()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { st.Start(ctx); close(done) }()
		defer func() { cancel(); <-done }()
		for i := range logs.ExportChunk + 10 {
			st.LogQuery(logs.QueryEvent{Time: time.Now().Add(-time.Duration(i) * time.Millisecond), ClientIP: "10.0.0.1",
				QName: fmt.Sprintf("q%d.example", i), QType: "A", Status: "forwarded"})
		}
		time.Sleep(6 * time.Second)
		synctest.Wait()
		// Every write to the client takes two minutes.
		s.exportMaxTime = time.Minute
		r := httptest.NewRequest(http.MethodGet, "/api/v1/logs/queries/export?format=ndjson", nil)
		w := &slowWriter{ResponseRecorder: httptest.NewRecorder(), delay: 2 * time.Minute}
		if err := s.logsExport(w, r); err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(strings.NewReader(w.Body.String()))
		var last string
		n := 0
		for sc.Scan() {
			last = sc.Text()
			n++
		}
		if last != `{"truncated":true,"reason":"time"}` || n > logs.ExportChunk {
			t.Fatalf("%d lines, last %q", n, last)
		}
	})
}

// slowWriter is a client that takes delay for every write.
type slowWriter struct {
	*httptest.ResponseRecorder
	delay time.Duration
}

func (w *slowWriter) Write(p []byte) (int, error) {
	time.Sleep(w.delay)
	return w.ResponseRecorder.Write(p)
}

// PATCH /settings/health validates the thresholds.
func TestHealthSettingsSection(t *testing.T) {
	e := newDiagEnv(t)
	w := e.do("PATCH", "/api/v1/settings/health", `{"memoryAvailableMinPercent":10,"temperatureMaxCelsius":70}`, e.admin)
	if w.Code != 200 || e.set.Get().Health != (settings.Health{MemoryAvailableMinPercent: 10, LoadPerCPUMax: 2, TemperatureMaxCelsius: 70}) {
		t.Fatalf("patch %d %s", w.Code, w.Body)
	}
	coreWantError(t, e.do("PATCH", "/api/v1/settings/health", `{"loadPerCpuMax":17}`, e.admin), 400, "invalid", "health.loadPerCpuMax")
	var d settings.All
	coreDecode(t, e.do("GET", "/api/v1/settings/defaults", "", e.admin), &d)
	if d.Health != settings.Defaults().Health {
		t.Fatalf("defaults %+v", d.Health)
	}
}
