package update

// The prefill service from the web UI (docs/ARCHITECTURE.md 20.1): the
// unprivileged service writes <data>/prefill-requests/request with the
// action "enable" or "disable"; picache-update.path starts the root helper,
// which (before an update request, PrefillApplyPending) installs the unit
// of the prefill service when the host has none, enables and starts it, or
// stops and disables it, and reports the outcome in status.json, with
// systemd's view and the journal of a start that fails. The request
// carries only the action: a compromised service can at most switch the
// prefill service on or off.
//
// Queue protocol (names inside the requests directory, which belongs to
// the service): request (written atomically by the service, 0640), .claim
// (the request, renamed by the helper while it works on it), status.json
// (written by the helper with the directory's owner, 0640).

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// PrefillRequestsDirName is the queue directory below the data directory.
	PrefillRequestsDirName = "prefill-requests"
	// PrefillUnit is the unit of the prefill service.
	PrefillUnit = "picache-prefill.service"

	prefillSettle    = 5 * time.Second // a start must survive this long
	prefillMaxReport = 2 << 10
)

// Prefill actions.
const (
	PrefillEnable  = "enable"
	PrefillDisable = "disable"
)

// PrefillRequest is everything the service tells the root helper.
type PrefillRequest struct {
	Action      string    `json:"action"`
	RequestedAt time.Time `json:"requestedAt"`
	RequestedBy string    `json:"requestedBy"`
}

// PrefillStatus is status.json: the last request's outcome.
type PrefillStatus struct {
	// State: running, succeeded or failed.
	State      string    `json:"state"`
	Action     string    `json:"action"`
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt,omitzero"`
	Message    string    `json:"message"`
}

// PrefillRequestsDir returns <data>/prefill-requests.
func PrefillRequestsDir(dataDir string) string { return filepath.Join(dataDir, PrefillRequestsDirName) }

// HelperAvailable reports whether the root helper runs on this host: a
// systemd host with the helper's marker (install.sh, not the Debian
// package or a container).
func HelperAvailable() bool {
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		return false
	}
	fi, err := os.Stat(HelperMarker)
	return err == nil && fi.Mode().IsRegular()
}

// QueuePrefill writes the request file for the root helper (service side).
func QueuePrefill(dataDir string, req PrefillRequest) error {
	if req.Action != PrefillEnable && req.Action != PrefillDisable {
		return fmt.Errorf("unknown prefill action %q", req.Action)
	}
	dir := PrefillRequestsDir(dataDir)
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

// ReadPrefillStatus returns the state of the queue for the service (nil
// when nothing was ever requested). A queued request is running until the
// helper claims it and failed once it has waited for 3 minutes.
func ReadPrefillStatus(dataDir string, now time.Time) *PrefillStatus {
	r, err := os.OpenRoot(PrefillRequestsDir(dataDir))
	if err != nil {
		return nil
	}
	defer r.Close()
	for _, name := range []string{requestFile, claimFile} {
		fi, err := r.Lstat(name)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		st := &PrefillStatus{State: StateRunning, StartedAt: fi.ModTime().UTC(), Message: msgPrefillWaiting}
		if req, err := readPrefillRequest(r, name); err == nil {
			st.Action = req.Action
		}
		if name == claimFile {
			st.Message = "the helper is setting up the prefill service"
			if now.Sub(fi.ModTime()) > 5*time.Minute {
				st.State, st.FinishedAt, st.Message = StateFailed, now.UTC(), msgPrefillInterrupted
			}
		} else if now.Sub(fi.ModTime()) > staleRequest {
			st.State, st.FinishedAt, st.Message = StateFailed, now.UTC(), msgPrefillNotPickedUp
		}
		return st
	}
	b, err := readSmallFile(r, statusFile, maxStatusSize)
	if err != nil {
		return nil
	}
	var st PrefillStatus
	if err := json.Unmarshal(b, &st); err != nil {
		return nil
	}
	if st.State != StateRunning && st.State != StateSucceeded && st.State != StateFailed {
		return nil
	}
	st.Action = clip(st.Action, 16)
	st.Message = sanitizeReport(st.Message)
	return &st
}

const (
	msgPrefillInterrupted = "the helper stopped before it finished; check `journalctl -u picache-update`, then try again"
	msgPrefillWaiting     = "waiting for the helper (picache-update.service) to start"
	msgPrefillNotPickedUp = "the helper has not picked up the request for 3 minutes. Check `systemctl status picache-update.path`; " +
		"after an update from 1.4.0 or earlier with a custom PICACHE_DATA_DIR, run the one-line installer once"
)

func readPrefillRequest(r *os.Root, name string) (PrefillRequest, error) {
	var req PrefillRequest
	b, err := readSmallFile(r, name, maxRequestSize)
	if err != nil {
		return req, err
	}
	if err := json.Unmarshal(b, &req, json.RejectUnknownMembers(true)); err != nil {
		return req, errors.New("the request is not valid JSON {action, requestedAt, requestedBy}")
	}
	if req.Action != PrefillEnable && req.Action != PrefillDisable {
		return req, fmt.Errorf("unknown action %q", clip(req.Action, 16))
	}
	return req, nil
}

// sanitizeReport keeps the lines of a report (the journal of a failed
// start): every line sanitised, at most prefillMaxReport bytes.
func sanitizeReport(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = sanitizeMessage(l)
	}
	out := strings.TrimSpace(strings.Join(lines, "\n"))
	if len(out) > prefillMaxReport {
		out = out[:prefillMaxReport] + " …"
	}
	return out
}

// PrefillHost is what the helper needs of the host.
type PrefillHost struct {
	// CheckPrivileges fails unless this process is root.
	CheckPrivileges func() error
	// Systemctl runs systemctl and returns its standard output.
	Systemctl func(ctx context.Context, args ...string) (string, error)
	// Journal returns the unit's journal since t (a few lines).
	Journal func(ctx context.Context, unit string, since time.Time) string
	// UnitDir is where a missing unit is installed ("" = never).
	UnitDir string
	// Strict writes the unit as root:root (a root run).
	Strict bool
	// Sleep waits (tests shorten it).
	Sleep func(ctx context.Context, d time.Duration) error
}

// PrefillOptions configure PrefillApplyPending.
type PrefillOptions struct {
	DataDir string
	// UnitFile is the unit to install when the host has none, its
	// ExecStart naming this binary.
	UnitFile string
	Host     PrefillHost
	Log      *slog.Logger
}

// PrefillApplyPending is the helper's part for the prefill service; it
// does nothing without a request.
func PrefillApplyPending(ctx context.Context, o PrefillOptions) error {
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
	r, err := openVerifiedDir(PrefillRequestsDir(o.DataDir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer r.Close()
	q := &queue{r: r, log: log, uid: -1, gid: -1}
	if fi, err := r.Stat("."); err == nil {
		q.uid, q.gid, _ = fileOwner(fi)
	}
	if _, err := r.Lstat(claimFile); err == nil {
		// A run that died: reported, not retried.
		st := PrefillStatus{State: StateFailed, StartedAt: time.Now().UTC(), FinishedAt: time.Now().UTC(), Message: msgPrefillInterrupted}
		if req, err := readPrefillRequest(r, claimFile); err == nil {
			st.Action = req.Action
		}
		if b, err := json.Marshal(st); err == nil {
			_ = writeFileAtomic(r, statusFile, b, 0o640, q.uid, q.gid)
		}
		q.drop()
	}
	if err := q.claim(); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	defer q.drop()
	st := PrefillStatus{State: StateRunning, StartedAt: time.Now().UTC()}
	write := func() {
		b, err := json.Marshal(st)
		if err == nil {
			err = writeFileAtomic(r, statusFile, b, 0o640, q.uid, q.gid)
		}
		if err != nil {
			log.Warn("prefill status not written", slog.Any("err", err))
		}
	}
	req, err := readPrefillRequest(r, claimFile)
	if err != nil {
		st.State, st.FinishedAt, st.Message = StateFailed, time.Now().UTC(), sanitizeMessage("invalid prefill request: "+err.Error())
		write()
		return fmt.Errorf("invalid prefill request: %w", err)
	}
	st.Action = req.Action
	st.Message = "setting up the prefill service"
	write()
	log.Info("prefill service requested in the web UI", slog.String("action", req.Action),
		slog.String("requested_by", clip(req.RequestedBy, 64)))
	var msg string
	if req.Action == PrefillEnable {
		msg, err = enablePrefill(ctx, o)
	} else {
		msg, err = disablePrefill(ctx, o)
	}
	st.FinishedAt = time.Now().UTC()
	if err != nil {
		st.State, st.Message = StateFailed, sanitizeReport(err.Error())
		write()
		log.Error("prefill service not "+req.Action+"d", slog.Any("err", err))
		return err
	}
	st.State, st.Message = StateSucceeded, msg
	write()
	log.Info("prefill service "+req.Action+"d", slog.String("message", msg))
	return nil
}

func enablePrefill(ctx context.Context, o PrefillOptions) (string, error) {
	h := o.Host
	frag, err := h.Systemctl(ctx, "show", "-p", "FragmentPath", "--value", PrefillUnit)
	if err != nil {
		return "", err
	}
	installed := ""
	if strings.TrimSpace(frag) == "" {
		if h.UnitDir == "" || o.UnitFile == "" {
			return "", errors.New("this host has no unit " + PrefillUnit + " and the helper cannot install one")
		}
		if err := os.MkdirAll(h.UnitDir, 0o755); err != nil {
			return "", err
		}
		r, err := os.OpenRoot(h.UnitDir)
		if err != nil {
			return "", err
		}
		err = writeUnit(r, PrefillUnit, []byte(o.UnitFile), h.Strict)
		r.Close()
		if err != nil {
			return "", fmt.Errorf("install %s: %w", PrefillUnit, err)
		}
		syncDir(h.UnitDir)
		if _, err := h.Systemctl(ctx, "daemon-reload"); err != nil {
			return "", err
		}
		installed = "installed " + filepath.Join(h.UnitDir, PrefillUnit) + ", "
	}
	start := time.Now()
	_, _ = h.Systemctl(ctx, "reset-failed", PrefillUnit)
	if _, err := h.Systemctl(ctx, "enable", "--now", PrefillUnit); err != nil {
		return "", prefillFailure(ctx, h, start, err)
	}
	// systemd reports a start once the process runs: it must still run
	// after a few seconds, without a restart.
	sleep := h.Sleep
	if sleep == nil {
		sleep = sleepCtx
	}
	if err := sleep(ctx, prefillSettle); err != nil {
		return "", err
	}
	out, err := h.Systemctl(ctx, "show", "-p", "ActiveState", "-p", "SubState", "-p", "NRestarts", PrefillUnit)
	if err != nil {
		return "", err
	}
	props := map[string]string{}
	for l := range strings.SplitSeq(out, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(l), "="); ok {
			props[k] = v
		}
	}
	if props["ActiveState"] != "active" || props["SubState"] != "running" || (props["NRestarts"] != "" && props["NRestarts"] != "0") {
		return "", prefillFailure(ctx, h, start, fmt.Errorf("the prefill service does not keep running (%s/%s, restarts %s)",
			props["ActiveState"], props["SubState"], props["NRestarts"]))
	}
	return installed + "enabled and started " + PrefillUnit, nil
}

// prefillFailure adds the journal of the failed start to err.
func prefillFailure(ctx context.Context, h PrefillHost, since time.Time, err error) error {
	if h.Journal == nil {
		return err
	}
	if j := strings.TrimSpace(h.Journal(context.WithoutCancel(ctx), PrefillUnit, since)); j != "" {
		return fmt.Errorf("%w\n%s", err, j)
	}
	return err
}

func disablePrefill(ctx context.Context, o PrefillOptions) (string, error) {
	if _, err := o.Host.Systemctl(ctx, "disable", "--now", PrefillUnit); err != nil {
		return "", err
	}
	return "stopped and disabled " + PrefillUnit + " (its sign-ins and settings are kept)", nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
