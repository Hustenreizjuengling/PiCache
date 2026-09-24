package api

import (
	"bufio"
	"context"
	"encoding/json/v2"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/logs"
)

// logsTestServer returns a Server wired only with a logs store on a temp
// database (settings default). Handlers are called directly: permissions are
// covered by the route/auth tests.
func logsTestServer(t *testing.T, dir string) (*Server, *logs.Store, func()) {
	t.Helper()
	ldb, err := db.Open(filepath.Join(dir, "logs.db"), 2)
	if err != nil {
		t.Fatal(err)
	}
	st, err := logs.New(context.Background(), ldb, nil, nil)
	if err != nil {
		ldb.Close()
		t.Fatal(err)
	}
	log := slog.New(slog.DiscardHandler)
	return &Server{d: Deps{Logs: st, Log: log}, log: log}, st, func() { ldb.Close() }
}

// logsGet runs h for target and maps a returned error like route does.
func logsGet(s *Server, h handlerFunc, target string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, target, nil)
	w := httptest.NewRecorder()
	if err := h(w, r); err != nil {
		writeError(w, r, s.log, err)
	}
	return w
}

func logsDecode[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %s: %v", w.Body.String(), err)
	}
	return v
}

func TestLogsRoutesParametersAndDefaults(t *testing.T) {
	s, _, closeDB := logsTestServer(t, t.TempDir())
	defer closeDB()
	for _, tc := range []struct {
		name   string
		h      handlerFunc
		target string
		status int
		field  string
	}{
		{"query log", s.logsQueries, "/api/v1/logs/queries", 200, ""},
		{"short domain search", s.logsQueries, "/api/v1/logs/queries?domain=ab", 400, "domain"},
		{"unknown status", s.logsQueries, "/api/v1/logs/queries?status=forwarded,nope", 400, "status"},
		{"bad cursor", s.logsQueries, "/api/v1/logs/queries?cursor=xyz", 400, "cursor"},
		{"bad limit", s.logsQueries, "/api/v1/logs/queries?limit=abc", 400, "limit"},
		{"summary", s.logsSummary, "/api/v1/stats/summary?range=7d", 200, ""},
		{"bad range", s.logsSummary, "/api/v1/stats/summary?range=forever", 400, "range"},
		{"dns series", s.logsDNSSeries, "/api/v1/stats/dns", 200, ""},
		{"too many points", s.logsDNSSeries, "/api/v1/stats/dns?range=30h&step=60", 400, "step"},
		{"negative step", s.logsDNSSeries, "/api/v1/stats/dns?step=-5", 400, "step"},
		{"cache series", s.logsCacheSeries, "/api/v1/stats/cache?range=6h&service=steam", 200, ""},
		{"top without kind", s.logsTop, "/api/v1/stats/top", 400, "kind"},
		{"top bad kind", s.logsTop, "/api/v1/stats/top?kind=nope", 400, "kind"},
		{"top", s.logsTop, "/api/v1/stats/top?kind=content&limit=5", 200, ""},
		{"services", s.logsServiceStats, "/api/v1/stats/services", 200, ""},
		{"clients", s.logsClientStats, "/api/v1/stats/clients?range=15m", 200, ""},
		{"downloads", s.logsDownloads, "/api/v1/cache/downloads?active=true&offset=0", 200, ""},
		{"downloads bad offset", s.logsDownloads, "/api/v1/cache/downloads?offset=-1", 400, "offset"},
		{"requests", s.logsCacheRequests, "/api/v1/cache/requests?status=HIT", 200, ""},
		{"sni events", s.logsSNIEvents, "/api/v1/cache/sni-events?search=ab", 400, "search"},
		{"evictions", s.logsEvictions, "/api/v1/cache/evictions?status=size&range=90d", 200, ""},
		{"stream bad filter", s.logsStreamQueries, "/api/v1/stream/queries?client=ab", 400, "client"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := logsGet(s, tc.h, tc.target)
			if w.Code != tc.status {
				t.Fatalf("status %d, want %d: %s", w.Code, tc.status, w.Body.String())
			}
			if tc.field != "" {
				if body := logsDecode[errorBody](t, w); body.Error.Field != tc.field {
					t.Fatalf("field %q, want %q", body.Error.Field, tc.field)
				}
			}
		})
	}

	// Default ranges: query log 1 h, statistics 24 h (≤ 300 points, 5-minute step).
	page := logsDecode[logs.QueryPage](t, logsGet(s, s.logsQueries, "/api/v1/logs/queries"))
	if page.Items == nil || page.Total != -1 {
		t.Fatalf("empty page = %+v", page)
	}
	ser := logsDecode[logs.Series](t, logsGet(s, s.logsDNSSeries, "/api/v1/stats/dns"))
	if ser.Step != 300 || len(ser.Timestamps) < 288 || len(ser.Timestamps) > 289 || len(ser.Values["blocked"]) != len(ser.Timestamps) {
		t.Fatalf("default series: step %d, %d points", ser.Step, len(ser.Timestamps))
	}
	sum := logsDecode[logs.Summary](t, logsGet(s, s.logsSummary, "/api/v1/stats/summary"))
	if d := sum.To.Sub(sum.From); d != 24*time.Hour {
		t.Fatalf("default summary range = %v", d)
	}
	ser = logsDecode[logs.Series](t, logsGet(s, s.logsCacheSeries, "/api/v1/stats/cache?range=30d"))
	if ser.Step != 3*3600 {
		t.Fatalf("30-day series step = %d", ser.Step)
	}
}

func TestLogsRoutesServeData(t *testing.T) {
	dir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		s, st, closeDB := logsTestServer(t, dir)
		defer closeDB()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { st.Start(ctx); close(done) }()
		defer func() { cancel(); <-done }()

		now := time.Now()
		for i, q := range []logs.QueryEvent{
			{ClientIP: "10.0.0.1", QName: "a.example", QType: "A", Status: "forwarded"},
			{ClientIP: "10.0.0.2", QName: "ads.example", QType: "A", Status: "blocked-list"},
			{ClientIP: "10.0.0.2", QName: "a.example", QType: "AAAA", Status: "cached"},
		} {
			q.Time = now.Add(time.Duration(i) * time.Second)
			st.LogQuery(q)
		}
		st.LogCache(logs.CacheEvent{Time: now, ClientIP: "10.0.0.3", Service: "steam", Host: "cdn.example",
			Path: "/depot/1/chunk/a", Method: "GET", Status: 200, CacheStatus: "HIT", BytesSent: 100, BytesHit: 100,
			GroupKey: "steam:depot:1"})
		time.Sleep(6 * time.Second) // one flush
		synctest.Wait()

		page := logsDecode[logs.QueryPage](t, logsGet(s, s.logsQueries, "/api/v1/logs/queries?status=blocked&client=10.0.0.2"))
		if len(page.Items) != 1 || page.Items[0].QName != "ads.example" {
			t.Fatalf("filtered query log = %+v", page)
		}
		page = logsDecode[logs.QueryPage](t, logsGet(s, s.logsQueries, "/api/v1/logs/queries?limit=2"))
		if len(page.Items) != 2 || page.Next == "" {
			t.Fatalf("first page = %+v", page)
		}
		page = logsDecode[logs.QueryPage](t, logsGet(s, s.logsQueries, "/api/v1/logs/queries?limit=2&cursor="+page.Next))
		if len(page.Items) != 1 || page.Next != "" {
			t.Fatalf("second page = %+v", page)
		}
		top := logsDecode[[]logs.TopItem](t, logsGet(s, s.logsTop, "/api/v1/stats/top?kind=domains&limit=1"))
		if len(top) != 1 || top[0].Key != "a.example" || top[0].Count != 2 {
			t.Fatalf("top = %+v", top)
		}
		sum := logsDecode[logs.Summary](t, logsGet(s, s.logsSummary, "/api/v1/stats/summary?range=1h"))
		if sum.DNSQueries != 3 || sum.DNSBlocked != 1 || sum.CacheBytesHit != 100 || sum.ActiveClients != 3 {
			t.Fatalf("summary = %+v", sum)
		}
		// Top lists and activeClients start at the full hour before "from".
		if !sum.TopFrom.Equal(sum.From.Truncate(time.Hour)) {
			t.Fatalf("summary topFrom %v for from %v", sum.TopFrom, sum.From)
		}
		dl := logsDecode[struct {
			Items []logs.Download `json:"items"`
			Total int             `json:"total"`
		}](t, logsGet(s, s.logsDownloads, "/api/v1/cache/downloads?service=steam"))
		if dl.Total != 1 || dl.Items[0].GroupKey != "steam:depot:1" {
			t.Fatalf("downloads = %+v", dl)
		}
	})
}

func TestLogsStreams(t *testing.T) {
	s, st, closeDB := logsTestServer(t, t.TempDir())
	defer closeDB()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { st.Start(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := s.logsStreamQueries
		if r.URL.Path == "/api/v1/stream/cache" {
			h = s.logsStreamCache
		}
		if err := h(w, r); err != nil {
			writeError(w, r, s.log, err)
		}
	}))
	defer srv.Close()

	reqCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	req, _ := http.NewRequestWithContext(reqCtx, http.MethodGet, srv.URL+"/api/v1/stream/queries?status=blocked", nil)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream response %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	// Headers are sent after subscribing, so these events are seen.
	st.LogQuery(logs.QueryEvent{Time: time.Now(), ClientIP: "10.0.0.1", QName: "ok.example", Status: "forwarded"})
	st.LogQuery(logs.QueryEvent{Time: time.Now(), ClientIP: "10.0.0.1", QName: "ads.example", Status: "blocked-rule"})
	sc := bufio.NewScanner(resp.Body)
	var lines []string
	for len(lines) < 2 && sc.Scan() {
		if l := sc.Text(); l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) != 2 || lines[0] != "event: query" || !strings.Contains(lines[1], `"qname":"ads.example"`) ||
		!strings.Contains(lines[1], `"seq":`) || strings.Contains(lines[1], `"seq":0`) {
		t.Fatalf("stream lines = %q", lines)
	}

	// The subscriber limit is shared by all streams: 429 beyond it.
	var cancels []func()
	defer func() {
		for _, c := range cancels {
			c()
		}
	}()
	for range logs.MaxSubscribers - 1 {
		_, c, err := st.SubscribeCache(nil)
		if err != nil {
			t.Fatal(err)
		}
		cancels = append(cancels, c)
	}
	r2, err := srv.Client().Get(srv.URL + "/api/v1/stream/cache")
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()
	if r2.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status beyond the limit = %d, want 429", r2.StatusCode)
	}
}

func TestLogsRoutesUnavailableWhenLoggingDisabled(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	s := &Server{d: Deps{Logs: logs.Discard("broken", log), Log: log}, log: log}
	for _, h := range []handlerFunc{s.logsQueries, s.logsSummary, s.logsStreamCache} {
		if w := logsGet(s, h, "/api/v1/x"); w.Code != http.StatusServiceUnavailable {
			t.Fatalf("status %d, want 503", w.Code)
		}
	}
}
