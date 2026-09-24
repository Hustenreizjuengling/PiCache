package storage

// Host-apply: the service only queues <data>/storage-requests/<id>. The root
// helper (`picache storage apply-pending`, started by picache-storage.path)
// or the admin (`sudo picache storage apply <id>`) re-validates the target,
// writes the credentials file and a systemd .mount unit, starts it and
// leaves <id>.result for the UI. The requests directory is writable by the
// unprivileged service, so the root side never follows links in it and
// never trusts anything but the file name.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/config"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/secrets"
)

const (
	requestsDirName = "storage-requests"
	resultSuffix    = ".result"
	maxResultSize   = 4 << 10 // bytes of a result file
	maxPending      = 64      // requests handled per helper run
	maxMessage      = 300     // runes of a result message
	applyQueued     = "queued"
	applyApplied    = "applied"
)

// applyRequest is the (informational) content of a request file.
type applyRequest struct {
	ID          string    `json:"id"`
	RequestedAt time.Time `json:"requestedAt"`
}

// applyResult is written by the root helper for the UI.
type applyResult struct {
	OK      bool      `json:"ok"`
	Message string    `json:"message,omitempty"`
	Time    time.Time `json:"time"`
}

func requestsDir(cfg *config.Config) string { return filepath.Join(cfg.DataDir, requestsDirName) }

// RequestApply queues a host-apply request for the root helper (writes
// <data>/storage-requests/<id>); apperr.Unavailable if the helper is not installed.
func (m *Manager) RequestApply(ctx context.Context, id string) (Status, error) {
	t, ok := m.get(id)
	if !ok {
		return Status{}, apperr.NotFound("storage target", id)
	}
	if t.Mode != ModeHostApply {
		return Status{}, apperr.Conflict("the storage target %q is not in host-apply mode", t.Name)
	}
	if err := ValidateTarget(t, m.cfg); err != nil {
		return Status{}, err
	}
	if !fileExists(m.hostApplyFlag) {
		return Status{}, apperr.Unavailable("the root helper is not installed (%s is missing); run 'sudo picache storage apply %s' on the host instead",
			m.hostApplyFlag, id)
	}
	if err := writeRequest(requestsDir(m.cfg), id); err != nil {
		return Status{}, fmt.Errorf("storage: queue apply request: %w", err)
	}
	m.mu.Lock()
	var st Status
	if e := m.targets[id]; e != nil {
		e.st.ApplyState = applyQueued
		st = e.st
	}
	m.mu.Unlock()
	m.log.Info("host-apply requested", slog.String("target", id))
	return st, nil
}

// writeRequest replaces any old result with a new request file.
func writeRequest(dir, id string) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	r, err := openVerifiedDir(dir)
	if err != nil {
		return err
	}
	defer r.Close()
	if err := r.Remove(id + resultSuffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	b, err := json.Marshal(applyRequest{ID: id, RequestedAt: time.Now().UTC()})
	if err != nil {
		return err
	}
	return writeFileAtomic(r, id, b, 0o640)
}

// readApplyState returns queued, applied, "failed: <msg>" or "".
func readApplyState(dir, id string) string {
	r, err := os.OpenRoot(dir)
	if err != nil {
		return ""
	}
	defer r.Close()
	if _, err := r.Lstat(id); err == nil {
		return applyQueued
	}
	f, err := r.Open(id + resultSuffix)
	if err != nil {
		return ""
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxResultSize+1))
	var res applyResult
	if err != nil || len(b) > maxResultSize || json.Unmarshal(b, &res) != nil {
		return "failed: unreadable result file"
	}
	if res.OK {
		return applyApplied
	}
	return "failed: " + sanitizeMessage(res.Message)
}

// removeRequestFiles deletes the request and result of a deleted target.
func removeRequestFiles(dir, id string) {
	r, err := os.OpenRoot(dir)
	if err != nil {
		return
	}
	defer r.Close()
	_ = r.Remove(id)
	_ = r.Remove(id + resultSuffix)
}

// sanitizeMessage makes a message single-line, printable and short.
func sanitizeMessage(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == unicode.ReplacementChar {
			return ' '
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > maxMessage {
		s = string(r[:maxMessage]) + "…"
	}
	if s == "" {
		s = "unknown error"
	}
	return s
}

// writeFileAtomic writes name in r via a new temp file (O_EXCL, never
// through an existing file or link) and a rename over the old one.
func writeFileAtomic(r *os.Root, name string, data []byte, perm fs.FileMode) error {
	var rnd [6]byte
	rand.Read(rnd[:])
	tmp := "." + name + ".tmp-" + hex.EncodeToString(rnd[:])
	f, err := r.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|oNoFollow, perm)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Chmod(perm) // independent of the umask
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = r.Rename(tmp, name)
	}
	if err != nil {
		_ = r.Remove(tmp)
	}
	return err
}

// openVerifiedDir opens dir as an os.Root after checking that it is a real
// directory (not a symbolic link) and was not swapped while opening.
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

// hostEnv is what the root helper touches on the host (tests use temp dirs
// and a fake systemctl).
type hostEnv struct {
	credDir string // /etc/picache/credentials
	unitDir string // /etc/systemd/system
	// strict enables what only a real root run can do: ownership checks of
	// the directories involved and chown of the mountpoint.
	strict          bool
	checkPrivileges func() error
	systemctl       func(ctx context.Context, args ...string) error // fixed arguments, no shell
	serviceOwner    func(dataDir string) (uid, gid int, err error)  // the PiCache user
}

// ApplyHost is the root-only `picache storage apply <id>` implementation. It
// opens picache.db read-only (db.OpenReadOnly), re-validates the target,
// loads the master key without generating one (secrets.Load) to decrypt the
// password (or reads it from passwordStdin when non-nil), writes
// /etc/picache/credentials/<id>.cred (0600, O_EXCL|O_NOFOLLOW, root-owned
// 0700 dir) and a systemd .mount unit for MountRoot/<id>, then runs
// `systemctl daemon-reload` and `systemctl enable --now <unit>`.
func ApplyHost(ctx context.Context, cfg *config.Config, id string, passwordStdin []byte, log *slog.Logger) error {
	return applyHost(ctx, systemHostEnv(), cfg, id, passwordStdin, log)
}

// ApplyPending processes all queued requests (root helper started by the
// picache-storage.path unit). Results are written to
// <data>/storage-requests/<id>.result for the service to display.
func ApplyPending(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	return applyPending(ctx, systemHostEnv(), cfg, log)
}

func applyHost(ctx context.Context, env hostEnv, cfg *config.Config, id string, pw []byte, log *slog.Logger) error {
	if err := env.checkPrivileges(); err != nil {
		return err
	}
	if !targetIDRE.MatchString(id) {
		return errors.New("invalid storage target id (32 hex characters, see the web UI)")
	}
	h, err := newHelper(env, cfg, log)
	if err != nil {
		return err
	}
	defer h.close()
	err = h.apply(ctx, id, pw)
	if r, derr := openVerifiedDir(requestsDir(cfg)); derr == nil {
		_ = r.Remove(id) // a queued request for the same target is done now
		writeResult(r, id, err, h.log)
		r.Close()
	}
	return err
}

func applyPending(ctx context.Context, env hostEnv, cfg *config.Config, log *slog.Logger) error {
	if err := env.checkPrivileges(); err != nil {
		return err
	}
	r, err := openVerifiedDir(requestsDir(cfg))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer r.Close()
	ids, err := pendingRequests(r)
	if err != nil || len(ids) == 0 {
		return err
	}
	h, herr := newHelper(env, cfg, log)
	var errs []error
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(errs, err)...)
		}
		// Removed first: a new request arriving meanwhile triggers another run.
		if err := r.Remove(id); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
			continue
		}
		err := herr
		if h != nil {
			err = h.apply(ctx, id, nil)
		}
		writeResult(r, id, err, componentLog(log))
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", id, err))
		}
	}
	if h != nil {
		h.close()
	}
	return errors.Join(errs...)
}

// pendingRequests lists request files (names are target ids; bounded).
func pendingRequests(r *os.Root) ([]string, error) {
	d, err := r.Open(".")
	if err != nil {
		return nil, err
	}
	names, err := d.Readdirnames(4 * maxPending)
	d.Close()
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	var ids []string
	for _, n := range names {
		if !targetIDRE.MatchString(n) {
			continue
		}
		if fi, err := r.Lstat(n); err != nil || !fi.Mode().IsRegular() {
			continue
		}
		if ids = append(ids, n); len(ids) == maxPending {
			break
		}
	}
	slices.Sort(ids)
	return ids, nil
}

// writeResult records the outcome for the UI (never containing a password:
// errors from this package never include it).
func writeResult(r *os.Root, id string, applyErr error, log *slog.Logger) {
	res := applyResult{OK: applyErr == nil, Message: "mounted", Time: time.Now().UTC()}
	if applyErr != nil {
		res.Message = sanitizeMessage(applyErr.Error())
	}
	b, err := json.Marshal(res)
	if err == nil {
		err = writeFileAtomic(r, id+resultSuffix, b, 0o644)
	}
	if err != nil {
		log.Warn("cannot write the host-apply result", slog.String("target", id), slog.Any("err", err))
	}
}

func componentLog(log *slog.Logger) *slog.Logger {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return log.With(slog.String("component", "storage"))
}

// helper is one root helper run.
type helper struct {
	env hostEnv
	cfg *config.Config
	db  *db.DB
	box *secrets.Box // loaded on first use
	log *slog.Logger
}

func newHelper(env hostEnv, cfg *config.Config, log *slog.Logger) (*helper, error) {
	d, err := db.OpenReadOnly(cfg.Paths().ConfigDB)
	if err != nil {
		return nil, err
	}
	return &helper{env: env, cfg: cfg, db: d, log: componentLog(log)}, nil
}

func (h *helper) close() { h.db.Close() }

// apply mounts one target. Nothing from the database is trusted: the row is
// validated again and the mountpoint is computed from the id.
func (h *helper) apply(ctx context.Context, id string, stdinPassword []byte) error {
	t, err := loadTarget(ctx, h.db.R, id)
	if err != nil {
		return err
	}
	if t.Mode != ModeHostApply {
		return fmt.Errorf("storage target %s is not in host-apply mode", id)
	}
	t.Path = hostApplyPath(h.cfg, id)
	if err := ValidateTarget(t, h.cfg); err != nil {
		return fmt.Errorf("refusing storage target %s: %w", id, err)
	}
	uid, gid, err := h.env.serviceOwner(h.cfg.DataDir)
	if err != nil {
		return fmt.Errorf("cannot determine the PiCache user: %w", err)
	}
	if err := h.prepareMountpoint(t.Path, uid, gid); err != nil {
		return err
	}
	p := mountParams{uid: int64(uid), gid: int64(gid)}
	if t.Kind == KindSMB && t.Username != "" {
		pw, err := h.password(ctx, t, stdinPassword)
		if err != nil {
			return err
		}
		p.credPath, err = h.writeCredentials(t, pw)
		clear(pw)
		if err != nil {
			return err
		}
	} else if err := h.removeCredentials(id); err != nil {
		return err
	}
	unit := mountUnitName(t.Path)
	content := renderMountUnit(t, t.Path, p)
	if strings.Contains(content, "%") {
		return errors.New("refusing to write a mount unit containing '%' (systemd specifier)")
	}
	if h.env.strict {
		if err := checkRootOwnedChain(h.env.unitDir); err != nil {
			return err
		}
	}
	ud, err := openVerifiedDir(h.env.unitDir)
	if err != nil {
		return err
	}
	err = writeFileAtomic(ud, unit, []byte(content), 0o644)
	ud.Close()
	if err != nil {
		return fmt.Errorf("write %s: %w", unit, err)
	}
	if err := h.env.systemctl(ctx, "daemon-reload"); err != nil {
		return err
	}
	if err := h.env.systemctl(ctx, "enable", "--now", unit); err != nil {
		return err
	}
	h.log.Info("storage mount applied", slog.String("target", id), slog.String("unit", unit), slog.String("where", t.Path))
	return nil
}

// prepareMountpoint creates MountRoot/<id>. In strict mode MountRoot and all
// its parents must be root-owned and not writable by others, so the
// unprivileged service cannot swap the mountpoint for a symbolic link.
func (h *helper) prepareMountpoint(where string, uid, gid int) error {
	root := filepath.Clean(h.cfg.MountRoot)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
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
			return err
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
			return nil, err
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

// writeCredentials writes the mount.cifs credentials file (0600) into the
// root-owned 0700 credentials directory and returns its path.
func (h *helper) writeCredentials(t Target, pw []byte) (string, error) {
	dir := h.env.credDir
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return "", err
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return "", err
	}
	if h.env.strict {
		if err := checkRootOwnedChain(dir); err != nil {
			return "", err
		}
	}
	r, err := openVerifiedDir(dir)
	if err != nil {
		return "", err
	}
	defer r.Close()
	if err := r.Chmod(".", 0o700); err != nil {
		return "", err
	}
	buf := make([]byte, 0, 96+len(pw))
	buf = append(buf, "username="+t.Username+"\npassword="...)
	buf = append(buf, pw...)
	buf = append(buf, '\n')
	if t.Domain != "" {
		buf = append(buf, "domain="+t.Domain+"\n"...)
	}
	name := t.ID + ".cred"
	err = writeFileAtomic(r, name, buf, 0o600)
	clear(buf)
	if err != nil {
		return "", fmt.Errorf("write credentials: %w", err)
	}
	return filepath.Join(dir, name), nil
}

// removeCredentials deletes a stale credentials file (guest access).
func (h *helper) removeCredentials(id string) error {
	r, err := openVerifiedDir(h.env.credDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer r.Close()
	if err := r.Remove(id + ".cred"); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
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
