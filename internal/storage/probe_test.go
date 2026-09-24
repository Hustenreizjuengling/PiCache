package storage

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestParseMountinfo(t *testing.T) {
	in := `22 1 179:2 / / rw,noatime shared:1 - ext4 /dev/root rw
36 22 0:45 / /srv/picache/nas rw,nosuid,nodev,noexec,relatime shared:80 - cifs //192.168.1.10/picache rw,vers=3.1.1
37 22 0:46 / /srv/picache/my\040disk rw master:3 - nfs4 192.168.1.10:/volume1 rw
broken line
38 22 8:1 / /mnt/usb rw shared:9 opt:1 - xfs /dev/sda1 rw
`
	got, err := parseMountinfo(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	want := []mountEntry{
		{"179:2", "/", "ext4", "/dev/root"},
		{"0:45", "/srv/picache/nas", "cifs", "//192.168.1.10/picache"},
		{"0:46", "/srv/picache/my disk", "nfs4", "192.168.1.10:/volume1"},
		{"8:1", "/mnt/usb", "xfs", "/dev/sda1"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
	for in, want := range map[string]string{`a\040b`: "a b", `a\011b\012c\134d`: "a\tb\nc\\d", `a\9`: `a\9`, `x\777`: `x\777`} {
		if got := unescapeMountinfo(in); got != want {
			t.Errorf("unescape(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFSMismatch(t *testing.T) {
	smb := Target{ID: testID, Kind: KindSMB, Path: "/srv/picache/nas"}
	nfs := Target{ID: testID, Kind: KindNFS, Path: "/srv/picache/nas"}
	disk := Target{ID: testID, Kind: KindLocal, Path: "/srv/picache/disk"}
	builtin := Target{ID: LocalTargetID, Kind: KindLocal}
	for _, tc := range []struct {
		t     Target
		magic uint32
		known bool
		ok    bool
	}{
		{smb, magicCIFS, true, true},
		{smb, magicSMB2, true, true},
		{smb, magicExt4, true, false}, // share not mounted over a local dir
		{smb, 0, false, false},
		{nfs, magicNFS, true, true},
		{nfs, magicCIFS, true, false},
		{disk, magicZFS, true, true},
		{disk, magicNFS, true, false},
		{disk, 0, false, true},
		{builtin, magicNFS, true, true},
	} {
		if got := fsMismatch(tc.t, fsInfo{typ: tc.magic}, tc.known) == ""; got != tc.ok {
			t.Errorf("%s on 0x%x (known %v): ok = %v", tc.t.Kind, tc.magic, tc.known, got)
		}
	}
	if fsTypeName(0x2fc12fc1) != "zfs" || fsTypeName(0xff534d42) != "cifs" || fsTypeName(0x12345) != "0x12345" {
		t.Error("fsTypeName")
	}
}

func TestNoSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symbolic links needs extra privileges on Windows")
	}
	base := t.TempDir()
	mkdir(t, filepath.Join(base, "real", "store"))
	if err := os.Symlink(filepath.Join(base, "real"), filepath.Join(base, "link")); err != nil {
		t.Fatal(err)
	}
	if err := noSymlinks(base, filepath.Join(base, "real", "store")); err != nil {
		t.Fatal(err)
	}
	if err := noSymlinks(base, filepath.Join(base, "link", "store")); err == nil {
		t.Fatal("symbolic link accepted")
	}
	if err := noSymlinks(base, filepath.Dir(base)); err == nil {
		t.Fatal("path outside base accepted")
	}
}

func TestProbeRefusesSymlinkedTarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symbolic links needs extra privileges on Windows")
	}
	cfg := testConfig(t)
	outside := mkdir(t, filepath.Join(t.TempDir(), "etc"))
	if err := os.Symlink(outside, filepath.Join(cfg.MountRoot, "nas")); err != nil {
		t.Fatal(err)
	}
	tg := Target{ID: testID, Name: "x", Kind: KindLocal, Mode: ModeExternal, Path: filepath.Join(cfg.MountRoot, "nas")}
	m := bareManager(cfg, tg)
	res := m.probe(tg)
	if res.st.Online || res.located || !strings.Contains(res.st.Reason, "symbolic link") {
		t.Fatalf("status %+v", res.st)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("wrote through the link: %v", entries)
	}
}

func TestFormatBytes(t *testing.T) {
	for n, want := range map[uint64]string{0: "0 B", 1023: "1023 B", 1024: "1.0 KiB", 3 << 30: "3.0 GiB", 5 << 40: "5.0 TiB"} {
		if got := formatBytes(n); got != want {
			t.Errorf("formatBytes(%d) = %q, want %q", n, got, want)
		}
	}
}
