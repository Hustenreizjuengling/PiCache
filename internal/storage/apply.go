package storage

// Host-apply: the service only queues <data>/storage-requests/<id>. The root
// helper (`picache storage apply-pending`, started by picache-storage.path)
// reconciles the host with the database for every queued id: a target in
// host-apply mode is (re)mounted, a deleted target or one that switched to
// another mode has its mount removed. The admin can do the same by hand
// (`sudo picache storage apply <id>`, `sudo picache storage remove <id>`).
// The helper leaves <id>.result for the UI. The requests directory is
// writable by the unprivileged service, so the root side never follows links
// in it and never trusts anything but the file name.
//
// Queue protocol (names inside the requests directory):
//
//	<id>          a request, written atomically by the service. The glob of
//	              picache-storage.path matches exactly these names.
//	.<id>.claim   the request, renamed by the helper while it works on it. A
//	              newer request for the same id is a new <id> file.
//	<id>.result   the outcome, written before the claim is dropped.
//
// The helper lists the directory again after every batch, so requests that
// arrive while it runs are handled in the same run. systemd checks the glob
// again when the helper exits (unlike PathChanged=) and starts it once more
// if a request is left. A claim that outlives its run (the helper was killed)
// is reported as failed by the next run instead of being retried.

import (
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
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/config"
)

const (
	requestsDirName = "storage-requests"
	resultSuffix    = ".result"
	claimSuffix     = ".claim"
	maxResultSize   = 4 << 10 // bytes of a result file
	maxPending      = 64      // requests handled per helper run
	maxScan         = 16384   // directory entries looked at per listing
	maxMessage      = 300     // runes of a result message
	applyQueued     = "queued"
	applyApplied    = "applied"

	pendingBudget  = 3 * time.Minute  // a helper run starts no new request after this
	requestTimeout = 4 * time.Minute  // one request, all systemctl calls included
	staleRequest   = 3 * time.Minute  // an unclaimed request this old: the helper does not run
	staleClaim     = 15 * time.Minute // a claim this old: its run died (TimeoutStartSec is 10 min)
)

// Request actions. They are informational (logs): the helper decides from
// the database what a request means.
const (
	actionApply  = "apply"
	actionRemove = "remove"
)

// Messages the service shows when the helper does not answer.
const (
	msgNotPickedUp = "the root helper has not picked up the request for 3 minutes. Check `systemctl status picache-storage.path` " +
		"(after `systemctl reset-failed picache-storage.path` start it again); with a custom PICACHE_DATA_DIR re-run install.sh"
	msgInterrupted = "the root helper stopped before it finished (killed or timed out); see `journalctl -u picache-storage` and try again"
)

var claimRE = regexp.MustCompile(`^\.([0-9a-f]{32})\.claim$`)

// applyRequest is the (informational) content of a request file.
type applyRequest struct {
	ID          string    `json:"id"`
	Action      string    `json:"action"`
	RequestedAt time.Time `json:"requestedAt"`
}

// applyResult is written by the root helper for the UI.
type applyResult struct {
	OK      bool      `json:"ok"`
	Removed bool      `json:"removed,omitempty"` // the outcome of removing the host mount
	Message string    `json:"message,omitempty"`
	Time    time.Time `json:"time"`
}

func requestsDir(cfg *config.Config) string { return filepath.Join(cfg.DataDir, requestsDirName) }

func claimName(id string) string { return "." + id + claimSuffix }

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
	if err := writeRequest(requestsDir(m.cfg), id, actionApply); err != nil {
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

// queueRemoval asks the root helper to remove the host mount of a target
// that was deleted or left host-apply mode: disable and delete the .mount
// unit, delete the credentials file, remove the mountpoint. Without the
// helper the admin has to run `sudo picache storage remove <id>`.
func (m *Manager) queueRemoval(id string) {
	dir := requestsDir(m.cfg)
	cmd := "sudo picache storage remove " + id
	if !fileExists(m.hostApplyFlag) {
		removeRequestFiles(dir, id)
		m.log.Warn("the root helper is not installed; remove the host mount of the storage target yourself",
			slog.String("target", id), slog.String("command", cmd))
		return
	}
	if err := writeRequest(dir, id, actionRemove); err != nil {
		m.log.Error("cannot queue the removal of the host mount; remove it yourself",
			slog.String("target", id), slog.String("command", cmd), slog.Any("err", err))
		return
	}
	m.log.Info("host mount removal requested", slog.String("target", id))
}

// removalOutstanding reports whether a removal of id's host mount is queued,
// running or failed (the target is no longer in host-apply mode).
func removalOutstanding(dir, id string) bool {
	r, err := os.OpenRoot(dir)
	if err != nil {
		return false
	}
	defer r.Close()
	for _, n := range []string{id, claimName(id)} {
		if _, err := r.Lstat(n); err == nil {
			return true
		}
	}
	res, ok := readResult(r, id)
	return ok && res.Removed && !res.OK
}

// writeRequest replaces any old result with a new request file.
func writeRequest(dir, id, action string) error {
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
	b, err := json.Marshal(applyRequest{ID: id, Action: action, RequestedAt: time.Now().UTC()})
	if err != nil {
		return err
	}
	return writeFileAtomic(r, id, b, 0o640)
}

// readApplyState returns queued, applied, "failed: <msg>" or "". A request
// the helper has not picked up in time, or a claim whose run died, is
// reported as failed so the UI does not show "queued" forever. For targets
// in another mode (hostApply false) only removals are reported, and only
// when they failed.
func readApplyState(dir, id string, hostApply bool) string {
	r, err := os.OpenRoot(dir)
	if err != nil {
		return ""
	}
	defer r.Close()
	now := time.Now()
	if fi, err := r.Lstat(id); err == nil {
		if now.Sub(fi.ModTime()) > staleRequest {
			return "failed: " + msgNotPickedUp
		}
		if hostApply {
			return applyQueued
		}
		return ""
	}
	if fi, err := r.Lstat(claimName(id)); err == nil {
		if now.Sub(fi.ModTime()) > staleClaim {
			return "failed: " + msgInterrupted
		}
		if hostApply {
			return applyQueued
		}
		return ""
	}
	res, ok := readResult(r, id)
	switch {
	case !ok:
		if _, err := r.Lstat(id + resultSuffix); err == nil {
			return "failed: unreadable result file"
		}
		return ""
	case !res.OK && (hostApply || res.Removed):
		return "failed: " + sanitizeMessage(res.Message)
	case res.OK && hostApply && !res.Removed:
		return applyApplied
	}
	return ""
}

// readResult reads <id>.result (false if missing, oversized or invalid).
func readResult(r *os.Root, id string) (applyResult, bool) {
	f, err := r.OpenFile(id+resultSuffix, os.O_RDONLY|oNoFollow, 0)
	if err != nil {
		return applyResult{}, false
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxResultSize+1))
	var res applyResult
	if err != nil || len(b) > maxResultSize || json.Unmarshal(b, &res) != nil {
		return applyResult{}, false
	}
	return res, true
}

// removeRequestFiles deletes the request and result of a target (a claim
// belongs to a running helper and is left alone).
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

// ApplyHost is the root-only `picache storage apply <id>` implementation. It
// opens picache.db read-only (regular file only, untrusted schema),
// re-validates the target, loads the master key without generating one
// (secrets.Load) to decrypt the password (or reads it from passwordStdin
// when non-nil), writes /etc/picache/credentials/<id>.cred (0600,
// O_EXCL|O_NOFOLLOW, root-owned 0700 dir) and a systemd .mount unit for
// MountRoot/<id>, runs `systemctl daemon-reload` and `systemctl enable`, and
// starts the unit, or restarts it when the mounted settings are outdated.
func ApplyHost(ctx context.Context, cfg *config.Config, id string, passwordStdin []byte, log *slog.Logger) error {
	return applyHost(ctx, systemHostEnv(), cfg, id, passwordStdin, log)
}

// RemoveHost is the root-only `picache storage remove <id>` implementation:
// it disables and stops the .mount unit of MountRoot/<id> (`systemctl
// disable` + `stop`), deletes the unit and /etc/picache/credentials/<id>.cred
// and removes the empty mountpoint. It does not need the database, so it
// also cleans up after a target that no longer exists.
func RemoveHost(ctx context.Context, cfg *config.Config, id string, log *slog.Logger) error {
	return removeHost(ctx, systemHostEnv(), cfg, id, log)
}

// ApplyPending processes all queued requests (root helper started by the
// picache-storage.path unit): host-apply targets are mounted, deleted
// targets and targets in another mode have their mount removed. Results are
// written to <data>/storage-requests/<id>.result for the service to display.
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
	finishManual(cfg, id, err, false, !isNotFound(err), h.log)
	return err
}

func removeHost(ctx context.Context, env hostEnv, cfg *config.Config, id string, log *slog.Logger) error {
	if err := env.checkPrivileges(); err != nil {
		return err
	}
	if !targetIDRE.MatchString(id) {
		return errors.New("invalid storage target id (32 hex characters, see the web UI)")
	}
	h := &helper{env: env, cfg: cfg, log: componentLog(log)}
	err := h.remove(ctx, id)
	// Only a target that still exists has a status to show the result in.
	known := false
	if d, derr := openConfigDB(cfg.Paths().ConfigDB); derr == nil {
		_, lerr := loadTarget(ctx, d.R, id)
		known = lerr == nil
		d.Close()
	}
	finishManual(cfg, id, err, true, known, h.log)
	return err
}

// finishManual records the outcome of a command the admin ran by hand and
// drops queued work for the same target, which the command superseded.
func finishManual(cfg *config.Config, id string, err error, removed, known bool, log *slog.Logger) {
	r, derr := openVerifiedDir(requestsDir(cfg))
	if derr != nil {
		return
	}
	defer r.Close()
	_ = r.Remove(id)
	_ = r.Remove(claimName(id))
	if known {
		writeResult(r, id, err, removed, log)
	} else {
		_ = r.Remove(id + resultSuffix)
	}
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
	q := &queue{env: env, cfg: cfg, rawLog: log, log: componentLog(log), r: r, ours: make(map[string]bool),
		deadline: time.Now().Add(pendingBudget)}
	defer q.close()
	return q.run(ctx)
}

// queue is one run of the root helper over the requests directory.
type queue struct {
	env      hostEnv
	cfg      *config.Config
	rawLog   *slog.Logger // as given (newHelper adds the component)
	log      *slog.Logger
	r        *os.Root
	h        *helper // opened on first use
	herr     error
	opened   bool
	ours     map[string]bool // claims taken by this run and not dropped yet
	handled  int
	deadline time.Time
	errs     []error
}

func (q *queue) helper() (*helper, error) {
	if !q.opened {
		q.h, q.herr = newHelper(q.env, q.cfg, q.rawLog)
		q.opened = true
	}
	return q.h, q.herr
}

func (q *queue) close() {
	if q.h != nil {
		q.h.close()
	}
}

// run lists the directory until no request is left, bounded by maxPending
// and pendingBudget (systemd starts the helper again for what is left).
func (q *queue) run(ctx context.Context) error {
	for {
		s, err := scanRequests(q.r)
		if err != nil {
			return errors.Join(append(q.errs, err)...)
		}
		for _, n := range s.junk {
			// Only the service can put it there; left alone, it would match
			// the path unit's glob and start the helper again and again.
			q.log.Warn("removing an unexpected entry from the requests directory", slog.String("name", n))
			if err := q.r.RemoveAll(n); err != nil {
				q.errs = append(q.errs, err)
			}
		}
		for _, id := range s.claims {
			if !q.ours[id] && !slices.Contains(s.requests, id) {
				q.interrupted(ctx, id)
			}
		}
		if len(s.requests) == 0 {
			return errors.Join(q.errs...)
		}
		for _, id := range s.requests {
			if err := ctx.Err(); err != nil {
				return errors.Join(append(q.errs, err)...)
			}
			if q.handled >= maxPending || time.Now().After(q.deadline) {
				q.log.Warn("storage requests are left for the next helper run", slog.Int("handled", q.handled))
				return errors.Join(q.errs...)
			}
			q.handled++
			q.process(ctx, id)
		}
	}
}

// process claims one request, reconciles the target and records the result
// before the claim is dropped (a run that dies leaves the claim behind).
func (q *queue) process(ctx context.Context, id string) {
	if err := q.claim(id); err != nil {
		if !errors.Is(err, fs.ErrNotExist) { // gone meanwhile: nothing to do
			q.errs = append(q.errs, fmt.Errorf("%s: %w", id, err))
		}
		return
	}
	q.ours[id] = true
	var removed, gone bool
	h, err := q.helper()
	if err == nil {
		rctx, cancel := context.WithTimeout(ctx, requestTimeout)
		removed, gone, err = h.reconcile(rctx, id)
		cancel()
	}
	if gone { // a deleted target has no status to show a result in
		_ = q.r.Remove(id + resultSuffix)
		if err != nil {
			q.log.Error("cannot remove the host mount of a deleted storage target", slog.String("target", id),
				slog.String("command", "sudo picache storage remove "+id), slog.Any("err", err))
		}
	} else {
		writeResult(q.r, id, err, removed, q.log)
	}
	if err != nil {
		q.errs = append(q.errs, fmt.Errorf("%s: %w", id, err))
	}
	if err := q.r.Remove(claimName(id)); err == nil || errors.Is(err, fs.ErrNotExist) {
		delete(q.ours, id)
	}
}

// claim renames <id> to .<id>.claim and stamps the claim time.
func (q *queue) claim(id string) error {
	c := claimName(id)
	err := q.r.Rename(id, c)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		// Something other than a file sits at the claim name: clear it once.
		_ = q.r.RemoveAll(c)
		err = q.r.Rename(id, c)
	}
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			_ = q.r.RemoveAll(id) // never leave a request that restarts the helper forever
		}
		return err
	}
	now := time.Now()
	_ = q.r.Chtimes(c, now, now)
	return nil
}

// interrupted reports a claim left by a run that died. It is not retried:
// whatever killed that run would most likely kill this one too.
func (q *queue) interrupted(ctx context.Context, id string) {
	known, removed := true, false
	if h, err := q.helper(); err == nil {
		t, lerr := loadTarget(ctx, h.db.R, id)
		known = !isNotFound(lerr)
		removed = lerr == nil && t.Mode != ModeHostApply
	}
	if known {
		writeResult(q.r, id, errors.New(msgInterrupted), removed, q.log)
	}
	_ = q.r.Remove(claimName(id))
	q.log.Warn("an earlier helper run did not finish a storage request", slog.String("target", id))
}

// matchesUnitGlob reports whether picache-storage.path would start the
// helper for name. Its globs are "????…" (32 characters) and ".????….claim":
// systemd limits a glob component to 255 bytes, too short for 32 "[0-9a-f]".
func matchesUnitGlob(n string) bool {
	switch {
	case len(n) == 32:
		return n[0] != '.'
	case len(n) == 1+32+len(claimSuffix):
		return n[0] == '.' && strings.HasSuffix(n, claimSuffix)
	}
	return false
}

// requestScan is one listing of the requests directory.
type requestScan struct {
	requests []string // ids with a request file
	claims   []string // ids with a claim file
	junk     []string // names that match the path unit's globs but are no request or claim file
}

// scanRequests lists request and claim files (bounded; names are target ids).
func scanRequests(r *os.Root) (requestScan, error) {
	var s requestScan
	d, err := r.Open(".")
	if err != nil {
		return s, err
	}
	defer d.Close()
	for seen := 0; seen < maxScan; {
		names, err := d.Readdirnames(256)
		seen += len(names)
		for _, n := range names {
			id, claim := n, false
			switch m := claimRE.FindStringSubmatch(n); {
			case m != nil:
				id, claim = m[1], true
			case targetIDRE.MatchString(n):
			case matchesUnitGlob(n):
				s.junk = append(s.junk, n)
				continue
			default:
				continue
			}
			fi, err := r.Lstat(n)
			switch {
			case err != nil:
			case !fi.Mode().IsRegular():
				s.junk = append(s.junk, n)
			case claim:
				s.claims = append(s.claims, id)
			default:
				s.requests = append(s.requests, id)
			}
		}
		if err != nil || len(names) == 0 {
			if errors.Is(err, io.EOF) || err == nil {
				break
			}
			return s, err
		}
	}
	slices.Sort(s.requests)
	slices.Sort(s.claims)
	return s, nil
}

// writeResult records the outcome for the UI (never containing a password:
// errors from this package never include it).
func writeResult(r *os.Root, id string, opErr error, removed bool, log *slog.Logger) {
	res := applyResult{OK: opErr == nil, Removed: removed, Message: "mounted", Time: time.Now().UTC()}
	switch {
	case opErr != nil:
		res.Message = sanitizeMessage(opErr.Error())
	case removed:
		res.Message = "removed"
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

func isNotFound(err error) bool { return err != nil && apperr.KindOf(err) == apperr.KindNotFound }
