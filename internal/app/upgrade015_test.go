package app

import (
	"bytes"
	"context"
	"log/slog"
	"maps"
	"path/filepath"
	"testing"

	"github.com/hustenreizjuengling/picache/internal/auth"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// configSchema014 is a copy of ConfigSchemaVersions of 0.14.0: a database
// with a newer component version is refused by it (db.Migrate).
var configSchema014 = map[string]int{"app": 1, "auth": 2, "clients": 4, "dhcp": 2, "dns": 3, "filter": 3, "notify": 1,
	"parental": 2, "proxy": 1, "services": 1, "settings": 5, "storage": 1}

// make014DB builds a picache.db with the steps of 0.14 (auth v2, settings
// v5), an admin with a read and an admin token, and a settings document
// with includePrereleases.
func make014DB(t *testing.T, path string, prereleases bool) {
	t.Helper()
	migrateConfig(t, path, map[string]int{"auth": 2, "settings": 5})
	d, err := db.Open(path, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	pre := "false"
	if prereleases {
		pre = "true"
	}
	if _, err := d.W.Exec(`INSERT INTO settings (id, doc, updated_at) VALUES (1, ?, 0)`,
		`{"updates":{"checkEnabled":true,"includePrereleases":`+pre+`},"web":{"language":"de"}}`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.W.Exec(`INSERT INTO auth_users (id, username, password_hash, role, created_at) VALUES (1, 'owner', 'x', 'admin', 1)`); err != nil {
		t.Fatal(err)
	}
	for i, scope := range []string{"read", "admin"} {
		if _, err := d.W.Exec(`INSERT INTO auth_tokens (id, hash, user_id, name, scope, prefix, created_at) VALUES (?, ?, 1, ?, ?, 'pc_', 1)`,
			i+1, []byte("hash"+scope), "t"+scope, scope); err != nil {
			t.Fatal(err)
		}
	}
}

// A 0.14 picache.db migrates to 0.15: the tokens keep their scopes, the
// channel is derived from includePrereleases, settings_secrets is empty,
// the schema equals a fresh one, and its versions are newer than 0.14's
// (0.14 refuses it).
func TestUpgradeFrom014(t *testing.T) {
	ctx := context.Background()
	log := slog.New(slog.DiscardHandler)
	dir := t.TempDir()
	path := filepath.Join(dir, "picache.db")
	make014DB(t, path, true)
	d, err := db.Open(path, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := MigrateConfigDB(ctx, d); err != nil {
		t.Fatal(err)
	}
	set, err := settings.Open(ctx, d, log)
	if err != nil {
		t.Fatal(err)
	}
	if u := set.Get().Updates; u.Channel != settings.ChannelBeta || !u.IncludePrereleases {
		t.Errorf("updates %+v", u)
	}
	if set.Get().Web.Language != "de" {
		t.Error("the stored settings were lost")
	}
	rows, err := d.R.Query(`SELECT scope FROM auth_tokens ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	var scopes []string
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		scopes = append(scopes, s)
	}
	rows.Close()
	if len(scopes) != 2 || scopes[0] != "read" || scopes[1] != "admin" {
		t.Errorf("token scopes %v", scopes)
	}
	var n int
	_ = d.R.QueryRow(`SELECT COUNT(*) FROM settings_secrets`).Scan(&n)
	if n != 0 {
		t.Errorf("%d secrets", n)
	}
	if _, err := d.W.Exec(`INSERT INTO auth_tokens (hash, user_id, name, scope, prefix, created_at) VALUES (x'01', 1, 'sync', 'sync', 'pc_', 1)`); err != nil {
		t.Errorf("the migrated table refuses the scope sync: %v", err)
	}

	fresh := filepath.Join(dir, "fresh.db")
	migrateConfig(t, fresh, nil)
	fd, err := db.Open(fresh, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer fd.Close()
	got, err := schemaNames(ctx, d.R)
	if err != nil {
		t.Fatal(err)
	}
	want, err := schemaNames(ctx, fd.R)
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(got, want) {
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%v: migrated %q, fresh %q", k, got[k], v)
			}
		}
		for k := range got {
			if _, ok := want[k]; !ok {
				t.Errorf("%v only in the migrated database", k)
			}
		}
	}
	newer := false
	for comp, v := range ConfigSchemaVersions() {
		old, ok := configSchema014[comp]
		if !ok {
			t.Errorf("component %s is not in the copy of 0.14", comp)
			continue
		}
		if v < old {
			t.Errorf("%s: %d is older than 0.14's %d", comp, v, old)
		}
		if v > old {
			newer = true
		}
	}
	if !newer {
		t.Error("0.15 has no component newer than 0.14: 0.14 would open the migrated database")
	}
	for comp, want := range map[string]int{"auth": 3, "settings": 6} {
		var v int
		_ = d.R.QueryRow(`SELECT MAX(version) FROM schema_migrations WHERE component = ?`, comp).Scan(&v)
		if v != want {
			t.Errorf("%s at v%d", comp, v)
		}
	}
}

// A 0.14 backup is accepted and migrated by the start that applies it,
// full and partial.
func TestRestore014Backup(t *testing.T) {
	ctx := context.Background()
	for _, sections := range [][]string{nil, {settings.SectionSettings}} {
		a := newRestoreApp(t)
		makeConfigDB(t, a.paths.ConfigDB, "owner", "owner password", "en")
		migrateConfig(t, a.paths.ConfigDB, nil)
		openLive(t, a)
		up := filepath.Join(t.TempDir(), "upload.db")
		make014DB(t, up, false)
		if _, err := a.StageRestore(ctx, bytes.NewReader(readFile(t, up)), sections); err != nil {
			t.Fatalf("sections %v: %v", sections, err)
		}
		closeLive(a)
		if restored, err := a.applyStagedRestore(); err != nil || !restored {
			t.Fatalf("sections %v: applyStagedRestore = %v, %v", sections, restored, err)
		}
		d, set, _ := openRestored(t, a)
		if u := set.Get().Updates; u.Channel != settings.ChannelStable || u.IncludePrereleases {
			t.Errorf("sections %v: updates %+v", sections, u)
		}
		if set.Get().Web.Language != "de" {
			t.Errorf("sections %v: the settings of the backup were not restored", sections)
		}
		var v int
		_ = d.R.QueryRow(`SELECT MAX(version) FROM schema_migrations WHERE component = 'settings'`).Scan(&v)
		if v != 6 {
			t.Errorf("sections %v: settings v%d", sections, v)
		}
		users, err := auth.ListUsers(ctx, d)
		if err != nil || len(users) != 1 || users[0].Username != "owner" {
			t.Errorf("sections %v: the live accounts were not kept: %+v %v", sections, users, err)
		}
	}
}
