package prefill

import (
	"context"
	"errors"
	"slices"
	"strings"
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

// provider is one store of the prefill: Steam, Battle.net, Epic.
type provider interface {
	// ready reports whether a job would download something of it under
	// set (a sign-in where one is needed, and a selection). It must not
	// lock Daemon.mu.
	ready(set prefillapi.Settings) bool
	// prefill downloads its chosen games through the cache.
	prefill(ctx context.Context, r *jobRun, set prefillapi.Settings) error
	// idle reports that no sign-in waits.
	idle() bool
	// label names the store in messages.
	label() string
}

func (d *Daemon) providers() []provider { return []provider{d.steam, d.bnet, d.epic} }

// startJob starts a job unless one runs. deadline, if not zero, ends it
// (the schedule window).
func (d *Daemon) startJob(trigger string, force bool, window, deadline time.Time) (prefillapi.Job, error) {
	// Before d.mu: the providers lock their own mutexes first, then d.mu.
	work := d.hasWork()
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
	if !work {
		d.mu.Unlock()
		return prefillapi.Job{}, conflict("nothing to prefill: choose games first (and sign in where the store needs it)")
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
		// Every store in turn; one that fails does not stop the others.
		var errs []string
		for _, p := range d.providers() {
			if ctx.Err() != nil {
				break
			}
			if !p.ready(set) {
				continue
			}
			if perr := p.prefill(ctx, r, set); perr != nil && ctx.Err() == nil {
				d.log.Warn("prefill of a store failed", "store", p.label(), "err", perr)
				errs = append(errs, perr.Error())
			}
		}
		if len(errs) > 0 {
			err = errors.New(strings.Join(errs, "; "))
		}
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
		if !running && binaryReplaced() && d.allIdle() {
			d.mu.Lock()
			d.restarting = true
			d.mu.Unlock()
			d.restart()
			return
		}
		d.epic.maintain(ctx)
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

// hasWork reports whether a job would download anything.
func (d *Daemon) hasWork() bool {
	set := d.currentSettings()
	for _, p := range d.providers() {
		if p.ready(set) {
			return true
		}
	}
	return false
}

// allIdle reports that no sign-in waits.
func (d *Daemon) allIdle() bool {
	for _, p := range d.providers() {
		if !p.idle() {
			return false
		}
	}
	return true
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
