package filter

import (
	"context"
	"errors"
	"fmt"
	"regexp/syntax"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestValidDomain(t *testing.T) {
	for d, want := range map[string]bool{
		"example.com": true, "a.b-c.example": true, "_dmarc.example.com": true, "xn--bcher-kva.de": true,
		"com": true, "-rtb.example.com": true, "": false, "a..b": false, ".a.b": false, "a.b.": false,
		"1.2.3.4": false, "example.com-": false, "example.-com": false, "ex ample.com": false,
		"ex*ample.com": false, "üml.de": false, strings.Repeat("a", 64) + ".com": false,
		strings.Repeat("a.", 127) + "com": false, strings.Repeat("a", 63) + ".com": true,
	} {
		if got := validDomain(d); got != want {
			t.Errorf("validDomain(%q) = %v, want %v", d, got, want)
		}
	}
}

func TestSuffixHashes(t *testing.T) {
	var sf suffixHashes
	sf.compute("www.example.com")
	want := []string{"com", "example.com", "www.example.com"}
	if sf.n != len(want) {
		t.Fatalf("n = %d", sf.n)
	}
	for i, w := range want {
		if sf.h[i] != hashName(w) {
			t.Errorf("suffix %d: hash mismatch for %q", i, w)
		}
	}
}

// parseCase describes the expected result of parsing one line.
type parseCase struct {
	line    string
	subtree bool // list plainDomains = subtree
	status  lineStatus
	want    []entry // kind, allow, important, badfilter, domain, re (only these are compared)
}

func TestParseLine(t *testing.T) {
	ex := func(d string) entry { return entry{kind: kindExact, domain: d} }
	sub := func(d string) entry { return entry{kind: kindSubtree, domain: d} }
	pat := func(re string) entry { return entry{kind: kindPattern, re: re} }
	cases := []parseCase{
		// StevenBlack (hosts)
		{line: "0.0.0.0 ads.example.com", status: lineOK, want: []entry{ex("ads.example.com")}},
		{line: "0.0.0.0\tads.example.com  tracker.example.org # comment", status: lineOK, want: []entry{ex("ads.example.com"), ex("tracker.example.org")}},
		{line: "127.0.0.1 localhost", status: lineSkip},
		{line: "127.0.0.1 localhost.localdomain", status: lineSkip},
		{line: "255.255.255.255 broadcasthost", status: lineSkip},
		{line: "::1 ip6-localhost ip6-loopback", status: lineSkip},
		{line: "fe80::1%lo0 localhost", status: lineSkip},
		{line: "ff02::2 ip6-allrouters", status: lineSkip},
		{line: "0.0.0.0 0.0.0.0", status: lineSkip},
		{line: "# Title: StevenBlack/hosts", status: lineSkip},
		{line: "0.0.0.0 Ads.Example.COM.", status: lineOK, want: []entry{ex("ads.example.com")}},
		{line: "0.0.0.0 intranet", status: lineInvalid},
		{line: "0.0.0.0", status: lineInvalid},
		{line: "example.com also.example.com", status: lineInvalid},
		// OISD / HaGeZi / 1Hosts (ABP)
		{line: "[Adblock Plus]", status: lineSkip},
		{line: "! Title: oisd big", status: lineSkip},
		{line: "||0-02.net^", status: lineOK, want: []entry{sub("0-02.net")}},
		{line: "||ad.001zb.com^", status: lineOK, want: []entry{sub("ad.001zb.com")}},
		{line: "||zip^", status: lineOK, want: []entry{sub("zip")}},
		// HaGeZi wildcard asterisk
		{line: "*.ad.001zb.com", status: lineOK, want: []entry{sub("ad.001zb.com")}},
		// plain domains (1Hosts domains / wildcards, OISD domainswild2)
		{line: "ads.example.com", status: lineOK, want: []entry{ex("ads.example.com")}},
		{line: "ads.example.com", subtree: true, status: lineOK, want: []entry{sub("ads.example.com")}},
		{line: "localhost", status: lineSkip},
		{line: "com", status: lineInvalid},
		// AdGuard DNS filter
		{line: "@@||example.org^", status: lineOK, want: []entry{{kind: kindSubtree, allow: true, domain: "example.org"}}},
		{line: "@@||example.org^|", status: lineOK, want: []entry{{kind: kindSubtree, allow: true, domain: "example.org"}}},
		{line: "|ads.example.com^", status: lineOK, want: []entry{ex("ads.example.com")}},
		{line: "||gwrtdp-tn690BFAdt.tclclouds.com^", status: lineOK, want: []entry{sub("gwrtdp-tn690bfadt.tclclouds.com")}},
		{line: "||ad*.example.com^", status: lineOK, want: []entry{pat(`^(?:[^.]+\.)*ad.*\.example\.com$`)}},
		{line: "||example.", status: lineOK, want: []entry{pat(`^(?:[^.]+\.)*example\.`)}},
		{line: ".example.com^", status: lineOK, want: []entry{pat(`\.example\.com$`)}},
		{line: "example.com^", status: lineOK, want: []entry{pat(`example\.com$`)}},
		{line: "://ads.example^", status: lineOK, want: []entry{pat(`^ads\.example$`)}},
		{line: "*.ads.example.com^", status: lineOK, want: []entry{pat(`.*\.ads\.example\.com$`)}},
		{line: "||example.com^$important", status: lineOK, want: []entry{{kind: kindSubtree, important: true, domain: "example.com"}}},
		{line: "@@||example.com^$important", status: lineOK, want: []entry{{kind: kindSubtree, allow: true, important: true, domain: "example.com"}}},
		{line: "||example.com^$badfilter", status: lineOK, want: []entry{{kind: kindSubtree, badfilter: true, domain: "example.com"}}},
		{line: "||example.com^$important,badfilter", status: lineOK, want: []entry{{kind: kindSubtree, important: true, badfilter: true, domain: "example.com"}}},
		{line: "/^ad[0-9]+\\.Example\\.com$/", status: lineOK, want: []entry{{kind: kindPattern, re: `^ad[0-9]+\.Example\.com$`}}},
		{line: "@@/^good\\./", status: lineOK, want: []entry{{kind: kindPattern, allow: true, re: `^good\.`}}},
		{line: "/tracker/$important", status: lineOK, want: []entry{{kind: kindPattern, important: true, re: `tracker`}}},
		// unsupported
		{line: "||example.com^$third-party", status: lineUnsupported},
		{line: "||example.com^$dnstype=AAAA", status: lineUnsupported},
		{line: "||example.com^$client=192.168.1.1", status: lineUnsupported},
		{line: "/ads/banner.gif", status: lineUnsupported},
		{line: "||example.com/path^", status: lineUnsupported},
		{line: "/" + strings.Repeat("a", maxRegexLen+1) + "/", status: lineUnsupported},
		{line: "/" + strings.Repeat(`[^.]{999}`, 91) + "x/", status: lineUnsupported}, // 823 chars, ~91 000 instructions
		{line: `/^(?:[a-z0-9-]{1,63}\.){1,10}ads\.example$/`, status: lineOK, want: []entry{pat(`^(?:[a-z0-9-]{1,63}\.){1,10}ads\.example$`)}},
		// cosmetic and comments
		{line: "example.com##.banner", status: lineSkip},
		{line: "example.com#@#.banner", status: lineSkip},
		{line: "##.ad", status: lineSkip},
		{line: "; comment", status: lineSkip},
		{line: "   ", status: lineSkip},
		// invalid
		{line: "1.2.3.4", status: lineInvalid},
		{line: "ümlaut.de", status: lineInvalid},
		{line: "ex@mple.com", status: lineInvalid},
		{line: "*", status: lineInvalid},
		{line: "||*^", status: lineInvalid},
		{line: "||ex^ample.com^", status: lineInvalid},
		{line: "/(a/", status: lineInvalid},
		{line: "||example.com^$", status: lineInvalid},
		{line: strings.Repeat("a", 250) + ".com", status: lineInvalid},
	}
	for _, c := range cases {
		lp := lineParser{plainSubtree: c.subtree}
		got, st := lp.parse(c.line)
		if st != c.status {
			t.Errorf("%q: status %d, want %d", c.line, st, c.status)
			continue
		}
		if len(got) != len(c.want) {
			t.Errorf("%q: %d entries, want %d", c.line, len(got), len(c.want))
			continue
		}
		for i, w := range c.want {
			g := got[i]
			if g.kind != w.kind || g.allow != w.allow || g.important != w.important || g.badfilter != w.badfilter ||
				g.domain != w.domain || g.re != w.re {
				t.Errorf("%q: entry %d = %+v, want %+v", c.line, i, g, w)
			}
		}
	}
}

func TestABPPatternSemantics(t *testing.T) {
	cases := []struct {
		rule    string
		match   []string
		noMatch []string
	}{
		{"||ad*.example.com^", []string{"ads.example.com", "x.adserver.example.com", "ad.example.com"}, []string{"example.com", "bad.example.com.evil", "ad.example.org"}},
		{"||example.", []string{"example.com", "a.example.net"}, []string{"badexample.com", "example"}},
		{".example.com^", []string{"a.example.com"}, []string{"example.com", "a.example.com.au"}},
		{"|ads.*.net^", []string{"ads.x.net"}, []string{"x.ads.y.net"}},
		{"/^ad[0-9]+\\.Example\\.com$/", []string{"ad12.example.com"}, []string{"ad.example.com"}},
	}
	for _, c := range cases {
		var lp lineParser
		es, st := lp.parse(c.rule)
		if st != lineOK || len(es) != 1 || es[0].kind != kindPattern {
			t.Fatalf("%q: not parsed as pattern (%d)", c.rule, st)
		}
		re, err := es[0].compile()
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range c.match {
			if !re.MatchString(q) || !strings.Contains(q, es[0].lit) {
				t.Errorf("%q should match %q (literal %q)", c.rule, q, es[0].lit)
			}
		}
		for _, q := range c.noMatch {
			if re.MatchString(q) {
				t.Errorf("%q should not match %q", c.rule, q)
			}
		}
	}
}

func TestRequiredLiteral(t *testing.T) {
	for re, want := range map[string]string{
		`^ad[0-9]+\.Tracker\.com$`:         ".tracker.com",
		`(ads|track)\.example\.org`:        ".example.org",
		`^(?:[^.]+\.)*ad.*\.example\.com$`: ".example.com",
		`a|b`:                              "",
		`(foo)+bar`:                        "foo",
	} {
		if got := regexLiteral(re); got != want {
			t.Errorf("regexLiteral(%q) = %q, want %q", re, got, want)
		}
	}
}

func TestParseList(t *testing.T) {
	const list = "\xef\xbb\xbf[Adblock Plus]\r\n" +
		"! Title: test\r\n" +
		"||ads.example.com^\r\n" +
		"||ads.example.com^\r\n" + // duplicate
		"||keep.example.com^\r\n" +
		"||gone.example.com^\r\n" +
		"||gone.example.com^$badfilter\r\n" +
		"@@||good.example.com^\r\n" +
		"||imp.example.com^$important\r\n" +
		"||ad*.pattern.com^\r\n" +
		"||ad*.pattern.com^\r\n" + // duplicate pattern
		"/^re[0-9]\\.example\\.net$/\r\n" +
		"||x.example.com^$third-party\r\n" +
		"not a domain\r\n" +
		"plain.example.org\r\n"
	p, err := parseList(context.Background(), strings.NewReader(list), "block", "exact")
	if err != nil {
		t.Fatal(err)
	}
	if got := len(p.sets[tierBlock][kindSubtree]); got != 2 {
		t.Errorf("block subtree entries = %d, want 2 (ads, keep; gone cancelled by $badfilter)", got)
	}
	if len(p.sets[tierAllow][kindSubtree]) != 1 || len(p.sets[tierImpBlock][kindSubtree]) != 1 {
		t.Errorf("allow/important sets = %v", p.sets)
	}
	if len(p.sets[tierBlock][kindExact]) != 1 {
		t.Errorf("plain exact entries = %d", len(p.sets[tierBlock][kindExact]))
	}
	if len(p.pats) != 2 {
		t.Errorf("patterns = %d, want 2", len(p.pats))
	}
	if p.entries != 7 || p.invalid != 1 || p.unsupported != 1 {
		t.Errorf("entries/invalid/unsupported = %d/%d/%d, want 7/1/1", p.entries, p.invalid, p.unsupported)
	}

	// an allow list turns every entry into an allow entry
	p, err = parseList(context.Background(), strings.NewReader("||a.example.com^\n0.0.0.0 b.example.com\n"), "allow", "exact")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.sets[tierAllow][kindSubtree]) != 1 || len(p.sets[tierAllow][kindExact]) != 1 || len(p.sets[tierBlock][kindSubtree]) != 0 {
		t.Errorf("allow list sets = %v", p.sets)
	}
}

func TestParseListRejects(t *testing.T) {
	for name, c := range map[string]struct {
		body string
		want error
	}{
		"html":         {"\n\n  <!DOCTYPE html>\n<html><body>Not found</body></html>", errHTML},
		"html-lower":   {"<html>\n", errHTML},
		"binary":       {"||a.example.com^\n\x00\x01\x02PK\x03\x04", errBinary},
		"binary-first": {"\x1f\x8b\x08gzip", errBinary},
		"ok-utf8":      {"! Tïtle: ünïcode comment\n||a.example.com^\n", nil},
	} {
		_, err := parseList(context.Background(), strings.NewReader(c.body), "block", "exact")
		if !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", name, err, c.want)
		}
	}
}

func TestParseListLongLineAndPatternCap(t *testing.T) {
	var b strings.Builder
	b.WriteString(strings.Repeat("a", maxLineLen+10) + "\n")
	b.WriteString("||ok.example.com^\n")
	for i := range maxPatterns + 5 {
		b.WriteString("||ad" + strconv.Itoa(i) + "*.example.com^\n")
	}
	p, err := parseList(context.Background(), strings.NewReader(b.String()), "block", "exact")
	if err != nil {
		t.Fatal(err)
	}
	if p.invalid != 1 || p.unsupported != 5 || len(p.pats) != maxPatterns || p.entries != maxPatterns+1 {
		t.Errorf("invalid=%d unsupported=%d pats=%d entries=%d", p.invalid, p.unsupported, len(p.pats), p.entries)
	}
}

func TestParseListCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := parseList(ctx, strings.NewReader("||a.example.com^\n"), "block", "exact"); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
}

// TestRegexCost: the estimate walks the unexpanded parse tree and bounds the
// size of the program regexp/syntax compiles.
func TestRegexCost(t *testing.T) {
	for _, src := range []string{
		`(?i)^ad[0-9]+\.tracker\.com$`,
		`^(?:[^.]+\.)*ad.*\.example\.com$`,
		`(?i)^[a-z0-9]{1,63}\.example\.com$`,
		`(?i)^(?:[a-z0-9-]{1,63}\.){1,10}x\.com$`,
		`(?i)(a|bb|ccc){2,30}x+y*z?`,
		`(?i)[^.]{999}[^.]{999}x`,
		`(?i)(?:ab){3,}`,
	} {
		re, err := syntax.Parse(src, syntax.Perl)
		if err != nil {
			t.Fatal(err)
		}
		cost := regexCost(re)
		prog, err := syntax.Compile(re.Simplify())
		if err != nil {
			t.Fatal(err)
		}
		// The program adds a fail, a match and the capture of the whole match.
		if n := int64(len(prog.Inst)); cost+3 < n || cost > 3*n+8 {
			t.Errorf("regexCost(%q) = %d, program has %d instructions", src, cost, n)
		}
	}
	// Large character classes count (one-pass programs copy them per instruction).
	re, _ := syntax.Parse(`(?i)^(?:\pL\pN){200}`, syntax.Perl)
	if cost := regexCost(re); cost <= maxRegexCost {
		t.Errorf("regexCost of 400 Unicode classes = %d, want > %d", cost, maxRegexCost)
	}
}

// TestParseListHostilePatterns: patterns that are short in source but
// compile to huge programs are counted as unsupported and never compiled.
func TestParseListHostilePatterns(t *testing.T) {
	var b strings.Builder
	b.WriteString("||ok.example^\n")
	const hostile = 20
	for i := range hostile {
		// 823 characters, valid RE2, ~91 000 instructions (~3.6 MB compiled)
		fmt.Fprintf(&b, "/%sx%d/\n", strings.Repeat(`[^.]{999}`, 91), i)
	}
	// Adblock-style wildcards are converted to RE2 and bounded the same way.
	fmt.Fprintf(&b, "||%s^\n", strings.Repeat("a*", 1500))
	b.WriteString("||ad*.example.com^\n")
	start := time.Now()
	p, err := parseList(context.Background(), strings.NewReader(b.String()), "block", "exact")
	if err != nil {
		t.Fatal(err)
	}
	if p.unsupported != hostile+1 || len(p.pats) != 1 || p.entries != 2 {
		t.Errorf("unsupported=%d pats=%d entries=%d, want %d/1/2", p.unsupported, len(p.pats), p.entries, hostile+1)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("parsing took %v", d)
	}
}

// TestParseListPatternCostBudget: the compiled patterns of one list are
// bounded in total (maxPatternCost), not only in number.
func TestParseListPatternCostBudget(t *testing.T) {
	// \pL is one instruction with a large rune table: expensive by the
	// estimate, cheap to compile here.
	var b strings.Builder
	const n = 400
	for i := range n {
		fmt.Fprintf(&b, `/\pL{20}x%03d\.example/`+"\n", i)
	}
	p, err := parseList(context.Background(), strings.NewReader(b.String()), "block", "exact")
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, pp := range p.pats {
		total += int64(pp.cost)
	}
	if len(p.pats) == 0 || len(p.pats) == n || total > maxPatternCost || p.unsupported != n-len(p.pats) {
		t.Errorf("pats=%d unsupported=%d total cost=%d, want a budget of %d to cut the list", len(p.pats), p.unsupported, total, maxPatternCost)
	}
	if last := p.pats[len(p.pats)-1]; total+int64(last.cost) <= maxPatternCost {
		t.Errorf("stopped early: total cost %d, one more pattern costs %d", total, last.cost)
	}
}
