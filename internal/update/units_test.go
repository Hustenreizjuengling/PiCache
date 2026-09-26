package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
)

// tarEntry is one entry of a test archive.
type tarEntry struct {
	name string
	typ  byte
	data []byte
	link string
}

// deployArchive builds a gzip-compressed tar archive.
func deployArchive(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		hdr := &tar.Header{Name: e.name, Typeflag: typ, Mode: 0o644, Size: int64(len(e.data)), Linkname: e.link, Format: tar.FormatGNU}
		if typ != tar.TypeReg {
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if typ == tar.TypeReg {
			if _, err := tw.Write(e.data); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// newUnits is the unit content of the new release.
func newUnit(name string) []byte { return []byte("# new " + name + "\n[Service]\n") }

// standardArchive holds deploy/ with every allow-listed unit and more.
func standardArchive(t *testing.T) []byte {
	e := []tarEntry{{name: "deploy/install.sh", data: []byte("#!/bin/sh\n")}, {name: "LICENSE", data: []byte("license")},
		{name: "deploy/systemd/", typ: tar.TypeDir}}
	for _, n := range unitNames {
		e = append(e, tarEntry{name: "deploy/systemd/" + n, data: newUnit(n)})
	}
	return deployArchive(t, e...)
}

// signedRelease returns the release files with the binary bin and, unless
// nil, the deploy archive.
func signedRelease(bin, deploy []byte) map[string][]byte {
	sums := fmt.Sprintf("%x  picache-linux-amd64\n", sha256.Sum256(bin))
	files := map[string][]byte{"picache-linux-amd64": bin}
	if deploy != nil {
		sums += fmt.Sprintf("%x  %s\n", sha256.Sum256(deploy), DeployArchive)
		files[DeployArchive] = deploy
	}
	files[SumsFile] = []byte(sums)
	files[SigFile] = []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(testKey, []byte(sums))) + "\n")
	return files
}

// unitEnv is an installed PiCache with a unit directory: picache.service
// (old), picache-update.service (already the new one), picache-storage.path
// a symbolic link; the other units are not installed.
func unitEnv(t *testing.T) (*installEnv, string) {
	e := newInstallEnv(t, "v0.9.0")
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "picache.service"), []byte("# old picache.service\n"))
	writeFile(t, filepath.Join(dir, "picache-update.service"), newUnit("picache-update.service"))
	writeFile(t, filepath.Join(dir, "picache-update.path"), []byte("# old picache-update.path\n"))
	if err := os.Symlink(filepath.Join(dir, "picache.service"), filepath.Join(dir, "picache-storage.path")); err != nil &&
		runtime.GOOS != "windows" {
		t.Fatal(err)
	}
	return e, dir
}

func (e *installEnv) unitOptions(t *testing.T, dir string, files Files) Options {
	o := e.options("v0.9.0", "v0.9.1", files)
	o.Host.UnitDir = dir
	o.Host.UnitFragment = func(context.Context) (string, error) { return filepath.Join(dir, Service), nil }
	return o
}

func releaseWithUnits(t *testing.T, g *fakeGitHub, deploy []byte) {
	g.addRelease("v0.9.1", signedRelease(fakeBinary("v0.9.1"), deploy))
}

// The archive: only allow-listed regular files below deploy/systemd/ are
// kept; a link, a duplicate or an oversized unit, more than 64 MiB unpacked
// or more than 4096 entries abort.
func TestParseDeployArchive(t *testing.T) {
	units, err := parseDeployArchive(deployArchive(t,
		tarEntry{name: "./deploy/systemd/picache.service", data: []byte("a")},
		tarEntry{name: "deploy/systemd/other.service", data: []byte("b")},
		tarEntry{name: "deploy/systemd/sub/picache.service", data: []byte("c")},
		tarEntry{name: "deploy/picache.service", data: []byte("d")},
		tarEntry{name: "deploy/systemd/picache.service.d/x.conf", data: []byte("e")},
	))
	if err != nil || len(units) != 1 || string(units["picache.service"]) != "a" {
		t.Fatalf("units %v %v", units, err)
	}
	many := make([]tarEntry, maxDeployEntries+1)
	for i := range many {
		many[i] = tarEntry{name: fmt.Sprintf("deploy/f%d", i), data: []byte{1}}
	}
	for name, archive := range map[string][]byte{
		"symlink":   deployArchive(t, tarEntry{name: "deploy/systemd/picache.service", typ: tar.TypeSymlink, link: "/etc/shadow"}),
		"hard link": deployArchive(t, tarEntry{name: "deploy/systemd/picache.service", typ: tar.TypeLink, link: "deploy/install.sh"}),
		"duplicate": deployArchive(t, tarEntry{name: "deploy/systemd/picache.path", data: nil},
			tarEntry{name: "deploy/systemd/picache-update.path", data: []byte("x")}, tarEntry{name: "deploy/systemd/picache-update.path", data: []byte("y")}),
		"oversized": deployArchive(t, tarEntry{name: "deploy/systemd/picache.service", data: make([]byte, maxUnitFile+1)}),
		"bomb":      deployArchive(t, tarEntry{name: "deploy/zeros", data: make([]byte, maxDeployUnpacked+1)}),
		"entries":   deployArchive(t, many...),
		"not gzip":  []byte("deploy"),
		"truncated": standardArchive(t)[:100],
	} {
		if _, err := parseDeployArchive(archive); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
}

// An update replaces the installed units that differ (existing regular
// files only; the current one kept as .prev), runs daemon-reload before the
// restart and never touches identical ones, links or missing units.
func TestApplyUnits(t *testing.T) {
	useTestKey(t)
	g := newFakeGitHub(t)
	releaseWithUnits(t, g, standardArchive(t))
	e, dir := unitEnv(t)
	res, err := Apply(t.Context(), e.unitOptions(t, dir, g.client().Files("v0.9.1")))
	if err != nil || res.State != StateSucceeded || res.Message != "PiCache v0.9.1 is running" {
		t.Fatalf("Apply = %+v, %v", res, err)
	}
	for name, want := range map[string]string{
		"picache.service": string(newUnit("picache.service")), "picache.service.prev": "# old picache.service\n",
		"picache-update.path": string(newUnit("picache-update.path")), "picache-update.path.prev": "# old picache-update.path\n",
		"picache-update.service": string(newUnit("picache-update.service")),
	} {
		if got := readFile(t, filepath.Join(dir, name)); got != want {
			t.Errorf("%s = %q", name, got)
		}
	}
	for _, name := range []string{"picache-update.service.prev", "picache-storage.service", "picache-shared-mounts.service", "picache-storage.path.prev"} {
		if exists(filepath.Join(dir, name)) {
			t.Errorf("%s written", name)
		}
	}
	if fi, err := os.Lstat(filepath.Join(dir, "picache-storage.path")); err == nil && fi.Mode()&os.ModeSymlink == 0 {
		t.Error("the link was replaced")
	}
	if calls := e.host.systemctlCalls(); !slices.Equal(calls, []string{"daemon-reload", "restart picache.service"}) {
		t.Errorf("systemctl %v", calls)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(filepath.Join(dir, "picache.service")); fi.Mode().Perm() != 0o644 {
			t.Errorf("mode %v", fi.Mode())
		}
	}
}

// picache.service loaded from another file (a copy in /etc): the unit step
// is skipped, the final message says so; the archive is not even fetched.
func TestApplyUnitsFragmentElsewhere(t *testing.T) {
	useTestKey(t)
	g := newFakeGitHub(t)
	releaseWithUnits(t, g, standardArchive(t))
	e, dir := unitEnv(t)
	o := e.unitOptions(t, dir, g.client().Files("v0.9.1"))
	o.Host.UnitFragment = func(context.Context) (string, error) { return "/etc/systemd/system/picache.service", nil }
	res, err := Apply(t.Context(), o)
	if err != nil || !strings.Contains(res.Message, "the unit files were not updated (picache.service is loaded from /etc/systemd/system/picache.service") ||
		!strings.HasSuffix(res.Message, "run the one-line installer once") {
		t.Fatalf("Apply = %+v, %v", res, err)
	}
	if readFile(t, filepath.Join(dir, "picache.service")) != "# old picache.service\n" {
		t.Fatal("unit replaced")
	}
	if slices.ContainsFunc(g.requested(), func(p string) bool { return strings.HasSuffix(p, DeployArchive) }) {
		t.Fatal("archive downloaded")
	}
}

// A read-only unit directory (an older helper unit's sandbox) or a failed
// daemon-reload: the units replaced so far are put back and the update
// succeeds with the binary alone.
func TestApplyUnitsFailureIsNotFatal(t *testing.T) {
	useTestKey(t)
	for _, tc := range []string{"erofs", "daemon-reload"} {
		t.Run(tc, func(t *testing.T) {
			g := newFakeGitHub(t)
			releaseWithUnits(t, g, standardArchive(t))
			e, dir := unitEnv(t)
			if tc == "erofs" {
				old := writeUnit
				writes := 0
				writeUnit = func(r *os.Root, name string, data []byte, strict bool) error {
					if writes++; writes == 4 { // picache.service.prev, picache.service, picache-update.path.prev, then this
						return &os.PathError{Op: "open", Path: name, Err: syscall.EROFS}
					}
					return old(r, name, data, strict)
				}
				t.Cleanup(func() { writeUnit = old })
			} else {
				e.host.failCmd = "daemon-reload"
			}
			res, err := Apply(t.Context(), e.unitOptions(t, dir, g.client().Files("v0.9.1")))
			if err != nil || res.State != StateSucceeded || !strings.Contains(res.Message, "PiCache v0.9.1 is running; the unit files were not updated (") ||
				!strings.HasSuffix(res.Message, "): run the one-line installer once") {
				t.Fatalf("Apply = %+v, %v", res, err)
			}
			if readFile(t, e.bin) != string(fakeBinary("v0.9.1")) {
				t.Fatal("binary not installed")
			}
			for _, name := range []string{"picache.service", "picache-update.path"} {
				if got := readFile(t, filepath.Join(dir, name)); got != "# old "+name+"\n" {
					t.Errorf("%s not put back: %q", name, got)
				}
			}
		})
	}
}

// The health-failure and the SIGTERM rollback put back exactly the units
// this run replaced, then daemon-reload, then the binary.
func TestApplyUnitsRollback(t *testing.T) {
	useTestKey(t)
	for _, tc := range []string{"health", "sigterm"} {
		t.Run(tc, func(t *testing.T) {
			g := newFakeGitHub(t)
			releaseWithUnits(t, g, standardArchive(t))
			e, dir := unitEnv(t)
			writeFile(t, filepath.Join(dir, "picache-update.service.prev"), []byte("older"))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			e.host.healthy = func(running string) bool { return strings.Contains(running, "v0.9.0") }
			if tc == "sigterm" {
				e.host.healthy = func(string) bool { return true }
				e.host.onStart = func(running string) {
					if strings.Contains(running, "v0.9.1") {
						cancel()
					}
				}
			}
			res, err := Apply(ctx, e.unitOptions(t, dir, g.client().Files("v0.9.1")))
			if err == nil || res.State != StateRolledBack {
				t.Fatalf("Apply = %+v, %v", res, err)
			}
			for _, name := range []string{"picache.service", "picache-update.path"} {
				if got := readFile(t, filepath.Join(dir, name)); got != "# old "+name+"\n" {
					t.Errorf("%s not put back: %q", name, got)
				}
			}
			if readFile(t, filepath.Join(dir, "picache-update.service")) != string(newUnit("picache-update.service")) ||
				readFile(t, filepath.Join(dir, "picache-update.service.prev")) != "older" {
				t.Error("a unit this run did not replace was restored")
			}
			calls := e.host.systemctlCalls()
			if len(calls) < 3 || calls[0] != "daemon-reload" || calls[1] != "restart picache.service" || calls[2] != "daemon-reload" {
				t.Errorf("systemctl %v", calls)
			}
		})
	}
}

// Without the archive in SHA256SUMS, or with --from without the file, the
// unit step is skipped and older .prev files stay; a present file in the
// directory counts like a download; a tampered archive aborts before
// anything changed.
func TestApplyUnitsSkipped(t *testing.T) {
	useTestKey(t)
	g := newFakeGitHub(t)
	releaseWithUnits(t, g, nil)
	e, dir := unitEnv(t)
	writeFile(t, filepath.Join(dir, "picache.service.prev"), []byte("older"))
	res, err := Apply(t.Context(), e.unitOptions(t, dir, g.client().Files("v0.9.1")))
	if err != nil || res.Message != "PiCache v0.9.1 is running" || readFile(t, filepath.Join(dir, "picache.service.prev")) != "older" ||
		readFile(t, filepath.Join(dir, "picache.service")) != "# old picache.service\n" {
		t.Fatalf("without the archive: %+v %v", res, err)
	}

	for _, withFile := range []bool{false, true} {
		src := t.TempDir()
		for name, b := range signedRelease(fakeBinary("v0.9.1"), standardArchive(t)) {
			if name == DeployArchive && !withFile {
				continue
			}
			writeFile(t, filepath.Join(src, name), b)
		}
		e, dir := unitEnv(t)
		o := e.unitOptions(t, dir, DirFiles(src))
		o.Version = ""
		res, err := Apply(t.Context(), o)
		replaced := readFile(t, filepath.Join(dir, "picache.service")) == string(newUnit("picache.service"))
		if err != nil || res.State != StateSucceeded || replaced != withFile {
			t.Fatalf("--from (file %v): %+v %v, replaced %v", withFile, res, err, replaced)
		}
	}

	g = newFakeGitHub(t)
	files := signedRelease(fakeBinary("v0.9.1"), standardArchive(t))
	files[DeployArchive] = append(files[DeployArchive], 0)
	g.addRelease("v0.9.1", files)
	e, dir = unitEnv(t)
	res, err = Apply(t.Context(), e.unitOptions(t, dir, g.client().Files("v0.9.1")))
	if err == nil || res.State != StateFailed || !strings.Contains(res.Message, DeployArchive+" does not match") {
		t.Fatalf("tampered: %+v %v", res, err)
	}
	if readFile(t, e.bin) != string(fakeBinary("v0.9.0")) || len(e.host.systemctlCalls()) != 0 ||
		readFile(t, filepath.Join(dir, "picache.service")) != "# old picache.service\n" {
		t.Fatal("a tampered archive changed something")
	}
	// A broken archive that matches SHA256SUMS aborts too.
	g = newFakeGitHub(t)
	g.addRelease("v0.9.1", signedRelease(fakeBinary("v0.9.1"), deployArchive(t,
		tarEntry{name: "deploy/systemd/picache.service", typ: tar.TypeSymlink, link: "/etc/passwd"})))
	e, dir = unitEnv(t)
	if res, err := Apply(t.Context(), e.unitOptions(t, dir, g.client().Files("v0.9.1"))); err == nil || !strings.Contains(res.Message, "not a regular file") {
		t.Fatalf("link in the archive: %+v %v", res, err)
	}
}
