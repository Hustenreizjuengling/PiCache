package app

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"encoding/pem"
	"errors"
	"io"
	stdlog "log"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/api"
	"github.com/hustenreizjuengling/picache/internal/clients"
	"github.com/hustenreizjuengling/picache/internal/config"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/dns/filter"
	"github.com/hustenreizjuengling/picache/internal/dns/parental"
	dnsserver "github.com/hustenreizjuengling/picache/internal/dns/server"
	"github.com/hustenreizjuengling/picache/internal/secrets"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// syncApp builds the parts of an App a sync touches: the configuration
// database, the settings (with the sealer), clients, filter (offline),
// parental controls and the DNS server.
func syncApp(t *testing.T) *App {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	cfg := &config.Config{DataDir: filepath.Join(dir, "data"), CacheDir: filepath.Join(dir, "cache"), MountRoot: filepath.Join(dir, "mnt")}
	log := slog.New(slog.DiscardHandler)
	a := newApp(cfg, log)
	if err := a.prepareDirs(); err != nil {
		t.Fatal(err)
	}
	var err error
	if a.cdb, err = db.Open(a.paths.ConfigDB, 2); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.cdb.Close() })
	if err := a.cdb.Migrate(ctx, "app", appMigrations); err != nil {
		t.Fatal(err)
	}
	if a.set, err = settings.Open(ctx, a.cdb, log); err != nil {
		t.Fatal(err)
	}
	if a.box, err = secrets.New(make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	a.set.SetSealer(a.box.Seal, a.box.Open)
	if a.clients, err = clients.New(ctx, a.cdb, nil, log); err != nil {
		t.Fatal(err)
	}
	if a.filter, err = filter.New(ctx, a.cdb, a.set, &http.Client{Transport: offline{}}, a.paths.ListsDir, log); err != nil {
		t.Fatal(err)
	}
	if a.parental, err = parental.New(ctx, a.cdb, a.clients, log); err != nil {
		t.Fatal(err)
	}
	if a.dns, err = dnsserver.New(ctx, dnsserver.Deps{DB: a.cdb, Settings: a.set, Clients: a.clients, Log: log}); err != nil {
		t.Fatal(err)
	}
	a.sync = newSyncer(a)
	return a
}

// exportOf builds the export of a like GET /system/export does.
func exportOf(t *testing.T, a *App, sections ...string) *api.ConfigExport {
	t.Helper()
	ctx := context.Background()
	out := &api.ConfigExport{Format: api.ExportFormat, FormatVersion: api.ExportFormatVersion, Version: "v0.15.0",
		Schema: ConfigSchemaVersions(), ExportedAt: time.Now().UTC(), Groups: []api.ExportGroupName{},
		Sections: map[string]jsontext.Value{}}
	groups, err := a.clients.Groups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range groups {
		out.Groups = append(out.Groups, api.ExportGroupName{ID: g.ID, Name: g.Name})
	}
	add := func(name string, v any) {
		b, err := json.Marshal(v, json.Deterministic(true))
		if err != nil {
			t.Fatal(err)
		}
		out.Sections[name] = b
	}
	for _, sec := range sections {
		switch sec {
		case settings.SectionClientsGroups:
			cl, err := a.clients.Clients(ctx)
			if err != nil {
				t.Fatal(err)
			}
			add(sec, api.ExportClientsGroups{Groups: groups, Clients: cl})
		case settings.SectionListsRules:
			var v api.ExportListsRules
			if v.Lists, err = a.filter.Lists(ctx); err != nil {
				t.Fatal(err)
			}
			if v.Rules, err = a.filter.Rules(ctx, filter.RuleQuery{}); err != nil {
				t.Fatal(err)
			}
			if v.IPRules, err = a.filter.IPRules(ctx, filter.IPRuleQuery{}); err != nil {
				t.Fatal(err)
			}
			add(sec, v)
		case settings.SectionLocalDNS:
			var v api.ExportLocalDNS
			if v.Records, err = a.dns.Records(ctx); err != nil {
				t.Fatal(err)
			}
			if v.Forwarders, err = a.dns.Forwarders(ctx); err != nil {
				t.Fatal(err)
			}
			add(sec, v)
		case settings.SectionParental:
			gcs, err := a.parental.List(ctx)
			if err != nil {
				t.Fatal(err)
			}
			v := api.ExportParental{Groups: []api.ExportParentalGroup{}}
			for _, gc := range gcs {
				v.Groups = append(v.Groups, api.ExportParentalGroup{GroupID: gc.GroupID, BlockedServices: gc.BlockedServices,
					Schedules: gc.Schedules, SafeSearch: gc.SafeSearch, Categories: map[string]bool{}})
			}
			add(sec, v)
		case settings.SectionDNSSettings:
			raw, err := settings.SyncableSettings(a.set.Get())
			if err != nil {
				t.Fatal(err)
			}
			out.Sections[sec] = raw
		}
	}
	if out.ContentSHA256, err = api.ExportContentSHA256(out.Sections); err != nil {
		t.Fatal(err)
	}
	return out
}

// seedPrimary gives a primary groups, a client, a list, rules, a record,
// a forwarder and parental controls; the group Kids gets the id kidsID.
func seedPrimary(t *testing.T, a *App, gap int) (kidsID int64) {
	t.Helper()
	ctx := context.Background()
	for i := range gap { // shifts the ids against the follower's
		if _, err := a.clients.CreateGroup(ctx, clients.GroupInput{Name: "extra" + string(rune('a'+i))}); err != nil {
			t.Fatal(err)
		}
	}
	kids, err := a.clients.CreateGroup(ctx, clients.GroupInput{Name: "Kids", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.clients.CreateClient(ctx, clients.ClientInput{Name: "Tablet", Identifiers: []string{"192.168.1.50", "iface:eth1"},
		GroupIDs: []int64{kids.ID}}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.filter.CreateList(ctx, filter.ListInput{URL: "https://lists.example/kids.txt", Enabled: true, GroupIDs: []int64{kids.ID}}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.filter.CreateRule(ctx, filter.RuleInput{Action: "block", Type: "subtree", Pattern: "games.example", Enabled: true,
		GroupIDs: []int64{kids.ID}}); err != nil {
		t.Fatal(err)
	}
	groups := dnsserver.ScopeGroups
	if _, err := a.dns.CreateRecord(ctx, dnsserver.RecordInput{Name: "nas.lan", Type: "A", Value: "192.168.1.2", Enabled: true,
		Scope: &groups, GroupIDs: []int64{kids.ID}}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.dns.CreateForwarder(ctx, dnsserver.ForwarderInput{Domains: []string{"corp.example"}, Upstreams: []string{"192.168.1.1"},
		Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.parental.Update(ctx, kids.ID, parental.UpdateInput{BlockedServices: []string{"tiktok"}}); err != nil {
		t.Fatal(err)
	}
	return kids.ID
}

var allSynced = []string{settings.SectionClientsGroups, settings.SectionListsRules, settings.SectionLocalDNS, settings.SectionParental}

// Everything group-linked is synced: the follower gets the primary's rows
// and IDs (its own group of the same name had another id), the
// parental override of a group whose name stays is kept, and a list keeps
// its id.
func TestSyncApplyAllSections(t *testing.T) {
	ctx := context.Background()
	p, f := syncApp(t), syncApp(t)
	kidsP := seedPrimary(t, p, 3)
	kidsF, err := f.clients.CreateGroup(ctx, clients.GroupInput{Name: "kids", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if kidsF.ID == kidsP {
		t.Fatalf("test setup: the ids must differ (%d)", kidsP)
	}
	if _, err := f.clients.CreateClient(ctx, clients.ClientInput{Name: "Old", Identifiers: []string{"192.168.1.60"}}); err != nil {
		t.Fatal(err)
	}
	mins := 30
	if _, err := f.parental.Update(ctx, kidsF.ID, parental.UpdateInput{BlockedServices: []string{"youtube"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.parental.SetPause(ctx, kidsF.ID, parental.PauseInput{Minutes: &mins}); err != nil {
		t.Fatal(err)
	}
	exp := exportOf(t, p, allSynced...)
	if err := checkExport(exp, allSynced); err != nil {
		t.Fatal(err)
	}
	if err := f.applySync(ctx, exp, allSynced); err != nil {
		t.Fatal(err)
	}
	groups, _ := f.clients.Groups(ctx)
	if len(groups) != 5 || !slices.ContainsFunc(groups, func(g clients.Group) bool { return g.ID == kidsP && g.Name == "Kids" }) {
		t.Fatalf("groups %+v", groups)
	}
	cl, _ := f.clients.Clients(ctx)
	if len(cl) != 1 || cl[0].Name != "Tablet" || !slices.Equal(cl[0].GroupIDs, []int64{kidsP}) ||
		!slices.Equal(cl[0].Identifiers, []string{"192.168.1.50", "iface:eth1"}) {
		t.Fatalf("clients %+v", cl)
	}
	if id := f.clients.Identify(netip.MustParseAddr("192.168.1.50")); id.Name != "Tablet" {
		t.Errorf("the synced client does not identify: %+v", id)
	}
	lists, _ := f.filter.Lists(ctx)
	pl, _ := p.filter.Lists(ctx)
	if len(lists) != len(pl) || lists[len(lists)-1].ID != pl[len(pl)-1].ID || !slices.Equal(lists[len(lists)-1].GroupIDs, []int64{kidsP}) {
		t.Fatalf("lists %+v, primary %+v", lists, pl)
	}
	if !f.filter.Check("x.games.example", 1, []int64{kidsP}).Blocked() {
		t.Error("the synced rule does not block")
	}
	recs, _ := f.dns.Records(ctx)
	if len(recs) != 1 || !slices.Equal(recs[0].GroupIDs, []int64{kidsP}) {
		t.Fatalf("records %+v", recs)
	}
	fwds, _ := f.dns.Forwarders(ctx)
	if len(fwds) != 1 || fwds[0].Domain != "corp.example" {
		t.Fatalf("forwarders %+v", fwds)
	}
	gc, err := f.parental.Get(ctx, kidsP)
	if err != nil || !slices.Equal(gc.BlockedServices, []string{"tiktok"}) {
		t.Fatalf("parental %+v %v", gc, err)
	}
	if !gc.State.Paused {
		t.Errorf("the pause of the group (same name) was not kept: %+v", gc.State)
	}
}

// A group-linked section without clients-and-groups is mapped onto the
// follower's groups by name; an unknown name fails and keeps everything.
func TestSyncMapsGroupsByName(t *testing.T) {
	ctx := context.Background()
	p, f := syncApp(t), syncApp(t)
	kidsP := seedPrimary(t, p, 2)
	exp := exportOf(t, p, settings.SectionLocalDNS)
	err := f.applySync(ctx, exp, []string{settings.SectionLocalDNS})
	if err == nil || !strings.Contains(err.Error(), `the group "Kids" of the primary does not exist here`) {
		t.Fatalf("unknown group: %v", err)
	}
	if recs, _ := f.dns.Records(ctx); len(recs) != 0 {
		t.Fatalf("a failed run kept records: %+v", recs)
	}
	kidsF, err := f.clients.CreateGroup(ctx, clients.GroupInput{Name: "KIDS", Enabled: true})
	if err != nil || kidsF.ID == kidsP {
		t.Fatalf("group %+v %v", kidsF, err)
	}
	if err := f.applySync(ctx, exp, []string{settings.SectionLocalDNS}); err != nil {
		t.Fatal(err)
	}
	recs, _ := f.dns.Records(ctx)
	if len(recs) != 1 || !slices.Equal(recs[0].GroupIDs, []int64{kidsF.ID}) {
		t.Fatalf("records %+v (follower group %d)", recs, kidsF.ID)
	}
	if groups, _ := f.clients.Groups(ctx); len(groups) != 2 {
		t.Fatalf("the follower's groups changed: %+v", groups)
	}
}

// RQ-05: a reload that fails after the commit fails the run (its state is
// not stored), so the next run applies the export again and the components
// follow the tables instead of keeping the previous configuration.
func TestSyncReloadFailureFailsTheRun(t *testing.T) {
	ctx := context.Background()
	p, f := syncApp(t), syncApp(t)
	kidsP := seedPrimary(t, p, 0)
	exp := exportOf(t, p, allSynced...)
	// Every read of the components fails after the commit: the reader pool
	// is swapped for a closed one (the transaction uses the writer).
	closed, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "closed.db"))
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()
	good := f.cdb.R
	f.cdb.R = closed
	err = f.applySync(ctx, exp, allSynced)
	f.cdb.R = good
	if err == nil || !strings.Contains(err.Error(), "reload after the sync") {
		t.Fatalf("a failed reload: %v", err)
	}
	if recs, _ := f.dns.Records(ctx); len(recs) != 1 {
		t.Fatalf("the tables were not committed: records %+v", recs)
	}
	if f.filter.Check("x.games.example", 1, []int64{kidsP}).Blocked() {
		t.Fatal("test setup: the filter was reloaded")
	}
	if err := f.applySync(ctx, exp, allSynced); err != nil {
		t.Fatalf("the next run: %v", err)
	}
	if !f.filter.Check("x.games.example", 1, []int64{kidsP}).Blocked() {
		t.Error("the next run did not reload the filter")
	}
}

// CR-2: the run after a failed reload brings the lists in line with the
// database although the database already equals the export: a list whose
// URL and kind changed in the failed run is reloaded with them (not kept
// with its old ones), and the downloaded copies of the lists that run
// removed or changed are deleted.
func TestSyncRetryAppliesChangedAndRemovedLists(t *testing.T) {
	ctx := context.Background()
	p, f := syncApp(t), syncApp(t)
	seedPrimary(t, p, 0)
	gone, err := p.filter.CreateList(ctx, filter.ListInput{URL: "https://lists.example/gone.txt", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.applySync(ctx, exportOf(t, p, allSynced...), allSynced); err != nil {
		t.Fatal(err)
	}
	lists, err := p.filter.Lists(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var kids filter.List
	for _, l := range lists {
		if l.URL == "https://lists.example/kids.txt" {
			kids = l
		}
	}
	if kids.ID == 0 {
		t.Fatalf("test setup: no kids list in %+v", lists)
	}
	// The follower has downloaded both lists.
	copyOf := func(id int64) string { return filepath.Join(f.paths.ListsDir, strconv.FormatInt(id, 10)+".txt") }
	for _, id := range []int64{kids.ID, gone.ID} {
		if err := os.WriteFile(copyOf(id), []byte("ads.example\n"), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	// The primary turns the kids list into an allowlist of another URL and
	// removes the other list; the follower's reload of that run fails.
	if _, err := p.filter.UpdateList(ctx, kids.ID, filter.ListInput{URL: "https://lists.example/allow.txt", Kind: "allow",
		Enabled: true, GroupIDs: kids.GroupIDs}); err != nil {
		t.Fatal(err)
	}
	if err := p.filter.DeleteList(ctx, gone.ID); err != nil {
		t.Fatal(err)
	}
	exp := exportOf(t, p, allSynced...)
	closed, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "closed.db"))
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()
	good := f.cdb.R
	f.cdb.R = closed
	err = f.applySync(ctx, exp, allSynced)
	f.cdb.R = good
	if err == nil || !strings.Contains(err.Error(), "reload after the sync") {
		t.Fatalf("a failed reload: %v", err)
	}
	if err := f.applySync(ctx, exp, allSynced); err != nil {
		t.Fatalf("the next run: %v", err)
	}
	got, err := f.filter.Lists(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[int64]filter.List{}
	for _, l := range got {
		byID[l.ID] = l
	}
	if l := byID[kids.ID]; l.URL != "https://lists.example/allow.txt" || l.Kind != "allow" {
		t.Fatalf("the changed list after the retry: %+v", l)
	}
	if _, ok := byID[gone.ID]; ok || len(got) != len(lists)-1 {
		t.Fatalf("the follower's lists after the retry: %+v", got)
	}
	for _, id := range []int64{kids.ID, gone.ID} {
		if _, err := os.Stat(copyOf(id)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the copy of list %d was kept: %v", id, err)
		}
	}
}

// A run whose data fails the validation keeps everything of the follower.
func TestSyncValidationFailureKeepsEverything(t *testing.T) {
	ctx := context.Background()
	p, f := syncApp(t), syncApp(t)
	seedPrimary(t, p, 0)
	if _, err := f.dns.CreateRecord(ctx, dnsserver.RecordInput{Name: "keep.lan", Type: "A", Value: "192.168.1.9", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	exp := exportOf(t, p, allSynced...)
	var ld api.ExportLocalDNS
	if err := json.Unmarshal(exp.Sections[settings.SectionLocalDNS], &ld); err != nil {
		t.Fatal(err)
	}
	ld.Records[0].Value = "not an address"
	b, _ := json.Marshal(ld, json.Deterministic(true))
	exp.Sections[settings.SectionLocalDNS] = b
	if err := f.applySync(ctx, exp, allSynced); err == nil || !strings.Contains(err.Error(), "local-dns") {
		t.Fatalf("invalid record: %v", err)
	}
	if recs, _ := f.dns.Records(ctx); len(recs) != 1 || recs[0].Name != "keep.lan" {
		t.Fatalf("records %+v", recs)
	}
	if groups, _ := f.clients.Groups(ctx); len(groups) != 1 {
		t.Fatalf("groups %+v", groups)
	}
}

// The synced DNS settings replace only the syncable members.
func TestSyncDNSSettings(t *testing.T) {
	ctx := context.Background()
	p, f := syncApp(t), syncApp(t)
	if _, err := p.set.Update(ctx, func(s *settings.All) error {
		s.DNS.Upstreams = []string{"9.9.9.9"}
		s.DNS.AllowedNetworks = []string{"203.0.113.0/24"}
		s.Filter.BlockingMode = "nxdomain"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.set.Update(ctx, func(s *settings.All) error { s.DNS.AllowedNetworks = []string{"198.51.100.0/24"}; return nil }); err != nil {
		t.Fatal(err)
	}
	secs := []string{settings.SectionDNSSettings}
	if err := f.applySync(ctx, exportOf(t, p, secs...), secs); err != nil {
		t.Fatal(err)
	}
	got := f.set.Get()
	if !slices.Equal(got.DNS.Upstreams, []string{"9.9.9.9"}) || got.Filter.BlockingMode != "nxdomain" {
		t.Errorf("synced members: %v %q", got.DNS.Upstreams, got.Filter.BlockingMode)
	}
	if !slices.Equal(got.DNS.AllowedNetworks, []string{"198.51.100.0/24"}) {
		t.Errorf("a member that is never synced changed: %v", got.DNS.AllowedNetworks)
	}
}

// REV-3: a 0.17 primary exports upstreams, fallbacks, group upstreams and
// forwarder targets with text after "#" as it stored them (1.0 refuses such
// input). The follower applies the export with 0.17's meaning (the text
// dropped, as for its own stored settings) and logs each entry; before, the
// validation refused the dns-settings and with them every other section.
func TestSyncLegacyHashUpstreamsFrom017Primary(t *testing.T) {
	ctx := context.Background()
	p, f := syncApp(t), syncApp(t)
	var logBuf strings.Builder
	f.log = slog.New(slog.NewTextHandler(&logBuf, nil))
	kids := seedPrimary(t, p, 0)
	secs := append([]string{settings.SectionDNSSettings}, allSynced...)
	exp := exportOf(t, p, secs...)
	// As 0.17 exports them.
	var dnsSet map[string]map[string]jsontext.Value
	if err := json.Unmarshal(exp.Sections[settings.SectionDNSSettings], &dnsSet); err != nil {
		t.Fatal(err)
	}
	dnsSet["dns"]["upstreams"] = jsontext.Value(`["9.9.9.9#dns.quad9.net","tls://1.1.1.1#cloudflare-dns.com"]`)
	dnsSet["dns"]["fallbackUpstreams"] = jsontext.Value(`["8.8.8.8#google"]`)
	exp.Sections[settings.SectionDNSSettings] = mustJSON(t, dnsSet)
	var cg api.ExportClientsGroups
	if err := json.Unmarshal(exp.Sections[settings.SectionClientsGroups], &cg); err != nil {
		t.Fatal(err)
	}
	for i := range cg.Groups {
		if cg.Groups[i].ID == kids {
			cg.Groups[i].Upstreams = []string{"1.1.1.1#family"}
		}
	}
	exp.Sections[settings.SectionClientsGroups] = mustJSON(t, cg)
	var ld api.ExportLocalDNS
	if err := json.Unmarshal(exp.Sections[settings.SectionLocalDNS], &ld); err != nil {
		t.Fatal(err)
	}
	ld.Forwarders[0].Upstreams = []string{"192.168.1.1#corp"}
	exp.Sections[settings.SectionLocalDNS] = mustJSON(t, ld)
	exp.Version = "v0.17.0"
	var err error
	if exp.ContentSHA256, err = api.ExportContentSHA256(exp.Sections); err != nil {
		t.Fatal(err)
	}
	if err := checkExport(exp, secs); err != nil {
		t.Fatal(err)
	}
	if err := f.applySync(ctx, exp, secs); err != nil {
		t.Fatalf("refused: %v", err)
	}
	got := f.set.Get().DNS
	if !slices.Equal(got.Upstreams, []string{"9.9.9.9", "tls://1.1.1.1"}) || !slices.Equal(got.FallbackUpstreams, []string{"8.8.8.8"}) {
		t.Errorf("dns-settings: %v %v", got.Upstreams, got.FallbackUpstreams)
	}
	groups, _ := f.clients.Groups(ctx)
	if i := slices.IndexFunc(groups, func(g clients.Group) bool { return g.ID == kids }); i < 0 || !slices.Equal(groups[i].Upstreams, []string{"1.1.1.1"}) {
		t.Errorf("groups %+v", groups)
	}
	fwds, _ := f.dns.Forwarders(ctx)
	if len(fwds) != 1 || !slices.Equal(fwds[0].Upstreams, []string{"192.168.1.1"}) {
		t.Errorf("forwarders %+v", fwds)
	}
	if !f.filter.Check("x.games.example", 1, []int64{kids}).Blocked() {
		t.Error("the other sections were not applied")
	}
	for _, want := range []string{`where=dns-settings sent="\"9.9.9.9#dns.quad9.net\"" used="\"9.9.9.9\""`,
		`sent="\"tls://1.1.1.1#cloudflare-dns.com\""`, `sent="\"8.8.8.8#google\""`,
		`where="clients-and-groups: group Kids" sent="\"1.1.1.1#family\""`,
		`where="local-dns: forwarder corp.example" sent="\"192.168.1.1#corp\""`} {
		if !strings.Contains(logBuf.String(), want) {
			t.Errorf("log lacks %s:\n%s", want, logBuf.String())
		}
	}
	if n := strings.Count(logBuf.String(), `the primary sent an upstream with text after`); n != 5 {
		t.Errorf("%d log lines:\n%s", n, logBuf.String())
	}
}

// An export of a newer schema, another format version or a wrong content
// hash is refused.
func TestCheckExport(t *testing.T) {
	p := syncApp(t)
	secs := []string{settings.SectionLocalDNS}
	exp := exportOf(t, p, secs...)
	if err := checkExport(exp, secs); err != nil {
		t.Fatal(err)
	}
	newer := *exp
	newer.Schema = map[string]int{"clients": 999}
	newer.Version = "v9.0.0"
	if err := checkExport(&newer, secs); err == nil ||
		err.Error() != "the primary runs PiCache v9.0.0 with a newer configuration schema: update this follower" {
		t.Errorf("newer schema: %v", err)
	}
	unknown := *exp
	unknown.Schema = map[string]int{"future": 1}
	if err := checkExport(&unknown, secs); err == nil || !strings.Contains(err.Error(), "newer configuration schema") {
		t.Errorf("unknown component: %v", err)
	}
	format := *exp
	format.FormatVersion = 2
	if err := checkExport(&format, secs); err == nil {
		t.Error("format version 2 accepted")
	}
	hash := *exp
	hash.ContentSHA256 = strings.Repeat("0", 64)
	if err := checkExport(&hash, secs); err == nil {
		t.Error("a wrong content hash accepted")
	}
	if err := checkExport(exp, []string{settings.SectionParental}); err == nil {
		t.Error("a missing section accepted")
	}
}

// syncCA is a CA and a leaf certificate for primary.test.
type syncCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  string
}

func newSyncCA(t *testing.T, name string) *syncCA {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return &syncCA{cert: cert, key: key, pem: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}
}

func (ca *syncCA) leaf(t *testing.T, serial int64) tls.Certificate {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "primary.test"},
		DNSNames: []string{"primary.test"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// primaryServer serves exp (or answer) over TLS with cert and counts the
// requests that reached it with the token.
type primaryServer struct {
	srv    *httptest.Server
	tokens atomic.Int64
	hits   atomic.Int64
}

func newPrimaryServer(t *testing.T, cert tls.Certificate, handler http.HandlerFunc) *primaryServer {
	t.Helper()
	ps := &primaryServer{}
	ps.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ps.hits.Add(1)
		if r.Header.Get("Authorization") == "Bearer "+testSyncToken {
			ps.tokens.Add(1)
		}
		handler(w, r)
	}))
	ps.srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	ps.srv.Config.ErrorLog = stdlog.New(io.Discard, "", 0) // the refused handshakes
	ps.srv.StartTLS()
	t.Cleanup(ps.srv.Close)
	return ps
}

const testSyncToken = "pc_synctokensecret0123456789"

// follow configures f as a follower of https://primary.test:8443 trusting
// caPEM, and dials srv for every connection.
func follow(t *testing.T, f *App, caPEM string, ps *primaryServer, sections []string) {
	t.Helper()
	tok := testSyncToken
	if _, err := f.set.Update(context.Background(), func(s *settings.All) error {
		s.Sync = settings.Sync{Mode: settings.SyncFollower, Source: "https://primary.test:8443", Token: &tok, CAPEM: caPEM,
			IntervalMinutes: 15, Sections: sections}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f.sync.dial = func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, ps.srv.Listener.Addr().String())
	}
}

// The transport: the chain is verified against caPem (a leaf re-issued by
// the same CA keeps working), the token is sent only after a verified
// handshake, no redirect is followed, and an unchanged export is not
// applied again (until a restore made the follower forget it).
func TestSyncTransport(t *testing.T) {
	ctx := context.Background()
	p, f := syncApp(t), syncApp(t)
	seedPrimary(t, p, 1)
	if _, err := f.clients.CreateGroup(ctx, clients.GroupInput{Name: "Kids", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	secs := []string{settings.SectionLocalDNS}
	exp := exportOf(t, p, secs...)
	body, _ := json.Marshal(exp)
	serve := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/system/export" || r.URL.Query().Get("sections") != "local-dns" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}
	ca := newSyncCA(t, "Primary CA")
	ps := newPrimaryServer(t, ca.leaf(t, 2), serve)
	follow(t, f, ca.pem, ps, secs)
	if err := f.sync.syncNow(ctx); err != nil {
		t.Fatal(err)
	}
	if recs, _ := f.dns.Records(ctx); len(recs) != 1 {
		t.Fatalf("records %+v", recs)
	}
	st := f.sync.Status()
	if st.LastAppliedSHA256 != exp.ContentSHA256 || st.PrimaryVersion != "v0.15.0" || st.Source != "https://primary.test:8443" {
		t.Errorf("status %+v", st)
	}

	// Unchanged: not applied again (a record deleted on the follower stays
	// deleted), until the state is forgotten (a restore).
	if _, err := f.cdb.W.ExecContext(ctx, `DELETE FROM dns_records`); err != nil {
		t.Fatal(err)
	}
	if err := f.sync.syncNow(ctx); err != nil {
		t.Fatal(err)
	}
	if recs, _ := f.dns.Records(ctx); len(recs) != 0 {
		t.Fatal("an unchanged export was applied again")
	}
	f.sync.forget(ctx)
	if err := f.sync.syncNow(ctx); err != nil {
		t.Fatal(err)
	}
	if recs, _ := f.dns.Records(ctx); len(recs) != 1 {
		t.Fatal("the export was not applied after forget")
	}

	// A leaf re-issued by the same CA.
	ps2 := newPrimaryServer(t, ca.leaf(t, 3), serve)
	follow(t, f, ca.pem, ps2, secs)
	f.sync.forget(ctx)
	if err := f.sync.syncNow(ctx); err != nil {
		t.Fatalf("re-issued leaf: %v", err)
	}

	// Another CA: the handshake fails, the token never reaches the server.
	other := newSyncCA(t, "Other CA")
	ps3 := newPrimaryServer(t, other.leaf(t, 4), serve)
	follow(t, f, ca.pem, ps3, secs)
	if err := f.sync.syncNow(ctx); err == nil || !strings.Contains(err.Error(), "cannot reach the primary") {
		t.Fatalf("untrusted certificate: %v", err)
	}
	if ps3.hits.Load() != 0 || ps3.tokens.Load() != 0 {
		t.Fatal("a request was sent over an unverified connection")
	}

	// A redirect is not followed.
	var target atomic.Int64
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { target.Add(1) }))
	defer elsewhere.Close()
	ps4 := newPrimaryServer(t, ca.leaf(t, 5), func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/api/v1/system/export", http.StatusFound)
	})
	follow(t, f, ca.pem, ps4, secs)
	if err := f.sync.syncNow(ctx); err == nil || !strings.Contains(err.Error(), "302") {
		t.Fatalf("redirect: %v", err)
	}
	if target.Load() != 0 {
		t.Fatal("the redirect was followed")
	}

	// 401: the token message, never the token.
	ps5 := newPrimaryServer(t, ca.leaf(t, 6), func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"unauthorized"}}`, http.StatusUnauthorized)
	})
	follow(t, f, ca.pem, ps5, secs)
	f.sync.runOnce(ctx)
	st = f.sync.Status()
	if !strings.Contains(st.LastError, "refused the sync token") || strings.Contains(st.LastError, testSyncToken) {
		t.Errorf("last error %q", st.LastError)
	}
	if status, msg, _, show := f.sync.health(); !show || status != "warn" ||
		!strings.HasPrefix(msg, "sync from https://primary.test:8443 failed: ") {
		t.Errorf("health %s %q %v", status, msg, show)
	}
}

// Run: 409 while off, 429 within 30 s of a start, 409 while running.
func TestSyncRunLimits(t *testing.T) {
	f := syncApp(t)
	if err := f.sync.Run(); err == nil || err.Error() != "sync is off" {
		t.Fatalf("off: %v", err)
	}
	ca := newSyncCA(t, "CA")
	ps := newPrimaryServer(t, ca.leaf(t, 2), func(w http.ResponseWriter, r *http.Request) {})
	follow(t, f, ca.pem, ps, []string{settings.SectionLocalDNS})
	if err := f.sync.Run(); err != nil {
		t.Fatal(err)
	}
	if err := f.sync.Run(); err == nil || err.Error() != "a sync is running" {
		t.Fatalf("running: %v", err)
	}
	f.sync.mu.Lock()
	f.sync.running = false
	f.sync.mu.Unlock()
	if err := f.sync.Run(); err == nil || !strings.Contains(err.Error(), "30 seconds") {
		t.Fatalf("within 30 s: %v", err)
	}
}
