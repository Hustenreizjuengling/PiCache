package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/hustenreizjuengling/picache/internal/config"
	"github.com/hustenreizjuengling/picache/internal/db"
)

// fakeHost records systemctl calls; directories live in a temp dir.
type fakeHost struct {
	env   hostEnv
	calls [][]string
	fail  error // returned by systemctl
}

func newFakeHost(t *testing.T) *fakeHost {
	t.Helper()
	dir := t.TempDir()
	h := &fakeHost{}
	h.env = hostEnv{
		credDir:         filepath.Join(dir, "etc", "credentials"),
		unitDir:         mkdir(t, filepath.Join(dir, "systemd")),
		checkPrivileges: func() error { return nil },
		systemctl: func(_ context.Context, args ...string) error {
			h.calls = append(h.calls, args)
			return h.fail
		},
		serviceOwner: func(string) (int, int, error) { return 999, 998, nil },
	}
	mkdir(t, filepath.Dir(h.env.credDir))
	return h
}

func (h *fakeHost) unit(t *testing.T, where string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(h.env.unitDir, mountUnitName(where)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const nasPassword = "pa,ss w0rd=\"x\"$1"

func setupApply(t *testing.T) (*config.Config, *Manager, *db.DB, *fakeHost) {
	t.Helper()
	cfg := testConfig(t)
	m, d, _ := newTestManager(t, cfg)
	return cfg, m, d, newFakeHost(t)
}

func TestApplyHostSMB(t *testing.T) {
	cfg, m, _, h := setupApply(t)
	ctx := context.Background()
	tg, err := m.Create(ctx, smbInput(ptr(nasPassword)))
	if err != nil {
		t.Fatal(err)
	}
	if err := applyHost(ctx, h.env, cfg, tg.ID, nil, nil); err != nil {
		t.Fatal(err)
	}
	where := filepath.Join(cfg.MountRoot, tg.ID)
	if fi, err := os.Stat(where); err != nil || !fi.IsDir() {
		t.Fatalf("mountpoint: %v", err)
	}
	credPath := filepath.Join(h.env.credDir, tg.ID+".cred")
	cred, err := os.ReadFile(credPath)
	if err != nil {
		t.Fatal(err)
	}
	if want := "username=picache\npassword=" + nasPassword + "\ndomain=WORKGROUP\n"; string(cred) != want {
		t.Fatalf("credentials:\n%s", cred)
	}
	if runtime.GOOS == "linux" {
		if fi, _ := os.Stat(credPath); fi.Mode().Perm() != 0o600 {
			t.Errorf("credentials mode %v", fi.Mode().Perm())
		}
		if fi, _ := os.Stat(h.env.credDir); fi.Mode().Perm() != 0o700 {
			t.Errorf("credentials dir mode %v", fi.Mode().Perm())
		}
	}
	unit := h.unit(t, where)
	mustContain(t, "unit", unit, "What=//192.168.1.10/picache\n", "Where="+where+"\n", "Type=cifs\n",
		"Options=credentials="+credPath+",vers=3.1.1,uid=999,gid=998,file_mode=0640,dir_mode=0750,soft,"+
			"nosuid,nodev,noexec,noatime,_netdev,nofail,x-systemd.mount-timeout=30\n",
		"WantedBy=remote-fs.target\n")
	if strings.Contains(unit, "w0rd") {
		t.Fatal("password in the unit file")
	}
	want := [][]string{{"daemon-reload"}, {"enable", "--now", mountUnitName(where)}}
	if !slices.EqualFunc(h.calls, want, slices.Equal) {
		t.Fatalf("systemctl calls %v", h.calls)
	}
	if s := readApplyState(requestsDir(cfg), tg.ID); s != applyApplied {
		t.Fatalf("apply state %q", s)
	}
	// No temp files are left behind.
	for _, dir := range []string{h.env.credDir, h.env.unitDir} {
		entries, _ := os.ReadDir(dir)
		if len(entries) != 1 {
			t.Fatalf("%s: %v", dir, entries)
		}
	}

	// Re-applying replaces the files atomically (a changed password).
	in := smbInput(ptr("second"))
	if _, err := m.Update(ctx, tg.ID, in); err != nil {
		t.Fatal(err)
	}
	if err := applyHost(ctx, h.env, cfg, tg.ID, nil, nil); err != nil {
		t.Fatal(err)
	}
	if cred, _ := os.ReadFile(credPath); !strings.Contains(string(cred), "password=second\n") {
		t.Fatalf("credentials not replaced:\n%s", cred)
	}

	// --password-stdin wins over the stored password; control characters are refused.
	if err := applyHost(ctx, h.env, cfg, tg.ID, []byte("from-stdin"), nil); err != nil {
		t.Fatal(err)
	}
	if cred, _ := os.ReadFile(credPath); !strings.Contains(string(cred), "password=from-stdin\n") {
		t.Fatalf("stdin password not used:\n%s", cred)
	}
	if err := applyHost(ctx, h.env, cfg, tg.ID, []byte("x\nusername=root"), nil); err == nil {
		t.Fatal("password with a line break accepted")
	}

	// Guest access removes the credentials file.
	in.Username, in.Domain, in.Password = "", "", nil
	if _, err := m.Update(ctx, tg.ID, in); err != nil {
		t.Fatal(err)
	}
	if err := applyHost(ctx, h.env, cfg, tg.ID, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(credPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("credentials file left: %v", err)
	}
	mustContain(t, "guest unit", h.unit(t, where), "Options=guest,vers=3.1.1,")
}

func TestApplyHostNFS(t *testing.T) {
	cfg, m, _, h := setupApply(t)
	ctx := context.Background()
	tg, err := m.Create(ctx, TargetInput{Name: "NFS", Kind: KindNFS, Mode: ModeHostApply, Server: "192.168.1.20",
		Export: "/volume1/picache", NFSNConnect: 8})
	if err != nil {
		t.Fatal(err)
	}
	if err := applyHost(ctx, h.env, cfg, tg.ID, nil, nil); err != nil {
		t.Fatal(err)
	}
	mustContain(t, "unit", h.unit(t, filepath.Join(cfg.MountRoot, tg.ID)), "What=192.168.1.20:/volume1/picache\n", "Type=nfs4\n",
		"Options=vers=4.2,proto=tcp,softerr,timeo=100,retrans=2,nconnect=8,nosuid,nodev,noexec,noatime,_netdev,nofail,x-systemd.mount-timeout=30\n")
	if entries, _ := os.ReadDir(h.env.credDir); len(entries) != 0 {
		t.Fatalf("NFS wrote credentials: %v", entries)
	}
}

// TestApplyHostDistrustsDatabase: the root helper re-validates every row and
// never takes the mountpoint from the database.
func TestApplyHostDistrustsDatabase(t *testing.T) {
	cfg, m, d, h := setupApply(t)
	ctx := context.Background()
	tg, err := m.Create(ctx, smbInput(ptr(nasPassword)))
	if err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := d.W.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}

	// A path column pointing elsewhere is ignored.
	exec(`UPDATE storage_targets SET path = ? WHERE id = ?`, filepath.Join(cfg.DataDir, "evil"), tg.ID)
	if err := applyHost(ctx, h.env, cfg, tg.ID, nil, nil); err != nil {
		t.Fatal(err)
	}
	mustContain(t, "unit", h.unit(t, filepath.Join(cfg.MountRoot, tg.ID)), "Where="+filepath.Join(cfg.MountRoot, tg.ID)+"\n")
	if _, err := os.Stat(filepath.Join(cfg.DataDir, "evil")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("created the path from the database")
	}

	for name, q := range map[string]string{
		"share injection":    `UPDATE storage_targets SET share = 'x' || char(10) || 'ExecStart=/bin/sh' WHERE id = ?`,
		"host name":          `UPDATE storage_targets SET server = 'nas.lan' WHERE id = ?`,
		"options injection":  `UPDATE storage_targets SET smb_version = '3.1.1,uid=0' WHERE id = ?`,
		"username injection": `UPDATE storage_targets SET username = 'u' || char(10) || 'password=x' WHERE id = ?`,
		"external mode":      `UPDATE storage_targets SET mode = 'external' WHERE id = ?`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.RemoveAll(h.env.unitDir); err != nil {
				t.Fatal(err)
			}
			mkdir(t, h.env.unitDir)
			exec(`UPDATE storage_targets SET share = 'picache', server = '192.168.1.10', smb_version = '3.1.1',
				username = 'picache', mode = 'host-apply' WHERE id = ?`, tg.ID)
			exec(q, tg.ID)
			h.calls = nil
			if err := applyHost(ctx, h.env, cfg, tg.ID, nil, nil); err == nil {
				t.Fatal("tampered row accepted")
			}
			if entries, _ := os.ReadDir(h.env.unitDir); len(entries) != 0 || len(h.calls) != 0 {
				t.Fatalf("acted on a tampered row: %v %v", entries, h.calls)
			}
			if s := readApplyState(requestsDir(cfg), tg.ID); !strings.HasPrefix(s, "failed: ") {
				t.Fatalf("apply state %q", s)
			}
		})
	}

	for _, id := range []string{"../../etc/passwd", "local", "", strings.ToUpper(tg.ID), "ffffffffffffffffffffffffffffffff"} {
		if err := applyHost(ctx, h.env, cfg, id, nil, nil); err == nil {
			t.Errorf("id %q accepted", id)
		}
	}
	h.env.checkPrivileges = func() error { return errors.New("must be run as root") }
	if err := applyHost(ctx, h.env, cfg, tg.ID, nil, nil); err == nil {
		t.Error("privilege check ignored")
	}
}

func TestApplyPending(t *testing.T) {
	cfg, m, _, h := setupApply(t)
	ctx := context.Background()
	if err := os.WriteFile(m.hostApplyFlag, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	smb, err := m.Create(ctx, smbInput(ptr(nasPassword)))
	if err != nil {
		t.Fatal(err)
	}
	nfs, err := m.Create(ctx, TargetInput{Name: "NFS", Kind: KindNFS, Mode: ModeHostApply, Server: "192.168.1.20", Export: "/v1"})
	if err != nil {
		t.Fatal(err)
	}
	// Nothing queued: nothing happens.
	if err := applyPending(ctx, h.env, cfg, nil); err != nil || len(h.calls) != 0 {
		t.Fatalf("empty queue: %v %v", err, h.calls)
	}
	for _, id := range []string{smb.ID, nfs.ID} {
		if _, err := m.RequestApply(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	dir := requestsDir(cfg)
	junk := []string{"not-an-id", "0123", smb.ID + ".tmp", "ffffffffffffffffffffffffffffffff"}
	for _, n := range junk[:3] {
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mkdir(t, filepath.Join(dir, junk[3])) // id-shaped, but not a regular file
	if err := applyPending(ctx, h.env, cfg, nil); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{smb.ID, nfs.ID} {
		if s := readApplyState(dir, id); s != applyApplied {
			t.Fatalf("%s: apply state %q", id, s)
		}
		if _, err := os.Stat(filepath.Join(h.env.unitDir, mountUnitName(filepath.Join(cfg.MountRoot, id)))); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range junk {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			t.Fatalf("junk file %s touched: %v", n, err)
		}
	}
	if len(h.calls) != 4 {
		t.Fatalf("systemctl calls %v", h.calls)
	}

	// A failing systemctl is reported per target.
	h.fail = errors.New("systemctl enable --now srv-picache.mount: exit status 1: mount error(13): Permission denied")
	if _, err := m.RequestApply(ctx, smb.ID); err != nil {
		t.Fatal(err)
	}
	if err := applyPending(ctx, h.env, cfg, nil); err == nil {
		t.Fatal("failure not returned")
	}
	if s := readApplyState(dir, smb.ID); !strings.Contains(s, "failed: ") || strings.Contains(s, "w0rd") {
		t.Fatalf("apply state %q", s)
	}
	if st := m.Status(smb.ID); st.ApplyState != applyQueued { // in memory until the next check
		t.Fatalf("status %+v", st)
	}
	if res := check(t, m, smb.ID); !strings.HasPrefix(res.st.ApplyState, "failed: ") {
		t.Fatalf("after check: %+v", res.st)
	}
}

func TestApplyUnsupportedWithoutRoot(t *testing.T) {
	if runtime.GOOS == "linux" && os.Geteuid() == 0 {
		t.Skip("running as root")
	}
	cfg := testConfig(t)
	if err := ApplyHost(context.Background(), cfg, testID, nil, nil); err == nil {
		t.Fatal("ApplyHost ran without root")
	}
	if err := ApplyPending(context.Background(), cfg, nil); err == nil {
		t.Fatal("ApplyPending ran without root")
	}
}

func TestCappedBuffer(t *testing.T) {
	b := &cappedBuffer{max: 4}
	for _, s := range []string{"ab", "cdef", "gh"} {
		if n, err := b.Write([]byte(s)); n != len(s) || err != nil {
			t.Fatal(n, err)
		}
	}
	if b.String() != "abcd" {
		t.Fatalf("%q", b.String())
	}
}
