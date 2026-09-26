package filter

import (
	"context"
	"encoding/json/v2"
	"slices"
	"strings"
	"testing"

	"github.com/hustenreizjuengling/picache/internal/apperr"
)

func ptr[T any](v T) *T { return &v }

// wantInvalid checks a 400 with the field.
func wantInvalid(t *testing.T, what string, err error, field string) {
	t.Helper()
	ae, ok := apperr.As(err)
	if !ok || ae.Kind != apperr.KindInvalid || ae.Field != field {
		t.Errorf("%s: err = %v, want 400 on %s", what, err, field)
	}
}

// The validation of the rule modifiers (400 with the field) and their
// normalisation.
func TestRuleModifierValidation(t *testing.T) {
	base := func(action, typ, pattern string) RuleInput {
		return RuleInput{Action: action, Type: typ, Pattern: pattern}
	}
	with := func(in RuleInput, fn func(*RuleInput)) RuleInput { fn(&in); return in }
	bad := []struct {
		in    RuleInput
		field string
	}{
		{with(base("block", "exact", "x.example"), func(in *RuleInput) {
			in.Qtypes = strings.Split("A,AAAA,MX,TXT,NS,SOA,PTR,SRV,CNAME,HTTPS,SVCB,CAA,DS,DNSKEY,NAPTR,TLSA,SSHFP", ",")
		}), "qtypes"},
		{with(base("block", "exact", "x.example"), func(in *RuleInput) { in.Qtypes = []string{"A", "NOPE"} }), "qtypes[1]"},
		{with(base("block", "exact", "x.example"), func(in *RuleInput) { in.Qtypes = []string{"TYPE0"} }), "qtypes[0]"},
		{with(base("block", "exact", "x.example"), func(in *RuleInput) { in.QtypesNegate = ptr(true) }), "qtypesNegate"},
		{with(base("block", "exact", "x.example"), func(in *RuleInput) { in.Reply = ptr("drop") }), "reply"},
		{with(base("allow", "exact", "x.example"), func(in *RuleInput) { in.Reply = ptr("nodata") }), "reply"},
		{with(base("block", "exact", "x.example"), func(in *RuleInput) { in.Reply, in.ReplyIPv4 = ptr("nodata"), ptr("192.0.2.1") }), "replyIpv4"},
		{with(base("block", "exact", "x.example"), func(in *RuleInput) { in.ReplyIPv6 = ptr("2001:db8::1") }), "replyIpv6"},
		{with(base("block", "exact", "x.example"), func(in *RuleInput) { in.Reply = ptr("custom_ip") }), "replyIpv4"},
		{with(base("block", "exact", "x.example"), func(in *RuleInput) { in.Reply, in.ReplyIPv4 = ptr("custom_ip"), ptr("2001:db8::1") }), "replyIpv4"},
		{with(base("block", "exact", "x.example"), func(in *RuleInput) { in.Reply, in.ReplyIPv6 = ptr("custom_ip"), ptr("::ffff:192.0.2.1") }), "replyIpv6"},
		{with(base("block", "exact", "x.example"), func(in *RuleInput) { in.Denyallow = []string{"a.x.example"} }), "denyallow"},
		{with(base("allow", "subtree", "x.example"), func(in *RuleInput) { in.Denyallow = []string{"a.x.example"} }), "denyallow"},
		{with(base("block", "subtree", "x.example"), func(in *RuleInput) { in.Denyallow = []string{"y.example"} }), "denyallow[0]"},
		{with(base("block", "subtree", "x.example"), func(in *RuleInput) { in.Denyallow = []string{"a.x.example", "x.example"} }), "denyallow[1]"},
		{with(base("block", "subtree", "x.example"), func(in *RuleInput) { in.Denyallow = []string{"bad..x.example"} }), "denyallow[0]"},
		{with(base("block", "subtree", "x.example"), func(in *RuleInput) {
			for i := range 33 {
				in.Denyallow = append(in.Denyallow, strings.Repeat("a", i+1)+".x.example")
			}
		}), "denyallow"},
		{with(base("block", "subtree", "x.example"), func(in *RuleInput) { in.Invert = ptr(true) }), "invert"},
		{with(base("allow", "regex", "^x"), func(in *RuleInput) { in.Invert = ptr(true) }), "invert"},
		{base("block", "exact", "x.example$dnstype=A"), "pattern"},
	}
	for _, c := range bad {
		_, err := resolveRule(c.in, nil)
		wantInvalid(t, c.in.Pattern+" "+c.field, err, c.field)
	}

	sp, err := resolveRule(with(base("block", "subtree", "||x.example^"), func(in *RuleInput) {
		in.Qtypes = []string{"aaaa", "A", "TYPE1", "type65280"}
		in.QtypesNegate = ptr(true)
		in.Reply, in.ReplyIPv4, in.ReplyIPv6 = ptr(" Custom_IP "), ptr("SELF"), ptr("2001:DB8::1")
		in.Denyallow = []string{"*.b.x.example", "||a.x.example^", "a.x.example"}
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(sp.Qtypes, []string{"A", "AAAA", "TYPE65280"}) || !sp.QtypesNegate || sp.Reply != "custom_ip" ||
		sp.ReplyIPv4 != "self" || sp.ReplyIPv6 != "2001:db8::1" || !slices.Equal(sp.Denyallow, []string{"a.x.example", "b.x.example"}) {
		t.Fatalf("normalised %+v", sp)
	}
	if sp, err := resolveRule(with(base("block", "regex", "/^ads/"), func(in *RuleInput) {
		in.Invert, in.Denyallow = ptr(true), []string{"elsewhere.example"}
	}), nil); err != nil || !sp.Invert || sp.Pattern != "^ads" {
		t.Fatalf("regex %+v %v", sp, err)
	}
}

// A body of 0.12 (without the new members) keeps every stored modifier; a
// stored member that no longer fits is reset when the body does not name
// it and refused when it does.
func TestRuleModifierMerge(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	r, err := e.CreateRule(ctx, RuleInput{Action: "block", Type: "subtree", Pattern: "x.example", Enabled: true,
		Qtypes: []string{"AAAA"}, Reply: ptr("nodata"), Denyallow: []string{"ok.x.example"}})
	if err != nil {
		t.Fatal(err)
	}
	var old RuleInput
	if err := json.Unmarshal([]byte(`{"action":"block","type":"subtree","pattern":"x.example","enabled":true,"comment":"kept"}`), &old); err != nil {
		t.Fatal(err)
	}
	u, err := e.UpdateRule(ctx, r.ID, old)
	if err != nil || !slices.Equal(u.Qtypes, []string{"AAAA"}) || u.Reply != "nodata" || !slices.Equal(u.Denyallow, []string{"ok.x.example"}) ||
		u.Comment != "kept" || !slices.Equal(u.GroupIDs, []int64{1}) {
		t.Fatalf("a 0.12 body changed the new members: %+v %v", u, err)
	}
	// Now an allow rule: reply and denyallow do not fit and are reset; the
	// type set stays.
	u, err = e.UpdateRule(ctx, r.ID, RuleInput{Action: "allow", Type: "subtree", Pattern: "x.example", Enabled: true})
	if err != nil || u.Reply != "" || len(u.Denyallow) != 0 || !slices.Equal(u.Qtypes, []string{"AAAA"}) {
		t.Fatalf("reset: %+v %v", u, err)
	}
	_, err = e.UpdateRule(ctx, r.ID, RuleInput{Action: "allow", Type: "subtree", Pattern: "x.example", Reply: ptr("nodata")})
	wantInvalid(t, "a reply named on an allow rule", err, "reply")
	// A stored negation without types after the types are cleared is reset.
	u, _ = e.UpdateRule(ctx, r.ID, RuleInput{Action: "block", Type: "subtree", Pattern: "x.example", QtypesNegate: ptr(true)})
	if !u.QtypesNegate {
		t.Fatalf("negate %+v", u)
	}
	u, err = e.UpdateRule(ctx, r.ID, RuleInput{Action: "block", Type: "subtree", Pattern: "x.example", Qtypes: []string{}})
	if err != nil || u.QtypesNegate || len(u.Qtypes) != 0 {
		t.Fatalf("types cleared: %+v %v", u, err)
	}
	// A stored denyallow no longer below a changed pattern is reset.
	u, _ = e.UpdateRule(ctx, r.ID, RuleInput{Action: "block", Type: "subtree", Pattern: "x.example", Denyallow: []string{"a.x.example"}})
	u, err = e.UpdateRule(ctx, r.ID, RuleInput{Action: "block", Type: "subtree", Pattern: "y.example"})
	if err != nil || len(u.Denyallow) != 0 {
		t.Fatalf("denyallow after a new pattern: %+v %v", u, err)
	}
	// A stored invert of a regex rule that becomes a subtree rule.
	rr, err := e.CreateRule(ctx, RuleInput{Action: "block", Type: "regex", Pattern: "^keep", Enabled: true, Invert: ptr(true)})
	if err != nil {
		t.Fatal(err)
	}
	u, err = e.UpdateRule(ctx, rr.ID, RuleInput{Action: "block", Type: "subtree", Pattern: "keep.example"})
	if err != nil || u.Invert {
		t.Fatalf("invert reset: %+v %v", u, err)
	}
	_, err = e.UpdateRule(ctx, rr.ID, RuleInput{Action: "block", Type: "subtree", Pattern: "keep.example", Invert: ptr(true)})
	wantInvalid(t, "invert named on a subtree rule", err, "invert")
	// A stored custom_ip reply keeps its addresses; an explicit other reply
	// resets them, an explicit address with it is refused.
	c, err := e.CreateRule(ctx, RuleInput{Action: "block", Type: "exact", Pattern: "c.example", Enabled: true,
		Reply: ptr("custom_ip"), ReplyIPv4: ptr("self")})
	if err != nil || c.ReplyIPv4 != "self" {
		t.Fatalf("custom_ip: %+v %v", c, err)
	}
	u, err = e.UpdateRule(ctx, c.ID, RuleInput{Action: "block", Type: "exact", Pattern: "c.example", Reply: ptr("refused")})
	if err != nil || u.Reply != "refused" || u.ReplyIPv4 != "" {
		t.Fatalf("reply refused: %+v %v", u, err)
	}
	// Rules lists the new members and never null arrays.
	rules, err := e.Rules(ctx, RuleQuery{})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(rules)
	if strings.Contains(string(b), "null") || !strings.Contains(string(b), `"qtypes":[]`) || !strings.Contains(string(b), `"replyIpv4":""`) {
		t.Fatalf("rules JSON %s", b)
	}
}

// The reply of a block rule reaches the decision; allow rules and lists
// never carry one.
func TestRuleReplyDecision(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	if _, err := e.CreateRule(ctx, RuleInput{Action: "block", Type: "exact", Pattern: "r.example", Enabled: true,
		Reply: ptr("custom_ip"), ReplyIPv4: ptr("192.0.2.9"), ReplyIPv6: ptr("self")}); err != nil {
		t.Fatal(err)
	}
	d := e.Check("r.example", qtypeA, []int64{1})
	if d.Reply != "custom_ip" || d.ReplyIPv4 != "192.0.2.9" || d.ReplyIPv6 != "self" {
		t.Fatalf("decision %+v", d)
	}
	addLocalList(t, e, "l.txt", "||l.example^\n", ListInput{})
	for !e.Check("l.example", qtypeA, []int64{1}).Blocked() {
		e.runPending(ctx)
		e.compile()
	}
	if d := e.Check("l.example", qtypeA, []int64{1}); d.Reply != "" {
		t.Fatalf("list decision %+v", d)
	}
}
