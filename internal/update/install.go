package update

// The install procedure (docs/ARCHITECTURE.md 14.4), shared by `sudo
// picache update` and the root helper. Nothing is trusted before the
// signature of SHA256SUMS was verified; the binary is written next to the
// installed one and only renamed over it after its SHA-256 and its version
// were checked, so a failed update never leaves a half-written binary.

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"
)

const (
	downloadTimeout = 15 * time.Minute // SHA256SUMS, signature and binary together
	versionTimeout  = 30 * time.Second // `<new binary> version`
	probeTimeout    = 10 * time.Second // one health probe
	healthPasses    = 2                // consecutive passing probes that count as healthy
	maxInstalled    = 512 << 20        // the installed binary copied to picache.prev
	maxBackupScan   = 1024             // entries of <data>/backups looked at
	configDBName    = "picache.db"
	backupsDirName  = "backups"
)

// DefaultHealthTimeout is how long Apply waits for the restarted service.
const DefaultHealthTimeout = 90 * time.Second

// Options configure Apply.
type Options struct {
	// Version is the target version. It may be empty only with DirFiles:
	// then the version the verified binary reports is installed.
	Version        string
	Files          Files // where SHA256SUMS, SHA256SUMS.sig and the binary come from
	AllowDowngrade bool  // allow an older (or the same) version

	BinPath string // the installed binary; its directory gets .picache.update and picache.prev
	DataDir string // PICACHE_DATA_DIR: update lock and pre-upgrade database copies
	Current string // version of the installed binary
	Arch    string // "": runtime.GOARCH

	// Confirm, if set, is called when the target version is known and the
	// binary verified, before anything is changed; an error aborts.
	Confirm func(version string) error
	// Progress reports each step (status.json, CLI output).
	Progress func(step, message string)
	Log      *slog.Logger

	Host Host
}

// Host is what Apply does on the machine. SystemHost returns the real one;
// tests use fakes.
type Host struct {
	// CheckPrivileges fails unless this process may replace the binary.
	CheckPrivileges func() error
	// ServiceBinary returns the binary picache.service runs (optional): an
	// update must replace that one, not some other copy.
	ServiceBinary func(ctx context.Context) (string, error)
	// Systemctl runs systemctl with fixed arguments.
	Systemctl func(ctx context.Context, args ...string) error
	// Health runs the checks of `picache healthcheck` once.
	Health func(ctx context.Context) error
	// RunVersion returns the output of `<path> version` (nil: run it).
	RunVersion func(ctx context.Context, path string) (string, error)
	// Lock serialises updates of the CLI and the helper (nil: none).
	Lock func(dataDir string) (unlock func(), err error)
	// Strict enables what only a real root run can do: the directory of the
	// binary and all its parents must be owned by root and not writable by
	// others, and a restored database gets the owner of the file it replaces.
	Strict         bool
	HealthTimeout  time.Duration // 0: DefaultHealthTimeout
	HealthInterval time.Duration // 0: 2 s
}

// Result is the outcome of Apply.
type Result struct {
	State   string // StateSucceeded, StateFailed or StateRolledBack
	Version string // installed (or attempted) version
	From    string // version before the update
	Message string
}

// Apply verifies and installs a release, restarts PiCache and rolls back if
// the new version does not become healthy (docs/ARCHITECTURE.md 14.4). The
// error is nil only for StateSucceeded.
func Apply(ctx context.Context, o Options) (Result, error) {
	res := Result{State: StateFailed, Version: o.Version, From: o.Current}
	a, err := newApplier(o)
	if err == nil {
		res, err = a.run(ctx, res)
	}
	if err != nil {
		res.Message = sanitizeMessage(err.Error())
	}
	return res, err
}

type applier struct {
	o      Options
	h      Host
	log    *slog.Logger
	bin    string // installed binary
	binDir string
	staged string // <bindir>/.picache.update
	prev   string // <bindir>/picache.prev
	asset  string // picache-linux-<arch>
}

func newApplier(o Options) (*applier, error) {
	if o.BinPath == "" || o.DataDir == "" || o.Files == nil || o.Host.Systemctl == nil || o.Host.Health == nil {
		return nil, errors.New("update: incomplete options")
	}
	asset, ok := AssetName(cmp.Or(o.Arch, runtime.GOARCH))
	if !ok {
		return nil, fmt.Errorf("no release binaries are built for linux/%s", cmp.Or(o.Arch, runtime.GOARCH))
	}
	h := o.Host
	if h.CheckPrivileges == nil {
		h.CheckPrivileges = func() error { return nil }
	}
	if h.RunVersion == nil {
		h.RunVersion = runVersion
	}
	if h.Lock == nil {
		h.Lock = func(string) (func(), error) { return func() {}, nil }
	}
	h.HealthTimeout = cmp.Or(h.HealthTimeout, DefaultHealthTimeout)
	h.HealthInterval = cmp.Or(h.HealthInterval, 2*time.Second)
	log := o.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	bin := filepath.Clean(o.BinPath)
	dir, base := filepath.Split(bin)
	dir = filepath.Clean(dir)
	return &applier{o: o, h: h, log: log.With(slog.String("component", "update")), bin: bin, binDir: dir,
		staged: filepath.Join(dir, "."+base+".update"), prev: filepath.Join(dir, base+".prev"), asset: asset}, nil
}

func (a *applier) progress(step, msg string) {
	a.log.Info(msg, slog.String("step", step))
	if a.o.Progress != nil {
		a.o.Progress(step, msg)
	}
}

func (a *applier) run(ctx context.Context, res Result) (Result, error) {
	if err := a.h.CheckPrivileges(); err != nil {
		return res, err
	}
	if a.o.Version != "" {
		v, err := ParseVersion(a.o.Version)
		if err != nil {
			return res, err
		}
		if err := checkNewer(a.o.Current, v, a.o.AllowDowngrade); err != nil {
			return res, err
		}
	}
	if err := a.checkTarget(ctx); err != nil {
		return res, err
	}
	unlock, err := a.h.Lock(a.o.DataDir)
	if err != nil {
		return res, err
	}
	defer unlock()
	if err := a.checkInstalled(ctx); err != nil {
		return res, err
	}
	// The staged binary is gone after a successful rename; on every other
	// path it is removed here.
	defer os.Remove(a.staged)

	// Steps 1-3: signature, then the binary.
	dctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	want, err := a.verifiedSum(dctx)
	if err != nil {
		return res, err
	}
	if err := a.download(dctx, want); err != nil {
		return res, err
	}
	// Step 4: the binary must be the version that was asked for.
	out, err := a.h.RunVersion(ctx, a.staged)
	if err != nil {
		return res, fmt.Errorf("the new binary does not run on this machine: %w", err)
	}
	if a.o.Version == "" {
		v, err := versionFromOutput(out)
		if err != nil {
			return res, err
		}
		if err := checkNewer(a.o.Current, v, a.o.AllowDowngrade); err != nil {
			return res, err
		}
		res.Version = v.String()
	} else if !strings.HasPrefix(out, "picache "+a.o.Version+" ") {
		return res, fmt.Errorf("the new binary reports %q instead of version %s", clip(firstLine(out), 80), a.o.Version)
	}
	if a.o.Confirm != nil {
		if err := a.o.Confirm(res.Version); err != nil {
			return res, err
		}
	}
	// Step 5: keep the old binary, then replace it atomically.
	a.progress(StepInstall, "installing "+res.Version+" as "+a.bin+" (previous version kept as "+a.prev+")")
	if err := a.install(); err != nil {
		return res, err
	}
	// Step 6: restart and wait for the health probe.
	restartAt := time.Now()
	a.progress(StepRestart, "restarting "+Service)
	err = a.h.Systemctl(ctx, "restart", Service)
	if err == nil {
		a.progress(StepHealth, "waiting for PiCache "+res.Version+" to become healthy")
		err = a.waitHealthy(ctx)
	}
	if err == nil {
		res.State, res.Message = StateSucceeded, "PiCache "+res.Version+" is running"
		a.progress(StepDone, res.Message)
		return res, nil
	}
	// Step 7: roll back, even if ctx was cancelled (Ctrl-C in the CLI).
	return a.rollback(context.WithoutCancel(ctx), res, restartAt, err)
}

// checkTarget makes sure the binary to replace is the one picache.service
// runs and that nobody but root can change files in its directory.
func (a *applier) checkTarget(ctx context.Context) error {
	if a.h.ServiceBinary != nil {
		svc, err := a.h.ServiceBinary(ctx)
		if err != nil {
			return err
		}
		if resolved, err := filepath.EvalSymlinks(svc); err == nil {
			svc = resolved
		}
		// Compare resolved paths on both sides (a symlinked directory,
		// macOS /var → /private/var, Windows 8.3 short names).
		bin := a.bin
		if resolved, err := filepath.EvalSymlinks(bin); err == nil {
			bin = resolved
		}
		if filepath.Clean(svc) != filepath.Clean(bin) {
			return fmt.Errorf("%s runs %s, not %s; run `sudo %s update` instead", Service, svc, a.bin, svc)
		}
	}
	if a.h.Strict {
		return checkRootOwnedChain(a.binDir)
	}
	return nil
}

// checkInstalled runs with the lock held and makes sure the installed
// binary is still the version this run compares against (o.Current, the
// version of this process). Another update may have replaced it after this
// process started, for example the CLI while the helper was looking up its
// release: the newer-than check would then be against the wrong version (a
// downgrade) and a rollback would look for the wrong database copy.
func (a *applier) checkInstalled(ctx context.Context) error {
	out, err := a.h.RunVersion(ctx, a.bin)
	if err != nil {
		return fmt.Errorf("run the installed binary %s: %w", a.bin, err)
	}
	if !strings.HasPrefix(out, "picache "+a.o.Current+" ") {
		return fmt.Errorf("%s reports %q instead of %s: it was replaced while this update started (another update?); run the update again",
			a.bin, clip(firstLine(out), 80), a.o.Current)
	}
	return nil
}

// verifiedSum downloads SHA256SUMS and its signature, verifies the
// signature and returns the SHA-256 of the binary for this architecture.
func (a *applier) verifiedSum(ctx context.Context) ([32]byte, error) {
	a.progress(StepDownload, "downloading "+SumsFile+" and "+SigFile+" from "+a.o.Files.Describe())
	sums, err := readLimited(ctx, a.o.Files, SumsFile, maxSumsSize)
	if err != nil {
		return [32]byte{}, err
	}
	sig, err := readLimited(ctx, a.o.Files, SigFile, maxSigSize)
	if err != nil {
		return [32]byte{}, err
	}
	a.progress(StepVerify, "verifying the signature of "+SumsFile)
	if err := verifySignature(sums, sig, trustedKeys); err != nil {
		return [32]byte{}, err
	}
	table, err := parseSums(sums)
	if err != nil {
		return [32]byte{}, err
	}
	want, ok := table[a.asset]
	if !ok {
		return [32]byte{}, fmt.Errorf("%s has no entry for %s", SumsFile, a.asset)
	}
	return want, nil
}

// download writes the binary to the staging file (mode 0700) and checks
// its SHA-256.
func (a *applier) download(ctx context.Context, want [32]byte) error {
	a.progress(StepDownload, "downloading "+a.asset)
	// A leftover of an interrupted run: nobody else uses it (lock held).
	if err := os.Remove(a.staged); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	rc, size, err := a.o.Files.Open(ctx, a.asset)
	if err != nil {
		return err
	}
	defer rc.Close()
	if size > maxBinarySize {
		return fmt.Errorf("%s is larger than 256 MiB", a.asset)
	}
	f, err := os.OpenFile(a.staged, os.O_WRONLY|os.O_CREATE|os.O_EXCL|oNoFollow, 0o700)
	if err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(rc, maxBinarySize+1))
	if err == nil && n > maxBinarySize {
		err = fmt.Errorf("%s is larger than 256 MiB", a.asset)
	}
	if err == nil {
		err = f.Chmod(0o700) // independent of the umask
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("download %s: %w", a.asset, err)
	}
	a.progress(StepVerify, "checking the SHA-256 of "+a.asset)
	if !bytes.Equal(h.Sum(nil), want[:]) {
		return fmt.Errorf("%s does not match %s (the download is corrupt or was tampered with)", a.asset, SumsFile)
	}
	return nil
}

// install copies the installed binary to picache.prev and renames the
// staged one over it.
func (a *applier) install() error {
	if err := copyFile(a.bin, a.prev); err != nil {
		return fmt.Errorf("keep the installed binary as %s: %w", a.prev, err)
	}
	if err := os.Chmod(a.staged, 0o755); err != nil {
		return err
	}
	if err := os.Rename(a.staged, a.bin); err != nil {
		return err
	}
	syncDir(a.binDir)
	return nil
}

// waitHealthy waits until the health probe passes twice in a row (a binary
// that crashes right after its start must not count), at most HealthTimeout.
func (a *applier) waitHealthy(ctx context.Context) error {
	wctx, cancel := context.WithTimeout(ctx, a.h.HealthTimeout)
	defer cancel()
	last := errors.New("no answer")
	passed := 0
	t := time.NewTicker(a.h.HealthInterval)
	defer t.Stop()
	for {
		select {
		case <-wctx.Done():
			if ctx.Err() != nil {
				return errors.New("interrupted while waiting for the health check")
			}
			return fmt.Errorf("not healthy after %s: %w", a.h.HealthTimeout, last)
		case <-t.C:
		}
		pctx, pcancel := context.WithTimeout(wctx, probeTimeout)
		err := a.h.Health(pctx)
		pcancel()
		if err != nil {
			passed, last = 0, err
			continue
		}
		if passed++; passed >= healthPasses {
			return nil
		}
	}
}

// rollback puts the previous binary back and, with the service stopped,
// the database copy the new version made at its start, then starts the
// previous version again.
func (a *applier) rollback(ctx context.Context, res Result, since time.Time, cause error) (Result, error) {
	old := a.o.Current
	a.progress(StepRollback, fmt.Sprintf("%s did not become healthy (%v); rolling back to %s", res.Version, cause, old))
	var errs []error
	stopped := true
	if err := a.h.Systemctl(ctx, "stop", Service); err != nil {
		errs = append(errs, fmt.Errorf("stop %s: %w", Service, err))
		stopped = false
	}
	note := ""
	if err := os.Rename(a.prev, a.bin); err != nil {
		errs = append(errs, fmt.Errorf("put %s back: %w", a.prev, err))
	} else if syncDir(a.binDir); !stopped {
		// The database is only swapped under a stopped service: a running
		// one keeps writing to the replaced file and its -wal.
		note = "; the database was not restored because PiCache could not be stopped"
	} else {
		name, err := restoreDatabase(a.o.DataDir, old, since, a.h.Strict)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("restore the pre-upgrade database copy: %w", err))
		case name != "":
			note = "; the pre-upgrade database copy " + name + " was restored"
		default:
			note = "; the new version made no database copy, so the database was not changed"
		}
	}
	if err := a.h.Systemctl(ctx, "start", Service); err != nil {
		errs = append(errs, fmt.Errorf("start %s: %w", Service, err))
	} else if err := a.waitHealthy(ctx); err != nil {
		errs = append(errs, fmt.Errorf("%s is not healthy either: %w", old, err))
	}
	msg := fmt.Sprintf("%s did not become healthy (%v); rolled back to %s%s", res.Version, cause, old, note)
	if len(errs) > 0 {
		res.State = StateFailed
		return res, fmt.Errorf("%s; the rollback failed: %w", msg, errors.Join(errs...))
	}
	res.State = StateRolledBack
	a.progress(StepRollback, msg)
	return res, errors.New(msg)
}

var execStartPathRE = regexp.MustCompile(`(?:^|[{;]\s*)path=(/[^\s;]+)`)

// execStartPath extracts the executable from `systemctl show
// --property=ExecStart --value` ("{ path=/usr/local/bin/picache ; argv[]=…
// }"); the output is empty when the unit does not exist.
func execStartPath(out string) (string, error) {
	m := execStartPathRE.FindStringSubmatch(out)
	if m == nil {
		return "", fmt.Errorf("%s is not installed (see docs/DEPLOYMENT.md; in Docker pull the new image instead)", Service)
	}
	return m[1], nil
}

var versionOutputRE = regexp.MustCompile(`^picache (\S+) `)

// versionFromOutput reads the version from `picache version` output.
func versionFromOutput(out string) (Version, error) {
	m := versionOutputRE.FindStringSubmatch(out)
	if m == nil {
		return Version{}, fmt.Errorf("the new binary does not report a PiCache version (%q)", clip(firstLine(out), 80))
	}
	r := ParseRunning(m[1])
	if r.Dev || r.Base == nil {
		return Version{}, fmt.Errorf("the new binary is not a release build (it reports %q)", clip(m[1], 64))
	}
	return *r.Base, nil
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	return strings.ToValidUTF8(s, "?")
}

// helperEnv is the fixed, minimal environment of the commands Apply runs.
var helperEnv = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C", "SYSTEMD_PAGER=", "SYSTEMD_COLORS=0"}

// runVersion runs `<path> version` with a fixed environment and a timeout.
func runVersion(ctx context.Context, path string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, versionTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "version")
	cmd.Env = helperEnv
	out := &cappedBuffer{max: 4 << 10}
	cmd.Stdout = out
	err := cmd.Run()
	return out.String(), err
}

// cappedBuffer keeps the first max bytes written to it.
type cappedBuffer struct {
	buf bytes.Buffer
	max int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (b *cappedBuffer) String() string { return b.buf.String() }

// copyFile copies src to dst through a temporary file in dst's directory
// (mode 0755, synced, then renamed).
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() || fi.Size() > maxInstalled {
		return fmt.Errorf("%s is not a regular file of at most 512 MiB", src)
	}
	tmp := filepath.Join(filepath.Dir(dst), "."+filepath.Base(dst)+".tmp-"+randHex())
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|oNoFollow, 0o700)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, io.LimitReader(in, maxInstalled))
	if err == nil {
		err = out.Chmod(0o755)
	}
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, dst)
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}

func randHex() string {
	var b [6]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// openVerifiedDir opens dir as an os.Root after checking that it is a real
// directory (not a symbolic link) and was not swapped while opening. The
// data directory belongs to the unprivileged service; everything root does
// in it goes through the returned root.
func openVerifiedDir(dir string) (*os.Root, error) {
	fi, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("%s is not a directory (symbolic links are not accepted)", dir)
	}
	r, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	rfi, err := r.Stat(".")
	if err != nil || !os.SameFile(fi, rfi) {
		r.Close()
		return nil, fmt.Errorf("%s changed while it was opened", dir)
	}
	return r, nil
}

// freeSpace is userFreeBytes (tests replace it).
var freeSpace = userFreeBytes

// backupStampRE is the timestamp part of a pre-upgrade copy's name.
var backupStampRE = regexp.MustCompile(`^\d{8}T\d{6}\.db$`)

// restoreDatabase puts back the pre-upgrade copy of picache.db that the new
// version made at its start (<data>/backups/picache-<old>-<timestamp>.db,
// not older than since). The service must be stopped: its -wal and -shm
// files are removed first so they are never applied to the copy. The
// restored file keeps the owner and mode of the file it replaces. It
// returns the copy's name, "" if there is none.
func restoreDatabase(dataDir, oldVersion string, since time.Time, strict bool) (string, error) {
	r, err := openVerifiedDir(dataDir)
	if err != nil {
		return "", err
	}
	defer r.Close()
	name, err := findDatabaseCopy(r, oldVersion, since)
	if err != nil || name == "" {
		return "", err
	}
	src, err := r.OpenFile(filepath.Join(backupsDirName, name), os.O_RDONLY|oNoFollow, 0)
	if err != nil {
		return "", err
	}
	defer src.Close()
	fi, err := src.Stat()
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", name)
	}
	if n, ok := linkCount(fi); ok && n != 1 {
		// A hard link the service made to someone else's file: root would
		// copy that file into the service's database.
		return "", fmt.Errorf("%s has %d links; a database copy has one", name, n)
	}
	if free, ok := freeSpace(dataDir); ok && uint64(fi.Size()) > free {
		// Root may fill the blocks reserved for it; the copy must fit in
		// what the service itself could still write.
		return "", fmt.Errorf("%s (%d MiB) does not fit into the free space of %s", name, fi.Size()>>20, dataDir)
	}
	mode, uid, gid := fs.FileMode(0o640), -1, -1
	if cur, err := r.Lstat(configDBName); err == nil && cur.Mode().IsRegular() {
		mode = cur.Mode().Perm()
		uid, gid, _ = fileOwner(cur)
	} else if d, err := r.Stat("."); err == nil {
		uid, gid, _ = fileOwner(d)
	}
	tmp := "." + configDBName + ".rollback-" + randHex()
	dst, err := r.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|oNoFollow, 0o600)
	if err != nil {
		return "", err
	}
	defer r.Remove(tmp) // after the rename there is nothing left to remove
	n, err := io.Copy(dst, io.LimitReader(src, fi.Size()+1))
	if err == nil && n != fi.Size() {
		err = fmt.Errorf("%s changed while it was copied", name)
	}
	if err == nil && strict && uid >= 0 {
		err = fchown(dst, uid, gid)
	}
	if err == nil {
		err = dst.Chmod(mode)
	}
	if err == nil {
		err = dst.Sync()
	}
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	for _, sfx := range []string{"-wal", "-shm"} {
		if err := r.Remove(configDBName + sfx); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
	}
	if err := r.Rename(tmp, configDBName); err != nil {
		return "", err
	}
	syncDir(dataDir)
	return name, nil
}

// findDatabaseCopy returns the oldest regular file (with a single link)
// backups/picache-<old>-<timestamp>.db modified at or after since (with two
// seconds of slack for coarse file system timestamps). The oldest one of the
// run is the database as the old version left it: a new version that was
// restarted before it recorded its version copies again, possibly after a
// first migration.
func findDatabaseCopy(r *os.Root, oldVersion string, since time.Time) (string, error) {
	d, err := r.Open(backupsDirName)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer d.Close()
	names, err := d.Readdirnames(maxBackupScan)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	prefix := "picache-" + sanitizeFile(oldVersion) + "-"
	slices.Sort(names)
	best, bestMod := "", time.Time{}
	for _, n := range names {
		stamp, ok := strings.CutPrefix(n, prefix)
		if !ok || !backupStampRE.MatchString(stamp) {
			continue
		}
		fi, err := r.Lstat(filepath.Join(backupsDirName, n))
		if err != nil || !fi.Mode().IsRegular() || fi.ModTime().Before(since.Add(-2*time.Second)) {
			continue
		}
		if links, ok := linkCount(fi); ok && links != 1 {
			continue
		}
		if best == "" || fi.ModTime().Before(bestMod) {
			best, bestMod = n, fi.ModTime()
		}
	}
	return best, nil
}

// sanitizeFile maps a version to the form the service uses in backup file
// names (internal/app preUpgradeBackup).
func sanitizeFile(s string) string {
	b := []byte(s)
	for i, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '-' || c == '_') {
			b[i] = '_'
		}
	}
	return string(b)
}
