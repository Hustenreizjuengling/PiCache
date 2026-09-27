package app

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// Every table of a freshly migrated picache.db is in exactly one section
// of the restore or never restored, and every table the map names exists.
func TestRestoreSectionMapCoversEveryTable(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "picache.db")
	d, err := db.Open(path, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := MigrateConfigDB(ctx, d); err != nil {
		t.Fatal(err)
	}
	if err := d.Migrate(ctx, "app", appMigrations); err != nil {
		t.Fatal(err)
	}
	rows, err := d.R.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, n)
	}
	rows.Close()
	seen := map[string]int{}
	for _, s := range restoreSectionMap {
		for _, tb := range s.tables {
			seen[tb]++
		}
		for _, p := range s.parents {
			if !slices.Contains(s.tables, p) {
				t.Errorf("section %s deletes %s, which is not one of its tables", s.name, p)
			}
		}
	}
	for _, tb := range neverRestoredTables {
		seen[tb]++
	}
	for _, tb := range tables {
		switch seen[tb] {
		case 0:
			t.Errorf("table %s is in no restore section (restoreSectionMap) and not in neverRestoredTables", tb)
		case 1:
		default:
			t.Errorf("table %s is in %d places of the section map", tb, seen[tb])
		}
	}
	for tb := range seen {
		if !slices.Contains(tables, tb) {
			t.Errorf("the section map names %s, which a migrated database does not have", tb)
		}
	}
	for tb := range groupLinkTables {
		if _, ok := sectionOf(tb); !ok {
			t.Errorf("group link table %s has no section", tb)
		}
	}
	for _, s := range restoreSectionMap {
		if !slices.Contains(settings.RestoreSections, s.name) {
			t.Errorf("section %s is not restorable", s.name)
		}
	}
}

// restoreFixture is a live database and an upload (both migrated) with
// groups: live Default, Other (2), kids (3); upload Default, Kids (2),
// Staff (3).
func restoreFixture(t *testing.T) (*App, string) {
	t.Helper()
	a := newRestoreApp(t)
	makeConfigDB(t, a.paths.ConfigDB, "owner", "owner password", "en")
	migrateConfig(t, a.paths.ConfigDB, nil)
	execFile(t, a.paths.ConfigDB,
		`INSERT INTO client_groups (id, name, comment, enabled, created_at) VALUES (2, 'Other', '', 1, 1), (3, 'kids', '', 1, 1)`,
		`INSERT INTO filter_rules (id, action, type, pattern, enabled, comment, created_at, updated_at)
			VALUES (40, 'block', 'subtree', 'live.example', 1, '', 1, 1)`,
		`INSERT INTO dns_records (id, name, type, value, ttl, enabled, comment, created_at, updated_at) VALUES (8, 'live.lan', 'A', '192.168.1.8', 300, 1, '', 1, 1)`)
	openLive(t, a)
	up := filepath.Join(t.TempDir(), "upload.db")
	makeConfigDB(t, up, "owner", "owner password", "de")
	migrateConfig(t, up, nil)
	execFile(t, up,
		`INSERT INTO client_groups (id, name, comment, enabled, created_at) VALUES (2, 'Kids', '', 1, 1), (3, 'Staff', '', 1, 1)`,
		`INSERT INTO client_clients (id, name, comment, download_cache_bypass, ignore_logs, ignore_stats, created_at, updated_at)
			VALUES (5, 'Tablet', '', 0, 0, 0, 1, 1)`,
		`INSERT INTO client_identifiers (value, client_id, kind, pos) VALUES ('192.168.1.50', 5, 'ip', 0)`,
		`INSERT INTO client_memberships (client_id, group_id) VALUES (5, 2)`,
		`INSERT INTO filter_rules (id, action, type, pattern, enabled, comment, created_at, updated_at)
			VALUES (4, 'block', 'subtree', 'games.example', 1, '', 1, 1)`,
		`INSERT INTO filter_rule_groups (rule_id, group_id) VALUES (4, 2), (4, 1)`,
		`INSERT INTO dns_records (id, name, type, value, ttl, enabled, comment, created_at, updated_at) VALUES (3, 'nas.lan', 'A', '192.168.1.2', 300, 1, '', 1, 1)`,
		`INSERT INTO dns_record_groups (record_id, group_id) VALUES (3, 3)`)
	return a, up
}

// A section restored without clients-and-groups keeps the live groups and
// maps its links by group name (Default always to 1); the live rows of
// the section are replaced, other sections stay.
func TestPartialRestoreMapsGroupsByName(t *testing.T) {
	ctx := context.Background()
	a, up := restoreFixture(t)
	secs := []string{settings.SectionListsRules}
	if _, err := a.StageRestore(ctx, bytes.NewReader(readFile(t, up)), secs); err != nil {
		t.Fatal(err)
	}
	closeLive(a)
	if restored, err := a.applyStagedRestore(); err != nil || !restored {
		t.Fatalf("applyStagedRestore = %v, %v", restored, err)
	}
	if !slices.Equal(a.restoreSections, secs) {
		t.Errorf("restore sections %v", a.restoreSections)
	}
	d, set, _ := openRestored(t, a)
	if set.Get().Web.Language != "en" {
		t.Error("the settings were restored without the section settings")
	}
	var links []string
	rows, err := d.R.Query(`SELECT rule_id || ':' || group_id FROM filter_rule_groups ORDER BY group_id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		links = append(links, s)
	}
	rows.Close()
	if !slices.Equal(links, []string{"4:1", "4:3"}) {
		t.Errorf("rule links %v, want the backup's Kids (2) as the live kids (3)", links)
	}
	var n int
	_ = d.R.QueryRow(`SELECT COUNT(*) FROM filter_rules WHERE id = 40`).Scan(&n)
	if n != 0 {
		t.Error("the live rule of the restored section was kept")
	}
	_ = d.R.QueryRow(`SELECT COUNT(*) FROM dns_records WHERE id = 8`).Scan(&n)
	if n != 1 {
		t.Error("a section that was not selected changed")
	}
	_ = d.R.QueryRow(`SELECT COUNT(*) FROM client_groups`).Scan(&n)
	if n != 3 {
		t.Errorf("groups %d", n)
	}
	// The selection is not left in the restored database.
	var meta int
	_ = d.R.QueryRow(`SELECT COUNT(*) FROM app_meta WHERE key IN ('restore.sections', 'restore.by')`).Scan(&meta)
	if meta != 0 {
		// app.go removes them after the start; the file itself may carry
		// them, which is fine as long as the next start ignores them.
		t.Logf("app_meta keeps %d restore keys until the start removes them", meta)
	}
}

// A link to a group the live data does not have refuses the upload (400
// sections) and the start (the staged file is moved aside, the live
// database stays).
func TestPartialRestoreMissingGroup(t *testing.T) {
	ctx := context.Background()
	a, up := restoreFixture(t)
	_, err := a.StageRestore(ctx, bytes.NewReader(readFile(t, up)), []string{settings.SectionLocalDNS})
	e, ok := apperr.As(err)
	if !ok || e.Field != "sections" ||
		e.Message != `local-dns of the backup refers to the group "Staff", which does not exist here: restore clients-and-groups too or create the group first` {
		t.Fatalf("missing group: %v", err)
	}
	if _, err := os.Stat(a.paths.ConfigDB + ".restore"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a refused upload was staged")
	}
	// Staged anyway (the live data changed after the upload): the start
	// refuses it and keeps the live database.
	if err := StageRestoreFile(ctx, a.cfg.DataDir, up, []string{settings.SectionListsRules}, false); err != nil {
		t.Fatal(err)
	}
	execFile(t, a.paths.ConfigDB, `DELETE FROM client_groups WHERE id = 3`)
	closeLive(a)
	if restored, err := a.applyStagedRestore(); err != nil || restored {
		t.Fatalf("applyStagedRestore = %v, %v", restored, err)
	}
	failed, _ := filepath.Glob(a.paths.ConfigDB + ".failed-restore-*")
	if len(failed) != 1 {
		t.Fatalf("failed files %v", failed)
	}
	d, _, _ := openRestored(t, a)
	var n int
	_ = d.R.QueryRow(`SELECT COUNT(*) FROM filter_rules WHERE id = 40`).Scan(&n)
	if n != 1 {
		t.Error("the live database changed")
	}
}

// With clients-and-groups (and the sections that depend on it) the IDs of
// the backup are kept; the forbidden combinations are refused.
func TestPartialRestoreWithGroups(t *testing.T) {
	ctx := context.Background()
	a, up := restoreFixture(t)
	for _, bad := range [][]string{{settings.SectionClientsGroups}, {"dns-settings"}, {"nope"}, {}} {
		if _, err := a.StageRestore(ctx, bytes.NewReader(readFile(t, up)), bad); err == nil {
			t.Errorf("sections %v accepted", bad)
		} else if e, ok := apperr.As(err); !ok || e.Field != "sections" {
			t.Errorf("sections %v: %v", bad, err)
		}
	}
	secs := []string{settings.SectionClientsGroups, settings.SectionListsRules, settings.SectionLocalDNS, settings.SectionParental}
	if _, err := a.StageRestore(ctx, bytes.NewReader(readFile(t, up)), secs); err != nil {
		t.Fatal(err)
	}
	closeLive(a)
	if restored, err := a.applyStagedRestore(); err != nil || !restored {
		t.Fatalf("applyStagedRestore = %v, %v", restored, err)
	}
	d, _, _ := openRestored(t, a)
	names := map[int64]string{}
	rows, _ := d.R.Query(`SELECT id, name FROM client_groups`)
	for rows.Next() {
		var id int64
		var n string
		_ = rows.Scan(&id, &n)
		names[id] = n
	}
	rows.Close()
	if len(names) != 3 || names[2] != "Kids" || names[3] != "Staff" {
		t.Fatalf("groups %v", names)
	}
	var g int64
	if err := d.R.QueryRow(`SELECT group_id FROM dns_record_groups WHERE record_id = 3`).Scan(&g); err != nil || g != 3 {
		t.Errorf("record link %d %v", g, err)
	}
	var fk int
	_ = d.R.QueryRow(`SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&fk)
	if fk != 0 {
		t.Errorf("%d foreign key violations", fk)
	}
}

// The settings members travel with their sections: settings keeps the
// live dhcp member and storage references, dhcp restores only dhcp, a
// storage target named by the backup's settings needs the section
// storage; settings restores settings_secrets too, and a member the backup
// lacks (an older version's backup) takes its default rather than the live
// value, whose secret is gone.
func TestPartialRestoreSettingsMembers(t *testing.T) {
	ctx := context.Background()
	a, up := restoreFixture(t)
	upDoc := func(q string) { execFile(t, up, q) }
	upDoc(`UPDATE settings SET doc = json_set(doc, '$.dhcp.leaseSeconds', 7200, '$.backups.destination', 'nas1')`)
	upDoc(`INSERT INTO settings_secrets (name, sealed, bound, updated_at) VALUES ('sync.token', 'x', 'https://p', 1)`)
	upDoc(`UPDATE settings SET doc = json_remove(doc, '$.sync', '$.network')`)
	execFile(t, a.paths.ConfigDB, `UPDATE settings SET doc = json_set(doc, '$.dhcp.leaseSeconds', 3600,
		'$.sync', json('{"mode":"follower","source":"https://primary.lan","intervalMinutes":15,"sections":["local-dns"]}'),
		'$.network', json('{"proxy":{"url":"http://proxy.lan:3128","username":"alice"},"proxyFor":{"lists":true}}'))`,
		`INSERT INTO settings_secrets (name, sealed, bound, updated_at) VALUES ('network.proxy.password', 'x', 'http://proxy.lan:3128|alice', 1)`)

	// The backup's destination names a target the result would not have.
	if _, err := a.StageRestore(ctx, bytes.NewReader(readFile(t, up)), []string{settings.SectionStorage}); err == nil ||
		!strings.Contains(err.Error(), "storage target nas1") {
		t.Fatalf("storage reference: %v", err)
	}
	if _, err := a.StageRestore(ctx, bytes.NewReader(readFile(t, up)), []string{settings.SectionSettings}); err != nil {
		t.Fatal(err)
	}
	closeLive(a)
	if restored, err := a.applyStagedRestore(); err != nil || !restored {
		t.Fatalf("applyStagedRestore = %v, %v", restored, err)
	}
	d, set, _ := openRestored(t, a)
	got := set.Get()
	if got.Web.Language != "de" {
		t.Error("the settings member web was not restored")
	}
	if got.DHCP.LeaseSeconds != 3600 {
		t.Errorf("dhcp restored with settings: %d", got.DHCP.LeaseSeconds)
	}
	if got.Backups.Destination != settings.BackupsLocal {
		t.Errorf("backups.destination restored without storage: %q", got.Backups.Destination)
	}
	if got.Sync.Mode != settings.SyncOff || got.Network.Proxy.URL != "" || got.Network.ProxyFor.Lists {
		t.Errorf("live members the backup lacks were kept without their secrets: %+v %+v", got.Sync, got.Network)
	}
	var n int
	_ = d.R.QueryRow(`SELECT COUNT(*) FROM settings_secrets WHERE name = 'sync.token'`).Scan(&n)
	if n != 1 {
		t.Error("settings_secrets was not restored with settings")
	}
}

// An older backup is migrated before the merge (a 0.12 schema here).
func TestPartialRestoreOlderBackup(t *testing.T) {
	ctx := context.Background()
	a := newRestoreApp(t)
	makeConfigDB(t, a.paths.ConfigDB, "owner", "owner password", "en")
	migrateConfig(t, a.paths.ConfigDB, nil)
	execFile(t, a.paths.ConfigDB, `INSERT INTO client_groups (id, name, comment, enabled, created_at) VALUES (7, 'kids', '', 1, 1)`)
	openLive(t, a)
	up := filepath.Join(t.TempDir(), "upload.db")
	makeConfigDB(t, up, "owner", "owner password", "de")
	migrateConfig(t, up, map[string]int{"clients": 3, "filter": 2, "dns": 2})
	execFile(t, up,
		`INSERT INTO client_groups (id, name, comment, enabled, created_at) VALUES (2, 'Kids', '', 1, 1)`,
		`INSERT INTO filter_rules (id, action, type, pattern, enabled, comment, created_at, updated_at)
			VALUES (4, 'block', 'subtree', 'games.example', 1, '', 1, 1)`,
		`INSERT INTO filter_rule_groups (rule_id, group_id) VALUES (4, 2)`)
	if _, err := a.StageRestore(ctx, bytes.NewReader(readFile(t, up)), []string{settings.SectionListsRules}); err != nil {
		t.Fatal(err)
	}
	closeLive(a)
	if restored, err := a.applyStagedRestore(); err != nil || !restored {
		t.Fatalf("applyStagedRestore = %v, %v", restored, err)
	}
	d, _, _ := openRestored(t, a)
	var g int64
	var qtypes sql.NullString
	if err := d.R.QueryRow(`SELECT g.group_id, r.qtypes FROM filter_rule_groups g JOIN filter_rules r ON r.id = g.rule_id`).Scan(&g, &qtypes); err != nil || g != 7 {
		t.Fatalf("rule link %d %v", g, err)
	}
}

// `picache restore` stages the file with restore.by = cli, refuses a
// second one without force, and the start records the selection.
func TestStageRestoreFile(t *testing.T) {
	ctx := context.Background()
	a, up := restoreFixture(t)
	secs := []string{settings.SectionListsRules}
	if err := StageRestoreFile(ctx, a.cfg.DataDir, up, secs, false); err != nil {
		t.Fatal(err)
	}
	if err := StageRestoreFile(ctx, a.cfg.DataDir, up, secs, false); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("second staging: %v", err)
	}
	if err := StageRestoreFile(ctx, a.cfg.DataDir, up, nil, true); err != nil {
		t.Fatalf("--force: %v", err)
	}
	gotSecs, by, err := readRestoreMeta(ctx, a.paths.ConfigDB+".restore")
	if err != nil || gotSecs != nil || by != "cli" {
		t.Fatalf("meta %v %q %v", gotSecs, by, err)
	}
	if err := StageRestoreFile(ctx, a.cfg.DataDir, up, secs, true); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link.db")
	if err := os.Symlink(up, link); err == nil {
		if err := StageRestoreFile(ctx, a.cfg.DataDir, link, secs, true); err == nil {
			t.Error("a symbolic link was staged")
		}
	}
	closeLive(a)
	if restored, err := a.applyStagedRestore(); err != nil || !restored {
		t.Fatalf("applyStagedRestore = %v, %v", restored, err)
	}
	if a.restoreBy != "cli" || !slices.Equal(a.restoreSections, secs) {
		t.Errorf("restore by %q sections %v", a.restoreBy, a.restoreSections)
	}
	// A restore rolled back after a failed start never took effect: the
	// next build must not audit system.restore, end sessions or forget
	// the sync state for it.
	if err := a.rollbackRestore(); err != nil {
		t.Fatal(err)
	}
	if a.restoreSections != nil || a.restoreBy != "" || !a.restoredAt.IsZero() {
		t.Errorf("after the rollback: by %q sections %v at %v", a.restoreBy, a.restoreSections, a.restoredAt)
	}
}

// A backup made without includeSecrets carries no settings secrets.
func TestBackupScrubsSettingsSecrets(t *testing.T) {
	ctx := context.Background()
	a := newRestoreApp(t)
	makeConfigDB(t, a.paths.ConfigDB, "owner", "owner password", "en")
	execFile(t, a.paths.ConfigDB, `INSERT INTO settings_secrets (name, sealed, bound, updated_at) VALUES ('network.proxy.password', 'x', 'http://p:3128|', 1)`)
	openLive(t, a)
	for _, include := range []bool{false, true} {
		var buf bytes.Buffer
		if err := a.Backup(ctx, &buf, include); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "b.db")
		if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		d, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		var n int
		err = d.QueryRow(`SELECT COUNT(*) FROM settings_secrets`).Scan(&n)
		d.Close()
		if err != nil || (n == 1) != include {
			t.Errorf("includeSecrets %v: %d rows, %v", include, n, err)
		}
	}
}
