package update

// Updates from the web UI (docs/ARCHITECTURE.md 14.4): the unprivileged
// service writes <data>/update-requests/request; picache-update.path starts
// the root helper (`picache update apply-pending`, ApplyPending), which
// claims it, validates the version and installs exactly that release of
// Repository. The request carries only a version: a compromised service
// can at most ask for another signed release, and never for an older one.
// The directory belongs to the service, so the root side never follows
// links in it and trusts nothing it reads there but the version string.
//
// Queue protocol (names inside the requests directory):
//
//	request      written atomically by the service (temp file + rename, 0640).
//	.claim       the request, renamed by the helper while it works on it.
//	             One left behind means the helper was killed; the next run
//	             reports it as failed.
//	status.json  progress and result, written by the helper (owner of the
//	             directory, 0640) and read by the service.

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

const (
	// RequestsDirName is the queue directory below the data directory.
	RequestsDirName = "update-requests"
	requestFile     = "request"
	claimFile       = ".claim"
	statusFile      = "status.json"
	maxRequestSize  = 4 << 10
	maxStatusSize   = 16 << 10

	staleRequest = 3 * time.Minute  // not claimed after this: the helper does not run
	staleRun     = 45 * time.Minute // still "running" after this: the run died (TimeoutStartSec is 30 min)
)

// lookupTimeout bounds the helper's release lookup: a stalled answer must
// not keep it (and the UI's "running") for the 30 min of TimeoutStartSec.
// Tests shorten it.
var lookupTimeout = CheckTimeout

// Messages the service shows while the helper has not answered.
const (
	msgWaiting     = "waiting for the update helper (picache-update.service) to start"
	msgNotPickedUp = "the update helper has not picked up the request for 3 minutes. Check `systemctl status picache-update.path` " +
		"(after `systemctl reset-failed picache-update.path` start it again); with a custom PICACHE_DATA_DIR re-run install.sh"
	msgInterrupted = "the update helper stopped before it finished (killed or timed out); check `systemctl status picache` " +
		"and `journalctl -u picache-update`, then try again"
)

// Request is everything the service tells the root helper.
type Request struct {
	Version     string    `json:"version"`
	RequestedAt time.Time `json:"requestedAt"`
	RequestedBy string    `json:"requestedBy"`
}

// RequestsDir returns <data>/update-requests.
func RequestsDir(dataDir string) string { return filepath.Join(dataDir, RequestsDirName) }

// QueueRequest writes the request file for the root helper (service side).
func QueueRequest(dataDir string, req Request) error {
	if _, err := ParseVersion(req.Version); err != nil {
		return err
	}
	dir := RequestsDir(dataDir)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	r, err := openVerifiedDir(dir)
	if err != nil {
		return err
	}
	defer r.Close()
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	return writeFileAtomic(r, requestFile, b, 0o640, -1, -1)
}

// ReadStatus returns the state of the queue for the service (nil if no
// update was ever queued). A queued request is reported as running (step
// download) until the helper claims it, and as failed once it has waited
// for 3 minutes; a run that stays "running" for 45 minutes died.
func ReadStatus(dataDir, current string, now time.Time) *Status {
	r, err := os.OpenRoot(RequestsDir(dataDir))
	if err != nil {
		return nil
	}
	defer r.Close()
	if fi, err := r.Lstat(requestFile); err == nil && fi.Mode().IsRegular() {
		st := &Status{State: StateRunning, Step: StepDownload, From: current, StartedAt: fi.ModTime().UTC(), Message: msgWaiting}
		if req, err := readRequest(r, requestFile); err == nil {
			st.Version = req.Version
		}
		if now.Sub(fi.ModTime()) > staleRequest {
			st.State, st.FinishedAt, st.Message = StateFailed, now.UTC(), msgNotPickedUp
		}
		return st
	}
	st, err := readStatus(r)
	if err != nil {
		return nil
	}
	if st.State == StateRunning && now.Sub(st.StartedAt) > staleRun {
		st.State, st.FinishedAt, st.Message = StateFailed, now.UTC(), msgInterrupted
	}
	return st
}

// readRequest reads a request or claim file: a regular file, never through
// a link, bounded, with nothing but the known members.
func readRequest(r *os.Root, name string) (Request, error) {
	var req Request
	b, err := readSmallFile(r, name, maxRequestSize)
	if err != nil {
		return req, err
	}
	if err := json.Unmarshal(b, &req, json.RejectUnknownMembers(true)); err != nil {
		return req, errors.New("the request is not valid JSON {version, requestedAt, requestedBy}")
	}
	if !versionRE.MatchString(req.Version) {
		return req, fmt.Errorf("the requested version %q is not of the form vX.Y.Z[-pre]", clip(req.Version, 40))
	}
	if _, err := ParseVersion(req.Version); err != nil {
		return req, err
	}
	return req, nil
}

func readStatus(r *os.Root) (*Status, error) {
	b, err := readSmallFile(r, statusFile, maxStatusSize)
	if err != nil {
		return nil, err
	}
	var st Status
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, err
	}
	switch st.State {
	case StateRunning, StateSucceeded, StateFailed, StateRolledBack:
	default:
		return nil, fmt.Errorf("unknown state %q", clip(st.State, 20))
	}
	st.Version, st.From, st.Step = clip(st.Version, 64), clip(st.From, 64), clip(st.Step, 20)
	st.Message = sanitizeMessage(st.Message)
	return &st, nil
}

// readSmallFile reads a regular file (not a link) of at most limit bytes.
func readSmallFile(r *os.Root, name string, limit int64) ([]byte, error) {
	fi, err := r.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", name)
	}
	f, err := r.OpenFile(name, os.O_RDONLY|oNoFollow, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", name, limit)
	}
	return b, nil
}

// writeFileAtomic writes name in r via a new temp file (O_EXCL, never
// through an existing file or link) and a rename over the old one; uid ≥ 0
// sets the owner.
func writeFileAtomic(r *os.Root, name string, data []byte, perm fs.FileMode, uid, gid int) error {
	tmp := "." + name + ".tmp-" + randHex()
	f, err := r.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|oNoFollow, perm)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil && uid >= 0 {
		err = fchown(f, uid, gid)
	}
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

// ApplyPending is the root helper (`picache update apply-pending`, started
// by picache-update.path). It reports a claim left by a killed run, then
// claims the queued request, validates the version and installs exactly
// that release of Repository with Apply (never an older one), writing the
// progress to status.json. o carries the host part of Options (BinPath,
// DataDir, Current, Host, Log); the release comes from client.
func ApplyPending(ctx context.Context, client *Client, o Options) error {
	h := o.Host
	if h.CheckPrivileges != nil {
		if err := h.CheckPrivileges(); err != nil {
			return err
		}
	}
	log := o.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	log = log.With(slog.String("component", "update"))
	r, err := openVerifiedDir(RequestsDir(o.DataDir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer r.Close()
	q := &queue{r: r, log: log, uid: -1, gid: -1}
	if fi, err := r.Stat("."); err == nil { // status files belong to the service user
		q.uid, q.gid, _ = fileOwner(fi)
	}
	if _, err := r.Lstat(claimFile); err == nil {
		q.interrupted(ctx, h, o.Current)
	}
	if err := q.claim(); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	defer q.drop()

	st := Status{State: StateRunning, Step: StepDownload, From: o.Current, StartedAt: time.Now().UTC()}
	req, err := readRequest(r, claimFile)
	if err != nil {
		st.State, st.FinishedAt, st.Message = StateFailed, time.Now().UTC(), sanitizeMessage("invalid update request: "+err.Error())
		q.write(st)
		return fmt.Errorf("invalid update request: %w", err)
	}
	st.Version = req.Version
	log.Info("update requested in the web UI", slog.String("version", req.Version),
		slog.String("requested_by", clip(req.RequestedBy, 64)), slog.Time("requested_at", req.RequestedAt))
	st.Message = "looking up release " + req.Version
	q.write(st)

	o.Version, o.AllowDowngrade, o.Confirm = req.Version, false, nil
	o.Progress = func(step, msg string) {
		st.Step, st.Message = step, sanitizeMessage(msg)
		q.write(st)
	}
	res := Result{State: StateFailed, Version: req.Version, From: o.Current}
	lctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	rel, err := client.Release(lctx, req.Version)
	cancel()
	if err == nil && rel.Version != req.Version {
		err = fmt.Errorf("GitHub answered with release %s instead of %s", rel.Version, req.Version)
	}
	if err == nil {
		o.Files = client.Files(req.Version)
		res, err = Apply(ctx, o)
	} else {
		res.Message = sanitizeMessage(err.Error())
	}
	st.State, st.FinishedAt, st.Message = res.State, time.Now().UTC(), res.Message
	if res.State == StateSucceeded {
		st.Step = StepDone
	}
	q.write(st)
	if err != nil {
		log.Error("update failed", slog.String("version", req.Version), slog.String("state", res.State), slog.Any("err", err))
	}
	return err
}

// queue is one run of the root helper over the requests directory.
type queue struct {
	r        *os.Root
	log      *slog.Logger
	uid, gid int
}

// claim renames the request to .claim.
func (q *queue) claim() error {
	err := q.r.Rename(requestFile, claimFile)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		// Something other than a file sits at the claim name: clear it once.
		_ = q.r.RemoveAll(claimFile)
		err = q.r.Rename(requestFile, claimFile)
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		_ = q.r.RemoveAll(requestFile) // never leave a request that restarts the helper forever
	}
	return err
}

func (q *queue) drop() {
	if err := q.r.RemoveAll(claimFile); err != nil {
		q.log.Warn("cannot remove the update claim", slog.Any("err", err))
	}
}

// write records the progress for the service.
func (q *queue) write(st Status) {
	b, err := json.Marshal(st)
	if err == nil {
		err = writeFileAtomic(q.r, statusFile, b, 0o640, q.uid, q.gid)
	}
	if err != nil {
		q.log.Warn("cannot write the update status", slog.Any("err", err))
	}
}

// interrupted reports a claim left by a run that died. It is not retried:
// whatever killed that run would most likely kill this one too. PiCache is
// started in case the run died while it was stopped.
func (q *queue) interrupted(ctx context.Context, h Host, current string) {
	q.log.Warn("an earlier update run did not finish")
	st := Status{State: StateFailed, Step: StepDownload, From: current, StartedAt: time.Now().UTC()}
	if prev, err := readStatus(q.r); err == nil && prev.State == StateRunning {
		st = *prev
	} else if req, err := readRequest(q.r, claimFile); err == nil {
		st.Version = req.Version
	}
	st.State, st.FinishedAt, st.Message = StateFailed, time.Now().UTC(), msgInterrupted
	q.write(st)
	q.drop()
	if h.Systemctl != nil {
		if err := h.Systemctl(ctx, "start", Service); err != nil {
			q.log.Error("cannot start PiCache after an interrupted update", slog.Any("err", err))
		}
	}
}
