package clients

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
)

func sptr(s string) *string { return &s }

func wantErr(t *testing.T, what string, err error, kind apperr.Kind, field string) {
	t.Helper()
	ae, ok := apperr.As(err)
	if !ok || ae.Kind != kind || ae.Field != field {
		t.Errorf("%s: err = %v, want kind %d field %q", what, err, kind, field)
	}
}

// The upstreams of a group: a preset or its own list (never both, never
// for the Default group), validated and bounded; a body of 0.12 keeps them;
// PUT /groups/{id}/upstreams changes only them.
func TestGroupUpstreams(t *testing.T) {
	r := newTestRegistry(t, false)
	ctx := context.Background()
	kids, err := r.CreateGroup(ctx, GroupInput{Name: "Kids", Enabled: true, UpstreamPreset: sptr("cloudflare-family")})
	if err != nil || kids.UpstreamPreset != "cloudflare-family" || len(kids.Upstreams) != 0 || kids.DeviceClientID != nil {
		t.Fatalf("created %+v %v", kids, err)
	}
	var old GroupInput
	if err := json.Unmarshal([]byte(`{"name":"Kids 2","comment":"c","enabled":true}`), &old); err != nil {
		t.Fatal(err)
	}
	if g, err := r.UpdateGroup(ctx, kids.ID, old); err != nil || g.UpstreamPreset != "cloudflare-family" || g.Name != "Kids 2" {
		t.Fatalf("a 0.12 body changed the preset: %+v %v", g, err)
	}
	// Upstreams given: the stored preset gives way; both named: refused.
	g, err := r.UpdateGroup(ctx, kids.ID, GroupInput{Name: "Kids", Enabled: true, Upstreams: []string{"9.9.9.11", " 9.9.9.11 ", "tls://dns.example"}})
	if err != nil || g.UpstreamPreset != "" || !slices.Equal(g.Upstreams, []string{"9.9.9.11", "tls://dns.example"}) {
		t.Fatalf("own list %+v %v", g, err)
	}
	_, err = r.UpdateGroup(ctx, kids.ID, GroupInput{Name: "Kids", Upstreams: []string{"9.9.9.9"}, UpstreamPreset: sptr("opendns-familyshield")})
	wantErr(t, "both", err, apperr.KindInvalid, "upstreamPreset")
	g, err = r.UpdateGroup(ctx, kids.ID, GroupInput{Name: "Kids", Enabled: true, UpstreamPreset: sptr("opendns-familyshield")})
	if err != nil || g.UpstreamPreset != "opendns-familyshield" || len(g.Upstreams) != 0 {
		t.Fatalf("preset replaces the stored list: %+v %v", g, err)
	}
	for _, c := range []struct {
		in    GroupInput
		field string
	}{
		{GroupInput{Name: "X", Upstreams: []string{"https://"}}, "upstreams[0]"},
		{GroupInput{Name: "X", Upstreams: strings.Split("1.1.1.1,1.1.1.2,1.1.1.3,1.1.1.4,1.1.1.5,1.1.1.6,1.1.1.7,1.1.1.8,1.1.1.9", ",")}, "upstreams"},
		{GroupInput{Name: "X", UpstreamPreset: sptr("strict-mode")}, "upstreamPreset"},
	} {
		_, err := r.CreateGroup(ctx, c.in)
		wantErr(t, c.field, err, apperr.KindInvalid, c.field)
	}
	_, err = r.UpdateGroup(ctx, DefaultGroupID, GroupInput{Name: "Default", Enabled: true, Upstreams: []string{"9.9.9.9"}})
	wantErr(t, "Default upstreams", err, apperr.KindInvalid, "upstreams")
	_, err = r.SetGroupUpstreams(ctx, DefaultGroupID, nil, "cloudflare-family")
	wantErr(t, "Default preset", err, apperr.KindInvalid, "upstreamPreset")
	_, err = r.SetGroupUpstreams(ctx, kids.ID, []string{"9.9.9.9"}, "cloudflare-family")
	wantErr(t, "both at once", err, apperr.KindInvalid, "upstreamPreset")
	_, err = r.SetGroupUpstreams(ctx, 99, []string{}, "")
	wantErr(t, "unknown group", err, apperr.KindNotFound, "")
	// The dedicated route keeps name, comment and enabled.
	if _, err := r.UpdateGroup(ctx, kids.ID, GroupInput{Name: "Kids", Comment: "keep", Enabled: false}); err != nil {
		t.Fatal(err)
	}
	g, err = r.SetGroupUpstreams(ctx, kids.ID, []string{"https://dns.example/dns-query"}, "")
	if err != nil || g.Name != "Kids" || g.Comment != "keep" || g.Enabled || !slices.Equal(g.Upstreams, []string{"https://dns.example/dns-query"}) {
		t.Fatalf("upstreams only %+v %v", g, err)
	}
	// Disabled groups have no resolver in effect.
	cfg, err := r.GroupUpstreamConfigs(ctx)
	if err != nil || len(cfg) != 0 {
		t.Fatalf("configs %+v %v", cfg, err)
	}
	if _, err := r.UpdateGroup(ctx, kids.ID, GroupInput{Name: "Kids", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	cfg, _ = r.GroupUpstreamConfigs(ctx)
	if len(cfg) != 1 || cfg[0].ID != kids.ID || cfg[0].Name != "Kids" || cfg[0].Upstreams[0] != "https://dns.example/dns-query" {
		t.Fatalf("configs %+v", cfg)
	}
	groups, _ := r.Groups(ctx)
	if b, _ := json.Marshal(groups); strings.Contains(string(b), "null") || strings.Contains(string(b), "deviceClientId") {
		t.Fatalf("groups JSON %s", b)
	}
}

// At most 16 distinct group upstream lists (a preset counts as its list)
// and 64 distinct upstreams across them (409).
func TestGroupUpstreamBounds(t *testing.T) {
	r := newTestRegistry(t, false)
	ctx := context.Background()
	for i := range 16 {
		ups := []string{fmt.Sprintf("192.0.2.%d", i+1), fmt.Sprintf("198.51.100.%d", i+1)}
		if _, err := r.CreateGroup(ctx, GroupInput{Name: fmt.Sprintf("G%d", i), Upstreams: ups}); err != nil {
			t.Fatal(err)
		}
	}
	// The same list again is not a new one.
	if _, err := r.CreateGroup(ctx, GroupInput{Name: "Same", Upstreams: []string{"192.0.2.1", "198.51.100.1"}}); err != nil {
		t.Fatalf("a shared list: %v", err)
	}
	_, err := r.CreateGroup(ctx, GroupInput{Name: "More", UpstreamPreset: sptr("cloudflare-family")})
	wantErr(t, "17th list", err, apperr.KindConflict, "")
	if err == nil || !strings.Contains(err.Error(), "at most 16 different group upstream lists are supported") {
		t.Fatalf("message %v", err)
	}
	r2 := newTestRegistry(t, false)
	for i := range 8 {
		var ups []string
		for j := range 8 {
			ups = append(ups, fmt.Sprintf("10.%d.0.%d", i, j+1))
		}
		if _, err := r2.CreateGroup(ctx, GroupInput{Name: fmt.Sprintf("G%d", i), Upstreams: ups}); err != nil {
			t.Fatal(err)
		}
	}
	_, err = r2.CreateGroup(ctx, GroupInput{Name: "65th", Upstreams: []string{"10.99.0.1"}})
	wantErr(t, "65th upstream", err, apperr.KindConflict, "")
	if g, _ := r2.Groups(ctx); len(g) != 9 {
		t.Fatalf("a refused group was created: %d", len(g))
	}
}

// Group and client batches: all or nothing, the Default group never
// deleted, orphans moved into Default, one change notification.
func TestBatchGroupsAndClients(t *testing.T) {
	r := newTestRegistry(t, false)
	ctx := context.Background()
	calls := 0
	r.OnChange(func() { calls++ })
	a, b := mustGroup(t, r, "A", true), mustGroup(t, r, "B", true)
	only := mustClient(t, r, "only-a", []int64{a.ID}, "192.168.1.10")
	both := mustClient(t, r, "a-and-b", []int64{a.ID, b.ID}, "192.168.1.11")
	calls = 0
	_, err := r.BatchGroups(ctx, "delete", []int64{a.ID, DefaultGroupID})
	wantErr(t, "Default", err, apperr.KindForbidden, "")
	_, err = r.BatchGroups(ctx, "disable", []int64{a.ID, 777})
	wantErr(t, "unknown", err, apperr.KindNotFound, "")
	if calls != 0 {
		t.Fatalf("a refused batch notified %d times", calls)
	}
	if n, err := r.BatchGroups(ctx, "disable", []int64{a.ID, b.ID, DefaultGroupID}); err != nil || n != 3 || calls != 1 {
		t.Fatalf("disable %d %v (%d calls)", n, err, calls)
	}
	if n, _ := r.BatchGroups(ctx, "disable", []int64{a.ID}); n != 0 {
		t.Fatalf("disabling a disabled group changed %d", n)
	}
	if n, _ := r.BatchGroups(ctx, "enable", []int64{a.ID, b.ID, DefaultGroupID}); n != 3 {
		t.Fatalf("enable %d", n)
	}
	if n, err := r.BatchGroups(ctx, "delete", []int64{a.ID, b.ID}); err != nil || n != 2 {
		t.Fatalf("delete %d %v", n, err)
	}
	cl, _ := r.Clients(ctx)
	for _, c := range cl {
		if (c.ID == only.ID || c.ID == both.ID) && !slices.Equal(c.GroupIDs, []int64{DefaultGroupID}) {
			t.Fatalf("orphan %+v", c)
		}
	}
	_, err = r.BatchDeleteClients(ctx, []int64{only.ID, 999})
	wantErr(t, "unknown client", err, apperr.KindNotFound, "")
	if n, err := r.BatchDeleteClients(ctx, []int64{only.ID, both.ID}); err != nil || n != 2 {
		t.Fatalf("delete clients %d %v", n, err)
	}
	if cl, _ := r.Clients(ctx); len(cl) != 0 {
		t.Fatalf("clients left %+v", cl)
	}
}

// "Only for this device": a new client for an unknown address (with its
// MAC and host name, in Default), a new group named after it; the same
// call again reuses both; a device named like an admin's group gets its own
// group; a CIDR client leads to a new client with its groups and flags; an
// address of a CIDR client refuses; the revert removes exactly what was
// created.
func TestDeviceGroup(t *testing.T) {
	r := newTestRegistry(t, false)
	ctx := context.Background()
	d, err := r.EnsureDeviceGroup(ctx, DeviceRequest{IP: ip("192.168.1.50"), MAC: "02:00:00:00:00:50", Name: "tablet" + string(rune(0x200b))})
	if err != nil || !d.CreatedClient || !d.CreatedGroup {
		t.Fatalf("new %+v %v", d, err)
	}
	if d.Client.Name != "tablet" || !slices.Equal(d.Client.Identifiers, []string{"192.168.1.50", "02:00:00:00:00:50"}) ||
		!slices.Equal(d.Client.GroupIDs, []int64{DefaultGroupID, d.Group.ID}) {
		t.Fatalf("client %+v", d.Client)
	}
	if d.Group.Name != "tablet" || d.Group.Comment != "Only for tablet" || !d.Group.Enabled || d.Group.DeviceClientID == nil ||
		*d.Group.DeviceClientID != d.Client.ID || d.Group.ClientCount != 1 {
		t.Fatalf("group %+v", d.Group)
	}
	again, err := r.EnsureDeviceGroup(ctx, DeviceRequest{IP: ip("192.168.1.50")})
	if err != nil || again.CreatedClient || again.CreatedGroup || again.Group.ID != d.Group.ID || again.Client.ID != d.Client.ID {
		t.Fatalf("reuse %+v %v", again, err)
	}
	if id := r.Identify(ip("192.168.1.50")); !slices.Equal(id.GroupIDs, []int64{DefaultGroupID, d.Group.ID}) {
		t.Fatalf("identity %+v", id)
	}
	// An admin's group named "Kids" is never joined by a device named Kids.
	kids := mustGroup(t, r, "Kids", true)
	k, err := r.EnsureDeviceGroup(ctx, DeviceRequest{IP: ip("192.168.1.51"), Name: "KIDS"})
	if err != nil || k.Group.ID == kids.ID || k.Group.Name != "KIDS (2)" || slices.Contains(k.Client.GroupIDs, kids.ID) {
		t.Fatalf("named like a group %+v %v", k, err)
	}
	// Without a host name the address names the client; a hostile name is
	// cleaned and cut.
	h, err := r.EnsureDeviceGroup(ctx, DeviceRequest{IP: ip("fd00::52"), Name: string(rune(0x202e)) + strings.Repeat("x", 80)})
	if err != nil || h.Client.Name != strings.Repeat("x", 59) || h.Group.Name != strings.Repeat("x", 59) {
		t.Fatalf("hostile name %+v %v", h, err)
	}
	n, err := r.EnsureDeviceGroup(ctx, DeviceRequest{IP: ip("192.168.1.53")})
	if err != nil || n.Client.Name != "192.168.1.53" {
		t.Fatalf("no name %+v %v", n, err)
	}
	// A client that covers a network: the device gets a client of its own
	// with the network client's groups and flags.
	lan, err := r.CreateClient(ctx, ClientInput{Name: "lan", Identifiers: []string{"10.1.0.0/16", "10.1.0.9"}, GroupIDs: []int64{kids.ID},
		DownloadCacheBypass: true, IgnoreLogs: true})
	if err != nil {
		t.Fatal(err)
	}
	c, err := r.EnsureDeviceGroup(ctx, DeviceRequest{IP: ip("10.1.2.3"), MAC: "02:00:00:00:00:50", Name: "console"})
	if err != nil || !c.CreatedClient || !slices.Contains(c.Client.GroupIDs, kids.ID) || !c.Client.DownloadCacheBypass || !c.Client.IgnoreLogs ||
		!c.Client.IgnoreStats || !slices.Equal(c.Client.Identifiers, []string{"10.1.2.3"}) {
		t.Fatalf("CIDR client %+v %v (the MAC is another client's)", c, err)
	}
	_, err = r.EnsureDeviceGroup(ctx, DeviceRequest{IP: ip("10.1.0.9")})
	wantErr(t, "an address of a CIDR client", err, apperr.KindConflict, "")
	if err == nil || !strings.Contains(err.Error(), "the address belongs to client lan, which also covers a network") {
		t.Fatalf("message %v", err)
	}
	// Revert: the created client and group go, a reused client keeps its
	// groups.
	if err := r.RevertDeviceGroup(ctx, c); err != nil {
		t.Fatal(err)
	}
	if err := r.RevertDeviceGroup(ctx, again); err != nil { // nothing created: nothing done
		t.Fatal(err)
	}
	cl, _ := r.Clients(ctx)
	for _, x := range cl {
		if x.ID == c.Client.ID {
			t.Fatal("the created client survived the revert")
		}
		if x.ID == lan.ID && !slices.Equal(x.GroupIDs, []int64{kids.ID}) {
			t.Fatalf("lan changed %+v", x)
		}
	}
	groups, _ := r.Groups(ctx)
	if slices.ContainsFunc(groups, func(g Group) bool { return g.ID == c.Group.ID }) ||
		!slices.ContainsFunc(groups, func(g Group) bool { return g.ID == d.Group.ID }) {
		t.Fatalf("groups after the revert %+v", groups)
	}
	// Removing the client removes the marker (ON DELETE SET NULL).
	if err := r.DeleteClient(ctx, d.Client.ID); err != nil {
		t.Fatal(err)
	}
	if g, _ := r.group(ctx, d.Group.ID); g.DeviceClientID != nil {
		t.Fatalf("marker kept %+v", g)
	}
}

// A device group that got another member is not reused; a client in 64
// groups cannot join a device group (409, nothing created).
func TestDeviceGroupLimits(t *testing.T) {
	r := newTestRegistry(t, false)
	ctx := context.Background()
	d, err := r.EnsureDeviceGroup(ctx, DeviceRequest{IP: ip("192.168.1.60"), Name: "tv"})
	if err != nil {
		t.Fatal(err)
	}
	mustClient(t, r, "guest", []int64{d.Group.ID}, "192.168.1.61")
	again, err := r.EnsureDeviceGroup(ctx, DeviceRequest{IP: ip("192.168.1.60")})
	if err != nil || !again.CreatedGroup || again.Group.ID == d.Group.ID || again.Group.Name != "tv (2)" {
		t.Fatalf("shared group reused: %+v %v", again, err)
	}
	groups := []int64{DefaultGroupID}
	for i := range maxGroupsPerClient - 1 {
		groups = append(groups, mustGroup(t, r, fmt.Sprintf("g%d", i), true).ID)
	}
	mustClient(t, r, "full", groups, "192.168.1.62")
	before, _ := r.Groups(ctx)
	_, err = r.EnsureDeviceGroup(ctx, DeviceRequest{IP: ip("192.168.1.62")})
	wantErr(t, "64 groups", err, apperr.KindConflict, "")
	if after, _ := r.Groups(ctx); len(after) != len(before) {
		t.Fatalf("a group was created: %d → %d", len(before), len(after))
	}
}

// clients v4 adds the columns without touching groups, clients or
// memberships.
func TestMigration4(t *testing.T) {
	ctx := context.Background()
	cdb, err := db.Open(filepath.Join(t.TempDir(), "picache.db"), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer cdb.Close()
	if err := cdb.Migrate(ctx, "clients", migrations[:3]); err != nil {
		t.Fatal(err)
	}
	if _, err := cdb.W.ExecContext(ctx, `INSERT INTO client_groups (id, name, comment, enabled, created_at) VALUES (5, 'Kids', '', 1, 1);
		INSERT INTO client_clients (id, name, comment, download_cache_bypass, ignore_logs, ignore_stats, created_at, updated_at)
			VALUES (3, 'pc', '', 0, 0, 0, 1, 1);
		INSERT INTO client_memberships (client_id, group_id) VALUES (3, 5);`); err != nil {
		t.Fatal(err)
	}
	r, err := New(ctx, cdb, nil, quiet())
	if err != nil {
		t.Fatal(err)
	}
	groups, _ := r.Groups(ctx)
	if len(groups) != 2 || groups[1].ID != 5 || groups[1].ClientCount != 1 || len(groups[1].Upstreams) != 0 || groups[1].UpstreamPreset != "" ||
		groups[1].DeviceClientID != nil {
		t.Fatalf("groups %+v", groups)
	}
	if id := r.Identify(netip.MustParseAddr("192.0.2.1")); !slices.Equal(id.GroupIDs, []int64{DefaultGroupID}) {
		t.Fatalf("identity %+v", id)
	}
}
