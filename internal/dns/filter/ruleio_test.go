package filter

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
)

// importOne maps one line like ImportRules does (without the database): the
// rules, or the error field, or skip.
func importOne(line string) (rules []Rule, field string, skip bool) {
	parsed, skip, lerr := parseImportLine(line)
	if skip {
		return nil, "", true
	}
	if lerr != nil {
		return nil, lerr.field, false
	}
	for _, ir := range parsed {
		sp, err := resolveRule(ir.in, nil)
		if err != nil {
			f, _ := fieldOf(err)
			return nil, f, false
		}
		rules = append(rules, sp)
	}
	return rules, "", false
}

// The line mapping of the rule import.
func TestImportLineMapping(t *testing.T) {
	type want struct {
		action, typ, pattern string
		qtypes               []string
		negate               bool
		deny                 []string
		reply, v4, v6        string
		invert               bool
	}
	block := func(typ, pattern string) want { return want{action: "block", typ: typ, pattern: pattern} }
	allow := func(typ, pattern string) want { return want{action: "allow", typ: typ, pattern: pattern} }
	mod := func(w want, fn func(*want)) want { fn(&w); return w }
	cases := []struct {
		line  string
		want  []want
		field string
		skip  bool
	}{
		{line: "", skip: true},
		{line: "   ", skip: true},
		{line: "! comment", skip: true},
		{line: "# comment", skip: true},
		{line: "[Adblock Plus 2.0]", skip: true},
		{line: "example.com##.banner", skip: true},
		{line: "example.com#@#.banner", skip: true},
		{line: "127.0.0.1 localhost", skip: true},
		{line: "x.example", want: []want{block("exact", "x.example")}},
		{line: "X.Example.", want: []want{block("exact", "x.example")}},
		{line: "|x.example^", want: []want{block("exact", "x.example")}},
		{line: "|x.example|", want: []want{block("exact", "x.example")}},
		{line: "||x.example^", want: []want{block("subtree", "x.example")}},
		{line: "||x.example^|", want: []want{block("subtree", "x.example")}},
		{line: "*.x.example", want: []want{block("subtree", "x.example")}},
		{line: "0.0.0.0 a.example b.example # ads", want: []want{block("exact", "a.example"), block("exact", "b.example")}},
		{line: "0.0.0.0 localhost a.example", want: []want{block("exact", "a.example")}},
		{line: "0.0.0.0 bad..name", field: "pattern"},
		{line: "@@0.0.0.0 a.example", field: "syntax"},
		{line: "/^ads\\./", want: []want{block("regex", `^ads\.`)}},
		{line: "@@/^ads\\./", want: []want{allow("regex", `^ads\.`)}},
		{line: "@@||x.example^", want: []want{allow("subtree", "x.example")}},
		{line: "@@|x.example^", want: []want{allow("exact", "x.example")}},
		{line: "||ads*.example^", want: []want{block("regex", `^(?:[^.]+\.)*ads.*\.example$`)}},
		{line: ".x.example^", want: []want{block("regex", `\.x\.example$`)}},
		{line: "x.example^", want: []want{block("regex", `x\.example$`)}},
		{line: "||x.example^$dnstype=A|AAAA", want: []want{mod(block("subtree", "x.example"), func(w *want) { w.qtypes = []string{"A", "AAAA"} })}},
		{line: "||x.example^$dnstype=~A|~AAAA", want: []want{mod(block("subtree", "x.example"), func(w *want) {
			w.qtypes, w.negate = []string{"A", "AAAA"}, true
		})}},
		{line: "||x.example^$dnstype=~A|AAAA", field: "qtypes"},
		{line: "||x.example^$dnstype=NOPE", field: "qtypes"},
		{line: "||x.example^$denyallow=b.x.example|a.x.example", want: []want{mod(block("subtree", "x.example"), func(w *want) {
			w.deny = []string{"a.x.example", "b.x.example"}
		})}},
		{line: "||x.example^$reply=nxdomain", want: []want{mod(block("subtree", "x.example"), func(w *want) { w.reply = "nxdomain" })}},
		{line: "||x.example^$reply=custom_ip|192.0.2.1|", want: []want{mod(block("subtree", "x.example"), func(w *want) {
			w.reply, w.v4 = "custom_ip", "192.0.2.1"
		})}},
		{line: "||x.example^$reply=custom_ip|self|self", want: []want{mod(block("subtree", "x.example"), func(w *want) {
			w.reply, w.v4, w.v6 = "custom_ip", "self", "self"
		})}},
		{line: "||x.example^$reply=custom_ip|192.0.2.1", field: "reply"},
		{line: "||x.example^$reply=drop", field: "reply"},
		{line: "||x.example^$dnsrewrite=NXDOMAIN", want: []want{mod(block("subtree", "x.example"), func(w *want) { w.reply = "nxdomain" })}},
		{line: "||x.example^$dnsrewrite=REFUSED;;", want: []want{mod(block("subtree", "x.example"), func(w *want) { w.reply = "refused" })}},
		{line: "||x.example^$dnsrewrite=noerror", want: []want{mod(block("subtree", "x.example"), func(w *want) { w.reply = "nodata" })}},
		{line: "||x.example^$dnsrewrite=192.0.2.1", want: []want{mod(block("subtree", "x.example"), func(w *want) {
			w.reply, w.v4 = "custom_ip", "192.0.2.1"
		})}},
		{line: "||x.example^$dnsrewrite=NOERROR;AAAA;2001:db8::1", want: []want{mod(block("subtree", "x.example"), func(w *want) {
			w.reply, w.v6 = "custom_ip", "2001:db8::1"
		})}},
		{line: "||x.example^$dnsrewrite=NOERROR;A;2001:db8::1", field: "syntax"},
		{line: "||x.example^$dnsrewrite=NOERROR;CNAME;other.example", field: "syntax"},
		{line: "||x.example^$dnsrewrite=other.example", field: "syntax"},
		{line: "||x.example^$dnsrewrite=NOERROR;MX;10 mail.example", field: "syntax"},
		{line: "||x.example^$reply=nodata,dnsrewrite=NXDOMAIN", field: "syntax"},
		{line: "/^x/$invert", want: []want{mod(block("regex", "^x"), func(w *want) { w.invert = true })}},
		{line: "/^x/;invert", want: []want{mod(block("regex", "^x"), func(w *want) { w.invert = true })}},
		{line: "/^x/;querytype=A,AAAA", want: []want{mod(block("regex", "^x"), func(w *want) { w.qtypes = []string{"A", "AAAA"} })}},
		{line: "/^x/;querytype=!A,AAAA;invert", want: []want{mod(block("regex", "^x"), func(w *want) {
			w.qtypes, w.negate, w.invert = []string{"A", "AAAA"}, true, true
		})}},
		{line: "/^x/;reply=nodata", want: []want{mod(block("regex", "^x"), func(w *want) { w.reply = "nodata" })}},
		{line: "/^x/;reply=192.0.2.1;reply=2001:db8::1", want: []want{mod(block("regex", "^x"), func(w *want) {
			w.reply, w.v4, w.v6 = "custom_ip", "192.0.2.1", "2001:db8::1"
		})}},
		{line: "/^x/;reply=ip", want: []want{mod(block("regex", "^x"), func(w *want) { w.reply, w.v4, w.v6 = "custom_ip", "self", "self" })}},
		{line: "/^x/;reply=none", field: "syntax"},
		{line: "/^x/;reply=nodata;reply=refused", field: "reply"},
		{line: "/^x/;querytype=A;querytype=AAAA", field: "syntax"},
		{line: "/^x/;client=1.2.3.4", field: "syntax"},
		{line: "/ads/banner.gif", field: "syntax"},
		{line: "||x.example^$important", want: []want{block("subtree", "x.example")}},
		{line: "||x.example^$badfilter", field: "syntax"},
		{line: "||x.example^$client=192.168.1.5", field: "syntax"},
		{line: "||x.example^$ctag=device_phone", field: "syntax"},
		{line: "||x.example^$third-party", field: "syntax"},
		{line: "||x.example/path^", field: "syntax"},
		{line: "@@||x.example^$denyallow=a.x.example", field: "denyallow"},
		{line: "|x.example^$denyallow=a.x.example", field: "denyallow"},
		{line: "||x.example^$invert", field: "invert"},
		{line: "@@/^x/$invert", field: "invert"},
		{line: "@@||x.example^$reply=nodata", field: "reply"},
		{line: "@@||x.example^$dnsrewrite=NXDOMAIN", field: "reply"},
		{line: "^ads\\.example$", field: "syntax"},
		{line: "ads\\d+\\.example", field: "syntax"},
		{line: "192.0.2.1", field: "syntax"},
		{line: "203.0.113.0/24", field: "syntax"},
		{line: "||192.0.2.1^", field: "syntax"},
		{line: "hello world", field: "syntax"},
		{line: "bücher.example", field: "pattern"},
	}
	for _, c := range cases {
		got, field, skip := importOne(c.line)
		if skip != c.skip || field != c.field {
			t.Errorf("%q: skip %v field %q, want skip %v field %q", c.line, skip, field, c.skip, c.field)
			continue
		}
		if len(got) != len(c.want) {
			t.Errorf("%q: %d rules, want %d", c.line, len(got), len(c.want))
			continue
		}
		for i, w := range c.want {
			g := got[i]
			if g.Action != w.action || g.Type != w.typ || g.Pattern != w.pattern || !slices.Equal(g.Qtypes, nonNilStrings(w.qtypes)) ||
				g.QtypesNegate != w.negate || !slices.Equal(g.Denyallow, nonNilStrings(w.deny)) || g.Reply != w.reply ||
				g.ReplyIPv4 != w.v4 || g.ReplyIPv6 != w.v6 || g.Invert != w.invert {
				t.Errorf("%q: rule %d = %+v, want %+v", c.line, i, g, w)
			}
		}
	}
}

// All or nothing: an error writes nothing (the counts say what the valid
// lines would do); a dry run writes nothing; otherwise everything is
// written at once.
func TestImportRules(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	existing, err := e.CreateRule(ctx, RuleInput{Action: "block", Type: "subtree", Pattern: "old.example", Enabled: false,
		Comment: "keep", GroupIDs: []int64{2}, Qtypes: []string{"AAAA"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateRule(ctx, RuleInput{Action: "block", Type: "exact", Pattern: "other.example", Enabled: true, Reply: ptr("nodata")}); err != nil {
		t.Fatal(err)
	}
	text := "! my rules\r\n||a.example^\r\n0.0.0.0 b.example c.example\n||old.example^$dnstype=AAAA\n/^d\\./\n\n"
	res, err := e.ImportRules(ctx, RuleImport{Text: text, DryRun: true})
	if err != nil || res.Applied || res.Added != 4 || res.Unchanged != 1 || res.Skipped != 2 || res.ErrorCount != 0 || len(res.Errors) != 0 {
		t.Fatalf("dry run %+v %v", res, err)
	}
	if rules, _ := e.Rules(ctx, RuleQuery{}); len(rules) != 2 {
		t.Fatal("a dry run wrote rules")
	}
	bad := text + "||x.example^$client=1.2.3.4\n||a.example^\n|other.example^\n"
	res, err = e.ImportRules(ctx, RuleImport{Text: bad})
	if err != nil || res.Applied || res.Added != 4 || res.ErrorCount != 3 {
		t.Fatalf("with errors %+v %v", res, err)
	}
	wantErr := []ImportError{
		{Line: 7, Field: "syntax"}, {Line: 8, Field: "pattern", Message: "listed twice (line 2)"},
		{Line: 9, Field: "pattern", Message: fmt.Sprintf("a rule for this pattern already exists (rule %d) with other options; edit it instead", existing.ID+1)},
	}
	for i, w := range wantErr {
		if g := res.Errors[i]; g.Line != w.Line || g.Field != w.Field || (w.Message != "" && g.Message != w.Message) {
			t.Errorf("error %d = %+v, want %+v", i, g, w)
		}
	}
	if rules, _ := e.Rules(ctx, RuleQuery{}); len(rules) != 2 {
		t.Fatal("a failed import wrote rules")
	}
	res, err = e.ImportRules(ctx, RuleImport{Text: text})
	if err != nil || !res.Applied || res.Added != 4 {
		t.Fatalf("import %+v %v", res, err)
	}
	rules, _ := e.Rules(ctx, RuleQuery{})
	if len(rules) != 6 {
		t.Fatalf("%d rules", len(rules))
	}
	for _, r := range rules {
		switch r.Pattern {
		case "old.example":
			if r.Enabled || r.Comment != "keep" || !slices.Equal(r.GroupIDs, []int64{2}) {
				t.Errorf("unchanged rule changed: %+v", r)
			}
		case "other.example":
		default:
			if !r.Enabled || r.Comment != "" || !slices.Equal(r.GroupIDs, []int64{1}) {
				t.Errorf("imported %+v", r)
			}
		}
	}
	if !e.Check("x.a.example", qtypeA, []int64{1}).Blocked() || !e.Check("d.x", qtypeA, []int64{1}).Blocked() {
		t.Fatal("the matcher was not rebuilt")
	}
	// Groups: [] → none; unknown → 400.
	res, err = e.ImportRules(ctx, RuleImport{Text: "||nobody.example^", GroupIDs: []int64{}})
	if err != nil || !res.Applied {
		t.Fatalf("no group %+v %v", res, err)
	}
	if r, _ := e.Rules(ctx, RuleQuery{Search: "nobody"}); len(r) != 1 || len(r[0].GroupIDs) != 0 {
		t.Fatalf("no group rule %+v", r)
	}
	_, err = e.ImportRules(ctx, RuleImport{Text: "||g.example^", GroupIDs: []int64{42}})
	wantInvalid(t, "unknown group", err, "groupIds")
	_, err = e.ImportRules(ctx, RuleImport{Text: strings.Repeat("x.example\n", maxImportLines+1)})
	wantInvalid(t, "too many lines", err, "text")
}

// Two $dnsrewrite address lines of the same rule, one per family, become
// one rule; a third is listed twice.
func TestImportRewritePair(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	res, err := e.ImportRules(ctx, RuleImport{Text: "||r.example^$dnsrewrite=192.0.2.1\n||r.example^$dnsrewrite=NOERROR;AAAA;2001:db8::1\n"})
	if err != nil || !res.Applied || res.Added != 1 {
		t.Fatalf("pair %+v %v", res, err)
	}
	r, _ := e.Rules(ctx, RuleQuery{})
	if len(r) != 1 || r[0].Reply != "custom_ip" || r[0].ReplyIPv4 != "192.0.2.1" || r[0].ReplyIPv6 != "2001:db8::1" {
		t.Fatalf("merged %+v", r)
	}
	res, _ = e.ImportRules(ctx, RuleImport{Text: "||s.example^$dnsrewrite=192.0.2.1\n||s.example^$dnsrewrite=198.51.100.1\n", DryRun: true})
	if res.ErrorCount != 1 || res.Errors[0].Line != 2 || res.Errors[0].Field != "pattern" {
		t.Fatalf("same family twice %+v", res)
	}
	res, _ = e.ImportRules(ctx, RuleImport{Text: "||t.example^$dnsrewrite=192.0.2.1\n||t.example^$dnsrewrite=::1\n||t.example^$dnsrewrite=::2\n", DryRun: true})
	if res.ErrorCount != 1 || res.Errors[0].Line != 3 {
		t.Fatalf("a third line %+v", res)
	}
}

// The caps are checked inside the transaction ({line 0, field text}); the
// errors are capped at 1 000 while every line is counted.
func TestImportCaps(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	tx, err := e.db.W.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := range maxRegexRules - 1 {
		if _, err := tx.Exec(`INSERT INTO filter_rules (action, type, pattern, enabled, comment, created_at, updated_at) VALUES ('block', 'regex', ?, 0, '', 0, 0)`,
			fmt.Sprintf("^r%d$", i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	res, err := e.ImportRules(ctx, RuleImport{Text: "/^a$/\n/^b$/\n"})
	if err != nil || res.Applied || res.ErrorCount != 1 || res.Errors[0].Line != 0 || res.Errors[0].Field != "text" {
		t.Fatalf("regex cap %+v %v", res, err)
	}
	var b strings.Builder
	for range 1500 {
		b.WriteString("||x.example^$third-party\n")
	}
	res, err = e.ImportRules(ctx, RuleImport{Text: b.String(), DryRun: true})
	if err != nil || len(res.Errors) != maxImportErrors || res.ErrorCount != 1500 || res.Errors[999].Line != 1000 {
		t.Fatalf("errors cap: %d errors, count %d, %v", len(res.Errors), res.ErrorCount, err)
	}
	for i := range maxInverted {
		if _, err := e.db.W.Exec(`UPDATE filter_rules SET invert = 1 WHERE pattern = ?`, fmt.Sprintf("^r%d$", i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.db.W.Exec(`DELETE FROM filter_rules WHERE pattern = '^r998$'`); err != nil {
		t.Fatal(err)
	}
	res, _ = e.ImportRules(ctx, RuleImport{Text: "/^i$/$invert\n", DryRun: true})
	if res.ErrorCount != 1 || res.Errors[0].Line != 0 || !strings.Contains(res.Errors[0].Message, "inverted") {
		t.Fatalf("inverted cap %+v", res)
	}
}

// Importing an export recreates every enabled rule with its action, type,
// pattern and modifiers; comments and disabled rules are skipped.
func TestExportRoundTrip(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	inputs := []RuleInput{
		{Action: "block", Type: "exact", Pattern: "a.example", Enabled: true, Comment: "one"},
		{Action: "allow", Type: "subtree", Pattern: "b.example", Enabled: true, Qtypes: []string{"A"}},
		{Action: "block", Type: "subtree", Pattern: "c.example", Enabled: true, Qtypes: []string{"AAAA", "HTTPS"}, QtypesNegate: ptr(true),
			Denyallow: []string{"ok.c.example"}, Reply: ptr("custom_ip"), ReplyIPv4: ptr("self"), ReplyIPv6: ptr("2001:db8::1")},
		{Action: "block", Type: "regex", Pattern: `^ads[0-9]+\.x$`, Enabled: true, Invert: ptr(true), Denyallow: []string{"keep.example"}},
		{Action: "block", Type: "regex", Pattern: `a/b$`, Enabled: true, Reply: ptr("refused")},
		{Action: "block", Type: "exact", Pattern: "off.example", Enabled: false},
	}
	for _, in := range inputs {
		if _, err := e.CreateRule(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	rules, _ := e.Rules(ctx, RuleQuery{})
	var out strings.Builder
	now := time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)
	if err := WriteRuleExport(&out, rules, now); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, line := range []string{"! Title: PiCache rules", "! Exported 2026-09-26T10:00:00Z", "! one", "|a.example^", "@@||b.example^$dnstype=A",
		"||c.example^$dnstype=~AAAA|~HTTPS,denyallow=ok.c.example,reply=custom_ip|self|2001:db8::1",
		`/^ads[0-9]+\.x$/$denyallow=keep.example,invert`, "/a/b$/$reply=refused", "! [disabled] |off.example^"} {
		if !strings.Contains(text, line+"\n") {
			t.Errorf("export lacks %q:\n%s", line, text)
		}
	}
	f := newTestEngine(t)
	res, err := f.ImportRules(ctx, RuleImport{Text: text})
	if err != nil || !res.Applied || res.Added != 5 || res.Skipped != 4 {
		t.Fatalf("re-import %+v %v", res, err)
	}
	again, _ := f.Rules(ctx, RuleQuery{})
	for i, r := range again {
		o := rules[i]
		if r.Action != o.Action || r.Type != o.Type || r.Pattern != o.Pattern || !sameOptions(r, o) {
			t.Errorf("rule %d: %+v, want %+v", i, r, o)
		}
	}
}

// The rule import reads untrusted text: it never panics and every rule it
// makes is valid.
func FuzzImportLine(f *testing.F) {
	for _, s := range []string{"||x.example^$dnstype=A|AAAA,denyallow=a.x.example", "/^x/;querytype=!A;reply=ip;invert",
		"||x^$dnsrewrite=NOERROR;AAAA;::1", "0.0.0.0 a.example b", "@@|x.example|", "||a*b^", "/a/b/$reply=custom_ip|self|",
		"x.example$reply=custom_ip||", "^re$", "1.2.3.4/8", "||x^$client=a", "// 00//", "///0///"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, line string) {
		parsed, skip, lerr := parseImportLine(line)
		if skip && (lerr != nil || len(parsed) != 0) {
			t.Fatalf("%q: skipped with result", line)
		}
		for _, ir := range parsed {
			sp, err := resolveRule(ir.in, nil)
			if err != nil {
				continue
			}
			if sp.Type != "regex" && !validDomain(sp.Pattern) {
				t.Fatalf("%q: invalid pattern %q", line, sp.Pattern)
			}
			if again, err := resolveRule(RuleInput{Action: sp.Action, Type: sp.Type, Pattern: sp.Pattern, Qtypes: sp.Qtypes,
				QtypesNegate: &sp.QtypesNegate, Reply: &sp.Reply, ReplyIPv4: &sp.ReplyIPv4, ReplyIPv6: &sp.ReplyIPv6,
				Denyallow: sp.Denyallow, Invert: &sp.Invert}, nil); err != nil || !sameOptions(again, sp) {
				t.Fatalf("%q: not stable: %+v %v", line, again, err)
			}
			exported, _, _ := parseImportLine(RuleLine(sp))
			if len(exported) != 1 {
				t.Fatalf("%q: the export line %q does not read back", line, RuleLine(sp))
			}
			back, err := resolveRule(exported[0].in, nil)
			if err != nil || back.Pattern != sp.Pattern || back.Type != sp.Type || !sameOptions(back, sp) {
				t.Fatalf("%q: round trip %q → %+v %v", line, RuleLine(sp), back, err)
			}
		}
	})
}

var _ = apperr.KindInvalid

// A regular expression with a control character is refused (a line break
// would split its export line into other rules, an allow rule among them);
// a rule stored before that is exported as one line with \x{…} escapes.
func TestRegexControlCharacters(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	for _, p := range []string{"x/\n@@||bank.example^\n/y", "a\rb", "a\tb", "a\x00b", "a\u0085b"} {
		_, err := e.CreateRule(ctx, RuleInput{Action: "block", Type: "regex", Pattern: p, Enabled: true})
		wantInvalid(t, fmt.Sprintf("%q", p), err, "pattern")
	}
	legacy := Rule{Action: "block", Type: "regex", Pattern: "x/\n@@||bank.example^\n/y", Enabled: true}
	var out strings.Builder
	if err := WriteRuleExport(&out, []Rule{legacy}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if want := `/x/\x{a}@@||bank.example^\x{a}/y/` + "\n"; !strings.HasSuffix(out.String(), want) || strings.Count(out.String(), "\n") != 3 {
		t.Fatalf("export %q", out.String())
	}
	res, err := e.ImportRules(ctx, RuleImport{Text: out.String()})
	if err != nil || !res.Applied || res.Added != 1 {
		t.Fatalf("import %+v %v", res, err)
	}
	rules, _ := e.Rules(ctx, RuleQuery{})
	if len(rules) != 1 || rules[0].Action != "block" || rules[0].Type != "regex" {
		t.Fatalf("rules %+v", rules)
	}
}
