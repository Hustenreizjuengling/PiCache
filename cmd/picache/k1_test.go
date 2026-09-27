package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/config"
)

// fakeAPI is a PiCache API for the CLI commands: it records the requests
// and answers with fixed documents (strings with ESC, OSC and bidi
// controls, which the CLI must escape).
type fakeAPI struct {
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []fakeReq
}

type fakeReq struct {
	method, path, query, token string
	body                       string
}

const evil = "Evil\x1b]0;pwn\x07\u202e"

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	f := &fakeAPI{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		f.mu.Lock()
		f.reqs = append(f.reqs, fakeReq{r.Method, r.URL.Path, r.URL.RawQuery, tok, string(b)})
		f.mu.Unlock()
		if tok == "pc_bad" {
			w.WriteHeader(401)
			return
		}
		if tok == "pc_read" && r.Method != http.MethodGet && r.URL.Path != "/api/v1/filter/explain" && r.URL.Path != "/api/v1/dns/lookup" {
			w.WriteHeader(403)
			fmt.Fprint(w, `{"error":{"code":"forbidden","message":"admin permission required"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		jsonOut := func(v any) { _ = json.MarshalWrite(w, v) }
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/system/overview":
			fmt.Fprint(w, `{"blocking":{"enabled":true,"permanent":false},"proxy":{"bytesHit":300,"bytesWan":100},"downloadCacheEnabled":true}`)
		case "GET /api/v1/system/health":
			jsonOut(map[string]any{"ok": false, "checks": []map[string]string{{"name": "listeners", "status": "fail"},
				{"name": "sync", "status": "warn"}, {"name": "dns", "status": "ok"}}})
		case "GET /api/v1/settings":
			fmt.Fprint(w, `{"logs":{"statsEnabled":`+strconv.FormatBool(!strings.Contains(r.Header.Get("X-Test"), "off"))+
				`,"privacyLevel":"full","seenRetentionDays":30},"cache":{"activeStoreId":"local","maxSizeBytes":123456789012},`+
				`"sync":{"mode":"off","tokenSet":true,"sections":[]},"network":{"proxy":{"url":"","passwordSet":false},"proxyFor":{"lists":false}}}`)
		case "GET /api/v1/stats/summary":
			fmt.Fprint(w, `{"dnsQueries":1000,"dnsBlocked":125,"blockedPercent":12.5}`)
		case "GET /api/v1/stats/top":
			jsonOut([]map[string]any{{"key": "192.168.1.5", "label": evil, "count": 7}})
		case "POST /api/v1/dns/blocking":
			fmt.Fprint(w, `{"enabled":false,"pausedUntil":"2026-09-27T12:00:00Z"}`)
		case "POST /api/v1/filter/explain":
			jsonOut(map[string]any{"domain": "ads.example", "qtype": "A", "decision": map[string]string{"action": "block", "source": "list",
				"name": evil}, "matches": []map[string]any{{"action": "block", "source": "list", "name": evil, "pattern": "ads.example",
				"applies": true, "decisive": true}}})
		case "POST /api/v1/filter/lists/refresh":
			w.WriteHeader(202)
			fmt.Fprint(w, `{"started":true}`)
		case "GET /api/v1/groups":
			fmt.Fprint(w, `[{"id":1,"name":"Default"},{"id":2,"name":"Kids"}]`)
		case "POST /api/v1/filter/rules":
			if strings.Contains(string(b), "dup.example") {
				w.WriteHeader(409)
				fmt.Fprint(w, `{"error":{"code":"conflict","message":"this rule already exists"}}`)
				return
			}
			if strings.Contains(string(b), "synced.example") {
				w.WriteHeader(409)
				fmt.Fprint(w, `{"error":{"code":"conflict","message":"this is synced from https://p: change it on the primary"}}`)
				return
			}
			w.WriteHeader(201)
			fmt.Fprint(w, `{"id":9,"action":"allow","pattern":"good.example"}`)
		case "POST /api/v1/dns/lookup":
			jsonOut(map[string]any{"name": "x.example", "type": "A", "status": "forwarded", "rcode": "NOERROR",
				"answers": []string{"x.example. 60 IN A 192.0.2.1"}, "steps": []string{"step " + evil}})
		case "PUT /api/v1/settings":
			fmt.Fprint(w, `{}`)
		default:
			if r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/api/v1/settings/") {
				fmt.Fprint(w, `{}`)
				return
			}
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) last(t *testing.T, method, path string) fakeReq {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.reqs) - 1; i >= 0; i-- {
		if f.reqs[i].method == method && f.reqs[i].path == path {
			return f.reqs[i]
		}
	}
	t.Fatalf("no %s %s", method, path)
	return fakeReq{}
}

// noRawControls fails when s holds an unescaped control character.
func noRawControls(t *testing.T, what, s string) {
	t.Helper()
	for _, r := range s {
		if (r < 0x20 && r != '\n') || r == 0x7f || (r >= 0x80 && r <= 0x9f) || r == 0x202e {
			t.Fatalf("%s holds the raw control %U: %q", what, r, s)
		}
	}
}

func TestStatusCommand(t *testing.T) {
	cliEnv(t)
	t.Setenv("PICACHE_TOKEN", "pc_read")
	f := newFakeAPI(t)
	code, out, errOut := capture(t, func() int { return run([]string{"status", "--url", f.srv.URL}) })
	if code != 0 {
		t.Fatalf("status: %d %q", code, errOut)
	}
	for _, want := range []string{"Blocking   on", "Queries    1000 (24 h)", "Blocked    125 (12.5 %)", "Cache hits 75.0 %",
		"Health     1 failing, 1 warnings", "fail: listeners", "warn: sync", "1. Evil\\u001b]0;pwn"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	noRawControls(t, "status", out)
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if len([]rune(line)) > 48 {
			t.Errorf("line longer than a small screen: %q", line)
		}
	}
	// --json: the answers as they are.
	code, out, _ = capture(t, func() int { return run([]string{"status", "--json", "--url", f.srv.URL}) })
	if code != 0 || !strings.Contains(out, `"topClients"`) || !strings.Contains(out, `"bytesHit":300`) {
		t.Fatalf("--json: %d %s", code, out)
	}
	noRawControls(t, "status --json", out)
	for _, bad := range [][]string{{"status", "--interval", "1s"}, {"status", "--interval", "61s"}, {"status", "extra"}} {
		if code, _, _ := capture(t, func() int { return run(append(bad, "--url", f.srv.URL)) }); code != 2 {
			t.Errorf("%v: exit %d", bad, code)
		}
	}
	t.Setenv("PICACHE_TOKEN", "pc_bad")
	code, _, errOut = capture(t, func() int { return run([]string{"status", "--url", f.srv.URL}) })
	if code != 1 || !strings.Contains(errOut, "the API token is missing or invalid (PICACHE_TOKEN or --token-file)") {
		t.Errorf("401: %d %q", code, errOut)
	}
}

// statistics off: the statistics lines say so and the stats routes are
// not asked.
func TestRenderStatusStatsOff(t *testing.T) {
	d := &statusData{}
	d.Overview.Blocking.Enabled = false
	until := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	d.Overview.Blocking.PausedUntil = &until
	out := renderStatus(d, time.Now())
	if !strings.Contains(out, "Queries    statistics off") || !strings.Contains(out, "Blocked    statistics off") ||
		!strings.Contains(out, "Blocking   paused until") || !strings.Contains(out, "Health     ok") {
		t.Errorf("%s", out)
	}
}

// --watch redraws per interval: on a terminal with the clear sequence,
// else one block per interval; a failed fetch is shown and retried; it
// ends with exit 0.
func TestStatusWatch(t *testing.T) {
	cliEnv(t)
	t.Setenv("PICACHE_TOKEN", "pc_read")
	f := newFakeAPI(t)
	c, code := clientFor(f.srv.URL, "")
	if c == nil {
		t.Fatal(code)
	}
	for _, term := range []bool{true, false} {
		var buf bytes.Buffer
		n := 0
		sleep := func(context.Context, time.Duration) bool { n++; return n < 3 }
		if code := statusWatch(context.Background(), c, 2*time.Second, term, &buf, sleep); code != 0 {
			t.Fatalf("exit %d", code)
		}
		out := buf.String()
		if got := strings.Count(out, "PiCache  "); got != 3 {
			t.Errorf("term %v: %d blocks", term, got)
		}
		if term != strings.Contains(out, "\x1b[H\x1b[2J") {
			t.Errorf("term %v: clear sequence %v", term, !term)
		}
		noRawControls(t, "watch", strings.ReplaceAll(out, "\x1b[H\x1b[2J", ""))
	}
	down, _ := clientFor("http://127.0.0.1:1", "")
	var buf bytes.Buffer
	n := 0
	statusWatch(context.Background(), down, 2*time.Second, false, &buf, func(context.Context, time.Duration) bool { n++; return n < 2 })
	if strings.Count(buf.String(), "error: ") != 2 {
		t.Errorf("errors not shown per interval: %q", buf.String())
	}
}

func TestPauseResume(t *testing.T) {
	cliEnv(t)
	t.Setenv("PICACHE_TOKEN", "pc_admin")
	f := newFakeAPI(t)
	code, out, errOut := capture(t, func() int { return run([]string{"pause", "90m", "--url", f.srv.URL}) })
	if code != 0 || !strings.Contains(out, "blocking paused until") {
		t.Fatalf("pause: %d %q %q", code, out, errOut)
	}
	if b := f.last(t, "POST", "/api/v1/dns/blocking").body; b != `{"enabled":false,"pauseSeconds":5400}` {
		t.Errorf("body %s", b)
	}
	if code, _, _ := capture(t, func() int { return run([]string{"resume", "--url", f.srv.URL}) }); code != 0 {
		t.Fatal("resume")
	}
	if b := f.last(t, "POST", "/api/v1/dns/blocking").body; b != `{"enabled":true}` {
		t.Errorf("body %s", b)
	}
	for _, c := range []struct {
		in   string
		want time.Duration
		ok   bool
	}{{"1s", time.Second, true}, {"30m", 30 * time.Minute, true}, {"2h", 2 * time.Hour, true}, {"7d", 7 * 24 * time.Hour, true},
		{"8d", 0, false}, {"0s", 0, false}, {"500ms", 0, false}, {"x", 0, false}, {"1.5d", 0, false}} {
		d, err := parsePause(c.in)
		if (err == nil) != c.ok || (c.ok && d != c.want) {
			t.Errorf("%s: %v %v", c.in, d, err)
		}
	}
	if code, _, _ := capture(t, func() int { return run([]string{"pause", "8d", "--url", f.srv.URL}) }); code != 2 {
		t.Error("an 8 day pause is no usage error")
	}
	t.Setenv("PICACHE_TOKEN", "pc_read")
	code, _, errOut = capture(t, func() int { return run([]string{"pause", "1h", "--url", f.srv.URL}) })
	if code != 1 || !strings.Contains(errOut, "this command needs an admin token") {
		t.Errorf("read token: %d %q", code, errOut)
	}
}

func TestExplainQueryListsCommands(t *testing.T) {
	cliEnv(t)
	t.Setenv("PICACHE_TOKEN", "pc_read")
	f := newFakeAPI(t)
	code, out, _ := capture(t, func() int {
		return run([]string{"explain", "ads.example", "--client", "192.168.1.5", "--type", "AAAA", "--url", f.srv.URL})
	})
	if code != 0 || !strings.Contains(out, "decision: block by list") {
		t.Fatalf("explain: %d %q", code, out)
	}
	noRawControls(t, "explain", out)
	if b := f.last(t, "POST", "/api/v1/filter/explain").body; !strings.Contains(b, `"clientIp":"192.168.1.5"`) ||
		!strings.Contains(b, `"qtype":"AAAA"`) {
		t.Errorf("explain body %s", b)
	}
	code, out, _ = capture(t, func() int { return run([]string{"query", "x.example", "AAAA", "--url", f.srv.URL}) })
	if code != 0 || !strings.Contains(out, "x.example A: forwarded (NOERROR)") || !strings.Contains(out, "192.0.2.1") {
		t.Fatalf("query: %d %q", code, out)
	}
	noRawControls(t, "query", out)
	if b := f.last(t, "POST", "/api/v1/dns/lookup").body; !strings.Contains(b, `"type":"AAAA"`) {
		t.Errorf("lookup body %s", b)
	}
	code, out, _ = capture(t, func() int { return run([]string{"query", "x.example", "--json", "--url", f.srv.URL}) })
	if code != 0 || !strings.HasPrefix(out, "{") {
		t.Fatalf("query --json: %q", out)
	}
	noRawControls(t, "query --json", out)
	t.Setenv("PICACHE_TOKEN", "pc_admin")
	code, out, _ = capture(t, func() int { return run([]string{"lists", "update", "--url", f.srv.URL}) })
	if code != 0 || strings.TrimSpace(out) != "refresh started" {
		t.Fatalf("lists update: %d %q", code, out)
	}
	if code, _, _ := capture(t, func() int { return run([]string{"lists", "--url", f.srv.URL}) }); code != 2 {
		t.Error("lists without update")
	}
}

func TestAllowDeny(t *testing.T) {
	cliEnv(t)
	t.Setenv("PICACHE_TOKEN", "pc_admin")
	f := newFakeAPI(t)
	code, out, errOut := capture(t, func() int {
		return run([]string{"deny", "good.example", "--group", "kids", "--group", "1", "--comment", "c", "--url", f.srv.URL})
	})
	if code != 0 || !strings.Contains(out, "rule 9 added") {
		t.Fatalf("deny: %d %q %q", code, out, errOut)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(f.last(t, "POST", "/api/v1/filter/rules").body), &body); err != nil {
		t.Fatal(err)
	}
	if body["action"] != "block" || body["type"] != "subtree" || body["pattern"] != "good.example" || body["comment"] != "c" ||
		fmt.Sprint(body["groupIds"]) != "[2 1]" || body["enabled"] != true {
		t.Errorf("body %v", body)
	}
	if _, _, _ = capture(t, func() int { return run([]string{"allow", "good.example", "--url", f.srv.URL}) }); true {
		if b := f.last(t, "POST", "/api/v1/filter/rules").body; strings.Contains(b, "groupIds") || !strings.Contains(b, `"action":"allow"`) {
			t.Errorf("without --group: %s", b)
		}
	}
	code, out, _ = capture(t, func() int { return run([]string{"allow", "dup.example", "--url", f.srv.URL}) })
	if code != 0 || !strings.Contains(out, "this rule already exists") {
		t.Errorf("existing rule: %d %q", code, out)
	}
	code, _, errOut = capture(t, func() int { return run([]string{"allow", "synced.example", "--url", f.srv.URL}) })
	if code != 1 || !strings.Contains(errOut, "synced from") {
		t.Errorf("synced: %d %q", code, errOut)
	}
	code, _, errOut = capture(t, func() int { return run([]string{"allow", "x.example", "--group", "Staff", "--url", f.srv.URL}) })
	if code != 1 || !strings.Contains(errOut, "no group named Staff") {
		t.Errorf("unknown group: %d %q", code, errOut)
	}
}

func TestConfigCommands(t *testing.T) {
	cliEnv(t)
	t.Setenv("PICACHE_TOKEN", "pc_admin")
	f := newFakeAPI(t)
	code, out, _ := capture(t, func() int { return run([]string{"config", "get", "--url", f.srv.URL}) })
	if code != 0 {
		t.Fatal(code)
	}
	for _, gone := range []string{"activeStoreId", "privacyLevel", "tokenSet", "passwordSet"} {
		if strings.Contains(out, gone) {
			t.Errorf("config get shows %s:\n%s", gone, out)
		}
	}
	if !strings.Contains(out, `"maxSizeBytes": 123456789012`) || !strings.Contains(out, `"seenRetentionDays": 30`) {
		t.Errorf("config get lost members:\n%s", out)
	}
	code, out, _ = capture(t, func() int { return run([]string{"config", "get", "sync", "--url", f.srv.URL}) })
	if code != 0 || strings.Contains(out, "tokenSet") || !strings.Contains(out, `"mode": "off"`) || strings.Contains(out, "logs") {
		t.Errorf("config get sync: %d\n%s", code, out)
	}
	if code, _, _ := capture(t, func() int { return run([]string{"config", "get", "nope", "--url", f.srv.URL}) }); code != 2 {
		t.Error("unknown section")
	}

	dir := t.TempDir()
	file := filepath.Join(dir, "logs.json")
	if err := os.WriteFile(file, []byte(`{"seenRetentionDays":90,"privacyLevel":"full"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := capture(t, func() int { return run([]string{"config", "set", "logs", file, "--dry-run", "--url", f.srv.URL}) })
	if code != 0 || !strings.Contains(out, "nothing was stored") {
		t.Fatalf("set --dry-run: %d %q %q", code, out, errOut)
	}
	req := f.last(t, "PATCH", "/api/v1/settings/logs")
	if req.query != "dryRun=true" || strings.Contains(req.body, "privacyLevel") || !strings.Contains(req.body, `"seenRetentionDays": 90`) {
		t.Errorf("set request %+v", req)
	}
	whole := filepath.Join(dir, "all.json")
	if err := os.WriteFile(whole, []byte(`{"cache":{"activeStoreId":"nas1","maxSizeBytes":5},"sync":{"tokenSet":true,"mode":"off"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, _ = capture(t, func() int { return run([]string{"config", "apply", whole, "--url", f.srv.URL}) })
	if code != 0 || strings.TrimSpace(out) != "settings saved" {
		t.Fatalf("apply: %d %q", code, out)
	}
	req = f.last(t, "PUT", "/api/v1/settings")
	if req.query != "" || strings.Contains(req.body, "activeStoreId") || strings.Contains(req.body, "tokenSet") || !strings.Contains(req.body, "maxSizeBytes") {
		t.Errorf("apply request %+v", req)
	}
	// stdin
	r, w, _ := os.Pipe()
	oldIn := os.Stdin
	os.Stdin = r
	_, _ = w.Write([]byte(`{"enabled":true,"stratum":4}`))
	w.Close()
	code, _, _ = capture(t, func() int { return run([]string{"config", "set", "ntp", "-", "--url", f.srv.URL}) })
	os.Stdin = oldIn
	if code != 0 || !strings.Contains(f.last(t, "PATCH", "/api/v1/settings/ntp").body, `"stratum": 4`) {
		t.Errorf("stdin: %d", code)
	}
	// Refused inputs: duplicate names, a directory, an oversized file, a link.
	dup := filepath.Join(dir, "dup.json")
	_ = os.WriteFile(dup, []byte(`{"a":1,"a":2}`), 0o600)
	big := filepath.Join(dir, "big.json")
	_ = os.WriteFile(big, bytes.Repeat([]byte(" "), maxConfigFile+1), 0o600)
	for _, bad := range []string{dup, dir, big} {
		if code, _, _ := capture(t, func() int { return run([]string{"config", "apply", bad, "--url", f.srv.URL}) }); code != 1 {
			t.Errorf("%s: exit %d", bad, code)
		}
	}
	link := filepath.Join(dir, "link.json")
	if os.Symlink(whole, link) == nil {
		if code, _, _ := capture(t, func() int { return run([]string{"config", "apply", link, "--url", f.srv.URL}) }); code != 1 {
			t.Errorf("a link was read: exit %d", code)
		}
	}
	if code, _, _ := capture(t, func() int { return run([]string{"config", "set", "logs", "--url", f.srv.URL}) }); code != 2 {
		t.Error("set without a file")
	}
}

// listeners --reset removes the saved listeners; healthcheck, localHealth
// and the API client find a web listener that only listeners.json names.
func TestListenersFromFile(t *testing.T) {
	dir := cliEnv(t)
	t.Setenv("PICACHE_DNS_LISTEN", "")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			fmt.Fprint(w, "ok")
			return
		}
		fmt.Fprint(w, `{"logs":{"statsEnabled":false}}`)
	})}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	if err := config.WriteListenersFile(dir, config.ListenersFile,
		config.EncodeListeners(map[string][]string{config.RoleWeb: {":" + port}})); err != nil {
		t.Fatal(err)
	}
	if got := localURL(); got != "http://127.0.0.1:"+port+"/healthz" {
		t.Fatalf("localURL %s", got)
	}
	if err := webCheck(context.Background(), localURL(), true); err != nil {
		t.Fatal(err)
	}
	if got := baseURL(""); got != "http://127.0.0.1:"+port {
		t.Fatalf("baseURL %s", got)
	}
	// The first address on all addresses or loopback wins over a LAN
	// address listed first (a token goes over plain http to loopback
	// only), web before webTls; the LAN address only without one.
	for _, tc := range []struct {
		roles map[string][]string
		want  string
	}{
		{map[string][]string{config.RoleWeb: {"192.168.1.5:8080", "127.0.0.1:" + port}}, "http://127.0.0.1:" + port + "/healthz"},
		{map[string][]string{config.RoleWeb: {"192.168.1.5:8080"}, config.RoleWebTLS: {":8443"}}, "https://127.0.0.1:8443/healthz"},
		{map[string][]string{config.RoleWeb: {"192.168.1.5:8080"}, config.RoleWebTLS: {"[::1]:8443"}}, "https://[::1]:8443/healthz"},
		{map[string][]string{config.RoleWeb: {"192.168.1.5:8080"}, config.RoleWebTLS: {"192.168.1.5:8443"}}, "http://192.168.1.5:8080/healthz"},
	} {
		if err := config.WriteListenersFile(dir, config.ListenersFile, config.EncodeListeners(tc.roles)); err != nil {
			t.Fatal(err)
		}
		if got := localURL(); got != tc.want {
			t.Errorf("%v: localURL %s", tc.roles, got)
		}
	}
	if err := config.WriteListenersFile(dir, config.ListenersFile,
		config.EncodeListeners(map[string][]string{config.RoleWeb: {":" + port}})); err != nil {
		t.Fatal(err)
	}
	// The environment wins over the file.
	t.Setenv("PICACHE_WEB_LISTEN", "127.0.0.1:9")
	if got := localURL(); got != "http://127.0.0.1:9/healthz" {
		t.Fatalf("env: %s", got)
	}
	t.Setenv("PICACHE_WEB_LISTEN", "")
	// PICACHE_RUN_AS ignores the file.
	t.Setenv("PICACHE_RUN_AS", "65532:65532")
	if got := localURL(); got != "http://127.0.0.1:8080/healthz" {
		t.Fatalf("run as: %s", got)
	}
	t.Setenv("PICACHE_RUN_AS", "")

	if err := config.WriteListenersFile(dir, config.ListenersNextFile,
		config.EncodeListeners(map[string][]string{config.RoleWeb: {":8081"}})); err != nil {
		t.Fatal(err)
	}
	code, out, _ := capture(t, func() int { return run([]string{"listeners", "--reset"}) })
	if code != 0 || !strings.Contains(out, "the listeners use the environment and the defaults from the next start") {
		t.Fatalf("reset: %d %q", code, out)
	}
	for _, n := range []string{config.ListenersFile, config.ListenersNextFile} {
		if _, err := os.Stat(filepath.Join(dir, n)); !os.IsNotExist(err) {
			t.Errorf("%s kept", n)
		}
	}
	if code, _, _ := capture(t, func() int { return run([]string{"listeners"}) }); code != 2 {
		t.Error("listeners without --reset")
	}
	if err := os.WriteFile(filepath.Join(dir, "picache.db"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, _ = capture(t, func() int { return run([]string{"web-access", "--reset"}) })
	if code != 0 || !strings.Contains(out, "picache listeners --reset") {
		t.Errorf("web-access --reset does not mention the listeners: %q", out)
	}
}

// restore: a section error is a usage error, a missing file an error.
func TestRestoreCommandArgs(t *testing.T) {
	cliEnv(t)
	for _, bad := range [][]string{{"restore"}, {"restore", "x.db", "--sections", "nope"}, {"restore", "x.db", "--sections", ""},
		{"restore", "x.db", "--sections", "clients-and-groups"}} {
		if code, _, _ := capture(t, func() int { return run(bad) }); code != 2 {
			t.Errorf("%v: exit %d", bad, code)
		}
	}
	code, _, errOut := capture(t, func() int { return run([]string{"restore", filepath.Join(t.TempDir(), "missing.db")}) })
	if code != 1 || errOut == "" {
		t.Errorf("missing file: %d %q", code, errOut)
	}
	// A refused read: as root (already switched to the owner of the data
	// directory) the hint names the file and the owner, never sudo.
	err := &os.PathError{Op: "open", Path: "/root/b\x1b]0;x.db", Err: os.ErrPermission}
	if h := restorePermissionHint(err, true); strings.Contains(h, "sudo") || !strings.Contains(h, "/root/b\\u001b]0;x.db") ||
		!strings.Contains(h, "owner of the data directory") {
		t.Errorf("root hint %q", h)
	}
	if h := restorePermissionHint(err, false); !strings.Contains(h, "sudo picache restore") || !strings.Contains(h, "permission denied") {
		t.Errorf("user hint %q", h)
	}
}
