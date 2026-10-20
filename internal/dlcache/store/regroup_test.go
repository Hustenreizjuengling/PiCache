package cachestore

import (
	"context"
	"strings"
	"testing"
	"time"
)

// waitGroupRules waits until the background regroup stored version v.
func waitGroupRules(t *testing.T, s *Store, v int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var got int64
		_ = s.db.R.QueryRow(`SELECT num FROM store_meta WHERE key = 'group_rules'`).Scan(&got)
		if got == v {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("group rules version %d, want %d", got, v)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Objects cached under older grouping rules move to the groups the current
// rules derive, once per rules version; aggregates and pins move along.
func TestRegroup(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	s := e.open()
	host := "warframe:cdn.example.com"
	a, _ := putObject(t, s, "warframe", "/Game/a", host, 1000)
	putObject(t, s, "warframe", "/Tools/b", host, 2000)
	putObject(t, s, "steam", "/depot/1/x", "steam:depot:1", 500)
	s.Touch(a, 700)
	mustFlush(t, s)
	if err := s.SetGroupPinned(ctx, "warframe", host, true); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	byPath := func(svc, h, path string) string {
		switch {
		case svc != "warframe":
			return svc + ":depot:1"
		case strings.HasPrefix(path, "/Game/"):
			return "warframe:game"
		case strings.HasPrefix(path, "/Tools/"):
			return "warframe:" + strings.Repeat("x", maxGroupKeyLen) // invalid: falls back to the host
		}
		return svc + ":" + h
	}
	e.opt.GroupKey, e.opt.GroupRules = byPath, 1
	s = e.open()
	waitGroupRules(t, s, 1)
	groups, err := s.Groups(ctx, GroupQuery{Sort: "name"})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]GroupUsage{}
	for _, g := range groups.Items {
		got[g.GroupKey] = g
	}
	if g := got["warframe:game"]; len(got) != 3 || g.Objects != 1 || g.TotalBytes != 1000 || g.BytesServed != 700 || !g.Pinned {
		t.Fatalf("groups %+v", groups.Items)
	}
	if g := got[host]; g.Objects != 1 || g.TotalBytes != 2000 || !g.Pinned || got["steam:depot:1"].Objects != 1 {
		t.Fatalf("groups %+v", groups.Items)
	}
	objs, err := s.Objects(ctx, ObjectQuery{Service: "warframe", GroupKey: "warframe:game"})
	if err != nil || len(objs.Items) != 1 || objs.Items[0].ID != a || !objs.Items[0].Pinned {
		t.Fatalf("objects %+v %v", objs.Items, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// The same version again: nothing moves; a newer one moves the rest and
	// unpins the emptied group.
	e.opt.GroupKey = func(svc, h, path string) string { return svc + ":all" }
	s = e.open()
	time.Sleep(50 * time.Millisecond)
	mustFlush(t, s)
	if g, err := s.Groups(ctx, GroupQuery{GroupKey: "warframe:game"}); err != nil || len(g.Items) != 1 {
		t.Fatalf("regrouped again under the same rules version: %+v %v", g.Items, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	e.opt.GroupRules = 2
	s = e.open()
	waitGroupRules(t, s, 2)
	groups, err = s.Groups(ctx, GroupQuery{Sort: "name"})
	if err != nil || len(groups.Items) != 2 || groups.Items[1].GroupKey != "warframe:all" || groups.Items[1].Objects != 2 ||
		!groups.Items[1].Pinned || groups.Items[1].BytesServed != 700 {
		t.Fatalf("groups %+v %v", groups.Items, err)
	}
	var pins int
	if err := s.db.R.QueryRow(`SELECT count(*) FROM store_pinned_groups`).Scan(&pins); err != nil || pins != 1 {
		t.Fatalf("pinned groups %d %v", pins, err)
	}
	if u := s.Usage(); u.Objects != 3 || u.CachedBytes != 3500 {
		t.Fatalf("usage %+v", u)
	}
}
