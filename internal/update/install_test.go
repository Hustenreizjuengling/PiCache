package update

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestApplySuccess(t *testing.T) {
	useTestKey(t)
	g := newFakeGitHub(t)
	g.addRelease("v0.9.1", releaseFiles(fakeBinary("v0.9.1")))
	e := newInstallEnv(t, "v0.9.0")

	res, err := Apply(t.Context(), e.options("v0.9.0", "v0.9.1", g.client().Files("v0.9.1")))
	if err != nil || res.State != StateSucceeded || res.Version != "v0.9.1" || res.From != "v0.9.0" {
		t.Fatalf("Apply = %+v, %v", res, err)
	}
	if got := readFile(t, e.bin); got != string(fakeBinary("v0.9.1")) {
		t.Errorf("installed binary %q", got)
	}
	if got := readFile(t, e.bin+".prev"); got != string(fakeBinary("v0.9.0")) {
		t.Errorf("picache.prev %q", got)
	}
	if exists(filepath.Join(filepath.Dir(e.bin), ".picache.update")) {
		t.Error("staged binary left behind")
	}
	if calls := e.host.systemctlCalls(); !slices.Equal(calls, []string{"restart picache.service"}) {
		t.Errorf("systemctl calls %v", calls)
	}
	if want := []string{StepDownload, StepVerify, StepDownload, StepVerify, StepInstall, StepRestart, StepHealth, StepDone}; !slices.Equal(e.steps, want) {
		t.Errorf("steps %v, want %v", e.steps, want)
	}
	for _, p := range g.requested() {
		if strings.Contains(p, "picache-deploy") || strings.Contains(p, "arm") {
			t.Errorf("downloaded more than needed: %s", p)
		}
	}
	entries, _ := os.ReadDir(filepath.Dir(e.bin))
	if len(entries) != 2 {
		t.Errorf("files next to the binary: %v", entries)
	}
}

// The new version comes up but never becomes healthy: the old binary and
// the database copy the new version made at its start come back, with the
// service stopped, and the old version is started again.
func TestApplyRollsBack(t *testing.T) {
	useTestKey(t)
	g := newFakeGitHub(t)
	g.addRelease("v0.9.1", releaseFiles(fakeBinary("v0.9.1")))
	e := newInstallEnv(t, "v0.9.0")
	backups := filepath.Join(e.dataDir, "backups")
	db := filepath.Join(e.dataDir, "picache.db")
	// Older copies of the same version and a newer copy of another version
	// are not the one this run made.
	old := filepath.Join(backups, "picache-v0.9.0-20260101T000000.db")
	writeFile(t, old, []byte("copy from january"))
	past := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	e.host.healthy = func(running string) bool { return strings.Contains(running, "v0.9.0") }
	e.host.onStart = func(running string) {
		if !strings.Contains(running, "v0.9.1") {
			return
		}
		// What v0.9.1 does at its start: copy the database, then migrate it.
		writeFile(t, filepath.Join(backups, "picache-v0.9.0-"+time.Now().UTC().Format("20060102T150405")+".db"), []byte("database of v0.9.0"))
		writeFile(t, filepath.Join(backups, "picache-v0.8.0-"+time.Now().UTC().Format("20060102T150405")+".db"), []byte("other"))
		writeFile(t, db, []byte("migrated by v0.9.1"))
		writeFile(t, db+"-wal", []byte("wal of v0.9.1"))
		writeFile(t, db+"-shm", []byte("shm"))
	}

	res, err := Apply(t.Context(), e.options("v0.9.0", "v0.9.1", g.client().Files("v0.9.1")))
	if err == nil || res.State != StateRolledBack {
		t.Fatalf("Apply = %+v, %v", res, err)
	}
	for _, want := range []string{"v0.9.1 did not become healthy", "connection refused", "rolled back to v0.9.0", "database copy picache-v0.9.0-"} {
		if !strings.Contains(res.Message, want) {
			t.Errorf("message %q lacks %q", res.Message, want)
		}
	}
	if got := readFile(t, e.bin); got != string(fakeBinary("v0.9.0")) {
		t.Errorf("binary after rollback %q", got)
	}
	if got := readFile(t, db); got != "database of v0.9.0" {
		t.Errorf("database after rollback %q", got)
	}
	if exists(db+"-wal") || exists(db+"-shm") {
		t.Error("the new version's -wal/-shm were kept")
	}
	if want := []string{"restart picache.service", "stop picache.service", "start picache.service"}; !slices.Equal(e.host.systemctlCalls(), want) {
		t.Errorf("systemctl calls %v, want %v", e.host.systemctlCalls(), want)
	}
	if e.steps[len(e.steps)-1] != StepRollback {
		t.Errorf("steps %v", e.steps)
	}
	entries, _ := os.ReadDir(e.dataDir)
	for _, en := range entries {
		if strings.Contains(en.Name(), "rollback") {
			t.Errorf("temporary file left: %s", en.Name())
		}
	}
}

// Without a copy made during this run (the new version did not get that
// far) only the binary is rolled back; a failing restart counts like a
// failed health check.
func TestApplyRollbackWithoutDatabaseCopy(t *testing.T) {
	useTestKey(t)
	g := newFakeGitHub(t)
	g.addRelease("v0.9.1", releaseFiles(fakeBinary("v0.9.1")))
	e := newInstallEnv(t, "v0.9.0")
	e.host.failCmd = "restart picache.service"
	res, err := Apply(t.Context(), e.options("v0.9.0", "v0.9.1", g.client().Files("v0.9.1")))
	if err == nil || res.State != StateRolledBack || !strings.Contains(res.Message, "the database was not changed") {
		t.Fatalf("Apply = %+v, %v", res, err)
	}
	if readFile(t, filepath.Join(e.dataDir, "picache.db")) != "database of v0.9.0" || readFile(t, e.bin) != string(fakeBinary("v0.9.0")) {
		t.Error("rollback changed the database or kept the new binary")
	}

	// When the old version does not come back either, the result is failed.
	e = newInstallEnv(t, "v0.9.0")
	e.host.healthy = func(string) bool { return false }
	res, err = Apply(t.Context(), e.options("v0.9.0", "v0.9.1", g.client().Files("v0.9.1")))
	if err == nil || res.State != StateFailed || !strings.Contains(res.Message, "v0.9.0 is not healthy either") {
		t.Fatalf("Apply = %+v, %v", res, err)
	}
}

// Every check before the installation leaves the installed binary alone,
// runs no systemctl command and leaves no staged file.
func TestApplyRefuses(t *testing.T) {
	useTestKey(t)
	other := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	for _, tc := range []struct {
		name    string
		version string // requested
		files   func() map[string][]byte
		current string
		want    string
		noBin   bool // the binary must not even be downloaded
	}{
		{"version mismatch", "v0.9.1", func() map[string][]byte { return releaseFiles(fakeBinary("v0.9.2")) }, "v0.9.0",
			`reports "picache v0.9.2 (commit test`, false},
		{"not a picache binary", "v0.9.1", func() map[string][]byte { return releaseFiles([]byte("#!/bin/sh\necho hello\n")) }, "v0.9.0",
			"instead of version v0.9.1", false},
		{"wrong key", "v0.9.1", func() map[string][]byte {
			f := releaseFiles(fakeBinary("v0.9.1"))
			f[SigFile] = []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(other, f[SumsFile])))
			return f
		}, "v0.9.0", "does not match any trusted release key", true},
		{"tampered sums", "v0.9.1", func() map[string][]byte {
			f := releaseFiles(fakeBinary("v0.9.1"))
			f[SumsFile] = append(f[SumsFile], "0000000000000000000000000000000000000000000000000000000000000000  extra\n"...)
			return f
		}, "v0.9.0", "does not match any trusted release key", true},
		{"tampered binary", "v0.9.1", func() map[string][]byte {
			f := releaseFiles(fakeBinary("v0.9.1"))
			f["picache-linux-amd64"] = append(f["picache-linux-amd64"], "evil"...)
			return f
		}, "v0.9.0", "does not match SHA256SUMS", false},
		{"no signature", "v0.9.1", func() map[string][]byte {
			f := releaseFiles(fakeBinary("v0.9.1"))
			delete(f, SigFile)
			return f
		}, "v0.9.0", "HTTP 404", true},
		{"no entry for the architecture", "v0.9.1", func() map[string][]byte {
			bin := fakeBinary("v0.9.1")
			f := releaseFiles(bin)
			sums := strings.ReplaceAll(string(f[SumsFile]), "picache-linux-amd64", "picache-linux-arm64")
			f[SumsFile] = []byte(sums)
			f[SigFile] = []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(testKey, []byte(sums))))
			return f
		}, "v0.9.0", "SHA256SUMS has no entry for picache-linux-amd64", true},
		{"oversized SHA256SUMS", "v0.9.1", func() map[string][]byte {
			f := releaseFiles(fakeBinary("v0.9.1"))
			f[SumsFile] = make([]byte, maxSumsSize+1)
			return f
		}, "v0.9.0", "larger than 64 KiB", true},
		{"same version", "v0.9.1", func() map[string][]byte { return releaseFiles(fakeBinary("v0.9.1")) }, "v0.9.1",
			"not newer than the installed version", true},
		{"downgrade", "v0.9.1", func() map[string][]byte { return releaseFiles(fakeBinary("v0.9.1")) }, "v1.0.0",
			"use --allow-downgrade", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newFakeGitHub(t)
			g.addRelease(tc.version, tc.files())
			e := newInstallEnv(t, tc.current)
			res, err := Apply(t.Context(), e.options(tc.current, tc.version, g.client().Files(tc.version)))
			if err == nil || res.State != StateFailed || !strings.Contains(res.Message, tc.want) {
				t.Fatalf("Apply = %+v, %v; want %q", res, err, tc.want)
			}
			if got := readFile(t, e.bin); got != string(fakeBinary(tc.current)) {
				t.Errorf("installed binary changed: %q", got)
			}
			if calls := e.host.systemctlCalls(); len(calls) != 0 || exists(e.bin+".prev") {
				t.Errorf("systemctl calls %v, prev exists %v", calls, exists(e.bin+".prev"))
			}
			if entries, _ := os.ReadDir(filepath.Dir(e.bin)); len(entries) != 1 {
				t.Errorf("files left next to the binary: %v", entries)
			}
			if tc.noBin && slices.ContainsFunc(g.requested(), func(p string) bool { return strings.HasSuffix(p, "/picache-linux-amd64") }) {
				t.Error("the binary was downloaded before the signature was verified")
			}
		})
	}

	e := newInstallEnv(t, "v1.0.0")
	g := newFakeGitHub(t)
	g.addRelease("v0.9.1", releaseFiles(fakeBinary("v0.9.1")))
	o := e.options("v1.0.0", "v0.9.1", g.client().Files("v0.9.1"))
	o.Version, o.AllowDowngrade = "v0.9.1", true
	if res, err := Apply(t.Context(), o); err != nil || res.State != StateSucceeded {
		t.Fatalf("allowed downgrade: %+v, %v", res, err)
	}
}

// With a local directory and no version, the version is the one the
// verified binary reports; Confirm sees it before anything changes.
func TestApplyFromDirectory(t *testing.T) {
	useTestKey(t)
	dir := t.TempDir()
	for name, b := range releaseFiles(fakeBinary("v0.9.1")) {
		writeFile(t, filepath.Join(dir, name), b)
	}
	e := newInstallEnv(t, "v0.9.0")
	o := e.options("v0.9.0", "", DirFiles(dir))
	var confirmed string
	o.Confirm = func(v string) error { confirmed = v; return errors.New("aborted") }
	if res, err := Apply(t.Context(), o); err == nil || res.Message != "aborted" || confirmed != "v0.9.1" {
		t.Fatalf("declined: %+v, %v (confirmed %q)", res, err, confirmed)
	}
	if readFile(t, e.bin) != string(fakeBinary("v0.9.0")) || len(e.host.systemctlCalls()) != 0 {
		t.Fatal("a declined update changed something")
	}
	o.Confirm = nil
	if res, err := Apply(t.Context(), o); err != nil || res.Version != "v0.9.1" || readFile(t, e.bin) != string(fakeBinary("v0.9.1")) {
		t.Fatalf("from directory: %+v, %v", res, err)
	}

	// A development build in the directory is refused (no version to check).
	dev := t.TempDir()
	for name, b := range releaseFiles(fakeBinary("v0.9.2-3-gabcdef0")) {
		writeFile(t, filepath.Join(dev, name), b)
	}
	e = newInstallEnv(t, "v0.9.0")
	if res, err := Apply(t.Context(), e.options("v0.9.0", "", DirFiles(dev))); err == nil || !strings.Contains(res.Message, "not a release build") {
		t.Fatalf("development build: %+v, %v", res, err)
	}
}

// The binary replaced must be the one picache.service runs.
func TestApplyChecksServiceBinary(t *testing.T) {
	useTestKey(t)
	g := newFakeGitHub(t)
	g.addRelease("v0.9.1", releaseFiles(fakeBinary("v0.9.1")))
	e := newInstallEnv(t, "v0.9.0")
	o := e.options("v0.9.0", "v0.9.1", g.client().Files("v0.9.1"))
	o.Host.ServiceBinary = func(context.Context) (string, error) { return filepath.Join(t.TempDir(), "picache"), nil }
	if res, err := Apply(t.Context(), o); err == nil || !strings.Contains(res.Message, "picache.service runs") {
		t.Fatalf("other binary: %+v, %v", res, err)
	}
	o.Host.ServiceBinary = func(context.Context) (string, error) { return e.bin, nil }
	if res, err := Apply(t.Context(), o); err != nil {
		t.Fatalf("same binary: %+v, %v", res, err)
	}
}

func TestExecStartPath(t *testing.T) {
	out := "{ path=/usr/local/bin/picache ; argv[]=/usr/local/bin/picache serve ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=0 ; code=(null) ; status=0/0 }\n"
	if p, err := execStartPath(out); err != nil || p != "/usr/local/bin/picache" {
		t.Errorf("execStartPath = %q, %v", p, err)
	}
	if _, err := execStartPath("\n"); err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Errorf("no unit: %v", err)
	}
}

func TestVersionFromOutput(t *testing.T) {
	if v, err := versionFromOutput(string(fakeBinary("v1.2.3-rc.1"))); err != nil || v.String() != "v1.2.3-rc.1" {
		t.Errorf("versionFromOutput = %v, %v", v, err)
	}
	for _, out := range []string{"", "hello", "picache dev (commit x)", "picache v1.2.3-4-gabc1234 (x)", "picachev1.2.3 "} {
		if _, err := versionFromOutput(out); err == nil {
			t.Errorf("versionFromOutput(%q) succeeded", out)
		}
	}
}

// Another update replaced the binary after this run started (its version
// is o.Current): the run stops before downloading anything instead of
// comparing against the wrong version, which would allow a downgrade.
func TestApplyRefusesReplacedBinary(t *testing.T) {
	useTestKey(t)
	g := newFakeGitHub(t)
	g.addRelease("v0.9.5", releaseFiles(fakeBinary("v0.9.5")))
	e := newInstallEnv(t, "v0.9.0")
	writeFile(t, e.bin, fakeBinary("v1.0.0")) // installed by the CLI meanwhile
	res, err := Apply(t.Context(), e.options("v0.9.0", "v0.9.5", g.client().Files("v0.9.5")))
	if err == nil || res.State != StateFailed || !strings.Contains(res.Message, "instead of v0.9.0") {
		t.Fatalf("Apply = %+v, %v", res, err)
	}
	if readFile(t, e.bin) != string(fakeBinary("v1.0.0")) || len(e.host.systemctlCalls()) != 0 || len(g.requested()) != 0 {
		t.Fatalf("binary %q, systemctl %v, requests %v", readFile(t, e.bin), e.host.systemctlCalls(), g.requested())
	}
}

// The rollback puts back the first copy of the run: a new version that was
// restarted before it recorded its version copies the database again,
// possibly after a first migration.
func TestApplyRollbackRestoresFirstCopy(t *testing.T) {
	useTestKey(t)
	g := newFakeGitHub(t)
	g.addRelease("v0.9.1", releaseFiles(fakeBinary("v0.9.1")))
	e := newInstallEnv(t, "v0.9.0")
	backups := filepath.Join(e.dataDir, "backups")
	db := filepath.Join(e.dataDir, "picache.db")
	e.host.healthy = func(running string) bool { return strings.Contains(running, "v0.9.0") }
	e.host.onStart = func(running string) {
		if !strings.Contains(running, "v0.9.1") {
			return
		}
		now := time.Now()
		first := filepath.Join(backups, "picache-v0.9.0-"+now.UTC().Format("20060102T150405")+".db")
		second := filepath.Join(backups, "picache-v0.9.0-"+now.Add(time.Second).UTC().Format("20060102T150405")+".db")
		writeFile(t, first, []byte("database of v0.9.0"))
		writeFile(t, second, []byte("half migrated by v0.9.1"))
		if err := os.Chtimes(second, now.Add(time.Second), now.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		writeFile(t, db, []byte("migrated by v0.9.1"))
	}
	res, err := Apply(t.Context(), e.options("v0.9.0", "v0.9.1", g.client().Files("v0.9.1")))
	if err == nil || res.State != StateRolledBack {
		t.Fatalf("Apply = %+v, %v", res, err)
	}
	if got := readFile(t, db); got != "database of v0.9.0" {
		t.Fatalf("database after rollback %q", got)
	}
}

// A service that cannot be stopped keeps its database: swapping it under a
// running process would mix the restored file with the running one's -wal.
func TestApplyRollbackKeepsDatabaseOfRunningService(t *testing.T) {
	useTestKey(t)
	g := newFakeGitHub(t)
	g.addRelease("v0.9.1", releaseFiles(fakeBinary("v0.9.1")))
	e := newInstallEnv(t, "v0.9.0")
	db := filepath.Join(e.dataDir, "picache.db")
	e.host.failCmd = "stop picache.service"
	e.host.healthy = func(running string) bool { return strings.Contains(running, "v0.9.0") }
	e.host.onStart = func(running string) {
		if strings.Contains(running, "v0.9.1") {
			writeFile(t, filepath.Join(e.dataDir, "backups", "picache-v0.9.0-"+time.Now().UTC().Format("20060102T150405")+".db"), []byte("database of v0.9.0"))
			writeFile(t, db, []byte("migrated by v0.9.1"))
		}
	}
	res, err := Apply(t.Context(), e.options("v0.9.0", "v0.9.1", g.client().Files("v0.9.1")))
	if err == nil || res.State != StateFailed || !strings.Contains(res.Message, "could not be stopped") {
		t.Fatalf("Apply = %+v, %v", res, err)
	}
	if got := readFile(t, db); got != "migrated by v0.9.1" {
		t.Fatalf("database replaced under a running service: %q", got)
	}
	if readFile(t, e.bin) != string(fakeBinary("v0.9.0")) {
		t.Error("the previous binary was not put back")
	}
}

// Root may use the blocks the file system reserves for it: a copy that is
// larger than what the service could still write (for example a sparse
// file the service made) is refused before anything is written.
func TestRestoreDatabaseNeedsFreeSpace(t *testing.T) {
	old := freeSpace
	freeSpace = func(string) (uint64, bool) { return 1 << 20, true }
	t.Cleanup(func() { freeSpace = old })
	data := t.TempDir()
	db := filepath.Join(data, configDBName)
	writeFile(t, db, []byte("migrated"))
	if err := os.MkdirAll(filepath.Join(data, backupsDirName), 0o750); err != nil {
		t.Fatal(err)
	}
	since := time.Now()
	writeFile(t, filepath.Join(data, backupsDirName, "picache-v1.0.0-20260925T101010.db"), make([]byte, 1<<20+1))
	if name, err := restoreDatabase(data, "v1.0.0", since, false); err == nil || !strings.Contains(err.Error(), "does not fit") || name != "" {
		t.Fatalf("restoreDatabase = %q, %v", name, err)
	}
	entries, _ := os.ReadDir(data)
	if readFile(t, db) != "migrated" || len(entries) != 2 {
		t.Fatalf("database %q, entries %v", readFile(t, db), entries)
	}
	freeSpace = func(string) (uint64, bool) { return 2 << 20, true }
	if name, err := restoreDatabase(data, "v1.0.0", since, false); err != nil || name == "" {
		t.Fatalf("copy that fits: %q, %v", name, err)
	}
}
