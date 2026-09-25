package services

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hustenreizjuengling/picache/internal/apperr"
)

func TestValidatePattern(t *testing.T) {
	tests := []struct {
		p  string
		ok bool
	}{
		{"example.com", true},
		{"*.example.com", true},
		{"*.cdn.blizzard.com", true},
		{"lancache.steamcontent.com", true},
		{"xn--bcher-kva.example", true},
		{"a-b.example.com", true},
		{"Example.COM", true}, // case-insensitive
		{"", false},
		{"*", false},
		{"*.", false},
		{"com", false},
		{"*.com", false},         // public suffix
		{"co.uk", false},         // public suffix
		{"*.co.uk", false},       // public suffix
		{"github.io", false},     // private-section public suffix
		{"*.github.io", false},   // would cover every GitHub Pages site
		{"user.github.io", true}, // below a public suffix
		{"foo*.example.com", false},
		{"*foo.example.com", false},
		{"a.*.example.com", false},
		{"**.example.com", false},
		{"1.2.3.4", false},
		{"example.123", false},
		{"-a.example.com", false},
		{"a-.example.com", false},
		{"a..example.com", false},
		{".example.com", false},
		{"example.com.", false}, // callers normalise first
		{"foo bar.com", false},
		{"foo_bar.example.com", false},
		{"ex\x00ample.com", false},
		{strings.Repeat("a", 64) + ".com", false},
		{strings.Repeat("a.", 127) + "com", false},
	}
	for _, tt := range tests {
		err := ValidatePattern(tt.p)
		if (err == nil) != tt.ok {
			t.Errorf("ValidatePattern(%q) = %v, want ok=%v", tt.p, err, tt.ok)
		}
	}
}

func TestParseDomainFile(t *testing.T) {
	var sk skipList
	in := "# header\r\n\r\n  Example.COM.  \r\n*.cdn.example.net\r\nexample.com\n*.co.uk\nbad host.example\n# trailing"
	got, err := parseDomainFile("x.txt", []byte(in), &sk)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"example.com", "*.cdn.example.net"}; !slices.Equal(got, want) {
		t.Fatalf("patterns = %q, want %q", got, want)
	}
	if len(sk.items) != 2 || !strings.Contains(sk.items[0], "public suffix") || !strings.Contains(sk.items[1], "x.txt") {
		t.Fatalf("skipped = %q", sk.items)
	}

	if _, err := parseDomainFile("big.txt", make([]byte, maxFileBytes+1), &sk); err == nil {
		t.Fatal("oversized file accepted")
	}
	many := strings.Repeat("# c\n", maxFileLines) + "example.com\n"
	if _, err := parseDomainFile("many.txt", []byte(many), &sk); err == nil || !skippableFileError(err) {
		t.Fatalf("too many lines: err = %v", err)
	}
	exact := strings.Repeat("# c\n", maxFileLines-1) + "example.com\n"
	if got, err := parseDomainFile("ok.txt", []byte(exact), &sk); err != nil || len(got) != 1 {
		t.Fatalf("file with exactly %d lines: %v %v", maxFileLines, got, err)
	}
}

func TestParseIndex(t *testing.T) {
	entry := func(name string, files ...string) string {
		b, err := json.Marshal(indexEntry{Name: name, DomainFiles: files})
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	doc := func(entries ...string) []byte {
		return []byte(`{"cache_domains":[` + strings.Join(entries, ",") + `]}`)
	}

	var sk skipList
	got, err := parseIndex(doc(
		entry("steam", "steam.txt"),
		entry("Bad", "a.txt"),
		entry("../etc", "a.txt"),
		entry(strings.Repeat("a", 33), "a.txt"),
		entry("custom-x", "a.txt"),
		entry("steam", "dup.txt"),
		entry("trav", "../passwd.txt", "a/b.txt", ".hidden.txt", "x.txt\x00", "%2e%2e.txt", "c:\\x.txt", "ok.txt", "ok.txt"),
		entry("nofiles", "nope"),
	), &sk)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, e := range got {
		ids = append(ids, e.Name)
	}
	if !slices.Equal(ids, []string{"steam", "trav"}) {
		t.Fatalf("ids = %q", ids)
	}
	if files := got[1].DomainFiles; !slices.Equal(files, []string{"ok.txt"}) {
		t.Fatalf("trav files = %q", files)
	}
	if len(sk.items) < 11 {
		t.Fatalf("expected every rejection to be reported, got %q", sk.items)
	}

	tooMany := make([]string, maxIndexServices+1)
	for i := range tooMany {
		tooMany[i] = entry(fmt.Sprintf("s%d", i), "a.txt")
	}
	manyFiles := make([]string, maxFilesPerService+2)
	for i := range manyFiles {
		manyFiles[i] = fmt.Sprintf("f%d.txt", i)
	}
	for name, in := range map[string][]byte{
		"invalid json":   []byte(`{"cache_domains":`),
		"duplicate keys": []byte(`{"cache_domains":[],"cache_domains":[]}`),
		"empty":          doc(),
		"too many":       doc(tooMany...),
		"oversized":      append(doc(entry("steam", "steam.txt")), make([]byte, maxIndexBytes)...),
		"none valid":     doc(entry("BAD", "a.txt")),
	} {
		if _, err := parseIndex(in, &skipList{}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	sk = skipList{}
	got, err = parseIndex(doc(entry("many", manyFiles...)), &sk)
	if err != nil || len(got[0].DomainFiles) != maxFilesPerService {
		t.Fatalf("file cap: %v %v", got, err)
	}
}

func TestSkipListBounded(t *testing.T) {
	var sk skipList
	for i := range maxSkipped + 5 {
		sk.add("item %d", i)
	}
	l := sk.list()
	if len(l) != maxSkipped+1 || !strings.Contains(l[maxSkipped], "5 more") {
		t.Fatalf("list = %d entries, last %q", len(l), l[len(l)-1])
	}
}

func TestRefreshBuildsCatalogue(t *testing.T) {
	r, e := loadedRegistry(t)
	st := r.Status()
	if !st.Ready || st.Error != "" || st.ServiceCount != 6 || st.LastFetched.IsZero() || st.Source != testSource {
		t.Fatalf("status = %+v", st)
	}
	// epicgames: both files, de-duplicated; blizzard: CRLF, comment, case, dot.
	ep, err := r.Service(context.Background(), "epicgames")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"download.epicgames.com", "cdn1.epicgames.com", "egdownload.fastly-edge.com"}; !slices.Equal(ep.Domains, want) {
		t.Fatalf("epic domains = %q", ep.Domains)
	}
	bz, _ := r.Service(context.Background(), "blizzard")
	if want := []string{"*.cdn.blizzard.com", "level3.blizzard.com", "dist.blizzard.com"}; !slices.Equal(bz.Domains, want) {
		t.Fatalf("blizzard domains = %q", bz.Domains)
	}
	og, _ := r.Service(context.Background(), "origin")
	if !og.MixedContent || og.Notes != "HTTP only" || og.Name != "EA (Origin)" {
		t.Fatalf("origin = %+v", og)
	}
	// The public-suffix pattern and the malformed one are reported.
	joined := strings.Join(st.Skipped, "\n")
	if !strings.Contains(joined, `"*.com"`) || !strings.Contains(joined, `"foo bar.com"`) {
		t.Fatalf("skipped = %q", st.Skipped)
	}
	// test is disabled by default, everything else enabled.
	list, _ := r.Services(context.Background())
	for _, s := range list {
		if s.Enabled != (s.ID != "test") {
			t.Errorf("%s enabled = %v", s.ID, s.Enabled)
		}
	}
	if _, err := os.Stat(filepath.Join(e.dir, snapshotFile)); err != nil {
		t.Fatalf("snapshot not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.dir, snapshotTemp)); !os.IsNotExist(err) {
		t.Fatalf("temp file left behind: %v", err)
	}
}

func TestOfflineStartUsesSnapshot(t *testing.T) {
	_, e := loadedRegistry(t)
	e.cdn.setDown(true)
	r2 := e.registry(t)
	st := r2.Status()
	if !st.Ready || st.ServiceCount != 6 || st.LastFetched.IsZero() || !st.LastAttempt.Equal(st.LastFetched) {
		t.Fatalf("offline status = %+v", st)
	}
	if id, ok := r2.MatchDNS("us.cdn.blizzard.com"); !ok || id != "blizzard" {
		t.Fatalf("MatchDNS from snapshot = %q %v", id, ok)
	}
	// A failed refresh keeps the snapshot and reports the error.
	err := r2.Refresh(context.Background())
	if apperr.KindOf(err) != apperr.KindUnavailable {
		t.Fatalf("Refresh err = %v", err)
	}
	st = r2.Status()
	if !st.Ready || st.Error == "" || st.LastAttempt.Before(st.LastFetched) {
		t.Fatalf("status after failure = %+v", st)
	}
	if _, ok := r2.MatchDNS("us.cdn.blizzard.com"); !ok {
		t.Fatal("snapshot lost after failed refresh")
	}
}

func TestNoSnapshotMeansNoOverrides(t *testing.T) {
	e := newEnv(t, newFakeCDN(nil))
	r := e.registry(t)
	if st := r.Status(); st.Ready || st.Error == "" {
		t.Fatalf("status = %+v", st)
	}
	if _, ok := r.MatchDNS(SteamTrigger); ok {
		t.Fatal("overrides must stay inactive without a snapshot")
	}
	// The Steam User-Agent rule and SNI membership do not need the source.
	if id, en, known := r.Classify("cache1.steamcontent.com", "x "+SteamUserAgentSuffix, "/depot/1/chunk/x"); id != "steam" || !en || !known {
		t.Fatalf("Classify = %q %v %v", id, en, known)
	}
}

func TestCorruptSnapshotIgnored(t *testing.T) {
	e := newEnv(t, newFakeCDN(nil))
	if err := os.MkdirAll(e.dir, 0o750); err != nil {
		t.Fatal(err)
	}
	bad := `{"format":1,"source":"x","services":[{"id":"../x","domains":["*.com"]}]}`
	if err := os.WriteFile(filepath.Join(e.dir, snapshotFile), []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	r := e.registry(t)
	if r.Status().Ready {
		t.Fatal("invalid snapshot loaded")
	}
	mixed := `{"format":1,"source":"x","services":[{"id":"ok","domains":["good.example.com","*.com","UPPER.example.com","good.example.com"]},{"id":"Bad!","domains":["x.example.com"]}]}`
	if err := os.WriteFile(filepath.Join(e.dir, snapshotFile), []byte(mixed), 0o600); err != nil {
		t.Fatal(err)
	}
	r = e.registry(t)
	sv, err := r.Service(context.Background(), "ok")
	if err != nil || !slices.Equal(sv.Domains, []string{"good.example.com"}) {
		t.Fatalf("re-validated snapshot: %+v %v", sv, err)
	}
	if _, err := r.Service(context.Background(), "Bad!"); err == nil {
		t.Fatal("invalid service loaded from snapshot")
	}
}

func TestRefreshFailures(t *testing.T) {
	tests := []struct {
		name  string
		setup func(f *fakeCDN)
		want  string
	}{
		{"redirect", func(f *fakeCDN) {
			f.set("/cache-domains/cache_domains.json", fakeResp{status: http.StatusFound, location: "https://evil.test/x.json"})
		}, "redirects are not followed"},
		{"index oversized", func(f *fakeCDN) {
			f.set("/cache-domains/cache_domains.json", fakeResp{status: 200, body: strings.Repeat(" ", maxIndexBytes+1)})
		}, "larger than"},
		{"server error on a file", func(f *fakeCDN) {
			f.set("/cache-domains/steam.txt", fakeResp{status: http.StatusBadGateway})
		}, "HTTP 502"},
		{"transport error", func(f *fakeCDN) { f.setDown(true) }, "GET https://cdn.test/cache-domains/cache_domains.json"},
		{"no usable services", func(f *fakeCDN) {
			f.set("/cache-domains/cache_domains.json", fakeResp{status: 200, body: `{"cache_domains":[{"name":"x","domain_files":["x.txt"]}]}`})
			f.set("/cache-domains/x.txt", fakeResp{status: 200, body: "*.com\n"})
		}, "no usable services"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cdn := newFakeCDN(standardSource())
			tt.setup(cdn)
			e := newEnv(t, cdn)
			r := e.registry(t)
			err := r.Refresh(context.Background())
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
			if st := r.Status(); st.Ready || !strings.Contains(st.Error, tt.want) {
				t.Fatalf("status = %+v", st)
			}
			if cdn.count("/x.json") != 0 {
				t.Fatal("redirect followed")
			}
		})
	}
}

func TestRefreshSkipsMissingAndRejectedFiles(t *testing.T) {
	src := standardSource()
	delete(src, "/cache-domains/epic-extra.txt") // 404 → skipped, the rest is used
	src["/cache-domains/origin.txt"] = strings.Repeat("x", maxFileBytes+1)
	e := newEnv(t, newFakeCDN(src))
	r := e.registry(t)
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	st := r.Status()
	joined := strings.Join(st.Skipped, "\n")
	if !strings.Contains(joined, "epic-extra.txt: GET") || !strings.Contains(joined, "origin.txt: larger than") ||
		!strings.Contains(joined, "service origin: no valid domains") {
		t.Fatalf("skipped = %q", st.Skipped)
	}
	if _, err := r.Service(context.Background(), "origin"); err == nil {
		t.Fatal("service without domains listed")
	}
	if id, ok := r.MatchDNS("download.epicgames.com"); !ok || id != "epicgames" {
		t.Fatal("epicgames lost")
	}
}

func TestRedactURL(t *testing.T) {
	got := redactURL("https://user:pw@example.com/a/b.txt?token=secret#frag")
	if got != "https://example.com/a/b.txt" {
		t.Fatalf("redactURL = %q", got)
	}
}

func TestCleanText(t *testing.T) {
	if got := cleanText("  a\x00b\nc\xffd  ", 100); got != "a b cd" {
		t.Fatalf("cleanText = %q", got)
	}
	if got := cleanText("ääää", 3); got != "ä" {
		t.Fatalf("cleanText cut = %q", got)
	}
}
