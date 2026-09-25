package storage

import (
	"context"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/config"
	"github.com/hustenreizjuengling/picache/internal/db"
)

// fakeHost records systemctl calls; directories live in a temp dir.
type fakeHost struct {
	env    hostEnv
	calls  [][]string
	fail   error            // returned by every systemctl call
	failOn map[string]error // returned by calls with this verb (first argument)
	state  string           // `systemctl show -p ActiveState` output
	diag   unitDiag         // returned by diagnose
	onCall func(args []string)
}

func newFakeHost(t *testing.T) *fakeHost {
	t.Helper()
	dir := t.TempDir()
	h := &fakeHost{failOn: map[string]error{}, state: "inactive"}
	h.env = hostEnv{
		credDir:         filepath.Join(dir, "etc", "credentials"),
		unitDir:         mkdir(t, filepath.Join(dir, "systemd")),
		checkPrivileges: func() error { return nil },
		systemctl: func(_ context.Context, args ...string) (string, error) {
			h.calls = append(h.calls, args)
			if h.onCall != nil {
				h.onCall(args)
			}
			if h.fail != nil {
				return "", h.fail
			}
			if err := h.failOn[args[0]]; err != nil {
				return "", err
			}
			if args[0] == "show" {
				return h.state + "\n", nil
			}
			return "", nil
		},
		diagnose:     func(context.Context, string, time.Time) unitDiag { return h.diag },
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

// verbs returns "verb unit" of the recorded calls and forgets them.
func (h *fakeHost) verbs() []string {
	var out []string
	for _, c := range h.calls {
		out = append(out, strings.Join(c, " "))
	}
	h.calls = nil
	return out
}

func wantCalls(t *testing.T, h *fakeHost, want ...string) {
	t.Helper()
	if got := h.verbs(); !slices.Equal(got, want) {
		t.Fatalf("systemctl calls\n got %q\nwant %q", got, want)
	}
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
	unit := mountUnitName(where)
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
	u := h.unit(t, where)
	mustContain(t, "unit", u, "What=//192.168.1.10/picache\n", "Where="+where+"\n", "Type=cifs\n",
		"Options=credentials="+credPath+",vers=3.1.1,uid=999,gid=998,file_mode=0640,dir_mode=0750,soft,"+
			"nosuid,nodev,noexec,noatime,_netdev,nofail,x-systemd.mount-timeout=30\n",
		"WantedBy=remote-fs.target\n")
	if strings.Contains(u, "w0rd") {
		t.Fatal("password in the unit file")
	}
	wantCalls(t, h, "daemon-reload", "enable "+unit, "reset-failed "+unit, "start "+unit)
	if s := readApplyState(requestsDir(cfg), tg.ID, true); s != applyApplied {
		t.Fatalf("apply state %q", s)
	}
	// No temp files are left behind: the credentials, the fingerprint of the
	// mounted settings and the unit.
	for dir, n := range map[string]int{h.env.credDir: 2, h.env.unitDir: 1} {
		entries, _ := os.ReadDir(dir)
		if len(entries) != n {
			t.Fatalf("%s: %v", dir, entries)
		}
	}

	// Applying again without changes keeps the mount (start is a no-op).
	if err := applyHost(ctx, h.env, cfg, tg.ID, nil, nil); err != nil {
		t.Fatal(err)
	}
	wantCalls(t, h, "daemon-reload", "enable "+unit, "reset-failed "+unit, "start "+unit)

	// A changed password replaces the file atomically and remounts.
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
	wantCalls(t, h, "daemon-reload", "enable "+unit, "reset-failed "+unit, "restart "+unit)

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

	// Guest access removes the credentials file and remounts.
	in.Username, in.Domain, in.Password = "", "", nil
	if _, err := m.Update(ctx, tg.ID, in); err != nil {
		t.Fatal(err)
	}
	h.calls = nil
	if err := applyHost(ctx, h.env, cfg, tg.ID, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(credPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("credentials file left: %v", err)
	}
	mustContain(t, "guest unit", h.unit(t, where), "Options=guest,vers=3.1.1,")
	wantCalls(t, h, "daemon-reload", "enable "+unit, "reset-failed "+unit, "restart "+unit)
}

// TestReapplyChangedTargetRemounts: `enable --now` keeps an active mount
// unchanged, so a changed NAS address must restart the unit; a failed
// remount is reported (with a hint) and retried on the next apply.
func TestReapplyChangedTargetRemounts(t *testing.T) {
	cfg, m, _, h := setupApply(t)
	ctx := context.Background()
	tg, err := m.Create(ctx, TargetInput{Name: "NFS", Kind: KindNFS, Mode: ModeHostApply, Server: "192.168.1.20", Export: "/v1"})
	if err != nil {
		t.Fatal(err)
	}
	unit := mountUnitName(filepath.Join(cfg.MountRoot, tg.ID))
	if err := applyHost(ctx, h.env, cfg, tg.ID, nil, nil); err != nil {
		t.Fatal(err)
	}
	wantCalls(t, h, "daemon-reload", "enable "+unit, "reset-failed "+unit, "start "+unit)

	if _, err := m.Update(ctx, tg.ID, TargetInput{Name: "NFS", Kind: KindNFS, Mode: ModeHostApply, Server: "192.168.1.21", Export: "/v1"}); err != nil {
		t.Fatal(err)
	}
	// The share is in use: the remount fails and says why.
	h.failOn["restart"] = errors.New("exit status 1")
	h.diag = unitDiag{result: "exit-code", lines: []string{
		"Unmounting srv-picache.mount - PiCache cache store...",
		"umount: " + filepath.Join(cfg.MountRoot, tg.ID) + ": target is busy.",
		unit + ": Mount process exited, code=exited, status=32/n/a",
		"Failed unmounting srv-picache.mount - PiCache cache store.",
	}}
	err = applyHost(ctx, h.env, cfg, tg.ID, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "target is busy") || !strings.Contains(err.Error(), "activate another storage target") {
		t.Fatalf("failed remount: %v", err)
	}
	wantCalls(t, h, "daemon-reload", "enable "+unit, "reset-failed "+unit, "restart "+unit)
	// The stored state is clipped to maxMessage runes; with the long temp
	// paths of macOS runners the reason may fall behind the clip.
	if s := readApplyState(requestsDir(cfg), tg.ID, true); !strings.HasPrefix(s, "failed: cannot remount") ||
		!(strings.Contains(s, "target is busy") || strings.HasSuffix(s, "…")) {
		t.Fatalf("apply state %q", s)
	}

	// The unit file is current now, but the mount is not: the next apply
	// still remounts instead of reporting the old mount as applied.
	delete(h.failOn, "restart")
	if err := applyHost(ctx, h.env, cfg, tg.ID, nil, nil); err != nil {
		t.Fatal(err)
	}
	wantCalls(t, h, "daemon-reload", "enable "+unit, "reset-failed "+unit, "restart "+unit)
	if err := applyHost(ctx, h.env, cfg, tg.ID, nil, nil); err != nil {
		t.Fatal(err)
	}
	wantCalls(t, h, "daemon-reload", "enable "+unit, "reset-failed "+unit, "start "+unit)

	// A unit written by an older version (no fingerprint) that matches is not remounted.
	if err := os.Remove(filepath.Join(h.env.credDir, tg.ID+appliedSuffix)); err != nil {
		t.Fatal(err)
	}
	if err := applyHost(ctx, h.env, cfg, tg.ID, nil, nil); err != nil {
		t.Fatal(err)
	}
	wantCalls(t, h, "daemon-reload", "enable "+unit, "reset-failed "+unit, "start "+unit)
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
	entries, _ := os.ReadDir(h.env.credDir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".cred") {
			t.Fatalf("NFS wrote credentials: %v", entries)
		}
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
			if s := readApplyState(requestsDir(cfg), tg.ID, true); !strings.HasPrefix(s, "failed: ") {
				t.Fatalf("apply state %q", s)
			}
		})
	}

	for _, id := range []string{"../../etc/passwd", "local", "", strings.ToUpper(tg.ID), "ffffffffffffffffffffffffffffffff"} {
		if err := applyHost(ctx, h.env, cfg, id, nil, nil); err == nil {
			t.Errorf("id %q accepted", id)
		}
		if err := removeHost(ctx, h.env, cfg, id, nil); err == nil && !targetIDRE.MatchString(id) {
			t.Errorf("remove: id %q accepted", id)
		}
	}
	// An unknown id leaves no result file behind.
	if _, err := os.Stat(filepath.Join(requestsDir(cfg), "ffffffffffffffffffffffffffffffff"+resultSuffix)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("result for an unknown target: %v", err)
	}
	h.env.checkPrivileges = func() error { return errors.New("must be run as root") }
	if err := applyHost(ctx, h.env, cfg, tg.ID, nil, nil); err == nil {
		t.Error("privilege check ignored")
	}
	if err := removeHost(ctx, h.env, cfg, tg.ID, nil); err == nil {
		t.Error("privilege check ignored by remove")
	}
}

// TestApplyHostNeedsMasterKey: without a master key the helper names the
// systemd credential as the likely cause.
func TestApplyHostNeedsMasterKey(t *testing.T) {
	cfg, m, _, h := setupApply(t)
	tg, err := m.Create(context.Background(), smbInput(ptr(nasPassword)))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(cfg.Paths().MasterKeyFile); err != nil {
		t.Fatal(err)
	}
	err = applyHost(context.Background(), h.env, cfg, tg.ID, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "LoadCredentialEncrypted=") || !strings.Contains(err.Error(), "picache-storage.service") {
		t.Fatalf("missing master key: %v", err)
	}
	if s := readApplyState(requestsDir(cfg), tg.ID, true); !strings.Contains(s, "no master key for the root helper") {
		t.Fatalf("apply state %q", s)
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
	junk := []string{"not-an-id", "0123", smb.ID + ".tmp", ".not-a-claim.claim"}
	for _, n := range junk {
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Id-shaped, but not a regular file, or not an id but matching the path
	// unit's glob: left alone, it would start the helper again and again.
	mkdir(t, filepath.Join(dir, "ffffffffffffffffffffffffffffffff", "sub"))
	unitGlobJunk := []string{strings.Repeat("Z", 32), "." + strings.Repeat("-", 32) + claimSuffix}
	for _, n := range unitGlobJunk {
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := applyPending(ctx, h.env, cfg, nil); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{smb.ID, nfs.ID} {
		if s := readApplyState(dir, id, true); s != applyApplied {
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
	for _, n := range append(unitGlobJunk, "ffffffffffffffffffffffffffffffff") {
		if _, err := os.Stat(filepath.Join(dir, n)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s left: %v", n, err)
		}
	}
	if len(h.calls) != 8 {
		t.Fatalf("systemctl calls %v", h.calls)
	}
	assertNoQueue(t, dir)

	// A failing systemctl is reported per target.
	h.fail = errors.New("systemctl enable srv-picache.mount: exit status 1: mount error(13): Permission denied")
	if _, err := m.RequestApply(ctx, smb.ID); err != nil {
		t.Fatal(err)
	}
	if err := applyPending(ctx, h.env, cfg, nil); err == nil {
		t.Fatal("failure not returned")
	}
	if s := readApplyState(dir, smb.ID, true); !strings.Contains(s, "failed: ") || strings.Contains(s, "w0rd") {
		t.Fatalf("apply state %q", s)
	}
	if st := m.Status(smb.ID); st.ApplyState != applyQueued { // in memory until the next check
		t.Fatalf("status %+v", st)
	}
	if res := check(t, m, smb.ID); !strings.HasPrefix(res.st.ApplyState, "failed: ") {
		t.Fatalf("after check: %+v", res.st)
	}
}

// assertNoQueue fails if a request or claim is left in dir.
func assertNoQueue(t *testing.T, dir string) {
	t.Helper()
	r, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	s, err := scanRequests(r)
	if err != nil || len(s.requests)+len(s.claims)+len(s.junk) != 0 {
		t.Fatalf("queue left: %+v %v", s, err)
	}
}

// TestApplyPendingRequestsDuringRun: systemd does not start the helper for
// PathChanged= events while it runs, so requests queued meanwhile (another
// target, or the same one again) are picked up by the running helper.
func TestApplyPendingRequestsDuringRun(t *testing.T) {
	cfg, m, _, h := setupApply(t)
	ctx := context.Background()
	if err := os.WriteFile(m.hostApplyFlag, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := m.Create(ctx, TargetInput{Name: "A", Kind: KindNFS, Mode: ModeHostApply, Server: "192.168.1.20", Export: "/a"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.Create(ctx, TargetInput{Name: "B", Kind: KindNFS, Mode: ModeHostApply, Server: "192.168.1.20", Export: "/b"})
	if err != nil {
		t.Fatal(err)
	}
	dir := requestsDir(cfg)
	unitA := mountUnitName(filepath.Join(cfg.MountRoot, a.ID))
	unitB := mountUnitName(filepath.Join(cfg.MountRoot, b.ID))
	queuedDuringRun := false
	h.onCall = func(args []string) {
		if args[0] != "start" || args[1] != unitA || queuedDuringRun {
			return
		}
		queuedDuringRun = true
		// While A is being mounted: its request is claimed (the UI still says
		// queued) and the admin clicks Apply on B and on A again.
		if _, err := os.Stat(filepath.Join(dir, a.ID)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("request not claimed: %v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, claimName(a.ID))); err != nil {
			t.Errorf("claim: %v", err)
		}
		if s := readApplyState(dir, a.ID, true); s != applyQueued {
			t.Errorf("state while running %q", s)
		}
		for _, id := range []string{b.ID, a.ID} {
			if _, err := m.RequestApply(ctx, id); err != nil {
				t.Error(err)
			}
		}
	}
	if _, err := m.RequestApply(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if err := applyPending(ctx, h.env, cfg, nil); err != nil {
		t.Fatal(err)
	}
	if !queuedDuringRun {
		t.Fatal("hook did not run")
	}
	// A once for the first request and once for the second (the order of A
	// and B in the second listing follows their random ids), B once.
	starts := map[string]int{}
	for _, c := range h.verbs() {
		starts[c]++
	}
	if starts["start "+unitA] != 2 || starts["start "+unitB] != 1 || starts["daemon-reload"] != 3 || starts["reset-failed "+unitB] != 1 {
		t.Fatalf("systemctl calls %v", starts)
	}
	for _, id := range []string{a.ID, b.ID} {
		if s := readApplyState(dir, id, true); s != applyApplied {
			t.Fatalf("%s: %q", id, s)
		}
	}
	assertNoQueue(t, dir)
}

// TestApplyPendingInterruptedClaim: a claim left by a helper run that died is
// reported as failed, not retried; one of a deleted target just disappears.
func TestApplyPendingInterruptedClaim(t *testing.T) {
	cfg, m, _, h := setupApply(t)
	ctx := context.Background()
	tg, err := m.Create(ctx, TargetInput{Name: "A", Kind: KindNFS, Mode: ModeHostApply, Server: "192.168.1.20", Export: "/a"})
	if err != nil {
		t.Fatal(err)
	}
	dir := requestsDir(cfg)
	for _, id := range []string{tg.ID, testID} {
		if err := os.WriteFile(filepath.Join(dir, claimName(id)), []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if s := readApplyState(dir, tg.ID, true); s != applyQueued {
		t.Fatalf("fresh claim: %q", s)
	}
	if err := applyPending(ctx, h.env, cfg, nil); err != nil {
		t.Fatal(err)
	}
	if len(h.calls) != 0 {
		t.Fatalf("retried: %v", h.calls)
	}
	if s := readApplyState(dir, tg.ID, true); s != "failed: "+msgInterrupted {
		t.Fatalf("state %q", s)
	}
	if _, err := os.Stat(filepath.Join(dir, testID+resultSuffix)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("result for a deleted target: %v", err)
	}
	assertNoQueue(t, dir)
}

// TestReadApplyStateStale: a request the helper never picks up (path unit
// stopped by its start limit, custom data dir) and a claim whose run died do
// not stay "queued" forever.
func TestReadApplyStateStale(t *testing.T) {
	dir := t.TempDir()
	if err := writeRequest(dir, testID, actionApply); err != nil {
		t.Fatal(err)
	}
	if s := readApplyState(dir, testID, true); s != applyQueued {
		t.Fatalf("fresh: %q", s)
	}
	if s := readApplyState(dir, testID, false); s != "" {
		t.Fatalf("fresh removal: %q", s)
	}
	old := time.Now().Add(-staleRequest - time.Minute)
	if err := os.Chtimes(filepath.Join(dir, testID), old, old); err != nil {
		t.Fatal(err)
	}
	for _, hostApply := range []bool{true, false} {
		if s := readApplyState(dir, testID, hostApply); s != "failed: "+msgNotPickedUp {
			t.Fatalf("stale request: %q", s)
		}
	}
	if err := os.Rename(filepath.Join(dir, testID), filepath.Join(dir, claimName(testID))); err != nil {
		t.Fatal(err)
	}
	if s := readApplyState(dir, testID, true); s != applyQueued {
		t.Fatalf("claim: %q", s)
	}
	old = time.Now().Add(-staleClaim - time.Minute)
	if err := os.Chtimes(filepath.Join(dir, claimName(testID)), old, old); err != nil {
		t.Fatal(err)
	}
	if s := readApplyState(dir, testID, true); s != "failed: "+msgInterrupted {
		t.Fatalf("stale claim: %q", s)
	}
}

// TestRemoveHost: `picache storage remove` disables, stops and deletes the
// unit, deletes the credentials and removes the empty mountpoint.
func TestRemoveHost(t *testing.T) {
	cfg, m, _, h := setupApply(t)
	ctx := context.Background()
	tg, err := m.Create(ctx, smbInput(ptr(nasPassword)))
	if err != nil {
		t.Fatal(err)
	}
	where := filepath.Join(cfg.MountRoot, tg.ID)
	unit := mountUnitName(where)
	if err := applyHost(ctx, h.env, cfg, tg.ID, nil, nil); err != nil {
		t.Fatal(err)
	}
	h.calls = nil

	// Unmounting fails while the share is busy: everything stays for a retry.
	h.failOn["stop"] = errors.New("exit status 1")
	h.state = "active"
	err = removeHost(ctx, h.env, cfg, tg.ID, nil)
	if err == nil || !strings.Contains(err.Error(), "cannot unmount") {
		t.Fatalf("busy: %v", err)
	}
	wantCalls(t, h, "disable "+unit, "stop "+unit, "show -p ActiveState --value "+unit)
	for _, p := range []string{filepath.Join(h.env.unitDir, unit), filepath.Join(h.env.credDir, tg.ID+".cred"), where} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s removed after a failed unmount: %v", p, err)
		}
	}
	if s := readApplyState(requestsDir(cfg), tg.ID, true); !strings.HasPrefix(s, "failed: cannot unmount") {
		t.Fatalf("state %q", s)
	}

	// A unit systemd has not loaded fails to stop, but nothing is mounted.
	h.state = "inactive"
	if err := removeHost(ctx, h.env, cfg, tg.ID, nil); err != nil {
		t.Fatal(err)
	}
	wantCalls(t, h, "disable "+unit, "stop "+unit, "show -p ActiveState --value "+unit, "daemon-reload", "reset-failed "+unit)
	for _, p := range []string{filepath.Join(h.env.unitDir, unit), filepath.Join(h.env.credDir, tg.ID+".cred"),
		filepath.Join(h.env.credDir, tg.ID+appliedSuffix), where} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s left: %v", p, err)
		}
	}
	// The target still exists: its status shows no apply state (not "applied").
	if s := readApplyState(requestsDir(cfg), tg.ID, true); s != "" {
		t.Fatalf("state %q", s)
	}
	// Removing again is a no-op; a mountpoint with files in it is left alone.
	delete(h.failOn, "stop")
	mkdir(t, filepath.Join(where, "left"))
	if err := removeHost(ctx, h.env, cfg, tg.ID, nil); err != nil {
		t.Fatal(err)
	}
	wantCalls(t, h)
	if _, err := os.Stat(where); err != nil {
		t.Fatalf("non-empty mountpoint removed: %v", err)
	}
}

// TestRemovalQueuedOnDeleteAndModeChange: deleting a host-apply target, or
// switching it to external, makes the root helper remove its mount unit and
// the plain-text NAS credentials.
func TestRemovalQueuedOnDeleteAndModeChange(t *testing.T) {
	cfg, m, _, h := setupApply(t)
	ctx := context.Background()
	dir := requestsDir(cfg)
	applied := func() (Target, string) {
		t.Helper()
		tg, err := m.Create(ctx, smbInput(ptr(nasPassword)))
		if err != nil {
			t.Fatal(err)
		}
		if err := applyHost(ctx, h.env, cfg, tg.ID, nil, nil); err != nil {
			t.Fatal(err)
		}
		h.calls = nil
		return tg, mountUnitName(filepath.Join(cfg.MountRoot, tg.ID))
	}
	gone := func(id, unit string) {
		t.Helper()
		for _, p := range []string{filepath.Join(h.env.unitDir, unit), filepath.Join(h.env.credDir, id+".cred"), filepath.Join(cfg.MountRoot, id)} {
			if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("%s left: %v", p, err)
			}
		}
	}

	// Without the helper nothing is queued (the admin runs the command).
	tg, unit := applied()
	if err := m.Delete(ctx, tg.ID, LocalTargetID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, tg.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("request without helper: %v", err)
	}
	if err := removeHost(ctx, h.env, cfg, tg.ID, nil); err != nil {
		t.Fatal(err)
	}
	gone(tg.ID, unit)
	if _, err := os.Stat(filepath.Join(dir, tg.ID+resultSuffix)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("result for a deleted target: %v", err)
	}

	if err := os.WriteFile(m.hostApplyFlag, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Run("delete", func(t *testing.T) {
		tg, unit := applied()
		if err := m.Delete(ctx, tg.ID, LocalTargetID); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(dir, tg.ID))
		var req applyRequest
		if err != nil || json.Unmarshal(b, &req) != nil || req.Action != actionRemove || req.ID != tg.ID {
			t.Fatalf("removal request %s %v", b, err)
		}
		if err := applyPending(ctx, h.env, cfg, nil); err != nil {
			t.Fatal(err)
		}
		wantCalls(t, h, "disable "+unit, "stop "+unit, "daemon-reload", "reset-failed "+unit)
		gone(tg.ID, unit)
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Fatalf("files left for a deleted target: %v", entries)
		}
	})

	t.Run("switch to external", func(t *testing.T) {
		tg, unit := applied()
		in := smbInput(nil)
		in.Mode, in.Path = ModeExternal, filepath.Join(cfg.MountRoot, "nas")
		if _, err := m.Update(ctx, tg.ID, in); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(dir, tg.ID)); err != nil {
			t.Fatalf("no removal request: %v", err)
		}
		// The removal fails first; the external target shows why.
		h.failOn["stop"] = errors.New("exit status 1")
		h.state = "active"
		if err := applyPending(ctx, h.env, cfg, nil); err == nil {
			t.Fatal("failure not returned")
		}
		if res := check(t, m, tg.ID); !strings.HasPrefix(res.st.ApplyState, "failed: cannot unmount") {
			t.Fatalf("status %+v", res.st)
		}
		// Deleting it now still removes the mount.
		delete(h.failOn, "stop")
		if err := m.Delete(ctx, tg.ID, LocalTargetID); err != nil {
			t.Fatal(err)
		}
		h.calls = nil
		if err := applyPending(ctx, h.env, cfg, nil); err != nil {
			t.Fatal(err)
		}
		wantCalls(t, h, "disable "+unit, "stop "+unit, "daemon-reload", "reset-failed "+unit)
		gone(tg.ID, unit)
	})
}

func TestDescribeFailure(t *testing.T) {
	journal := []string{
		"Mounting srv-picache-x.mount - PiCache cache store x...",
		"mount error(115): could not connect to 192.0.2.12",
		"Refer to the mount.cifs(8) manual page (e.g. man mount.cifs) and kernel log messages (dmesg)",
		"srv-picache-x.mount: Mount process exited, code=exited, status=32/n/a",
		"srv-picache-x.mount: Failed with result 'exit-code'.",
		"Failed to mount srv-picache-x.mount - PiCache cache store x.",
		"",
	}
	se := &systemctlError{args: []string{"start", "srv-picache-x.mount"}, err: errors.New("exit status 1"),
		output: systemctlOutput("Created symlink '/etc/systemd/system/remote-fs.target.wants/srv-picache-x.mount' → '/etc/systemd/system/srv-picache-x.mount'.\n" +
			"Job for srv-picache-x.mount failed because the control process exited with error code.\n" +
			"See \"systemctl status srv-picache-x.mount\" and \"journalctl -xeu srv-picache-x.mount\" for details.\n")}
	if se.output != "Job for srv-picache-x.mount failed because the control process exited with error code." {
		t.Fatalf("systemctl output %q", se.output)
	}
	err := describeFailure("mount failed", se, unitDiag{result: "exit-code", lines: journal}, "")
	if want := "mount failed: mount error(115): could not connect to 192.0.2.12 (systemd result: exit-code)"; err.Error() != want {
		t.Fatalf("got  %q\nwant %q", err, want)
	}
	if !errors.Is(err, se) {
		t.Fatal("cause lost")
	}
	// Without journal access systemctl's own message is used.
	err = describeFailure("mount failed", se, unitDiag{}, "hint")
	if want := "mount failed: Job for srv-picache-x.mount failed because the control process exited with error code.. hint"; err.Error() != want {
		t.Fatalf("got %q", err)
	}
	if got := relevantJournalLines([]string{"srv-picache-x.mount: Mounting timed out. Terminating.", "a", "b", "c"}); !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Fatalf("last three: %q", got)
	}
	if got := relevantJournalLines([]string{"srv-picache-x.mount: Mounting timed out. Terminating."}); !slices.Equal(got, []string{"Mounting timed out. Terminating."}) {
		t.Fatalf("timeout: %q", got)
	}
	if got := relevantJournalLines([]string{"mount error(13): Permission denied", "mount error(13): Permission denied"}); len(got) != 1 {
		t.Fatalf("repeated line: %q", got)
	}
}

// TestOpenConfigDBHardening: the root helper opens the service-owned
// database only as a regular file and does not trust its schema.
func TestOpenConfigDBHardening(t *testing.T) {
	cfg := testConfig(t)
	newTestManager(t, cfg) // creates picache.db
	d, err := openConfigDB(cfg.Paths().ConfigDB)
	if err != nil {
		t.Fatal(err)
	}
	var trusted int
	if err := d.R.QueryRow(`PRAGMA trusted_schema`).Scan(&trusted); err != nil || trusted != 0 {
		t.Fatalf("trusted_schema = %d, %v", trusted, err)
	}
	if _, err := d.R.Exec(`CREATE TABLE x (a)`); err == nil {
		t.Fatal("read-only database written")
	}
	d.Close()

	dir := t.TempDir()
	if _, err := openConfigDB(mkdir(t, filepath.Join(dir, "picache.db"))); err == nil {
		t.Fatal("directory accepted")
	}
	if _, err := openConfigDB(filepath.Join(dir, "missing.db")); err == nil {
		t.Fatal("missing file accepted")
	}
	link := filepath.Join(dir, "link.db")
	if err := os.Symlink(cfg.Paths().ConfigDB, link); err != nil {
		t.Skip("cannot create a symbolic link:", err)
	}
	if _, err := openConfigDB(link); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("symbolic link accepted: %v", err)
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
	if err := RemoveHost(context.Background(), cfg, testID, nil); err == nil {
		t.Fatal("RemoveHost ran without root")
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

// TestApplyHostNeedsMountHelper: without mount.nfs / mount.cifs the helper
// refuses before it writes a unit, and says which package to install.
func TestApplyHostNeedsMountHelper(t *testing.T) {
	cfg, m, _, h := setupApply(t)
	ctx := context.Background()
	h.env.hasHelper = func(name string) bool { return name != "mount.nfs" }
	tg, err := m.Create(ctx, TargetInput{Name: "NFS", Kind: KindNFS, Mode: ModeHostApply, Server: "192.168.1.20", Export: "/volume1/picache"})
	if err != nil {
		t.Fatal(err)
	}
	err = applyHost(ctx, h.env, cfg, tg.ID, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "sudo apt install nfs-common") {
		t.Fatalf("want the nfs-common hint, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.env.unitDir, mountUnitName(filepath.Join(cfg.MountRoot, tg.ID)))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a unit was written: %v", err)
	}
	if len(h.calls) != 0 {
		t.Fatalf("systemctl was called: %v", h.calls)
	}
}
