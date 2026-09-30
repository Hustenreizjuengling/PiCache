package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// logs export passes every --dnssec-status; the usage texts name it.
func TestLogsExportDNSSECStatus(t *testing.T) {
	cliEnv(t)
	t.Setenv("PICACHE_TOKEN", "pc_export")
	var query atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query.Store(r.URL.RawQuery)
		fmt.Fprint(w, "time,clientIp\n")
	}))
	defer srv.Close()
	out := filepath.Join(t.TempDir(), "q.csv")
	args := []string{"logs", "export", "--format", "csv", "--dnssec-status", "bogus", "--dnssec-status", "indeterminate",
		"--out", out, "--url", srv.URL}
	if code, _, errOut := capture(t, func() int { return run(args) }); code != 0 {
		t.Fatalf("export: %d %q", code, errOut)
	}
	q, _ := neturl.ParseQuery(query.Load().(string))
	if got := q["dnssecStatus"]; len(got) != 2 || got[0] != "bogus" || got[1] != "indeterminate" {
		t.Fatalf("query %v", q)
	}
	if !strings.Contains(logsUsage, "[--dnssec-status S]...") || !strings.Contains(usage, "[--dnssec-status S]...") {
		t.Fatal("the usage texts do not name --dnssec-status")
	}
}

// picache query prints the DNSSEC status (and its reason) after the
// upstream line.
func TestQueryPrintsDNSSEC(t *testing.T) {
	got := renderLookup(&lookupAnswer{Name: "bad.example", Type: "A", Status: "error", RCode: "SERVFAIL",
		Reason: "dnssec: example: bad signature", Upstream: "https://dns.quad9.net/dns-query", DNSSECStatus: "bogus",
		DNSSECReason: "bad signature"})
	if !strings.Contains(got, "upstream: https://dns.quad9.net/dns-query\ndnssec: bogus (bad signature)\n") {
		t.Fatalf("output %q", got)
	}
	got = renderLookup(&lookupAnswer{Name: "www.example", Type: "A", Status: "forwarded", DNSSECStatus: "secure"})
	if !strings.Contains(got, "dnssec: secure\n") {
		t.Fatalf("output %q", got)
	}
	if got = renderLookup(&lookupAnswer{Name: "x", Type: "A", Status: "local"}); strings.Contains(got, "dnssec") {
		t.Fatalf("output %q", got)
	}
}
