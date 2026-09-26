package app

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/auth"
	"github.com/hustenreizjuengling/picache/internal/clients"
	"github.com/hustenreizjuengling/picache/internal/config"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/secrets"
	"github.com/hustenreizjuengling/picache/internal/settings"
	"github.com/hustenreizjuengling/picache/internal/version"
)

// newRestoreApp returns an App with prepared directories and nothing open.
func newRestoreApp(t *testing.T) *App {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{DataDir: filepath.Join(dir, "data"), CacheDir: filepath.Join(dir, "cache"),
		MountRoot: filepath.Join(dir, "mnt")}
	a := newApp(cfg, slog.New(slog.DiscardHandler))
	if err := a.prepareDirs(); err != nil {
		t.Fatal(err)
	}
	return a
}

// openLive opens the live database like the running instance has it open.
func openLive(t *testing.T, a *App) {
	t.Helper()
	d, err := db.Open(a.paths.ConfigDB, 1)
	if err != nil {
		t.Fatal(err)
	}
	a.cdb = d
	t.Cleanup(func() { d.Close() })
}

// closeLive closes the live database (before a restart applies a restore).
func closeLive(a *App) {
	a.cdb.Close()
	a.cdb = nil
}

// makeConfigDB creates a picache.db at path with settings (language lang),
// one admin (user/password), a session, an API token and an audit entry.
// It returns the API token secret.
func makeConfigDB(t *testing.T, path, user, password, lang string) string {
	t.Helper()
	ctx := context.Background()
	d, err := db.Open(path, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	log := slog.New(slog.DiscardHandler)
	set, err := settings.Open(ctx, d, log)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := set.Update(ctx, func(a *settings.All) error { a.Web.Language = lang; return nil }); err != nil {
		t.Fatal(err)
	}
	box, err := secrets.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	a, err := auth.New(ctx, d, set, box, "", log)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Provision(ctx, user, password); err != nil {
		t.Fatal(err)
	}
	s, err := a.Login(ctx, user, password, "", auth.ReqMeta{IP: "192.168.1.10"})
	if err != nil {
		t.Fatal(err)
	}
	p := &auth.Principal{UserID: s.UserID, Username: user, SessionID: s.ID, Scope: auth.ScopeAdmin}
	tok, _, err := a.CreateToken(ctx, p, password, "ci", auth.ScopeAdmin, 0)
	if err != nil {
		t.Fatal(err)
	}
	a.Audit(ctx, p, "192.168.1.10", "test."+user, "", nil)
	return tok
}

func bearerReq(tok string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	return r
}

func execFile(t *testing.T, path string, stmts ...string) {
	t.Helper()
	d, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, q := range stmts {
		if _, err := d.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// openRestored opens the database after a restore like the next start does.
func openRestored(t *testing.T, a *App) (*db.DB, *settings.Store, *auth.Service) {
	t.Helper()
	ctx := context.Background()
	d, err := db.Open(a.paths.ConfigDB, 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	log := slog.New(slog.DiscardHandler)
	set, err := settings.Open(ctx, d, log)
	if err != nil {
		t.Fatal(err)
	}
	box, _ := secrets.New(make([]byte, 32))
	svc, err := auth.New(ctx, d, set, box, "", log)
	if err != nil {
		t.Fatal(err)
	}
	return d, set, svc
}

// SEC-02: an uploaded database may not bring triggers, views, objects the
// live database does not have, or a different account schema: they would
// survive the restore (e.g. keep revoked sessions or tokens alive).
func TestStageRestoreRejectsUntrustedSchema(t *testing.T) {
	a := newRestoreApp(t)
	makeConfigDB(t, a.paths.ConfigDB, "owner", "owner password", "en")
	openLive(t, a)
	ctx := context.Background()
	for name, stmt := range map[string]string{
		"trigger on tokens":   `CREATE TRIGGER keep BEFORE DELETE ON auth_tokens BEGIN SELECT RAISE(IGNORE); END`,
		"trigger on sessions": `CREATE TRIGGER keep BEFORE DELETE ON auth_sessions BEGIN SELECT RAISE(IGNORE); END`,
		"trigger elsewhere":   `CREATE TRIGGER t AFTER UPDATE ON settings BEGIN UPDATE auth_users SET totp_secret = NULL; END`,
		"view":                `CREATE VIEW v AS SELECT * FROM auth_users`,
		"unknown table":       `CREATE TABLE extra (x TEXT)`,
		"unknown index":       `CREATE INDEX settings_updated ON settings(updated_at)`,
		"changed auth table":  `ALTER TABLE auth_users ADD COLUMN extra TEXT`,
		"extra auth index":    `CREATE INDEX auth_users_hash ON auth_users(password_hash)`,
		"virtual table":       `CREATE VIRTUAL TABLE vt USING fts5(x)`,
		"generated column":    `ALTER TABLE settings ADD COLUMN g INTEGER GENERATED ALWAYS AS (id + 1)`,
	} {
		t.Run(name, func(t *testing.T) {
			up := filepath.Join(t.TempDir(), "upload.db")
			makeConfigDB(t, up, "admin", "attacker password", "en")
			// Some statements are refused by the SQLite build (e.g. an
			// unknown virtual table module); then there is nothing to test.
			d, err := sql.Open("sqlite", up)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := d.Exec(stmt); err != nil {
				d.Close()
				t.Skipf("cannot create %s here: %v", name, err)
			}
			d.Close()
			_, err = a.StageRestore(ctx, bytes.NewReader(readFile(t, up)))
			if apperr.KindOf(err) != apperr.KindInvalid {
				t.Fatalf("StageRestore = %v, want an invalid-backup error", err)
			}
			if _, err := os.Stat(a.paths.ConfigDB + ".restore"); !os.IsNotExist(err) {
				t.Fatalf("a rejected backup must not be staged (stat err %v)", err)
			}
			if tmps, _ := filepath.Glob(filepath.Join(filepath.Dir(a.paths.ConfigDB), "*.tmp")); len(tmps) != 0 {
				t.Fatalf("temporary files left: %v", tmps)
			}
		})
	}
}

// A backup made by PiCache is accepted and restores the configuration.
func TestBackupRoundTrip(t *testing.T) {
	a := newRestoreApp(t)
	ctx := context.Background()
	makeConfigDB(t, a.paths.ConfigDB, "owner", "owner password", "de")
	openLive(t, a)
	var buf bytes.Buffer
	if err := a.Backup(ctx, &buf, false); err != nil {
		t.Fatal(err)
	}
	if _, err := a.StageRestore(ctx, bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("a PiCache backup must be accepted: %v", err)
	}
	closeLive(a)
	if restored, err := a.applyStagedRestore(); err != nil || !restored {
		t.Fatalf("applyStagedRestore = %v, %v", restored, err)
	}
	_, set, svc := openRestored(t, a)
	if set.Get().Web.Language != "de" {
		t.Fatal("the configuration must come from the backup")
	}
	if _, err := svc.Login(ctx, "owner", "owner password", "", auth.ReqMeta{IP: "192.168.1.10"}); err != nil {
		t.Fatalf("the account must survive a round trip: %v", err)
	}
}

// SEC-01: a restore replaces the configuration, never the accounts: the
// running instance's users, API tokens and audit log are kept, and every
// session ends. The uploaded users, tokens and audit rows are dropped.
func TestRestoreKeepsLiveAccounts(t *testing.T) {
	a := newRestoreApp(t)
	ctx := context.Background()
	liveTok := makeConfigDB(t, a.paths.ConfigDB, "owner", "owner password", "en")
	openLive(t, a)

	up := filepath.Join(t.TempDir(), "upload.db")
	upTok := makeConfigDB(t, up, "owner", "attacker password", "de")
	// The attacker also adds a second admin.
	execFile(t, up, `INSERT INTO auth_users (username, password_hash, created_at) SELECT 'mallory', password_hash, 1 FROM auth_users`)

	if _, err := a.StageRestore(ctx, bytes.NewReader(readFile(t, up))); err != nil {
		t.Fatal(err)
	}
	closeLive(a)
	restored, err := a.applyStagedRestore()
	if err != nil || !restored {
		t.Fatalf("applyStagedRestore = %v, %v", restored, err)
	}

	d, set, svc := openRestored(t, a)
	if set.Get().Web.Language != "de" {
		t.Fatal("the configuration must come from the backup")
	}
	names, err := auth.Usernames(ctx, d)
	if err != nil || strings.Join(names, ",") != "owner" {
		t.Fatalf("users after restore = %v, %v; want the live account only", names, err)
	}
	if _, err := svc.Login(ctx, "owner", "attacker password", "", auth.ReqMeta{IP: "192.168.1.66"}); err == nil {
		t.Fatal("the password from the backup must not work")
	}
	var sessions int
	if err := d.R.QueryRow(`SELECT COUNT(*) FROM auth_sessions`).Scan(&sessions); err != nil || sessions != 0 {
		t.Fatalf("sessions after restore = %d, %v", sessions, err)
	}
	if _, err := svc.Login(ctx, "owner", "owner password", "", auth.ReqMeta{IP: "192.168.1.10"}); err != nil {
		t.Fatalf("the live password must keep working: %v", err)
	}
	if _, err := svc.Authenticate(bearerReq(liveTok)); err != nil {
		t.Fatalf("the live API token must keep working: %v", err)
	}
	if p, err := svc.Authenticate(bearerReq(upTok)); err == nil {
		t.Fatalf("a token from the backup must not work: %+v", p)
	}
	entries, _, err := svc.AuditLog(ctx, auth.AuditQuery{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	for _, en := range entries {
		actions = append(actions, en.Action)
	}
	joined := strings.Join(actions, ",")
	if !strings.Contains(joined, "test.owner") || !strings.Contains(joined, "system.restore.apply") {
		t.Fatalf("audit after restore = %v (want the live log and the restore entry)", actions)
	}
	if n := strings.Count(joined, "test.owner"); n != 1 {
		t.Fatalf("audit after restore = %v: the backup's audit log must not be imported", actions)
	}
}

// A backup made by 0.1.x (settings and clients schema v1: the download cache
// section and the client bypass column under their old names) is accepted
// and brought up to date by the migrations of the start that applies it.
func TestRestoreOlderBackupIsMigrated(t *testing.T) {
	const oldName = "lancache" // settings section of 0.1.x; the column was <oldName>_bypass
	ctx := context.Background()
	log := slog.New(slog.DiscardHandler)
	withClients := func(path string, fn func(*settings.Store, *clients.Registry)) {
		t.Helper()
		d, err := db.Open(path, 1)
		if err != nil {
			t.Fatal(err)
		}
		defer d.Close()
		set, err := settings.Open(ctx, d, log)
		if err != nil {
			t.Fatal(err)
		}
		reg, err := clients.New(ctx, d, nil, log)
		if err != nil {
			t.Fatal(err)
		}
		if fn != nil {
			fn(set, reg)
		}
	}
	a := newRestoreApp(t)
	makeConfigDB(t, a.paths.ConfigDB, "owner", "owner password", "en")
	withClients(a.paths.ConfigDB, nil)
	openLive(t, a)

	up := filepath.Join(t.TempDir(), "upload.db")
	makeConfigDB(t, up, "owner", "owner password", "de")
	withClients(up, func(set *settings.Store, reg *clients.Registry) {
		if _, err := set.Update(ctx, func(s *settings.All) error {
			s.DownloadCache.Enabled, s.DownloadCache.CacheIPv4 = true, []string{"192.168.1.2"}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := reg.CreateClient(ctx, clients.ClientInput{Name: "Console", Identifiers: []string{"192.168.1.50"},
			DownloadCacheBypass: true}); err != nil {
			t.Fatal(err)
		}
	})
	execFile(t, up, // back to the 0.1.x format
		`UPDATE settings SET doc = json_set(json_remove(doc, '$.downloadCache'), '$.`+oldName+`', json(json_extract(doc, '$.downloadCache')))`,
		`ALTER TABLE client_clients RENAME COLUMN download_cache_bypass TO `+oldName+`_bypass`,
		`DELETE FROM schema_migrations WHERE component IN ('settings', 'clients') AND version > 1`)

	if _, err := a.StageRestore(ctx, bytes.NewReader(readFile(t, up))); err != nil {
		t.Fatalf("a backup of an older version must be accepted: %v", err)
	}
	closeLive(a)
	if restored, err := a.applyStagedRestore(); err != nil || !restored {
		t.Fatalf("applyStagedRestore = %v, %v", restored, err)
	}
	d, set, _ := openRestored(t, a)
	if dc := set.Get().DownloadCache; set.Get().Web.Language != "de" || !dc.Enabled || !slices.Equal(dc.CacheIPv4, []string{"192.168.1.2"}) {
		t.Fatalf("restored settings: language %q, download cache %+v", set.Get().Web.Language, dc)
	}
	var stale int
	if err := d.R.QueryRow(`SELECT COUNT(*) FROM settings WHERE json_type(doc, '$.` + oldName + `') IS NOT NULL`).Scan(&stale); err != nil || stale != 0 {
		t.Fatalf("the old settings section must be gone: %d, %v", stale, err)
	}
	reg, err := clients.New(ctx, d, nil, log)
	if err != nil {
		t.Fatal(err)
	}
	list, err := reg.Clients(ctx)
	if err != nil || len(list) != 1 || !list[0].DownloadCacheBypass {
		t.Fatalf("restored clients: %+v, %v", list, err)
	}
}

// A staged file that does not pass the checks (staged by an older version or
// modified on disk) is set aside at start; the live database stays.
func TestApplyStagedRestoreDiscardsUntrustedFile(t *testing.T) {
	a := newRestoreApp(t)
	makeConfigDB(t, a.paths.ConfigDB, "owner", "owner password", "en")
	before := readFile(t, a.paths.ConfigDB)

	staged := a.paths.ConfigDB + ".restore"
	makeConfigDB(t, staged, "owner", "attacker password", "de")
	execFile(t, staged, `CREATE TRIGGER keep BEFORE DELETE ON auth_tokens BEGIN SELECT RAISE(IGNORE); END`)

	restored, err := a.applyStagedRestore()
	if err != nil || restored {
		t.Fatalf("applyStagedRestore = %v, %v; want false, nil", restored, err)
	}
	if !bytes.Equal(readFile(t, a.paths.ConfigDB), before) {
		t.Fatal("the live database must be unchanged")
	}
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Fatal("the rejected file must not stay staged")
	}
	if failed, _ := filepath.Glob(a.paths.ConfigDB + ".failed-restore-*"); len(failed) != 1 {
		t.Fatalf("the rejected file must be kept for inspection: %v", failed)
	}
}

// Only one upload is staged at a time.
func TestStageRestoreSerialised(t *testing.T) {
	a := newRestoreApp(t)
	restoreMu.Lock()
	_, err := a.StageRestore(context.Background(), strings.NewReader("x"))
	restoreMu.Unlock()
	if apperr.KindOf(err) != apperr.KindConflict {
		t.Fatalf("concurrent StageRestore = %v, want conflict", err)
	}
}

// SEC-01: backups never contain accounts (users with password hashes and
// TOTP secrets, sessions, API tokens); the audit log stays.
func TestBackupHasNoAccounts(t *testing.T) {
	ctx := context.Background()
	a := newRestoreApp(t)
	makeConfigDB(t, a.paths.ConfigDB, "owner", "owner password", "en")
	execFile(t, a.paths.ConfigDB, `UPDATE auth_users SET totp_secret = 'sealed'`)
	openLive(t, a)
	var buf bytes.Buffer
	if err := a.Backup(ctx, &buf, true); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "backup.db")
	if err := os.WriteFile(out, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := sql.Open("sqlite", out)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var users, sessions, tokens, audit int
	if err := d.QueryRow(`SELECT (SELECT COUNT(*) FROM auth_users), (SELECT COUNT(*) FROM auth_sessions),
		(SELECT COUNT(*) FROM auth_tokens), (SELECT COUNT(*) FROM auth_audit)`).Scan(&users, &sessions, &tokens, &audit); err != nil {
		t.Fatal(err)
	}
	if users != 0 || sessions != 0 || tokens != 0 || audit == 0 {
		t.Fatalf("backup has %d users, %d sessions, %d tokens, %d audit rows", users, sessions, tokens, audit)
	}
	if bytes.Contains(buf.Bytes(), []byte("$argon2id$")) {
		t.Fatal("the backup file still contains a password hash")
	}
}

// craftFile runs statements on one connection with writable_schema allowed
// (to disguise schema objects the way a hand-crafted upload can).
func craftFile(t *testing.T, path string, stmts ...string) {
	t.Helper()
	d, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	d.SetMaxOpenConns(1)
	for _, q := range stmts {
		if _, err := d.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

// SEC-02: indexes are accepted only as the live database has them: a named
// index disguised as an automatic index (sqlite_autoindex_*, writable_schema)
// or a live index name with another definition is refused, so an upload
// cannot add e.g. a UNIQUE index that breaks writes to a known table.
func TestStageRestoreRejectsDisguisedIndexes(t *testing.T) {
	a := newRestoreApp(t)
	makeConfigDB(t, a.paths.ConfigDB, "owner", "owner password", "en")
	// Stands for an index a PiCache migration creates on a configuration table.
	execFile(t, a.paths.ConfigDB, `CREATE INDEX settings_updated ON settings(updated_at)`)
	openLive(t, a)
	ctx := context.Background()
	for name, stmts := range map[string][]string{
		"named index as a new autoindex": {
			`CREATE UNIQUE INDEX x_idx ON settings(updated_at)`,
			`PRAGMA writable_schema=ON`,
			`UPDATE sqlite_master SET name = 'sqlite_autoindex_settings_9',
				sql = 'CREATE UNIQUE INDEX sqlite_autoindex_settings_9 ON settings(updated_at)' WHERE name = 'x_idx'`,
		},
		"expression index as an autoindex": {
			`CREATE INDEX x_idx ON settings(abs(updated_at - 9223372036854775807))`,
			`PRAGMA writable_schema=ON`,
			`UPDATE sqlite_master SET name = 'sqlite_autoindex_settings_9', sql = replace(sql, 'x_idx', 'sqlite_autoindex_settings_9')
				WHERE name = 'x_idx'`,
		},
		"live index made unique": {
			`DROP INDEX settings_updated`,
			`CREATE UNIQUE INDEX settings_updated ON settings(updated_at)`,
		},
		"live index made partial": {
			`DROP INDEX settings_updated`,
			`CREATE INDEX settings_updated ON settings(updated_at) WHERE updated_at > 0`,
		},
		"live index on another column": {
			`DROP INDEX settings_updated`,
			`CREATE INDEX settings_updated ON settings(doc)`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			up := filepath.Join(t.TempDir(), "upload.db")
			makeConfigDB(t, up, "admin", "attacker password", "de")
			execFile(t, up, `CREATE INDEX settings_updated ON settings(updated_at)`)
			craftFile(t, up, stmts...)
			_, err := a.StageRestore(ctx, bytes.NewReader(readFile(t, up)))
			if apperr.KindOf(err) != apperr.KindInvalid || !strings.Contains(err.Error(), "index") {
				t.Fatalf("StageRestore = %v, want an invalid-backup error about the index", err)
			}
			if _, err := os.Stat(a.paths.ConfigDB + ".restore"); !os.IsNotExist(err) {
				t.Fatal("a rejected backup must not be staged")
			}
		})
	}

	// The same definition with other whitespace, and the real automatic
	// indexes, are accepted.
	up := filepath.Join(t.TempDir(), "upload.db")
	makeConfigDB(t, up, "admin", "attacker password", "de")
	execFile(t, up, "CREATE  INDEX\tsettings_updated\n\tON settings(updated_at)")
	if _, err := a.StageRestore(ctx, bytes.NewReader(readFile(t, up))); err != nil {
		t.Fatalf("an upload with the live indexes must be accepted: %v", err)
	}
}

// A trigger that is already in the live database (planted before uploads
// were checked) neither leaks password hashes into a backup nor survives
// the next start.
func TestPlantedLiveTriggerIsRemoved(t *testing.T) {
	ctx := context.Background()
	a := newRestoreApp(t)
	makeConfigDB(t, a.paths.ConfigDB, "owner", "owner password", "en")
	execFile(t, a.paths.ConfigDB,
		`CREATE TRIGGER keep_users BEFORE DELETE ON auth_users BEGIN SELECT RAISE(IGNORE); END`,
		`CREATE TRIGGER keep_tokens BEFORE DELETE ON auth_tokens BEGIN SELECT RAISE(IGNORE); END`,
		`CREATE TRIGGER keep_nas BEFORE UPDATE ON settings BEGIN SELECT RAISE(IGNORE); END`,
		`CREATE VIEW hashes AS SELECT password_hash FROM auth_users`)
	openLive(t, a)

	var buf bytes.Buffer
	if err := a.Backup(ctx, &buf, false); err != nil {
		t.Fatalf("backup: %v", err)
	}
	if bytes.Contains(buf.Bytes(), []byte("$argon2id$")) {
		t.Fatal("the backup contains a password hash")
	}
	out := filepath.Join(t.TempDir(), "backup.db")
	if err := os.WriteFile(out, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	bd, err := sql.Open("sqlite", out)
	if err != nil {
		t.Fatal(err)
	}
	defer bd.Close()
	var objs, users, tokens int
	if err := bd.QueryRow(`SELECT (SELECT COUNT(*) FROM sqlite_master WHERE type IN ('trigger', 'view')),
		(SELECT COUNT(*) FROM auth_users), (SELECT COUNT(*) FROM auth_tokens)`).Scan(&objs, &users, &tokens); err != nil {
		t.Fatal(err)
	}
	if objs != 0 || users != 0 || tokens != 0 {
		t.Fatalf("backup has %d triggers/views, %d users, %d tokens", objs, users, tokens)
	}
	// The backup of a database with a planted trigger is still a valid
	// PiCache backup.
	if _, err := a.StageRestore(ctx, bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("the scrubbed backup must be accepted: %v", err)
	}

	// At start the live database loses them too (with a warning).
	if err := a.removePlantedSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if err := a.cdb.R.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type IN ('trigger', 'view')`).Scan(&objs); err != nil || objs != 0 {
		t.Fatalf("live database after start: %d triggers/views, %v", objs, err)
	}
	if _, err := a.cdb.W.Exec(`DELETE FROM auth_tokens`); err != nil {
		t.Fatal(err)
	}
	if err := a.cdb.R.QueryRow(`SELECT COUNT(*) FROM auth_tokens`).Scan(&tokens); err != nil || tokens != 0 {
		t.Fatalf("tokens after delete = %d, %v", tokens, err)
	}
}

// The pre-upgrade copy is the database as the previous version left it:
// made before this version migrates anything (app_meta included), because
// a rollback puts it back for the previous version, which refuses newer
// schema versions. Without a copy nothing is migrated and the start fails.
func TestPreUpgradeBackupBeforeMigrations(t *testing.T) {
	ctx := context.Background()
	a := newRestoreApp(t)
	openLive(t, a)
	oldVersion, oldMigrations := version.Version, appMigrations
	t.Cleanup(func() { version.Version, appMigrations = oldVersion, oldMigrations })
	appState := func(d *sql.DB) (schema int, bin string) {
		t.Helper()
		if err := d.QueryRow(`SELECT MAX(version) FROM schema_migrations WHERE component = 'app'`).Scan(&schema); err != nil {
			t.Fatal(err)
		}
		if err := d.QueryRow(`SELECT value FROM app_meta WHERE key = 'binary_version'`).Scan(&bin); err != nil {
			t.Fatal(err)
		}
		return schema, bin
	}

	version.Version = "v1.0.0"
	if err := a.preUpgradeBackup(ctx); err != nil {
		t.Fatal(err)
	}
	// v1.1.0 adds an app migration.
	appMigrations = append(slices.Clone(oldMigrations), `CREATE TABLE app_test_v2 (x INTEGER)`)
	version.Version = "v1.1.0"
	if err := a.preUpgradeBackup(ctx); err != nil {
		t.Fatal(err)
	}
	backups := filepath.Join(a.cfg.DataDir, "backups")
	copies, _ := filepath.Glob(filepath.Join(backups, "picache-v1.0.0-*.db"))
	if len(copies) != 1 {
		t.Fatalf("copies %v", copies)
	}
	cp, err := sql.Open("sqlite", copies[0])
	if err != nil {
		t.Fatal(err)
	}
	schema, bin := appState(cp)
	cp.Close()
	if schema != len(oldMigrations) || bin != "v1.0.0" {
		t.Fatalf("the copy has app schema %d and version %s: it was made after the new version's migration", schema, bin)
	}
	if schema, bin := appState(a.cdb.R); schema != len(oldMigrations)+1 || bin != "v1.1.0" {
		t.Fatalf("live database: app schema %d, version %s", schema, bin)
	}

	// No copy possible: nothing is migrated, the error fails the start.
	appMigrations = append(slices.Clone(appMigrations), `CREATE TABLE app_test_v3 (x INTEGER)`)
	version.Version = "v1.2.0"
	if err := os.RemoveAll(backups); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(backups, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.preUpgradeBackup(ctx); err == nil || !strings.Contains(err.Error(), "before the upgrade from v1.1.0") {
		t.Fatalf("copy failed: %v", err)
	}
	if schema, bin := appState(a.cdb.R); schema != len(oldMigrations)+1 || bin != "v1.1.0" {
		t.Fatalf("migrated without a copy: app schema %d, version %s", schema, bin)
	}
}

// Going back from 0.11.0: the copy made at the first start of 0.11.0 has
// the schema of 0.10 (auth v1 without roles, settings v4), because it is
// made before any migration; the live database is migrated after it.
func TestPreUpgradeCopyKeepsV010Schema(t *testing.T) {
	ctx := context.Background()
	a := newRestoreApp(t)
	makeConfigDB(t, a.paths.ConfigDB, "owner", "owner password", "en")
	// Back to the state 0.10 left: auth v1, settings v4, binary v0.10.0.
	execFile(t, a.paths.ConfigDB,
		`ALTER TABLE auth_users DROP COLUMN role`,
		`DELETE FROM schema_migrations WHERE (component = 'auth' AND version > 1) OR (component = 'settings' AND version > 4)`,
		`UPDATE settings SET doc = json_remove(doc, '$.web.restrictToNetworks')`,
		`CREATE TABLE IF NOT EXISTS app_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`INSERT OR REPLACE INTO app_meta (key, value) VALUES ('binary_version', 'v0.10.0')`,
		`INSERT OR IGNORE INTO schema_migrations (component, version, applied_at) VALUES ('app', 1, 1)`)
	openLive(t, a)
	oldVersion := version.Version
	t.Cleanup(func() { version.Version = oldVersion })
	version.Version = "v0.11.0"
	if err := a.preUpgradeBackup(ctx); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.DiscardHandler)
	set, err := settings.Open(ctx, a.cdb, log)
	if err != nil {
		t.Fatal(err)
	}
	box, _ := secrets.New(make([]byte, 32))
	if _, err := auth.New(ctx, a.cdb, set, box, "", log); err != nil {
		t.Fatal(err)
	}
	copies, _ := filepath.Glob(filepath.Join(a.cfg.DataDir, "backups", "picache-v0.10.0-*.db"))
	if len(copies) != 1 {
		t.Fatalf("copies %v", copies)
	}
	versions := func(d *sql.DB) (authV, setV int, hasRole bool) {
		t.Helper()
		if err := d.QueryRow(`SELECT (SELECT MAX(version) FROM schema_migrations WHERE component = 'auth'),
			(SELECT MAX(version) FROM schema_migrations WHERE component = 'settings'),
			EXISTS (SELECT 1 FROM pragma_table_info('auth_users') WHERE name = 'role')`).Scan(&authV, &setV, &hasRole); err != nil {
			t.Fatal(err)
		}
		return
	}
	cp, err := sql.Open("sqlite", copies[0])
	if err != nil {
		t.Fatal(err)
	}
	defer cp.Close()
	if authV, setV, hasRole := versions(cp); authV != 1 || setV != 4 || hasRole {
		t.Fatalf("the copy has auth v%d, settings v%d, role column %v", authV, setV, hasRole)
	}
	if authV, setV, hasRole := versions(a.cdb.R); authV != 2 || setV != 5 || !hasRole {
		t.Fatalf("live: auth v%d, settings v%d, role column %v", authV, setV, hasRole)
	}
	if set.Get().Web.RestrictToNetworks {
		t.Fatal("an upgraded installation keeps the web UI open to every address")
	}
	users, err := auth.ListUsers(ctx, a.cdb)
	if err != nil || len(users) != 1 || users[0].Role != auth.RoleAdmin {
		t.Fatalf("existing accounts become admins: %+v %v", users, err)
	}
}
