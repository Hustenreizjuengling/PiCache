package prefill

import (
	"context"
	"errors"
	"slices"
	"time"

	"golang.org/x/time/rate"

	"github.com/hustenreizjuengling/picache/internal/prefill/prefillapi"
)

var (
	errShutdown    = errors.New("the prefill service stopped")
	errWindowEnded = errors.New("the schedule's time window ended")
	errCancelled   = errors.New("cancelled")
)

// jobRun is the running job. Its job is guarded by Daemon.mu.
type jobRun struct {
	job    prefillapi.Job
	cancel context.CancelCauseFunc
	done   chan struct{}
	force  bool
	// window is the start of the schedule window of a scheduled job.
	window time.Time
	lim    *rate.Limiter
	conns  int
}

// startJob starts a job unless one runs. deadline, if not zero, ends it
// (the schedule window).
func (d *Daemon) startJob(trigger string, force bool, window, deadline time.Time) (prefillapi.Job, error) {
	// Before d.mu: steamService locks its own mutex first, then d.mu.
	signedIn := d.steam.signedIn()
	d.mu.Lock()
	if d.stopping {
		d.mu.Unlock()
		return prefillapi.Job{}, unavailable("the prefill service is stopping")
	}
	if d.run != nil {
		j := cloneJob(d.run.job)
		d.mu.Unlock()
		return j, conflict("a job is already running")
	}
	if len(d.settings.Steam.Apps) == 0 || !signedIn {
		d.mu.Unlock()
		return prefillapi.Job{}, conflict("nothing to prefill: sign in and choose games first")
	}
	d.nextID++
	set := cloneSettings(d.settings)
	ctx, cancel := context.WithCancelCause(context.Background())
	if !deadline.IsZero() {
		var c2 context.CancelFunc
		ctx, c2 = context.WithDeadlineCause(ctx, deadline, errWindowEnded)
		prev := cancel
		cancel = func(err error) { c2(); prev(err) }
	}
	r := &jobRun{
		job: prefillapi.Job{
			ID: d.nextID, Trigger: trigger, State: prefillapi.JobRunning, Started: d.now(),
			Apps: []prefillapi.JobApp{}, Hosts: []string{},
		},
		cancel: cancel,
		done:   make(chan struct{}),
		force:  force,
		window: window,
		lim:    newLimiter(set.MaxMbps),
		conns:  set.Connections,
	}
	d.run = r
	j := cloneJob(r.job)
	d.mu.Unlock()
	d.log.Info("prefill job started", "job", j.ID, "trigger", trigger, "force", force)
	go d.execute(ctx, r, set)
	return j, nil
}

// execute runs the job's steps and records the outcome.
func (d *Daemon) execute(ctx context.Context, r *jobRun, set prefillapi.Settings) {
	defer close(r.done)
	err := d.checkCache(ctx)
	if err == nil {
		d.setCurrent(r, "")
		err = d.steam.prefill(ctx, r, set)
	}
	if ctx.Err() != nil {
		err = context.Cause(ctx)
	}
	t := d.now()
	d.mu.Lock()
	j := &r.job
	j.Finished = &t
	j.Current = ""
	switch {
	case err == nil:
		j.State = prefillapi.JobDone
		if slices.ContainsFunc(j.Apps, func(a prefillapi.JobApp) bool { return a.State == prefillapi.AppFailed }) {
			j.State = prefillapi.JobFailed
			j.Error = "some apps failed"
		}
	case errors.Is(err, errWindowEnded) || errors.Is(err, errShutdown):
		j.State = prefillapi.JobStopped
		j.Error = err.Error()
	case errors.Is(err, errCancelled):
		j.State = prefillapi.JobCancelled
	default:
		j.State = prefillapi.JobFailed
		j.Error = err.Error()
	}
	for i := range j.Apps {
		if j.Apps[i].State == prefillapi.AppPending || j.Apps[i].State == prefillapi.AppRunning {
			j.Apps[i].State = prefillapi.AppPending
		}
	}
	if !r.window.IsZero() && !errors.Is(err, errShutdown) {
		d.hist.LastWindow = r.window
	}
	done := cloneJob(*j)
	d.hist.Jobs = append([]prefillapi.Job{done}, d.hist.Jobs...)
	if len(d.hist.Jobs) > historyLen {
		d.hist.Jobs = d.hist.Jobs[:historyLen]
	}
	hist := history{Jobs: slices.Clone(d.hist.Jobs), LastWindow: d.hist.LastWindow}
	d.run = nil
	d.mu.Unlock()
	if serr := d.st.save(fileHistory, hist); serr != nil {
		d.log.Warn("history.json could not be written", "err", serr)
	}
	d.log.Info("prefill job finished", "job", done.ID, "state", done.State, "error", done.Error,
		"bytes", done.Bytes, "fromCache", done.FromCache)
}

// cancelJob stops the running job.
func (d *Daemon) cancelJob() bool {
	d.mu.Lock()
	r := d.run
	d.mu.Unlock()
	if r == nil {
		return false
	}
	r.cancel(errCancelled)
	<-r.done
	return true
}

// scheduler starts a scheduled job when a window begins (or the service
// starts inside one that has not had its job yet).
func (d *Daemon) scheduler(ctx context.Context) {
	for {
		now := d.now()
		d.mu.Lock()
		sched := d.settings.Schedule
		last := d.hist.LastWindow
		running := d.run != nil
		d.mu.Unlock()
		if !running && binaryReplaced() && d.steam.idle() {
			d.log.Info("the picache binary was updated: the prefill service restarts")
			d.restart()
			return
		}
		wait := time.Minute
		if start, end, ok := window(sched, now); ok {
			if !now.Before(start) {
				if !running && !start.Equal(last) && d.hasWork() {
					if _, err := d.startJob("schedule", false, start, end); err != nil {
						d.log.Warn("the scheduled job did not start", "err", err)
					}
				}
			} else if w := start.Sub(now); w < wait {
				wait = w
			}
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-d.wake:
		case <-t.C:
		}
		t.Stop()
	}
}

// hasWork reports whether a job would download anything: a sign-in and
// a selection.
func (d *Daemon) hasWork() bool {
	d.mu.Lock()
	n := len(d.settings.Steam.Apps)
	d.mu.Unlock()
	return n > 0 && d.steam.signedIn()
}

func cloneJob(j prefillapi.Job) prefillapi.Job {
	j.Apps = slices.Clone(j.Apps)
	j.Hosts = slices.Clone(j.Hosts)
	if j.Apps == nil {
		j.Apps = []prefillapi.JobApp{}
	}
	if j.Hosts == nil {
		j.Hosts = []string{}
	}
	return j
}

// The job steps report through these.

func (d *Daemon) setCurrent(r *jobRun, what string) {
	d.mu.Lock()
	r.job.Current = what
	d.mu.Unlock()
}

// addApps adds apps to the job's list.
func (d *Daemon) addApps(r *jobRun, apps []prefillapi.JobApp) {
	d.mu.Lock()
	r.job.Apps = append(r.job.Apps, apps...)
	d.mu.Unlock()
}

// updateApp changes the job app with service and id.
func (d *Daemon) updateApp(r *jobRun, service, id string, f func(*prefillapi.JobApp)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for i := range r.job.Apps {
		if a := &r.job.Apps[i]; a.Service == service && a.ID == id {
			old := a.Total
			f(a)
			r.job.Total += a.Total - old
			return
		}
	}
}

// addBytes counts a download of the app.
func (d *Daemon) addBytes(r *jobRun, service, id string, n int64, hit bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	r.job.Bytes += n
	if hit {
		r.job.FromCache += n
	}
	for i := range r.job.Apps {
		if a := &r.job.Apps[i]; a.Service == service && a.ID == id {
			a.Bytes += n
			return
		}
	}
}

func (d *Daemon) jobHistory() prefillapi.History {
	d.mu.Lock()
	defer d.mu.Unlock()
	h := prefillapi.History{Jobs: make([]prefillapi.Job, 0, len(d.hist.Jobs))}
	for _, j := range d.hist.Jobs {
		h.Jobs = append(h.Jobs, cloneJob(j))
	}
	return h
}
