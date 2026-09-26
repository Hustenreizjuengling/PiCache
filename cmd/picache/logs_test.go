package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// cliEnv isolates the CLI from the host's configuration.
func cliEnv(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PICACHE_ENV_FILE", "")
	t.Setenv("PICACHE_DATA_DIR", dir)
	t.Setenv("PICACHE_TOKEN", "")
	t.Setenv("PICACHE_URL", "")
	t.Setenv("PICACHE_WEB_LISTEN", "")
	return dir
}

// The token goes only to a loopback http URL or to an https URL; a missing
// token is a usage error.
func TestAPIClientTokenPolicy(t *testing.T) {
	dir := cliEnv(t)
	if _, usage, err := newAPIClient("http://127.0.0.1:8080", ""); err == nil || !usage {
		t.Fatalf("without a token: usage %v, %v", usage, err)
	}
	t.Setenv("PICACHE_TOKEN", "pc_secret")
	for _, tc := range []struct {
		url     string
		refused bool
		usage   bool
	}{
		{"http://127.0.0.1:8080", false, false},
		{"http://[::1]:8080/", false, false},
		{"http://localhost:8080", false, false},
		{"https://picache.example:8443", false, false},
		{"https://127.0.0.1:8443", false, false},
		{"http://192.168.1.10:8080", true, false},
		{"http://picache.lan", true, false},
		{"http://localhost.evil.example", true, false},
		{"ftp://127.0.0.1", false, true},
		{"http://user:pw@127.0.0.1", false, true},
		{"127.0.0.1:8080", false, true},
	} {
		c, usage, err := newAPIClient(tc.url, "")
		var ref errRefused
		refused := err != nil && strings.Contains(err.Error(), "refusing to send the API token to")
		if refused != tc.refused || usage != tc.usage || (err == nil) != (c != nil) {
			t.Errorf("%s: refused %v usage %v err %v (%T %v)", tc.url, refused, usage, err, err, ref)
		}
	}
	// PICACHE_URL, else the local listener.
	t.Setenv("PICACHE_URL", "http://127.0.0.1:9999")
	if c, _, err := newAPIClient("", ""); err != nil || c.base.String() != "http://127.0.0.1:9999" {
		t.Fatalf("PICACHE_URL: %v %v", c, err)
	}
	t.Setenv("PICACHE_URL", "")
	t.Setenv("PICACHE_WEB_LISTEN", ":8081")
	if c, _, err := newAPIClient("", ""); err != nil || c.base.String() != "http://127.0.0.1:8081" {
		t.Fatalf("local listener: %v %v", c, err)
	}
	// An https loopback URL is verified when PiCache's CA is readable.
	if c, _, _ := newAPIClient("https://127.0.0.1:8443", ""); !c.http.Transport.(*http.Transport).TLSClientConfig.InsecureSkipVerify {
		t.Fatal("loopback https without the CA must not verify")
	}
	if err := os.MkdirAll(filepath.Join(dir, "tls"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tls", "ca.crt"), testCAPEM(t), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, _, _ := newAPIClient("https://127.0.0.1:8443", ""); c.http.Transport.(*http.Transport).TLSClientConfig.InsecureSkipVerify {
		t.Fatal("loopback https with the CA must verify")
	}
}

// testCAPEM returns a self-signed CA certificate.
func testCAPEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// The token file: a regular file, not a link, at most 4 KiB, trimmed.
func TestReadTokenFile(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "token")
	if err := os.WriteFile(good, []byte("  pc_abc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if tok, err := readTokenFile(good); err != nil || tok != "pc_abc" {
		t.Fatalf("token %q, %v", tok, err)
	}
	big := filepath.Join(dir, "big")
	if err := os.WriteFile(big, make([]byte, maxTokenFile+1), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{big, empty, dir, filepath.Join(dir, "missing")} {
		if _, err := readTokenFile(p); err == nil {
			t.Errorf("%s accepted", p)
		}
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(good, link); err == nil && runtime.GOOS == "linux" {
		if _, err := readTokenFile(link); err == nil {
			t.Error("a symbolic link was followed")
		}
	}
}

// PICACHE_TOKEN is never taken from the env file.
func TestEnvFileSkipsToken(t *testing.T) {
	cliEnv(t)
	os.Unsetenv("PICACHE_TOKEN")
	os.Unsetenv("PICACHE_LOG_FORMAT")
	t.Cleanup(func() { os.Unsetenv("PICACHE_TOKEN"); os.Unsetenv("PICACHE_LOG_FORMAT") })
	f := filepath.Join(t.TempDir(), "picache.env")
	if err := os.WriteFile(f, []byte("PICACHE_TOKEN=pc_from_file\nPICACHE_LOG_FORMAT=json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PICACHE_ENV_FILE", f)
	_, _, errOut := capture(t, func() int {
		if err := loadEnvFile(); err != nil {
			t.Error(err)
		}
		return 0
	})
	if _, set := os.LookupEnv("PICACHE_TOKEN"); set || os.Getenv("PICACHE_LOG_FORMAT") != "json" {
		t.Fatalf("PICACHE_TOKEN applied from the env file (or others not)")
	}
	if !strings.Contains(errOut, "PICACHE_TOKEN in "+f+" is ignored") {
		t.Fatalf("no warning: %q", errOut)
	}
}

func TestEscapeControls(t *testing.T) {
	if got := escapeControls("a\x1b[31mb\u202ec\x7fd\u0085e\u2066f\n"); got != `a\u001b[31mb\u202ec\u007fd\u0085e\u2066f\u000a` {
		t.Fatalf("escaped %q", got)
	}
}

// tail: one line per query (escaped), reconnect with a backoff on 5xx, 429
// and the end of the stream, exit 1 on 401/403 with the server's message.
func TestRunTail(t *testing.T) {
	cliEnv(t)
	t.Setenv("PICACHE_TOKEN", "pc_tail")
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer pc_tail" || r.URL.Path != "/api/v1/stream/queries" ||
			r.URL.Query()["status"][0] != "blocked" {
			http.Error(w, "bad request", 400)
			return
		}
		switch n.Add(1) {
		case 1:
			w.WriteHeader(503)
			fmt.Fprint(w, `{"error":{"code":"unavailable","message":"logs.db is unavailable"}}`)
		case 2:
			w.WriteHeader(429)
		case 3:
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, ": ping\n\nevent: query\ndata: {\"time\":\"2026-09-26T10:00:00Z\",\"clientIp\":\"10.0.0.1\",\"clientName\":\"pc\",\"qname\":\"evil\u202e\\u001b.example\",\"qtype\":\"A\",\"status\":\"blocked-list\",\"rcode\":\"NOERROR\",\"durationUs\":1500}\n\n")
		default:
			w.WriteHeader(403)
			fmt.Fprint(w, `{"error":{"code":"forbidden","message":"the token was revoked"}}`)
		}
	}))
	defer srv.Close()
	c, _, err := newAPIClient(srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	var slept []time.Duration
	sleep := func(_ context.Context, d time.Duration) bool { slept = append(slept, d); return true }
	var out strings.Builder
	code, _, errOut := capture(t, func() int {
		return runTail(context.Background(), c, neturl.Values{"status": {"blocked"}}, false, &out, sleep)
	})
	if code != 1 || !strings.Contains(errOut, "the token was revoked") || !strings.Contains(errOut, "logs.db is unavailable") {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	if fmt.Sprint(slept) != "[1s 2s 1s]" {
		t.Fatalf("backoff %v", slept)
	}
	line := out.String()
	if !strings.Contains(line, `10.0.0.1 (pc)  A  evil\u202e\u001b.example  blocked-list  NOERROR  1.5 ms`) {
		t.Fatalf("line %q", line)
	}
	// The backoff doubles to 30 s; the end of the context ends tail with 0.
	var calls atomic.Int32
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(502) }))
	defer down.Close()
	c, _, _ = newAPIClient(down.URL, "")
	slept = nil
	ctx, cancel := context.WithCancel(context.Background())
	sleep = func(_ context.Context, d time.Duration) bool {
		slept = append(slept, d)
		if len(slept) == 8 {
			cancel()
			return false
		}
		return true
	}
	code, _, _ = capture(t, func() int { return runTail(ctx, c, nil, true, &out, sleep) })
	if code != 0 || fmt.Sprint(slept) != "[1s 2s 4s 8s 16s 30s 30s 30s]" {
		t.Fatalf("exit %d, backoff %v", code, slept)
	}
}

// export: the output file is created exclusively; a truncation marker is
// reported on stderr; the server's error ends it with 1.
func TestLogsExportCommand(t *testing.T) {
	cliEnv(t)
	t.Setenv("PICACHE_TOKEN", "pc_export")
	var query atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query.Store(r.URL.RawQuery)
		if r.URL.Query().Get("format") == "csv" {
			fmt.Fprint(w, "time,clientIp\n2026,10.0.0.1\n# truncated: rows\n")
			return
		}
		w.WriteHeader(400)
		fmt.Fprint(w, `{"error":{"code":"invalid","message":"must be ndjson or csv","field":"format"}}`)
	}))
	defer srv.Close()
	out := filepath.Join(t.TempDir(), "q.csv")
	args := []string{"logs", "export", "--format", "csv", "--range", "7d", "--rcode", "NXDOMAIN", "--rcode", "SERVFAIL",
		"--client", "10.0.0.1", "--dnssec", "true", "--out", out, "--url", srv.URL}
	code, _, errOut := capture(t, func() int { return run(args) })
	if code != 0 || !strings.Contains(errOut, "truncated (rows)") {
		t.Fatalf("export: %d %q", code, errOut)
	}
	q, _ := neturl.ParseQuery(query.Load().(string))
	if q.Get("range") != "7d" || len(q["rcode"]) != 2 || q.Get("dnssec") != "true" || q.Get("client") != "10.0.0.1" {
		t.Fatalf("query %v", q)
	}
	b, err := os.ReadFile(out)
	if err != nil || !strings.HasPrefix(string(b), "time,clientIp") {
		t.Fatalf("file %q %v", b, err)
	}
	if fi, _ := os.Stat(out); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
	// An existing file is refused.
	code, _, errOut = capture(t, func() int { return run(args) })
	if code != 1 || !strings.Contains(errOut, "exists") {
		t.Fatalf("existing file: %d %q", code, errOut)
	}
	// The server's error; the incomplete file is removed.
	out2 := filepath.Join(t.TempDir(), "q.json")
	code, _, errOut = capture(t, func() int {
		return run([]string{"logs", "export", "--format", "ndjson", "--out", out2, "--url", srv.URL})
	})
	if code != 1 || !strings.Contains(errOut, "format: must be ndjson or csv") {
		t.Fatalf("server error: %d %q", code, errOut)
	}
	if _, err := os.Stat(out2); err == nil {
		t.Fatal("the incomplete file was kept")
	}
	for _, bad := range [][]string{
		{"logs", "export", "--out", "-"},
		{"logs", "export", "--format", "csv"},
		{"logs", "export", "--format", "csv", "--out", "-", "--range", "1h", "--from", "0"},
		{"logs", "export", "--format", "csv", "--out", "-", "--dnssec", "yes"},
		{"logs", "tail", "extra"},
		{"logs"},
		{"logs", "nope"},
	} {
		if code, _, _ := capture(t, func() int { return run(bad) }); code != 2 {
			t.Errorf("%v: exit %d", bad, code)
		}
	}
}

// A redirect (e.g. "Redirect HTTP to HTTPS" on the web listener) is never
// followed; tail and export end with 1 and name the address to use.
func TestAPIRedirectNamesTarget(t *testing.T) {
	cliEnv(t)
	t.Setenv("PICACHE_TOKEN", "pc_redirect")
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Redirect(w, r, "https://127.0.0.1:8443"+r.URL.RequestURI(), http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	want := "HTTP 307: PiCache redirects to https://127.0.0.1:8443; pass that address with --url or PICACHE_URL"
	code, _, errOut := capture(t, func() int {
		return run([]string{"logs", "export", "--format", "csv", "--out", "-", "--url", srv.URL})
	})
	if code != 1 || !strings.Contains(errOut, want) || calls.Load() != 1 {
		t.Fatalf("export: exit %d, %d calls, stderr %q", code, calls.Load(), errOut)
	}
	c, _, err := newAPIClient(srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	code, _, errOut = capture(t, func() int {
		return runTail(context.Background(), c, nil, false, &out, func(context.Context, time.Duration) bool { return true })
	})
	if code != 1 || !strings.Contains(errOut, want) || calls.Load() != 2 {
		t.Fatalf("tail: exit %d, %d calls, stderr %q", code, calls.Load(), errOut)
	}
}

func TestTruncationMarker(t *testing.T) {
	for line, want := range map[string]string{
		`{"truncated":true,"reason":"time"}`: "time", "# truncated: rows": "rows",
		`{"qname":"x"}`: "", `{"truncated":false,"reason":"rows"}`: "", "": "",
	} {
		if got := truncationMarker(line); got != want {
			t.Errorf("%q: %q", line, got)
		}
	}
}
