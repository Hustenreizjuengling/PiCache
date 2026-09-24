//go:build linux

package storage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// TestGuardRealMountAsRoot mounts a tmpfs to exercise mount point
// detection, fstatfs and the file system type check. It needs root (with
// CAP_SYS_ADMIN), so it only runs in a privileged container or VM.
func TestGuardRealMountAsRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	cfg := testConfig(t)
	dir := mkdir(t, filepath.Join(cfg.MountRoot, "nas"))
	if err := unix.Mount("tmpfs", dir, "tmpfs", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, "size=16m"); err != nil {
		t.Skip("cannot mount:", err)
	}
	mounted := true
	t.Cleanup(func() {
		if mounted {
			unix.Unmount(dir, unix.MNT_DETACH)
		}
	})
	m, _, _ := newTestManager(t, cfg)
	ctx := context.Background()
	in := localInput(dir)
	in.RequireMountpoint = true
	tg, err := m.Create(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	res := check(t, m, tg.ID)
	if !res.st.Mounted || res.st.FSType != "tmpfs" || res.st.TotalBytes != 16<<20 || !res.uninit || !res.usable() {
		t.Fatalf("mounted tmpfs: %+v %v", res.st, res.steps)
	}
	if _, err := m.InitStore(ctx, tg.ID, false); err != nil {
		t.Fatal(err)
	}
	if st := m.Status(tg.ID); !st.Online || st.SameFSAsData {
		t.Fatalf("after init: %+v", st)
	}

	// The same mount is not an SMB share.
	smb := Target{ID: testID, Name: "NAS", Kind: KindSMB, Mode: ModeExternal, Path: dir, Server: "192.168.1.10",
		Share: "picache", SMBVersion: "3.1.1", RequireMountpoint: true}
	if r := m.probe(smb); r.located || !strings.Contains(r.st.Reason, "expected an SMB/CIFS file system") {
		t.Fatalf("smb on tmpfs: %+v", r.st)
	}

	// Unmounted: offline, and nothing is written to the directory underneath.
	if err := unix.Unmount(dir, 0); err != nil {
		t.Fatal(err)
	}
	mounted = false
	res = check(t, m, tg.ID)
	if res.st.Online || res.st.Mounted || !strings.Contains(res.st.Reason, "nothing is mounted") {
		t.Fatalf("unmounted: %+v", res.st)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("wrote into the unmounted directory: %v", entries)
	}
}
