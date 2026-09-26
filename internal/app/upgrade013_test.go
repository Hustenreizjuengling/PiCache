package app

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"path/filepath"
	"slices"
	"testing"

	"github.com/hustenreizjuengling/picache/internal/clients"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/dns/filter"
	dnsserver "github.com/hustenreizjuengling/picache/internal/dns/server"
)

// migrateConfig builds every picache.db component at path, those named in
// at only up to the given version (the schema of an older release).
func migrateConfig(t *testing.T, path string, at map[string]int) {
	t.Helper()
	d, err := db.Open(path, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, c := range configComponents {
		steps := c.steps()
		if v, ok := at[c.name]; ok {
			steps = steps[:v]
		}
		if err := d.Migrate(context.Background(), c.name, steps); err != nil {
			t.Fatal(err)
		}
	}
}

type offline struct{}

func (offline) RoundTrip(*http.Request) (*http.Response, error) { return nil, errors.New("offline") }

// A picache.db of 0.12 (clients v3, filter v2, dns v2) with lists, rules,
// records and their group links is accepted as a backup, and the start that
// applies it migrates it: every link, list ID and the filter_lists
// sequence stay, the new columns take their defaults.
func TestRestore012Database(t *testing.T) {
	ctx := context.Background()
	log := slog.New(slog.DiscardHandler)
	a := newRestoreApp(t)
	makeConfigDB(t, a.paths.ConfigDB, "owner", "owner password", "en")
	migrateConfig(t, a.paths.ConfigDB, nil)
	openLive(t, a)

	up := filepath.Join(t.TempDir(), "upload.db")
	makeConfigDB(t, up, "owner", "owner password", "de")
	migrateConfig(t, up, map[string]int{"clients": 3, "filter": 2, "dns": 2})
	execFile(t, up,
		`INSERT INTO client_groups (id, name, comment, enabled, created_at) VALUES (2, 'Kids', '', 1, 1), (3, 'Staff', '', 0, 1)`,
		`INSERT INTO client_clients (id, name, comment, download_cache_bypass, ignore_logs, ignore_stats, created_at, updated_at)
			VALUES (5, 'Tablet', '', 0, 1, 0, 1, 1)`,
		`INSERT INTO client_identifiers (value, client_id, kind, pos) VALUES ('192.168.1.50', 5, 'ip', 0)`,
		`INSERT INTO client_memberships (client_id, group_id) VALUES (5, 2), (5, 3)`,
		`INSERT INTO filter_lists (id, name, url, kind, plain_domains, enabled, comment, created_at, category)
			VALUES (9, 'Mine', 'https://lists.example/mine.txt', 'block', 'subtree', 1, 'c', 1, 'other'),
			(12, 'Gone', 'https://lists.example/gone.txt', 'block', 'exact', 1, '', 1, 'other')`,
		`DELETE FROM filter_lists WHERE id = 12`,
		`INSERT INTO filter_list_groups (list_id, group_id) VALUES (9, 2), (9, 3)`,
		`INSERT INTO filter_rules (id, action, type, pattern, enabled, comment, created_at, updated_at)
			VALUES (4, 'block', 'subtree', 'ads.example', 1, 'mine', 1, 1), (6, 'allow', 'regex', '^ok\.', 0, '', 1, 1)`,
		`INSERT INTO filter_rule_groups (rule_id, group_id) VALUES (4, 2), (6, 1), (6, 3)`,
		`INSERT INTO dns_records (id, name, type, value, ttl, enabled, comment, created_at, updated_at)
			VALUES (3, 'nas.lan', 'A', '192.168.1.5', 300, 1, 'nas', 1, 1), (7, 'www.lan', 'CNAME', 'nas.lan', 60, 0, '', 1, 1)`)

	if _, err := a.StageRestore(ctx, bytes.NewReader(readFile(t, up))); err != nil {
		t.Fatalf("a backup of 0.12 must be accepted: %v", err)
	}
	closeLive(a)
	if restored, err := a.applyStagedRestore(); err != nil || !restored {
		t.Fatalf("applyStagedRestore = %v, %v", restored, err)
	}
	d, set, _ := openRestored(t, a)
	var seq int64
	if err := d.R.QueryRow(`SELECT seq FROM sqlite_sequence WHERE name = 'filter_lists'`).Scan(&seq); err != nil || seq != 12 {
		t.Fatalf("sequence %d, %v", seq, err)
	}
	reg, err := clients.New(ctx, d, nil, log)
	if err != nil {
		t.Fatal(err)
	}
	groups, err := reg.Groups(ctx)
	if err != nil || len(groups) != 3 {
		t.Fatalf("groups %+v %v", groups, err)
	}
	for _, g := range groups {
		if len(g.Upstreams) != 0 || g.UpstreamPreset != "" || g.DeviceClientID != nil {
			t.Errorf("group %+v", g)
		}
	}
	cl, err := reg.Clients(ctx)
	if err != nil || len(cl) != 1 || !slices.Equal(cl[0].GroupIDs, []int64{2, 3}) || !cl[0].IgnoreLogs || cl[0].IgnoreStats {
		t.Fatalf("clients %+v %v", cl, err)
	}
	eng, err := filter.New(ctx, d, set, &http.Client{Transport: offline{}}, filepath.Join(t.TempDir(), "lists"), log)
	if err != nil {
		t.Fatal(err)
	}
	lists, err := eng.Lists(ctx)
	if err != nil || len(lists) != 2 { // the default list 1 and 9
		t.Fatalf("lists %+v %v", lists, err)
	}
	if l := lists[1]; l.ID != 9 || !slices.Equal(l.GroupIDs, []int64{2, 3}) || l.Format != filter.FormatDomains || l.NameAuto ||
		l.PlainDomains != "subtree" || l.Comment != "c" {
		t.Errorf("list %+v", l)
	}
	rules, err := eng.Rules(ctx, filter.RuleQuery{})
	if err != nil || len(rules) != 2 {
		t.Fatalf("rules %+v %v", rules, err)
	}
	if r := rules[0]; r.ID != 4 || !slices.Equal(r.GroupIDs, []int64{2}) || len(r.Qtypes) != 0 || r.Reply != "" || len(r.Denyallow) != 0 ||
		r.Invert || r.Comment != "mine" {
		t.Errorf("rule %+v", r)
	}
	if r := rules[1]; r.ID != 6 || !slices.Equal(r.GroupIDs, []int64{1, 3}) || r.Enabled {
		t.Errorf("rule %+v", r)
	}
	if !eng.Check("x.ads.example", 28, []int64{2}).Blocked() {
		t.Error("the migrated rule does not block AAAA (every type)")
	}
	n, err := eng.CreateList(ctx, filter.ListInput{URL: "https://lists.example/new.txt"})
	if err != nil || n.ID != 13 || !n.NameAuto {
		t.Errorf("new list %+v %v", n, err)
	}
	srv, err := dnsserver.New(ctx, dnsserver.Deps{DB: d, Settings: set, Clients: reg, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	recs, err := srv.Records(ctx)
	if err != nil || len(recs) != 2 {
		t.Fatalf("records %+v %v", recs, err)
	}
	for _, r := range recs {
		if r.Scope != dnsserver.ScopeAll || len(r.GroupIDs) != 0 || r.OtherFamily != dnsserver.FamilyNoData || r.Data != nil {
			t.Errorf("record %+v", r)
		}
	}
	if recs[0].ID != 3 || recs[0].Comment != "nas" || recs[1].ID != 7 || recs[1].Enabled {
		t.Errorf("records %+v", recs)
	}
}
