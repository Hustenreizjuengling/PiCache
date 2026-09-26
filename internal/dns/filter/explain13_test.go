package filter

import (
	"context"
	"slices"
	"testing"
)

// Explain with a query type: entries that match the name but not the type
// or whose denyallow set excepts the name are listed as skipped (never
// applying, never decisive); inverted rules are listed when their
// expression does not match; the members of the modifiers are shown.
func TestExplainModifiers(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	mustRule := func(in RuleInput) Rule {
		t.Helper()
		in.Enabled = true
		r, err := e.CreateRule(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	typed := mustRule(RuleInput{Action: "block", Type: "subtree", Pattern: "x.example", Qtypes: []string{"AAAA"}, Reply: ptr("nxdomain")})
	deny := mustRule(RuleInput{Action: "block", Type: "subtree", Pattern: "example", Denyallow: []string{"ok.x.example"}})
	inv := mustRule(RuleInput{Action: "block", Type: "regex", Pattern: `(^|\.)allowed\.example$`, Invert: ptr(true)})
	l := addLocalList(t, e, "e.txt", "||x.example^$dnstype=~A\n", ListInput{})
	for e.Stats().Lists == 0 {
		e.runPending(ctx)
		e.compile()
	}

	find := func(ms []Match, src string, id int64) Match {
		for _, m := range ms {
			if (src == "rule" && m.RuleID == id) || (src == "list" && m.ListID == id) {
				return m
			}
		}
		t.Fatalf("no match of %s %d in %+v", src, id, ms)
		return Match{}
	}
	ms, err := e.Explain(ctx, "a.x.example", qtypeA, []int64{1})
	if err != nil {
		t.Fatal(err)
	}
	m := find(ms, "rule", typed.ID)
	if m.Skipped != SkippedQtype || m.Applies || m.Decisive || !slices.Equal(m.Qtypes, []string{"AAAA"}) || m.Reply != "nxdomain" {
		t.Fatalf("typed rule %+v", m)
	}
	if m := find(ms, "list", l.ID); m.Skipped != SkippedQtype || !m.QtypesNegate || !slices.Equal(m.Qtypes, []string{"A"}) || m.Reply != "" {
		t.Fatalf("list entry %+v", m)
	}
	if m := find(ms, "rule", deny.ID); !m.Applies || !m.Decisive || !slices.Equal(m.Denyallow, []string{"ok.x.example"}) {
		t.Fatalf("deny rule %+v", m)
	}
	if m := find(ms, "rule", inv.ID); !m.Invert || !m.Applies || m.Decisive {
		t.Fatalf("inverted rule %+v", m)
	}

	ms, _ = e.Explain(ctx, "ok.x.example", qtypeAAAA, []int64{1})
	if m := find(ms, "rule", typed.ID); !m.Applies || !m.Decisive || m.Skipped != "" {
		t.Fatalf("AAAA %+v", m)
	}
	if m := find(ms, "rule", deny.ID); m.Skipped != SkippedDenyallow || m.Applies {
		t.Fatalf("excepted %+v", m)
	}
	ms, _ = e.Explain(ctx, "www.allowed.example", qtypeA, []int64{1})
	for _, m := range ms {
		if m.RuleID == inv.ID {
			t.Fatalf("an inverted rule is listed although its expression matches: %+v", m)
		}
	}
}

// Explain marks the entry decisive that Check decides by: a tier's plain
// exact and subtree entries come before its modified entries ($dnstype,
// $denyallow), whatever their specificity or list order.
func TestExplainModifiedOrder(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	mod := addLocalList(t, e, "mod.txt", "|www.example.com^$dnstype=A\n||www.example.com^$dnstype=A\n", ListInput{})
	plain := addLocalList(t, e, "plain.txt", "||example.com^\n", ListInput{})
	for e.Stats().Lists < 2 {
		e.runPending(ctx)
		e.compile()
	}
	d := e.Check("www.example.com", qtypeA, []int64{1})
	if d.ListID != plain.ID {
		t.Fatalf("check %+v", d)
	}
	ms, err := e.Explain(ctx, "www.example.com", qtypeA, []int64{1})
	if err != nil {
		t.Fatal(err)
	}
	decisive := 0
	for _, m := range ms {
		if m.Decisive {
			decisive++
			if m.ListID != d.ListID {
				t.Fatalf("explain decides by list %d, check by %d: %+v", m.ListID, d.ListID, ms)
			}
		}
	}
	if decisive != 1 || len(ms) != 3 {
		t.Fatalf("matches %+v", ms)
	}
	// Among modified entries the most specific name decides, exact and
	// subtree rows together, then the list order (as the table is probed).
	if ms[1].ListID != mod.ID || ms[2].ListID != mod.ID {
		t.Fatalf("order %+v", ms)
	}
}
