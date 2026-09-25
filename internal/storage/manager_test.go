package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	cachestore "github.com/hustenreizjuengling/picache/internal/dlcache/store"
)

func TestNewInitialisesLocalStore(t *testing.T) {
	cfg := testConfig(t)
	m, d, box := newTestManager(t, cfg)
	local, err := m.Target(context.Background(), LocalTargetID)
	if err != nil {
		t.Fatal(err)
	}
	if !cachestore.ValidStoreID(local.StoreID) || local.Path != localPath(cfg) || local.Kind != KindLocal {
		t.Fatalf("local target %+v", local)
	}
	mk, err := cachestore.ReadMarker(cfg.CacheDir)
	if err != nil || mk.StoreID != local.StoreID {
		t.Fatalf("marker %+v %v", mk, err)
	}
	// A second start keeps the store.
	m2, err := New(context.Background(), d, box, cfg, func() int64 { return 1 << 20 }, nil)
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := m2.Target(context.Background(), LocalTargetID); again.StoreID != local.StoreID {
		t.Fatalf("store id changed: %s → %s", local.StoreID, again.StoreID)
	}

	// Online after the first check, with the marker's id.
	res := check(t, m, LocalTargetID)
	if !res.st.Online || !res.usable() || res.st.StoreID != local.StoreID {
		t.Fatalf("local status %+v steps %v", res.st, res.steps)
	}
	root, id, err := m.StoreRoot(LocalTargetID)
	if err != nil || root != localPath(cfg) || id != local.StoreID {
		t.Fatalf("StoreRoot = %q %q %v", root, id, err)
	}
	// The write test cleans up after itself.
	entries, _ := os.ReadDir(filepath.Join(cfg.CacheDir, "tmp"))
	if len(entries) != 0 {
		t.Fatalf("probe left files: %v", entries)
	}
}

func TestNewAdoptsOrLeavesLocalStore(t *testing.T) {
	t.Run("adopt marker", func(t *testing.T) {
		cfg := testConfig(t)
		const id = "fedcba9876543210fedcba9876543210"
		if _, err := cachestore.InitRoot(cfg.CacheDir, id, 1<<20); err != nil {
			t.Fatal(err)
		}
		m, _, _ := newTestManager(t, cfg)
		if local, _ := m.Target(context.Background(), LocalTargetID); local.StoreID != id {
			t.Fatalf("want adopted %s, got %q", id, local.StoreID)
		}
	})
	t.Run("not empty", func(t *testing.T) {
		cfg := testConfig(t)
		if err := os.WriteFile(filepath.Join(cfg.CacheDir, "foreign"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		m, _, _ := newTestManager(t, cfg)
		if local, _ := m.Target(context.Background(), LocalTargetID); local.StoreID != "" {
			t.Fatalf("non-empty cache dir must not be initialised: %q", local.StoreID)
		}
		if _, _, err := m.StoreRoot(LocalTargetID); !errors.Is(err, ErrNotInitialised) {
			t.Fatalf("before the first check: want ErrNotInitialised, got %v", err)
		}
		res := check(t, m, LocalTargetID)
		if res.st.Online || !res.uninit || !res.usable() {
			t.Fatalf("status %+v", res.st)
		}
		if _, _, err := m.StoreRoot(LocalTargetID); !errors.Is(err, ErrNotInitialised) {
			t.Fatalf("want ErrNotInitialised, got %v", err)
		}
		_, err := m.InitStore(context.Background(), LocalTargetID, false)
		if ae := wantKind(t, err, apperr.KindConflict); !strings.Contains(ae.Message, "not empty") {
			t.Fatalf("message %q", ae.Message)
		}
	})
}

func TestCRUD(t *testing.T) {
	cfg := testConfig(t)
	m, d, box := newTestManager(t, cfg)
	ctx := context.Background()

	// Invalid input is rejected with the field.
	bad := smbInput(nil)
	bad.Server = "nas.lan"
	_, err := m.Create(ctx, bad)
	if ae := wantKind(t, err, apperr.KindInvalid); ae.Field != "server" {
		t.Fatalf("field %q", ae.Field)
	}
	nfs := TargetInput{Name: "NFS", Kind: KindNFS, Mode: ModeExternal, Path: filepath.Join(cfg.MountRoot, "nfs"),
		Server: "192.168.1.10", Export: "/volume1/picache", Password: ptr("x"),
		SMBVersion: "3.1.1", Share: "ignored", Username: "ignored"}
	_, err = m.Create(ctx, nfs)
	if ae := wantKind(t, err, apperr.KindInvalid); ae.Field != "password" {
		t.Fatalf("field %q", ae.Field)
	}

	// Create: defaults, computed host-apply path, fields of other kinds ignored.
	nfs.Password = nil
	n, err := m.Create(ctx, nfs)
	if err != nil {
		t.Fatal(err)
	}
	if n.NFSVersion != "4.2" || n.NFSNConnect != 4 || !n.RequireMountpoint || n.Share != "" || n.SMBVersion != "" || n.Username != "" {
		t.Fatalf("nfs defaults %+v", n)
	}
	in := smbInput(ptr("secret-1"))
	in.Path = "/etc" // ignored for host-apply
	s, err := m.Create(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if s.Path != filepath.Join(cfg.MountRoot, s.ID) || !s.HasPassword || s.SMBVersion != "3.1.1" || !ValidTargetID(s.ID) {
		t.Fatalf("smb %+v", s)
	}
	sealedPW := func() string {
		var v *string
		if err := d.R.QueryRow(`SELECT password_sealed FROM storage_targets WHERE id = ?`, s.ID).Scan(&v); err != nil {
			t.Fatal(err)
		}
		if v == nil {
			return ""
		}
		pw, err := box.Open(*v, passwordAAD(s.ID))
		if err != nil {
			t.Fatal(err)
		}
		return string(pw)
	}
	if got := sealedPW(); got != "secret-1" {
		t.Fatalf("stored password %q", got)
	}

	// Update: nil keeps, a value replaces, "" clears the password.
	in.Password, in.Name = nil, "NAS 2"
	if s, err = m.Update(ctx, s.ID, in); err != nil || !s.HasPassword || s.Name != "NAS 2" || sealedPW() != "secret-1" {
		t.Fatalf("keep: %+v %v", s, err)
	}
	in.Password = ptr("secret-2")
	if s, err = m.Update(ctx, s.ID, in); err != nil || sealedPW() != "secret-2" {
		t.Fatalf("replace: %v", err)
	}
	in.Password = ptr("")
	if s, err = m.Update(ctx, s.ID, in); err != nil || s.HasPassword || sealedPW() != "" {
		t.Fatalf("clear: %+v %v", s, err)
	}
	// Changing the kind drops the password.
	in.Password = ptr("secret-3")
	if s, err = m.Update(ctx, s.ID, in); err != nil || !s.HasPassword {
		t.Fatal(err)
	}
	toNFS := nfs
	toNFS.Path = filepath.Join(cfg.MountRoot, "nfs2")
	if s, err = m.Update(ctx, s.ID, toNFS); err != nil || s.HasPassword || s.Kind != KindNFS || sealedPW() != "" {
		t.Fatalf("kind change: %+v %v", s, err)
	}
	if _, err := m.Update(ctx, "ffffffffffffffffffffffffffffffff", in); apperr.KindOf(err) != apperr.KindNotFound {
		t.Fatalf("update unknown: %v", err)
	}

	// The built-in target can only be renamed.
	local, err := m.Update(ctx, LocalTargetID, TargetInput{Name: "SSD", Kind: KindSMB, Path: "/etc"})
	if err != nil || local.Name != "SSD" || local.Kind != KindLocal || local.Path != localPath(cfg) {
		t.Fatalf("rename local: %+v %v", local, err)
	}

	// Overlapping store roots are refused.
	_, err = m.Create(ctx, localInput(filepath.Join(cfg.MountRoot, "nfs", "inner")))
	wantKind(t, err, apperr.KindConflict)

	// Delete rules.
	wantKind(t, m.Delete(ctx, LocalTargetID, "x"), apperr.KindForbidden)
	wantKind(t, m.Delete(ctx, n.ID, n.ID), apperr.KindConflict)
	if err := m.Delete(ctx, n.ID, LocalTargetID); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Target(ctx, n.ID); apperr.KindOf(err) != apperr.KindNotFound {
		t.Fatalf("deleted target still there: %v", err)
	}
	wantKind(t, m.Delete(ctx, n.ID, LocalTargetID), apperr.KindNotFound)

	// Everything survives a restart.
	m2, err := New(ctx, d, box, cfg, func() int64 { return 1 << 20 }, nil)
	if err != nil {
		t.Fatal(err)
	}
	list, _ := m2.Targets(ctx, s.ID)
	if len(list) != 2 || list[0].ID != LocalTargetID || list[0].Name != "SSD" || list[1].ID != s.ID || !list[1].Active {
		t.Fatalf("after restart: %+v", list)
	}
}

func TestTargetLimit(t *testing.T) {
	cfg := testConfig(t)
	m, _, _ := newTestManager(t, cfg)
	var err error
	for i := 0; i < maxTargets; i++ {
		_, err = m.Create(context.Background(), localInput(filepath.Join(cfg.MountRoot, fmt.Sprintf("d%d", i))))
		if err != nil {
			break
		}
	}
	wantKind(t, err, apperr.KindConflict)
}

func TestInitStore(t *testing.T) {
	cfg := testConfig(t)
	m, _, _ := newTestManager(t, cfg)
	ctx := context.Background()
	create := func(name string, mod func(*TargetInput)) Target {
		t.Helper()
		in := localInput(filepath.Join(cfg.MountRoot, name))
		if mod != nil {
			mod(&in)
		}
		tg, err := m.Create(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		return tg
	}

	t.Run("init empty", func(t *testing.T) {
		mkdir(t, filepath.Join(cfg.MountRoot, "empty"))
		tg := create("empty", nil)
		if res := check(t, m, tg.ID); res.st.Online || !res.uninit || !res.usable() || res.st.Initialised || !res.st.Writable {
			t.Fatalf("before init: %+v", res.st)
		}
		if _, _, err := m.StoreRoot(tg.ID); !errors.Is(err, ErrNotInitialised) {
			t.Fatalf("want ErrNotInitialised, got %v", err)
		}
		res, err := m.InitStore(ctx, tg.ID, false)
		if err != nil || res.Adopted || !cachestore.ValidStoreID(res.StoreID) {
			t.Fatalf("init: %+v %v", res, err)
		}
		root, id, err := m.StoreRoot(tg.ID)
		if err != nil || id != res.StoreID || root != tg.Path {
			t.Fatalf("StoreRoot = %q %q %v (status %+v)", root, id, err, m.Status(tg.ID))
		}
		if st := m.Status(tg.ID); !st.Initialised || !st.Writable {
			t.Fatalf("after init: %+v", st)
		}
		if mk, err := cachestore.ReadMarker(tg.Path); err != nil || mk.StoreID != res.StoreID {
			t.Fatalf("marker %+v %v", mk, err)
		}
		// A second init refuses to overwrite the store.
		_, err = m.InitStore(ctx, tg.ID, false)
		wantKind(t, err, apperr.KindConflict)
		// Adopting its own store again is harmless.
		if again, err := m.InitStore(ctx, tg.ID, true); err != nil || again.StoreID != res.StoreID || !again.Adopted {
			t.Fatalf("re-adopt: %+v %v", again, err)
		}
	})

	t.Run("refuse non-empty", func(t *testing.T) {
		dir := mkdir(t, filepath.Join(cfg.MountRoot, "full"))
		if err := os.WriteFile(filepath.Join(dir, "family-photos.jpg"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		tg := create("full", nil)
		_, err := m.InitStore(ctx, tg.ID, false)
		wantKind(t, err, apperr.KindConflict)
		if _, err := cachestore.ReadMarker(dir); !errors.Is(err, cachestore.ErrNoMarker) {
			t.Fatalf("marker written into a non-empty directory: %v", err)
		}
		_, err = m.InitStore(ctx, tg.ID, true)
		wantKind(t, err, apperr.KindConflict) // nothing to adopt
	})

	t.Run("adopt", func(t *testing.T) {
		dir := mkdir(t, filepath.Join(cfg.MountRoot, "old"))
		const id = "00112233445566778899aabbccddeeff"
		if _, err := cachestore.InitRoot(dir, id, 1<<20); err != nil {
			t.Fatal(err)
		}
		tg := create("old", nil)
		res := check(t, m, tg.ID)
		if res.st.Online || !res.uninit || res.st.StoreID != id || !strings.Contains(res.st.Hint, "adopt") || res.st.Initialised || !res.st.Writable {
			t.Fatalf("before adopt: %+v", res.st)
		}
		got, err := m.InitStore(ctx, tg.ID, true)
		if err != nil || got.StoreID != id || !got.Adopted {
			t.Fatalf("adopt: %+v %v", got, err)
		}
		if st := m.Status(tg.ID); !st.Online {
			t.Fatalf("after adopt: %+v", st)
		}
		// The same store (a copied marker) cannot be adopted twice.
		copyDir := mkdir(t, filepath.Join(cfg.MountRoot, "copy"))
		b, _ := os.ReadFile(filepath.Join(dir, cachestore.MarkerFile))
		if err := os.WriteFile(filepath.Join(copyDir, cachestore.MarkerFile), b, 0o600); err != nil {
			t.Fatal(err)
		}
		dup := create("copy", nil)
		_, err = m.InitStore(ctx, dup.ID, true)
		wantKind(t, err, apperr.KindConflict)
	})

	t.Run("create sub-directory", func(t *testing.T) {
		mkdir(t, filepath.Join(cfg.MountRoot, "share"))
		tg := create("share", func(in *TargetInput) { in.Subdir = "picache/store" })
		if res := check(t, m, tg.ID); !res.located || !res.rootMissing || res.usable() {
			t.Fatalf("missing subdir: %+v", res)
		}
		res, err := m.InitStore(ctx, tg.ID, false)
		if err != nil {
			t.Fatal(err)
		}
		root, id, err := m.StoreRoot(tg.ID)
		if err != nil || id != res.StoreID || root != filepath.Join(tg.Path, "picache", "store") {
			t.Fatalf("StoreRoot = %q %q %v", root, id, err)
		}
	})

	t.Run("missing path", func(t *testing.T) {
		tg := create("absent", nil)
		res := check(t, m, tg.ID)
		if res.st.Online || res.located || !strings.Contains(res.st.Reason, "does not exist") || res.st.Initialised || res.st.Writable {
			t.Fatalf("status %+v", res.st)
		}
		_, err := m.InitStore(ctx, tg.ID, false)
		wantKind(t, err, apperr.KindUnavailable)
		if _, _, err := m.StoreRoot(tg.ID); apperr.KindOf(err) != apperr.KindUnavailable {
			t.Fatalf("StoreRoot: %v", err)
		}
		if tr := m.Test(ctx, tg.ID); tr.OK || tr.Error == "" || tr.Hint == "" || len(tr.Steps) == 0 {
			t.Fatalf("test result %+v", tr)
		}
	})

	t.Run("marker mismatch", func(t *testing.T) {
		dir := mkdir(t, filepath.Join(cfg.MountRoot, "swap"))
		tg := create("swap", nil)
		if _, err := m.InitStore(ctx, tg.ID, false); err != nil {
			t.Fatal(err)
		}
		os.Remove(filepath.Join(dir, cachestore.MarkerFile))
		if _, err := cachestore.InitRoot(dir, "ffeeddccbbaa99887766554433221100", 1<<20); err != nil {
			t.Fatal(err)
		}
		res := check(t, m, tg.ID)
		if res.st.Online || res.uninit || !strings.Contains(res.st.Reason, "different cache store") || res.st.Initialised || !res.st.Writable {
			t.Fatalf("status %+v", res.st)
		}
		if _, _, err := m.StoreRoot(tg.ID); apperr.KindOf(err) != apperr.KindUnavailable {
			t.Fatalf("StoreRoot: %v", err)
		}
	})
}

// TestGuardRequiresMountpoint: a plain directory is never written to when a
// mount point is required (the NAS is "not mounted").
func TestGuardRequiresMountpoint(t *testing.T) {
	cfg := testConfig(t)
	m, _, _ := newTestManager(t, cfg)
	dir := mkdir(t, filepath.Join(cfg.MountRoot, "nas"))
	in := localInput(dir)
	in.RequireMountpoint = true
	tg, err := m.Create(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	res := check(t, m, tg.ID)
	if res.st.Online || res.st.Mounted || res.located {
		t.Fatalf("status %+v", res.st)
	}
	_, err = m.InitStore(context.Background(), tg.ID, false)
	wantKind(t, err, apperr.KindUnavailable)
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("wrote into an unmounted directory: %v", entries)
	}
}

func TestStatusChangeAndRequestApply(t *testing.T) {
	cfg := testConfig(t)
	m, _, _ := newTestManager(t, cfg)
	ctx := context.Background()
	var calls []Status
	m.OnStatusChange(func(id string, st Status) {
		if id == LocalTargetID {
			calls = append(calls, st)
		}
	})
	check(t, m, LocalTargetID)
	check(t, m, LocalTargetID) // unchanged: no second call
	if len(calls) != 1 || !calls[0].Online {
		t.Fatalf("callbacks %+v", calls)
	}

	tg, err := m.Create(ctx, smbInput(ptr("pw")))
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.RequestApply(ctx, tg.ID)
	wantKind(t, err, apperr.KindUnavailable) // helper not installed
	if err := os.WriteFile(m.hostApplyFlag, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := m.RequestApply(ctx, tg.ID)
	if err != nil || st.ApplyState != applyQueued {
		t.Fatalf("request: %+v %v", st, err)
	}
	if _, err := os.Stat(filepath.Join(requestsDir(cfg), tg.ID)); err != nil {
		t.Fatal(err)
	}
	if res := check(t, m, tg.ID); res.st.ApplyState != applyQueued || res.st.Online {
		t.Fatalf("status %+v", res.st)
	}
	local, _ := m.Create(ctx, localInput(filepath.Join(cfg.MountRoot, "disk")))
	_, err = m.RequestApply(ctx, local.ID)
	wantKind(t, err, apperr.KindConflict)

	// Delete replaces the queued apply with a removal of the host mount.
	if err := m.Delete(ctx, tg.ID, LocalTargetID); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(requestsDir(cfg), tg.ID))
	if err != nil || !strings.Contains(string(b), `"action":"remove"`) {
		t.Fatalf("request file %s %v", b, err)
	}
	// Deleting a target of another mode leaves nothing behind.
	if err := os.WriteFile(filepath.Join(requestsDir(cfg), local.ID+resultSuffix), []byte(`{"ok":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Delete(ctx, local.ID, LocalTargetID); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{local.ID, local.ID + resultSuffix} {
		if _, err := os.Stat(filepath.Join(requestsDir(cfg), n)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s left: %v", n, err)
		}
	}
}

func TestReadApplyState(t *testing.T) {
	dir := t.TempDir()
	r, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if s := readApplyState(dir, testID, true); s != "" {
		t.Fatalf("no files: %q", s)
	}
	writeResult(r, testID, errors.New("mount error(13): Permission denied\nsecond line"), false, componentLog(nil))
	if s := readApplyState(dir, testID, true); s != "failed: mount error(13): Permission denied second line" {
		t.Fatalf("failed: %q", s)
	}
	if s := readApplyState(dir, testID, false); s != "" { // an apply result means nothing in another mode
		t.Fatalf("failed apply, external: %q", s)
	}
	writeResult(r, testID, nil, false, componentLog(nil))
	if s := readApplyState(dir, testID, true); s != applyApplied {
		t.Fatalf("applied: %q", s)
	}
	if s := readApplyState(dir, testID, false); s != "" {
		t.Fatalf("applied, external: %q", s)
	}
	for _, hostApply := range []bool{true, false} {
		writeResult(r, testID, errors.New("cannot unmount"), true, componentLog(nil))
		if s := readApplyState(dir, testID, hostApply); s != "failed: cannot unmount" {
			t.Fatalf("failed removal: %q", s)
		}
		writeResult(r, testID, nil, true, componentLog(nil))
		if s := readApplyState(dir, testID, hostApply); s != "" {
			t.Fatalf("removed: %q", s)
		}
	}
	if err := writeRequest(dir, testID, actionApply); err != nil {
		t.Fatal(err)
	}
	if s := readApplyState(dir, testID, true); s != applyQueued {
		t.Fatalf("queued: %q", s)
	}
	if err := os.WriteFile(filepath.Join(dir, testID+resultSuffix), []byte(strings.Repeat("x", maxResultSize+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(dir, testID))
	if s := readApplyState(dir, testID, true); s != "failed: unreadable result file" {
		t.Fatalf("oversized: %q", s)
	}
}

// TestAppliedButNotMountedHint: a successful apply that PiCache cannot see
// (no mount propagation into its namespace) gets an explaining hint.
func TestAppliedButNotMountedHint(t *testing.T) {
	cfg := testConfig(t)
	tg := Target{ID: testID, Name: "NAS", Kind: KindSMB, Mode: ModeHostApply, Path: hostApplyPath(cfg, testID)}
	m := bareManager(cfg, tg)
	m.probeFn = func(t Target) checkResult {
		return checkResult{notMounted: true, st: Status{Reason: "nothing is mounted at " + t.Path, Hint: "Apply the mount", CheckedAt: time.Now()}}
	}
	if res := check(t, m, testID); res.st.Hint != "Apply the mount" {
		t.Fatalf("not applied: %+v", res.st)
	}
	r, err := os.OpenRoot(requestsDir(cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	writeResult(r, testID, nil, false, componentLog(nil))
	if res := check(t, m, testID); res.st.Hint != appliedNotMountedHint || res.st.ApplyState != applyApplied {
		t.Fatalf("applied: %+v", res.st)
	}
}
