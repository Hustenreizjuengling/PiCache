// Package notify tells the admin about events (health checks, cache
// storage, updates, scheduled backups, sign-in lockouts) through webhook,
// ntfy and Gotify channels (docs/ARCHITECTURE.md 15.1).
//
// Tables (picache.db, component "notify"): notify_channels
// (… secret_sealed TEXT NULL …).
//
// Rules:
//   - Emit never blocks its caller (health loop, sign-in, update check,
//     backup scheduler): it filters the message per channel and appends it
//     to that channel's bounded queue. One worker per channel delivers it:
//     at most 3 attempts, 10 s and 60 s apart. At most 20 messages per
//     channel are accepted in any 10 minutes; further ones are dropped and
//     counted, and one summary ("N notifications dropped") is sent once the
//     window allows again. At shutdown requests in flight get
//     shutdownGrace, queued messages are dropped.
//   - The secret (webhook Authorization header, ntfy access token, Gotify
//     application token) is sealed with the master key (AAD
//     "picache/notify/<id>/secret"), write-only in the API, never logged or
//     audited, and dropped from backups unless secrets are included. It is
//     kept on an update only while the kind and the URL's origin (scheme,
//     host, port) stay the same, so a changed URL never receives a stored
//     secret.
//   - Outbound HTTP (deliver.go): private and loopback destinations are
//     allowed on purpose (Home Assistant or a self-hosted ntfy on the LAN or
//     on this host); link-local (cloud metadata), multicast and unspecified
//     addresses never are. No redirects, no proxy, verified TLS, 10 s per
//     request, responses read up to 4 KiB and discarded.
//   - Messages never contain secrets, passwords, tokens or session data.
//     URLs are shown, logged and audited without their query string, and a
//     delivery error never contains the URL.
package notify

import (
	"context"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/secrets"
)

// Severity of a message. Channels receive messages of their minimum
// severity and above.
type Severity string

const (
	SeverityInfo    Severity = "info"
	SeverityWarning Severity = "warning"
	SeverityError   Severity = "error"
)

// rank orders severities (-1 for unknown values).
func (s Severity) rank() int {
	switch s {
	case SeverityInfo:
		return 0
	case SeverityWarning:
		return 1
	case SeverityError:
		return 2
	}
	return -1
}

// Event keys (docs/ARCHITECTURE.md 15.1).
const (
	EventHealthFailed    = "health.failed"
	EventHealthWarning   = "health.warning"
	EventHealthRecovered = "health.recovered"
	EventStorageOffline  = "storage.offline"
	EventStorageOnline   = "storage.online"
	EventUpdateAvailable = "update.available"
	EventUpdateInstalled = "update.installed"
	EventUpdateFailed    = "update.failed"
	EventBackupFailed    = "backup.failed"
	EventBackupSucceeded = "backup.succeeded"
	EventSecurityLockout = "security.lockout"
	EventTest            = "notify.test"
	// EventDropped is the summary of messages dropped by the rate limit. It
	// goes to the channel that dropped them and cannot be selected.
	EventDropped = "notify.dropped"
)

// EventInfo describes an event for the UI (GET /notifications/events).
type EventInfo struct {
	Key         string   `json:"key"`
	Severity    Severity `json:"severity"` // default severity
	Title       string   `json:"title"`
	Description string   `json:"description"`
}

var events = []EventInfo{
	{EventHealthFailed, SeverityError, "Health check failed",
		"A health check has been failing for two consecutive evaluations (about 2 minutes)."},
	{EventHealthWarning, SeverityWarning, "Health check warning",
		"A health check has reported a warning for two consecutive evaluations (about 2 minutes)."},
	{EventHealthRecovered, SeverityInfo, "Health check recovered",
		"A health check that was reported as failing or with a warning is OK again."},
	{EventStorageOffline, SeverityWarning, "Cache storage offline",
		"The active cache storage has been offline for 2 minutes; downloads are passed through uncached."},
	{EventStorageOnline, SeverityInfo, "Cache storage online",
		"The cache storage that was reported offline is back."},
	{EventUpdateAvailable, SeverityInfo, "Update available",
		"The update check found a new PiCache version (once per version)."},
	{EventUpdateInstalled, SeverityInfo, "Update installed",
		"An update from the web UI finished successfully (reported by the new version after the restart)."},
	{EventUpdateFailed, SeverityError, "Update failed",
		"An update from the web UI failed or was rolled back."},
	{EventBackupFailed, SeverityError, "Scheduled backup failed",
		"A scheduled backup could not be written."},
	{EventBackupSucceeded, SeverityInfo, "Scheduled backup written",
		"A scheduled backup was written (channels with the minimum severity warning do not get it)."},
	{EventSecurityLockout, SeverityWarning, "Sign-in lockout",
		"Sign-in throttling locked out a client, a browser or a session, or started delaying a user name (once per lockout)."},
	{EventTest, SeverityInfo, "Test notification",
		"Sent by the Send test button; always delivered to the tested channel."},
}

// Events returns the events a channel can subscribe to.
func Events() []EventInfo { return slices.Clone(events) }

func eventInfo(key string) (EventInfo, bool) {
	if key == EventDropped {
		return EventInfo{Key: EventDropped, Severity: SeverityWarning, Title: "Notifications dropped"}, true
	}
	i := slices.IndexFunc(events, func(e EventInfo) bool { return e.Key == key })
	if i < 0 {
		return EventInfo{}, false
	}
	return events[i], true
}

// Message is one notification. Emit fills in the event's default severity
// and the current time when they are empty.
type Message struct {
	Event    string
	Severity Severity
	Title    string
	Message  string
	Time     time.Time
}

// LogEntry is one delivery attempt (GET /notifications/log).
type LogEntry struct {
	Time        time.Time `json:"time"`
	ChannelID   string    `json:"channelId"`
	ChannelName string    `json:"channelName"`
	Event       string    `json:"event"`
	Severity    Severity  `json:"severity"`
	Title       string    `json:"title"`
	OK          bool      `json:"ok"`
	Error       string    `json:"error,omitempty"`
	Attempt     int       `json:"attempt"`
}

// TestResult is the outcome of a test notification.
type TestResult struct {
	OK         bool   `json:"ok"`
	Error      string `json:"error,omitempty"`
	Status     int    `json:"status,omitempty"` // HTTP status, if the server answered
	DurationMs int64  `json:"durationMs"`
}

// Options identify this installation in messages.
type Options struct {
	InstanceID string
	Hostname   string
	Version    string
}

// Bounds and timings.
const (
	maxChannels    = 10
	maxLogEntries  = 200
	queueSize      = 32               // messages waiting per channel
	rateLimit      = 20               // messages accepted per channel …
	rateWindow     = 10 * time.Minute // … in this window
	maxAttempts    = 3
	requestTimeout = 10 * time.Second
	maxResponse    = 4 << 10
	shutdownGrace  = 2 * time.Second // requests in flight at shutdown
	maxTitle       = 200             // characters
	maxMessageLen  = 2000            // characters
	maxConcurrent  = 2               // test notifications at the same time
)

// backoff is the wait before the second and the third attempt.
var backoff = []time.Duration{10 * time.Second, 60 * time.Second}

// Service keeps the channels and delivers messages. It is safe for
// concurrent use; Emit may be called on a nil *Service (it does nothing).
type Service struct {
	db     *db.DB
	box    *secrets.Box
	log    *slog.Logger
	opt    Options
	client *http.Client
	now    func() time.Time // clock of the rate limit and the log (tests replace it)

	opMu sync.Mutex // serialises Create, Update and Delete

	mu       sync.Mutex // guards the fields below
	workers  map[string]*worker
	runCtx   context.Context // set by Start
	stopping bool            // Start is waiting for the workers
	wg       sync.WaitGroup  // worker goroutines

	// sendCtx bounds the requests of the workers; it is cancelled
	// shutdownGrace after Start's context ended.
	sendCtx    context.Context
	cancelSend context.CancelFunc

	tests chan struct{} // test notifications running

	logMu   sync.Mutex
	logBuf  [maxLogEntries]LogEntry
	logNext int // index of the next entry
	logLen  int
}

// New loads the channels (component migration "notify").
func New(ctx context.Context, d *db.DB, box *secrets.Box, opt Options, log *slog.Logger) (*Service, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if err := d.Migrate(ctx, "notify", migrations); err != nil {
		return nil, err
	}
	s := &Service{
		db: d, box: box, log: log.With(slog.String("component", "notify")), opt: opt,
		client: newClient(), now: time.Now, workers: map[string]*worker{},
		tests: make(chan struct{}, maxConcurrent),
	}
	s.sendCtx, s.cancelSend = context.WithCancel(context.Background())
	chs, err := loadChannels(ctx, d.R)
	if err != nil {
		return nil, err
	}
	for _, c := range chs {
		s.workers[c.ID] = newWorker(c.Channel, c.sealed)
	}
	return s, nil
}

// Start runs the delivery workers until ctx ends (blocks until done).
// Requests in flight then get shutdownGrace; queued messages are dropped.
func (s *Service) Start(ctx context.Context) {
	s.mu.Lock()
	s.runCtx = ctx
	for _, w := range s.workers {
		s.startWorkerLocked(w)
	}
	s.mu.Unlock()
	<-ctx.Done()
	s.mu.Lock()
	s.stopping = true
	s.mu.Unlock()
	grace := time.AfterFunc(shutdownGrace, s.cancelSend)
	s.wg.Wait()
	grace.Stop()
	s.cancelSend()
}

// startWorkerLocked starts the delivery goroutine of w (s.mu held).
func (s *Service) startWorkerLocked(w *worker) {
	if s.runCtx == nil || s.stopping || w.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(s.runCtx)
	w.cancel = cancel
	s.wg.Go(func() { s.runWorker(ctx, w) })
}

// Emit queues m for every channel whose filter accepts it. It never blocks.
func (s *Service) Emit(m Message) {
	if s == nil {
		return
	}
	info, ok := eventInfo(m.Event)
	if !ok || m.Event == EventTest || m.Event == EventDropped {
		s.log.Error("unknown notification event; not sent", slog.String("event", m.Event))
		return
	}
	if m.Severity.rank() < 0 {
		m.Severity = info.Severity
	}
	now := s.now()
	if m.Time.IsZero() {
		m.Time = now
	}
	m.Time = m.Time.UTC()
	m.Title, m.Message = cleanText(m.Title, maxTitle, false), cleanText(m.Message, maxMessageLen, true)
	if m.Title == "" {
		m.Title = info.Title
	}
	s.mu.Lock()
	ws := make([]*worker, 0, len(s.workers))
	for _, w := range s.workers {
		ws = append(ws, w)
	}
	stopping := s.stopping
	s.mu.Unlock()
	if stopping {
		return
	}
	for _, w := range ws {
		w.offer(m, now)
	}
}

// Log returns the last delivery attempts, newest first (limit 1..200).
func (s *Service) Log(limit int) []LogEntry {
	limit = min(max(limit, 1), maxLogEntries)
	s.logMu.Lock()
	defer s.logMu.Unlock()
	out := make([]LogEntry, 0, min(limit, s.logLen))
	for i := 1; i <= s.logLen && len(out) < limit; i++ {
		out = append(out, s.logBuf[(s.logNext-i+maxLogEntries)%maxLogEntries])
	}
	return out
}

// record adds a delivery attempt to the log.
func (s *Service) record(c Channel, m Message, attempt int, err error) {
	e := LogEntry{Time: s.now().UTC().Truncate(time.Millisecond), ChannelID: c.ID, ChannelName: c.Name, Event: m.Event,
		Severity: m.Severity, Title: m.Title, OK: err == nil, Attempt: attempt}
	if err != nil {
		e.Error = err.Error()
	}
	s.logMu.Lock()
	s.logBuf[s.logNext] = e
	s.logNext = (s.logNext + 1) % maxLogEntries
	s.logLen = min(s.logLen+1, maxLogEntries)
	s.logMu.Unlock()
}

// cleanText removes control characters (keeping line breaks if multiline,
// otherwise turning them into spaces), trims and shortens s to max
// characters.
func cleanText(s string, max int, multiline bool) string {
	s = strings.ToValidUTF8(s, "�")
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\n' && multiline:
			return r
		case r == '\n' || r == '\t' || r == '\r':
			return ' '
		case unicode.IsControl(r) || r == ' ' || r == ' ':
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > max {
		s = string([]rune(s)[:max-1]) + "…"
	}
	return s
}
