package storage

// The root helper's work on the host: mount units, credentials, mountpoints.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/hustenreizjuengling/picache/internal/config"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/secrets"
)

const (
	appliedSuffix = ".applied" // in the credentials dir: fingerprint of the mounted settings
	maxHostFile   = 16 << 10   // an existing unit, credentials or fingerprint file read back
)

// hostEnv is what the root helper touches on the host (tests use temp dirs
// and a fake systemctl).
type hostEnv struct {
	credDir string // /etc/picache/credentials
	unitDir string // /etc/systemd/system
	// strict enables what only a real root run can do: ownership checks of
	// the directories involved and chown of the mountpoint.
	strict          bool
	checkPrivileges func() error
	// systemctl runs systemctl with fixed arguments (no shell) and returns
	// its standard output; failures are *systemctlError.
	systemctl func(ctx context.Context, args ...string) (string, error)
	// diagnose collects why a unit job failed (systemd's result and the
	// unit's journal since the job started). Optional.
	diagnose     func(ctx context.Context, unit string, since time.Time) unitDiag
	serviceOwner func(dataDir string) (uid, gid int, err error) // the PiCache user
	// hasHelper reports whether a mount program (mount.cifs, mount.nfs) is
	// installed. nil skips the check (tests).
	hasHelper func(name string) bool
}

// helper is one root helper run.
type helper struct {
	env hostEnv
	cfg *config.Config
	db  *db.DB       // nil for RemoveHost
	box *secrets.Box // loaded on first use
	log *slog.Logger
}

func newHelper(env hostEnv, cfg *config.Config, log *slog.Logger) (*helper, error) {
	d, err := openConfigDB(cfg.Paths().ConfigDB)
	if err != nil {
		return nil, err
	}
	return &helper{env: env, cfg: cfg, db: d, log: componentLog(log)}, nil
}

func (h *helper) close() {
	if h.db != nil {
		h.db.Close()
	}
}

// openConfigDB opens picache.db read-only for the root helper. The data
// directory belongs to the unprivileged service, so the database must be a
// regular file (not a symbolic link, FIFO or device) before and after the
// open, and its schema is not trusted (trusted_schema=OFF: no SQL functions
// with side effects from views or triggers). SQLite itself opens the -wal
// and -shm files with O_NOFOLLOW.
func openConfigDB(path string) (*db.DB, error) {
	if strings.ContainsAny(path, "?#%") {
		return nil, fmt.Errorf("db: path %s must not contain '?', '#' or '%%'", path)
	}
	before, err := regularFile(path)
	if err != nil {
		return nil, err
	}
	r, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(5000)&_pragma=query_only(1)&_pragma=trusted_schema(0)")
	if err != nil {
		return nil, err
	}
	// One connection that stays open: the file checked below is the one used.
	r.SetMaxOpenConns(1)
	r.SetMaxIdleConns(1)
	r.SetConnMaxLifetime(0)
	if err := r.Ping(); err != nil {
		r.Close()
		return nil, fmt.Errorf("db: open %s read-only: %w", path, err)
	}
	after, err := regularFile(path)
	if err != nil || !os.SameFile(before, after) {
		r.Close()
		return nil, fmt.Errorf("db: %s changed while it was opened", path)
	}
	return &db.DB{R: r, Path: path}, nil
}

// regularFile returns the FileInfo of path if it is a regular file.
func regularFile(path string) (os.FileInfo, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("db: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("db: %s is not a regular file (symbolic links are not accepted)", path)
	}
	return fi, nil
}

// reconcile makes the host match the database for id: a host-apply target
// is mounted; a deleted target, or one in another mode now, has its host
// mount removed. gone reports a deleted target.
func (h *helper) reconcile(ctx context.Context, id string) (removed, gone bool, err error) {
	t, err := loadTarget(ctx, h.db.R, id)
	switch {
	case isNotFound(err):
		return true, true, h.remove(ctx, id)
	case err != nil:
		return false, false, err
	case t.Mode != ModeHostApply:
		return true, false, h.remove(ctx, id)
	}
	return false, false, h.applyTarget(ctx, t, nil)
}

// apply mounts one target (`picache storage apply`).
func (h *helper) apply(ctx context.Context, id string, stdinPassword []byte) error {
	t, err := loadTarget(ctx, h.db.R, id)
	if err != nil {
		return err
	}
	if t.Mode != ModeHostApply {
		return fmt.Errorf("storage target %s is not in host-apply mode", id)
	}
	return h.applyTarget(ctx, t, stdinPassword)
}

// applyTarget mounts t. Nothing from the database is trusted: the row is
// validated again and the mountpoint is computed from the id. The unit is
// restarted (remounted) when the mounted settings differ from the new ones;
// `enable --now` alone would keep an active mount unchanged.
func (h *helper) applyTarget(ctx context.Context, t Target, stdinPassword []byte) error {
	id := t.ID
	t.Path = hostApplyPath(h.cfg, id)
	if err := ValidateTarget(t, h.cfg); err != nil {
		return fmt.Errorf("refusing storage target %s: %w", id, err)
	}
	if err := h.checkMountHelper(t.Kind); err != nil {
		return err
	}
	uid, gid, err := h.env.serviceOwner(h.cfg.DataDir)
	if err != nil {
		return fmt.Errorf("cannot determine the PiCache user: %w", err)
	}
	if err := h.prepareMountpoint(t.Path, uid, gid); err != nil {
		return err
	}
	p := mountParams{uid: int64(uid), gid: int64(gid)}
	var cred []byte
	if t.Kind == KindSMB && t.Username != "" {
		pw, err := h.password(ctx, t, stdinPassword)
		if err != nil {
			return err
		}
		cred = credentialsFile(t, pw)
		clear(pw)
		p.credPath = filepath.Join(h.env.credDir, id+".cred")
	}
	defer clear(cred)
	unit := mountUnitName(t.Path)
	content := []byte(renderMountUnit(t, t.Path, p))
	if bytes.Contains(content, []byte("%")) {
		return errors.New("refusing to write a mount unit containing '%' (systemd specifier)")
	}
	fp := fingerprint(content, cred)

	cd, err := h.openCredDir()
	if err != nil {
		return err
	}
	defer cd.Close()
	credChanged, err := syncFile(cd, id+".cred", cred, 0o600)
	if err != nil {
		return fmt.Errorf("write credentials: %w", err)
	}
	applied, _, _ := readHostFile(cd, id+appliedSuffix)

	if h.env.strict {
		if err := checkRootOwnedChain(h.env.unitDir); err != nil {
			return err
		}
	}
	ud, err := openVerifiedDir(h.env.unitDir)
	if err != nil {
		return err
	}
	_, hadUnit, _ := readHostFile(ud, unit)
	unitChanged, err := syncFile(ud, unit, content, 0o644)
	ud.Close()
	if err != nil {
		return fmt.Errorf("write %s: %w", unit, err)
	}

	if _, err := h.env.systemctl(ctx, "daemon-reload"); err != nil {
		return err
	}
	if _, err := h.env.systemctl(ctx, "enable", unit); err != nil {
		return err
	}
	verb := "restart"
	switch {
	case string(applied) == fp:
		verb = "start" // the mounted settings are current; a no-op while mounted
	case applied == nil && (!hadUnit || !unitChanged && !credChanged):
		verb = "start" // first apply, or mounted by an older version with these files
	}
	// Apply is an explicit retry: systemd's start limit of the mount unit
	// (hit after a few quick failures) must not refuse it.
	_, _ = h.env.systemctl(ctx, "reset-failed", unit)
	since := time.Now()
	if _, err := h.env.systemctl(ctx, verb, unit); err != nil {
		if verb == "restart" {
			return h.unitFailure(ctx, "cannot remount "+t.Path+" with the new settings", unit, since, err,
				"If it is the active cache store, activate another storage target first, then apply again.")
		}
		return h.unitFailure(ctx, "mount failed", unit, since, err, "")
	}
	if err := writeFileAtomic(cd, id+appliedSuffix, []byte(fp), 0o600); err != nil {
		h.log.Warn("cannot record the mounted settings", slog.String("target", id), slog.Any("err", err))
	}
	h.log.Info("storage mount applied", slog.String("target", id), slog.String("unit", unit),
		slog.String("where", t.Path), slog.Bool("remounted", verb == "restart"))
	return nil
}

// remove disables, stops and deletes the .mount unit of MountRoot/<id>,
// deletes the credentials and removes the empty mountpoint. It only touches
// names derived from the id, never anything from the database.
func (h *helper) remove(ctx context.Context, id string) error {
	where := hostApplyPath(h.cfg, id)
	unit := mountUnitName(where)
	if h.env.strict {
		if err := checkRootOwnedChain(h.env.unitDir); err != nil {
			return err
		}
	}
	ud, err := openVerifiedDir(h.env.unitDir)
	if err != nil {
		return err
	}
	defer ud.Close()
	fi, err := ud.Lstat(unit)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// Never mounted by the helper (or already removed).
	case err != nil:
		return err
	case !fi.Mode().IsRegular():
		return fmt.Errorf("%s is not a regular file; remove it yourself", filepath.Join(h.env.unitDir, unit))
	default:
		if _, err := h.env.systemctl(ctx, "disable", unit); err != nil {
			return err
		}
		since := time.Now()
		if _, err := h.env.systemctl(ctx, "stop", unit); err != nil {
			// "not loaded" and similar are fine as long as nothing is mounted.
			state, _ := h.env.systemctl(ctx, "show", "-p", "ActiveState", "--value", unit)
			switch strings.TrimSpace(state) {
			case "inactive", "failed":
			default:
				return h.unitFailure(ctx, "cannot unmount "+where, unit, since, err,
					"If it is the active cache store, activate another storage target first.")
			}
		}
		if err := ud.Remove(unit); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if _, err := h.env.systemctl(ctx, "daemon-reload"); err != nil {
			return err
		}
		// A unit that failed to mount stays listed as failed until reset.
		_, _ = h.env.systemctl(ctx, "reset-failed", unit)
	}
	if err := h.removeCredentials(id); err != nil {
		return err
	}
	if err := h.removeMountpoint(where); err != nil {
		return err
	}
	h.log.Info("storage mount removed", slog.String("target", id), slog.String("unit", unit), slog.String("where", where))
	return nil
}

// prepareMountpoint creates MountRoot/<id>. In strict mode MountRoot and all
// its parents must be root-owned and not writable by others, so the
// unprivileged service cannot swap the mountpoint for a symbolic link.
func (h *helper) prepareMountpoint(where string, uid, gid int) error {
	root := filepath.Clean(h.cfg.MountRoot)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return mountRootError(root, err)
	}
	if h.env.strict {
		if err := checkRootOwnedChain(root); err != nil {
			return fmt.Errorf("unsafe PICACHE_MOUNT_ROOT: %w", err)
		}
	}
	fi, err := os.Lstat(where)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := os.Mkdir(where, 0o750); err != nil {
			return mountRootError(root, err)
		}
		if h.env.strict {
			return chownPath(where, uid, gid)
		}
	case err != nil:
		return err
	case !fi.IsDir():
		return fmt.Errorf("%s exists and is not a directory", where)
	}
	return nil
}

// mountRootError explains a read-only mount root: the helper's sandbox only
// allows writes below the paths in its unit.
func mountRootError(root string, err error) error {
	if errors.Is(err, syscall.EROFS) {
		return fmt.Errorf("%w. The root helper may only write below the mount root of its unit (/srv/picache): "+
			"after changing PICACHE_MOUNT_ROOT (%s) re-run install.sh --with-host-apply, which adds it with a drop-in", err, root)
	}
	return err
}

// removeMountpoint removes MountRoot/<id> if it is an empty directory
// (rmdir never removes files or an active mount point).
func (h *helper) removeMountpoint(where string) error {
	fi, err := os.Lstat(where)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return err
	case !fi.IsDir():
		return fmt.Errorf("%s is not a directory; remove it yourself", where)
	}
	if h.env.strict {
		if err := checkRootOwnedChain(filepath.Clean(h.cfg.MountRoot)); err != nil {
			return fmt.Errorf("unsafe PICACHE_MOUNT_ROOT: %w", err)
		}
	}
	if err := os.Remove(where); err != nil {
		h.log.Warn("the mountpoint was left in place", slog.String("path", where), slog.Any("err", err))
	}
	return nil
}

// password returns the NAS password: from stdin when given, else decrypted
// from the database with the master key (never generated here).
func (h *helper) password(ctx context.Context, t Target, stdin []byte) ([]byte, error) {
	if stdin != nil {
		if err := validatePasswordBytes(stdin); err != nil {
			return nil, err
		}
		return bytes.Clone(stdin), nil
	}
	sealed, err := loadSealedPassword(ctx, h.db.R, t.ID)
	if err != nil {
		return nil, err
	}
	if sealed == "" {
		h.log.Warn("no NAS password stored; using an empty password (use --password-stdin to enter one)", slog.String("target", t.ID))
		return nil, nil
	}
	if h.box == nil {
		if h.box, err = secrets.Load(h.cfg.Paths().MasterKeyFile); err != nil {
			// The helper runs in its own unit: a key that picache.service gets as a
			// systemd credential must be given to picache-storage.service too.
			return nil, fmt.Errorf("cannot decrypt the NAS password: no master key for the root helper. If PiCache gets it as a "+
				"systemd credential, add the same LoadCredentialEncrypted= line to picache-storage.service, or run: "+
				"sudo picache storage apply %s --password-stdin (%w)", t.ID, err)
		}
	}
	pw, err := h.box.Open(sealed, passwordAAD(t.ID))
	if err != nil {
		return nil, err
	}
	if err := validatePasswordBytes(pw); err != nil {
		clear(pw)
		return nil, err
	}
	return pw, nil
}

// checkMountHelper refuses a share whose mount program is missing. Without
// it the kernel mounts on its own and fails with misleading messages (NFS:
// "Server address does not match proto= option").
func (h *helper) checkMountHelper(kind Kind) error {
	if h.env.hasHelper == nil {
		return nil
	}
	switch {
	case kind == KindSMB && !h.env.hasHelper("mount.cifs"):
		return errors.New("mount.cifs is not installed on this machine; install it with: sudo apt install cifs-utils")
	case kind == KindNFS && !h.env.hasHelper("mount.nfs"):
		return errors.New("mount.nfs is not installed on this machine; install it with: sudo apt install nfs-common " +
			"(NFS 4 does not need the rpcbind service that comes with it: sudo systemctl mask --now rpcbind.service rpcbind.socket)")
	}
	return nil
}

// credentialsFile renders the mount.cifs credentials file (the caller clears it).
func credentialsFile(t Target, pw []byte) []byte {
	buf := make([]byte, 0, 96+len(pw))
	buf = append(buf, "username="+t.Username+"\npassword="...)
	buf = append(buf, pw...)
	buf = append(buf, '\n')
	if t.Domain != "" {
		buf = append(buf, "domain="+t.Domain+"\n"...)
	}
	return buf
}

// openCredDir opens the root-owned 0700 credentials directory, creating it.
func (h *helper) openCredDir() (*os.Root, error) {
	dir := h.env.credDir
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return nil, err
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, err
	}
	if h.env.strict {
		if err := checkRootOwnedChain(dir); err != nil {
			return nil, err
		}
	}
	r, err := openVerifiedDir(dir)
	if err != nil {
		return nil, err
	}
	if err := r.Chmod(".", 0o700); err != nil {
		r.Close()
		return nil, err
	}
	return r, nil
}

// removeCredentials deletes the credentials file and the fingerprint of id.
func (h *helper) removeCredentials(id string) error {
	r, err := openVerifiedDir(h.env.credDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer r.Close()
	for _, n := range []string{id + ".cred", id + appliedSuffix} {
		if err := r.Remove(n); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// fingerprint identifies the settings of a mount: unit and credentials.
func fingerprint(unit, cred []byte) string {
	s := sha256.New()
	s.Write(unit)
	s.Write([]byte{0})
	s.Write(cred)
	return hex.EncodeToString(s.Sum(nil))
}

// readHostFile reads name in r without following a link (nil if missing, not a
// regular file or too large; exists reports whether anything is there).
func readHostFile(r *os.Root, name string) (data []byte, exists bool, err error) {
	fi, err := r.Lstat(name)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, false, nil
	case err != nil:
		return nil, true, err
	case !fi.Mode().IsRegular() || fi.Size() > maxHostFile:
		return nil, true, nil
	}
	f, err := r.OpenFile(name, os.O_RDONLY|oNoFollow, 0)
	if err != nil {
		return nil, true, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxHostFile+1))
	if err != nil || len(b) > maxHostFile {
		clear(b)
		return nil, true, err
	}
	return b, true, nil
}

// syncFile makes name in r hold data (nil = no file) and reports whether
// anything changed. Unchanged files are not rewritten.
func syncFile(r *os.Root, name string, data []byte, perm fs.FileMode) (bool, error) {
	old, exists, _ := readHostFile(r, name)
	defer clear(old)
	if data == nil {
		if !exists {
			return false, nil
		}
		if err := r.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return true, err
		}
		return true, nil
	}
	if old != nil && bytes.Equal(old, data) {
		return false, nil
	}
	return true, writeFileAtomic(r, name, data, perm)
}

// systemctlError is a failed systemctl call.
type systemctlError struct {
	args   []string
	err    error
	output string // systemctl's own messages, without enable/disable noise
}

func (e *systemctlError) Error() string {
	s := "systemctl " + strings.Join(e.args, " ") + ": " + e.err.Error()
	if e.output != "" {
		s += ": " + e.output
	}
	return s
}

func (e *systemctlError) Unwrap() error { return e.err }

// systemctlOutput keeps what explains a failure: no "Created symlink" or
// "Removed" lines, no "See systemctl status ..." pointers.
func systemctlOutput(s string) string {
	var keep []string
	for l := range strings.Lines(s) {
		l = strings.TrimSpace(l)
		if i := strings.Index(l, `See "systemctl status`); i >= 0 {
			l = strings.TrimSpace(l[:i])
		}
		if l == "" || strings.HasPrefix(l, "Created symlink") || strings.HasPrefix(l, "Removed ") {
			continue
		}
		keep = append(keep, l)
	}
	return strings.Join(keep, " ")
}

// unitDiag is what systemd knows about a failed unit job.
type unitDiag struct {
	result string   // `systemctl show -p Result` (exit-code, timeout, …)
	lines  []string // the unit's journal since the job started (message text)
}

// hostError is an error with a message for the admin that keeps its cause.
type hostError struct {
	msg string
	err error
}

func (e *hostError) Error() string { return e.msg }
func (e *hostError) Unwrap() error { return e.err }

// unitFailure explains a failed unit job with the unit's own messages.
func (h *helper) unitFailure(ctx context.Context, what, unit string, since time.Time, err error, hint string) error {
	var d unitDiag
	if h.env.diagnose != nil {
		d = h.env.diagnose(ctx, unit, since)
	}
	return describeFailure(what, err, d, hint)
}

// describeFailure puts the cause first (the UI shows about 300 characters):
// the mount helper's messages from the journal, systemd's result, a hint;
// systemctl's own words only when the journal has nothing.
func describeFailure(what string, err error, d unitDiag, hint string) error {
	var b strings.Builder
	b.WriteString(what)
	cause := relevantJournalLines(d.lines)
	if len(cause) > 0 {
		b.WriteString(": " + strings.Join(cause, "; "))
	} else {
		msg := err.Error()
		var se *systemctlError
		if errors.As(err, &se) && se.output != "" {
			msg = se.output
		}
		b.WriteString(": " + msg)
	}
	if r := strings.TrimSpace(d.result); r != "" && r != "success" {
		b.WriteString(" (systemd result: " + r + ")")
	}
	if hint != "" {
		b.WriteString(". " + hint)
	}
	return &hostError{msg: b.String(), err: err}
}

// Journal lines systemd writes for every mount job; they never say why.
var (
	journalNoisePrefix = []string{"Failed to mount ", "Failed unmounting ", "Failed to unmount ", "Mounted ", "Unmounted ",
		"Refer to the mount.cifs(8) manual page", "Consumed "}
	journalNoiseContains = []string{"Mount process exited", "Unmount process exited", "Failed with result",
		"Deactivated successfully", "Unit entered failed state"}
)

// relevantJournalLines drops systemd's generic lines (and its "<unit>: "
// prefix) and keeps the last three messages, such as
// "mount error(115): could not connect to 192.0.2.12".
func relevantJournalLines(lines []string) []string {
	var out []string
next:
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if i := strings.Index(l, ".mount: "); i > 0 && !strings.Contains(l[:i], " ") {
			l = strings.TrimSpace(l[i+len(".mount: "):])
		}
		if l == "" || (strings.HasPrefix(l, "Mounting ") || strings.HasPrefix(l, "Unmounting ")) && strings.HasSuffix(l, "...") ||
			slices.Contains(out, l) {
			continue
		}
		for _, p := range journalNoisePrefix {
			if strings.HasPrefix(l, p) {
				continue next
			}
		}
		for _, c := range journalNoiseContains {
			if strings.Contains(l, c) {
				continue next
			}
		}
		out = append(out, l)
	}
	if len(out) > 3 {
		out = out[len(out)-3:]
	}
	return out
}

// cappedBuffer keeps the first max bytes written to it (command output).
type cappedBuffer struct {
	buf bytes.Buffer
	max int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); room > 0 {
		c.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (c *cappedBuffer) String() string { return c.buf.String() }
