package app

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hustenreizjuengling/picache/internal/notify"
	"github.com/hustenreizjuengling/picache/internal/secrets"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// A master key file created while the configuration holds sealed secrets
// (keys/master.key was deleted) is reported: an ERROR at start and the
// health check "master-key" while stored secrets cannot be decrypted with
// the new key. An edit that keeps a secret (REV-2: a renamed channel, a
// newer updated_at) does not clear it; a secret entered again or deleted
// no longer counts, and the check disappears with the last one. A new
// installation (no secrets) reports nothing.
func TestReplacedMasterKeyReported(t *testing.T) {
	ctx := context.Background()
	a := newRestoreApp(t)
	makeConfigDB(t, a.paths.ConfigDB, "owner", "owner password", "en")
	migrateConfig(t, a.paths.ConfigDB, nil)
	openLive(t, a)
	var logBuf strings.Builder
	a.log = slog.New(slog.NewTextHandler(&logBuf, nil))

	box, err := secrets.Open(a.paths.MasterKeyFile)
	if err != nil || !box.Created {
		t.Fatalf("first key: %+v %v", box, err)
	}
	a.box = box
	a.checkReplacedKey(ctx) // a new installation: nothing sealed yet
	if _, _, _, show := a.masterKeyHealth(ctx); show || strings.Contains(logBuf.String(), "level=ERROR") {
		t.Fatalf("a new installation is reported: %s", logBuf.String())
	}
	seal := func(b *secrets.Box, v, aad string) string {
		t.Helper()
		s, err := b.Seal([]byte(v), aad)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	if _, err := a.cdb.W.Exec(`INSERT INTO notify_channels (id, name, kind, url, secret_sealed, created_at, updated_at)
		VALUES ('c1', 'Phone', 'ntfy', 'https://ntfy.example/x', ?, 1, 1)`, seal(box, "tk_abc", notify.SecretAAD("c1"))); err != nil {
		t.Fatal(err)
	}
	if _, err := a.cdb.W.Exec(`INSERT INTO settings_secrets (name, sealed, bound, updated_at) VALUES (?, ?, 'https://primary.example', 1)`,
		settings.SecretSyncToken, []byte(seal(box, "pc_token", settings.SecretAAD(settings.SecretSyncToken)))); err != nil {
		t.Fatal(err)
	}
	if n, err := unreadableSecrets(ctx, a.cdb.R, box.Open); n != 0 || err != nil {
		t.Fatalf("secrets sealed with the current key count as unreadable: %d %v", n, err)
	}
	if b, err := secrets.Open(a.paths.MasterKeyFile); err != nil || b.Created {
		t.Fatalf("an existing key is not created: %+v %v", b, err)
	}
	if err := os.Remove(a.paths.MasterKeyFile); err != nil {
		t.Fatal(err)
	}
	if a.box, err = secrets.Open(filepath.Clean(a.paths.MasterKeyFile)); err != nil || !a.box.Created {
		t.Fatalf("replaced key: %v", err)
	}
	a.checkReplacedKey(ctx)
	if !strings.Contains(logBuf.String(), "level=ERROR") || !strings.Contains(logBuf.String(), "secrets=2") {
		t.Fatalf("no error: %s", logBuf.String())
	}
	health := func(want string) {
		t.Helper()
		st, msg, hint, show := a.masterKeyHealth(ctx)
		if want == "" {
			if show {
				t.Fatalf("still reported: %s %q", st, msg)
			}
			return
		}
		if !show || st != "warn" || !strings.HasPrefix(msg, want+" stored secret(s) cannot be decrypted: keys/master.key was replaced on ") ||
			!strings.Contains(hint, "enter the secrets again") {
			t.Fatalf("want %s: health %v %s %q %q", want, show, st, msg, hint)
		}
	}
	health("2")
	// REV-2: the channel is edited without its secret (the row changes, the
	// sealed value stays): still reported.
	if _, err := a.cdb.W.Exec(`UPDATE notify_channels SET name = 'Phone renamed', updated_at = ?`, int64(1)<<52); err != nil {
		t.Fatal(err)
	}
	health("2")
	// The channel's secret is entered again: one is left.
	if _, err := a.cdb.W.Exec(`UPDATE notify_channels SET secret_sealed = ?, updated_at = ?`,
		seal(a.box, "tk_new", notify.SecretAAD("c1")), int64(1)<<52+1); err != nil {
		t.Fatal(err)
	}
	health("1")
	// The sync token is removed: the check goes, and so does the marker.
	if _, err := a.cdb.W.Exec(`DELETE FROM settings_secrets`); err != nil {
		t.Fatal(err)
	}
	health("")
	var marker int
	if err := a.cdb.R.QueryRow(`SELECT COUNT(*) FROM app_meta WHERE key = ?`, metaKeyReplaced).Scan(&marker); err != nil || marker != 0 {
		t.Fatalf("marker left: %d %v", marker, err)
	}
}
