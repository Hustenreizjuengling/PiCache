// Package applog is the application log of PiCache (docs/ARCHITECTURE.md 4):
// a slog.Handler that wraps the stderr handler and keeps the last records
// in a ring buffer for the web UI (GET /system/log, GET /stream/system-log),
// with a runtime debug level (PUT /system/log/level).
//
// Rules:
//   - Redaction for both sinks (stderr/journald and the ring): the values
//     of attributes whose key is a secret's name (the audit log's list,
//     normalised the same way, at any group depth) become "[redacted]";
//     string values that parse as an absolute URL are reduced to
//     scheme://host[:port]/path (no user information, query or fragment).
//     The one exception is a StderrOnly value (the first-run setup token,
//     which the journal is the documented place to find): stderr shows it,
//     the ring has "[redacted]".
//   - The ring only: every record is bounded (message 512 bytes, 32
//     attributes with groups flattened as group.key, keys 64 bytes, values
//     512 bytes, 2 KiB in total; a cut is marked "…", dropped attributes
//     are counted as "…": "<n> more"); 2000 records. While client
//     addresses are anonymised (logs.anonymizeClientIps) the values of the
//     address keys are masked to /16 or /48; while domains are hidden
//     (logs.hideDomains) the values of the domain keys are "[hidden]". The
//     app wires both switches as functions; stderr is not covered.
//   - Concurrency: a record is built and cut before the ring's mutex, which
//     guards only the slot write and the sequence number. The fan-out to
//     at most 4 stream subscribers never blocks (256 records buffered per
//     subscriber, a drop counter).
//   - Level: the stderr handler is created at debug level; this handler
//     decides Enabled: the base level (PICACHE_LOG_LEVEL), or a temporary
//     override (debug or info, for all components or one, 1–240 minutes;
//     memory only) with one atomic load on the hot path.
//
// It imports no PiCache package.
package applog

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Bounds of the ring and of its records.
const (
	Capacity      = 2000 // records kept
	MaxSubs       = 4    // stream subscribers at the same time
	subBuffer     = 256  // records buffered per subscriber
	maxMessage    = 512  // bytes
	maxAttrs      = 32   // attributes per record
	maxKey        = 64   // bytes
	maxValue      = 512  // bytes
	maxRecord     = 2048 // bytes of message, keys and values together
	MaxOverride   = 240  // minutes of a debug level override
	cutMark       = "…"
	redactedValue = "[redacted]"
	hiddenValue   = "[hidden]"
)

// AppComponent is the component of records without one.
const AppComponent = "app"

// Components are the component names of the records (the attribute
// "component" of a logger's With), AppComponent for records without one.
var Components = []string{
	"api", AppComponent, "auth", "backup", "cachestore", "clients", "dhcp", "dns", "filter", "logs", "network",
	"notify", "parental", "proxy", "services", "settings", "sni", "storage", "update", "upstream", "web-access", "web-tls",
}

// Attr is an attribute of a ring record.
type Attr struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// Record is a record of the ring (api.LogRecord).
type Record struct {
	Seq       uint64    `json:"seq"`
	Time      time.Time `json:"time"`
	Level     string    `json:"level"` // DEBUG | INFO | WARN | ERROR
	Component string    `json:"component"`
	Msg       string    `json:"msg"`
	Attrs     []Attr    `json:"attrs"`
	level     slog.Level
}

// Override is a temporary level for all components (Component "") or one.
type Override struct {
	Level     string    `json:"level"` // debug | info
	Component string    `json:"component,omitempty"`
	Until     time.Time `json:"until"`
	level     slog.Level
}

// Errors of SetOverride (the API maps them to its fields).
var (
	ErrLevel     = errors.New("level must be debug or info and more verbose than PICACHE_LOG_LEVEL")
	ErrComponent = errors.New("unknown component")
	ErrMinutes   = errors.New("minutes must be between 1 and 240")
	ErrTooMany   = errors.New("too many application log streams")
)

// Log is the ring buffer, the level and the subscribers shared by every
// handler derived from one New.
type Log struct {
	base     slog.Level
	override atomic.Pointer[Override]
	anon     atomic.Pointer[func() bool]
	hide     atomic.Pointer[func() bool]

	mu    sync.Mutex
	ring  [Capacity]Record
	next  int
	count int
	seq   uint64

	subMu   sync.RWMutex
	subs    map[*subscriber]struct{}
	dropped atomic.Uint64

	timerMu sync.Mutex
	timer   *time.Timer
	log     *slog.Logger // the log itself (the override's start and end lines)
}

type subscriber struct {
	ch        chan Record
	least     slog.Level
	component string
}

// Handler is the slog.Handler of the application log.
type Handler struct {
	l         *Log
	next      slog.Handler // the stderr handler (redacted attributes)
	attrs     []slog.Attr  // redacted attributes of With, flattened for the ring
	groups    []string
	component string
}

// New wraps next (the stderr handler, created at debug level) with base as
// the level of PICACHE_LOG_LEVEL.
func New(next slog.Handler, base slog.Level) *Handler {
	l := &Log{base: base, subs: map[*subscriber]struct{}{}}
	h := &Handler{l: l, next: next}
	l.log = slog.New(h)
	return h
}

// Log returns the shared state of the handler.
func (h *Handler) Log() *Log { return h.l }

// SetPrivacy sets the functions reporting logs.anonymizeClientIps and
// logs.hideDomains (nil: off).
func (l *Log) SetPrivacy(anonymize, hideDomains func() bool) {
	if anonymize != nil {
		l.anon.Store(&anonymize)
	}
	if hideDomains != nil {
		l.hide.Store(&hideDomains)
	}
}

func flag(p *atomic.Pointer[func() bool]) bool {
	if fn := p.Load(); fn != nil {
		return (*fn)()
	}
	return false
}

// BaseLevel returns the level of PICACHE_LOG_LEVEL ("debug", "info", …).
func (l *Log) BaseLevel() string { return levelName(l.base) }

// Enabled reports whether a record of level is kept (the base level or the
// override of all components or of the handler's component).
func (h *Handler) Enabled(_ context.Context, level slog.Level) bool {
	if level >= h.l.base {
		return true
	}
	o := h.l.override.Load()
	return o != nil && level >= o.level && (o.Component == "" || o.Component == h.component) && time.Now().Before(o.Until)
}

// WithAttrs returns a handler with redacted attributes; the attribute
// "component" sets the handler's component.
func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	c := *h
	red := make([]slog.Attr, 0, len(attrs))
	for _, a := range attrs {
		a = redact(a)
		if len(h.groups) == 0 && a.Key == "component" && a.Value.Kind() == slog.KindString {
			c.component = a.Value.String()
		}
		red = append(red, a)
	}
	c.next = h.next.WithAttrs(red)
	c.attrs = append(slices.Clip(h.attrs), prefixed(h.groups, red)...)
	return &c
}

// WithGroup returns a handler that nests the following attributes.
func (h *Handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	c := *h
	c.next = h.next.WithGroup(name)
	c.groups = append(slices.Clip(h.groups), name)
	return &c
}

// prefixed wraps attrs into the open groups (for the ring's flattening).
func prefixed(groups []string, attrs []slog.Attr) []slog.Attr {
	if len(groups) == 0 {
		return attrs
	}
	out := slog.Attr{Key: groups[len(groups)-1], Value: slog.GroupValue(attrs...)}
	for i := len(groups) - 2; i >= 0; i-- {
		out = slog.Attr{Key: groups[i], Value: slog.GroupValue(out)}
	}
	return []slog.Attr{out}
}

// Handle redacts the record, adds it to the ring and passes it on.
func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	red := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	var own []slog.Attr
	r.Attrs(func(a slog.Attr) bool {
		a = redact(a)
		red.AddAttrs(a)
		own = append(own, a)
		return true
	})
	h.l.add(h.ringRecord(r.Time, r.Level, r.Message, own))
	return h.next.Handle(ctx, red)
}

// levelName returns the lower-case name of a level (debug, info, warn,
// error).
func levelName(l slog.Level) string {
	switch {
	case l < slog.LevelInfo:
		return "debug"
	case l < slog.LevelWarn:
		return "info"
	case l < slog.LevelError:
		return "warn"
	}
	return "error"
}

// ParseLevel parses debug, info, warn or error.
func ParseLevel(s string) (slog.Level, bool) {
	switch s {
	case "debug":
		return slog.LevelDebug, true
	case "info":
		return slog.LevelInfo, true
	case "warn":
		return slog.LevelWarn, true
	case "error":
		return slog.LevelError, true
	}
	return 0, false
}

// ValidComponent reports whether c is one of Components.
func ValidComponent(c string) bool { return slices.Contains(Components, c) }

// add stores a record and fans it out (never blocks).
func (l *Log) add(rec Record) {
	l.mu.Lock()
	l.seq++
	rec.Seq = l.seq
	l.ring[l.next] = rec
	l.next = (l.next + 1) % Capacity
	l.count = min(l.count+1, Capacity)
	l.mu.Unlock()
	l.subMu.RLock()
	defer l.subMu.RUnlock()
	for s := range l.subs {
		if rec.level < s.least || (s.component != "" && s.component != rec.Component) {
			continue
		}
		select {
		case s.ch <- rec:
		default:
			l.dropped.Add(1)
		}
	}
}

// Records returns at most limit records of at least level least (of one
// component; "" = all), newest first.
func (l *Log) Records(least slog.Level, component string, limit int) []Record {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Record, 0, min(max(limit, 0), l.count))
	for i := 1; i <= l.count && len(out) < limit; i++ {
		r := l.ring[(l.next-i+Capacity)%Capacity]
		if r.level >= least && (component == "" || r.Component == component) {
			out = append(out, r)
		}
	}
	return out
}

// Dropped returns the records not delivered to slow stream subscribers.
func (l *Log) Dropped() uint64 { return l.dropped.Load() }

// Subscribe returns a stream of the new records of at least level least
// (of one component; "" = all) and its cancel function; ErrTooMany beyond
// MaxSubs subscribers.
func (l *Log) Subscribe(least slog.Level, component string) (<-chan Record, func(), error) {
	l.subMu.Lock()
	defer l.subMu.Unlock()
	if len(l.subs) >= MaxSubs {
		return nil, func() {}, ErrTooMany
	}
	s := &subscriber{ch: make(chan Record, subBuffer), least: least, component: component}
	l.subs[s] = struct{}{}
	return s.ch, sync.OnceFunc(func() {
		l.subMu.Lock()
		defer l.subMu.Unlock()
		delete(l.subs, s)
		close(s.ch)
	}), nil
}

// CurrentOverride returns the active override (nil if none).
func (l *Log) CurrentOverride() *Override {
	o := l.override.Load()
	if o == nil || !time.Now().Before(o.Until) {
		return nil
	}
	c := *o
	return &c
}

// SetOverride makes the log more verbose for d (1–240 minutes): level
// "debug" or "info", more verbose than the base level, for component (""
// = all components). It replaces an active override and logs a warning.
func (l *Log) SetOverride(level, component string, d time.Duration) (Override, error) {
	lv, ok := ParseLevel(level)
	if !ok || (level != "debug" && level != "info") || lv >= l.base {
		return Override{}, ErrLevel
	}
	if component != "" && !ValidComponent(component) {
		return Override{}, ErrComponent
	}
	if d < time.Minute || d > MaxOverride*time.Minute {
		return Override{}, ErrMinutes
	}
	o := &Override{Level: level, Component: component, Until: time.Now().Add(d).UTC().Truncate(time.Second), level: lv}
	l.timerMu.Lock()
	if l.timer != nil {
		l.timer.Stop()
	}
	l.override.Store(o)
	l.timer = time.AfterFunc(time.Until(o.Until), func() { l.end(o) })
	l.timerMu.Unlock()
	who := "all components"
	if component != "" {
		who = component
	}
	l.log.Warn(level+" logging on for "+who+" until "+o.Until.Format(time.RFC3339), slog.Int("minutes", int(d/time.Minute)))
	return *o, nil
}

// ClearOverride ends an active override at once (nothing happens without
// one).
func (l *Log) ClearOverride() {
	l.timerMu.Lock()
	o := l.override.Load()
	if l.timer != nil {
		l.timer.Stop()
		l.timer = nil
	}
	l.timerMu.Unlock()
	if o != nil {
		l.end(o)
	}
}

// end removes the override o (unless another one replaced it).
func (l *Log) end(o *Override) {
	if l.override.CompareAndSwap(o, nil) {
		l.log.Info(o.Level + " logging ended")
	}
}

// ringRecord builds the bounded, privacy-masked ring form of a record.
func (h *Handler) ringRecord(t time.Time, level slog.Level, msg string, own []slog.Attr) Record {
	anon, hide := flag(&h.l.anon), flag(&h.l.hide)
	rec := Record{Time: t.UTC(), Level: levelNames[levelIndex(level)], Component: h.component, level: level}
	if rec.Component == "" {
		rec.Component = AppComponent
	}
	b := budget{left: maxRecord}
	rec.Msg = b.take(cleanText(msg), maxMessage)
	rec.Attrs = make([]Attr, 0, min(len(h.attrs)+len(own), maxAttrs))
	more := 0
	var walk func(prefix string, a slog.Attr)
	walk = func(prefix string, a slog.Attr) {
		secret := isStderrOnly(a.Value)
		v := a.Value.Resolve()
		if v.Kind() == slog.KindGroup {
			p := prefix
			if a.Key != "" {
				p = prefix + a.Key + "."
			}
			for _, g := range v.Group() {
				walk(p, g)
			}
			return
		}
		if a.Key == "" || (prefix == "" && a.Key == "component") {
			return // an empty attribute; the component is a field of its own
		}
		if len(rec.Attrs) == maxAttrs || b.left <= 0 {
			more++
			return
		}
		val := valueString(v)
		switch {
		case secret:
			val = redactedValue
		case anon && addrKeys[a.Key]:
			val = maskAddr(val)
		case hide && domainKeys[a.Key]:
			val = hiddenValue
		}
		key := b.take(cleanText(prefix+a.Key), maxKey)
		rec.Attrs = append(rec.Attrs, Attr{Key: key, Value: b.take(val, maxValue)})
	}
	for _, a := range h.attrs {
		walk("", a)
	}
	for _, a := range own {
		walk(prefixString(h.groups), a)
	}
	if more > 0 {
		rec.Attrs = append(rec.Attrs, Attr{Key: cutMark, Value: strconv.Itoa(more) + " more"})
	}
	return rec
}

var levelNames = [4]string{"DEBUG", "INFO", "WARN", "ERROR"}

func levelIndex(l slog.Level) int {
	switch {
	case l < slog.LevelInfo:
		return 0
	case l < slog.LevelWarn:
		return 1
	case l < slog.LevelError:
		return 2
	}
	return 3
}

func prefixString(groups []string) string {
	s := ""
	for _, g := range groups {
		s += g + "."
	}
	return s
}

// budget cuts strings to their own limit and to what is left of the
// record's total.
type budget struct{ left int }

func (b *budget) take(s string, limit int) string {
	n := min(limit, max(b.left, 0))
	if len(s) > n {
		s = cut(s, n)
	}
	b.left -= len(s)
	return s
}

// cut shortens s to at most n bytes at a rune boundary, marked with "…"
// (included in n).
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n < len(cutMark) {
		return ""
	}
	i := n - len(cutMark)
	for i > 0 && i < len(s) && s[i]&0xc0 == 0x80 {
		i--
	}
	return s[:i] + cutMark
}
