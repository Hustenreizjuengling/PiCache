package applog

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Extra sinks of the application log (docs/ARCHITECTURE.md 4, DEPLOYMENT
// "Log file and syslog"): a log file (PICACHE_LOG_FILE) and a syslog server
// (PICACHE_LOG_SYSLOG). Both are off by default and sit beside stderr,
// which always continues. They receive exactly the records stderr
// receives, after the same redaction and the ring's privacy masking
// (anonymised client addresses, hidden domains; a StderrOnly value is
// "[redacted]"). Each sink is asynchronous: the record is formatted in the
// caller and queued (1024 records; full: dropped and counted), a goroutine
// writes it; a slog call never blocks on a sink. A file that cannot be
// opened or written, or an unreachable syslog server, never stops PiCache:
// the sink retries (the file every minute, syslog with a backoff of 1 s
// doubling to 60 s), counts the records it loses and reports its state
// (Sinks: the health check "logging", GET /system/log).

// Bounds and intervals of the sinks.
const (
	sinkQueue          = 1024
	fileRotateBytes    = 10 << 20
	fileGenerations    = 5
	fileRetry          = time.Minute
	syslogWriteTimeout = 5 * time.Second
	syslogMaxDatagram  = 2048
	syslogMaxBackoff   = time.Minute
	syslogFacility     = 3 // daemon
)

// SinkState is the state of a sink (api.LogSink).
type SinkState struct {
	Kind    string `json:"kind"`   // file | syslog
	Target  string `json:"target"` // the file's path, or host:port
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
	Dropped int64  `json:"dropped"`
}

// Sink kinds.
const (
	SinkFile   = "file"
	SinkSyslog = "syslog"
)

// sink is one extra destination of the records.
type sink struct {
	kind, target string
	format       func(rec sinkRecord) []byte
	queue        chan []byte
	dropped      atomic.Int64
	mu           sync.Mutex
	err          string // the last error ("" = ok)
	ok           bool   // written successfully since the last error
	run          func(ctx context.Context, s *sink)
}

// sinkRecord is a redacted and masked record in the form of the sinks.
type sinkRecord struct {
	t         time.Time
	level     slog.Level
	msg       string
	component string
	attrs     []slog.Attr // the handler's attributes (with their groups) and the record's own
}

// setState records the result of a write or connection attempt.
func (s *sink) setState(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil {
		s.ok, s.err = true, ""
		return
	}
	s.ok, s.err = false, cleanText(err.Error())
}

func (s *sink) state() SinkState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return SinkState{Kind: s.kind, Target: s.target, OK: s.ok && s.err == "", Error: s.err, Dropped: s.dropped.Load()}
}

// enqueue queues a formatted record (dropped and counted when full).
func (s *sink) enqueue(b []byte) {
	select {
	case s.queue <- b:
	default:
		s.dropped.Add(1)
	}
}

// sinks are the extra sinks of a Log (set before the first record).
type sinks struct {
	mu      sync.Mutex
	list    []*sink
	started bool
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

// AddFileSink adds the log file (PICACHE_LOG_FILE) in the format of
// PICACHE_LOG_FORMAT (text or json). Records are queued until StartSinks.
func (l *Log) AddFileSink(path, format string) {
	s := &sink{kind: SinkFile, target: path, queue: make(chan []byte, sinkQueue), ok: true}
	json := format == "json"
	s.format = func(rec sinkRecord) []byte { return formatRecord(rec, json) }
	s.run = func(ctx context.Context, s *sink) { runFile(ctx, s, path) }
	l.addSink(s)
}

// AddSyslogSink adds a syslog server (PICACHE_LOG_SYSLOG): network "udp"
// or "tcp", addr host:port (a name is resolved at connect), hostname the
// HOSTNAME of the messages.
func (l *Log) AddSyslogSink(network, addr, hostname string) {
	s := &sink{kind: SinkSyslog, target: addr, queue: make(chan []byte, sinkQueue), ok: true}
	host := syslogHostname(hostname)
	pid := strconv.Itoa(os.Getpid())
	s.format = func(rec sinkRecord) []byte { return formatSyslog(rec, host, pid) }
	s.run = func(ctx context.Context, s *sink) { runSyslog(ctx, s, network, addr) }
	l.addSink(s)
}

func (l *Log) addSink(s *sink) {
	l.sinks.mu.Lock()
	defer l.sinks.mu.Unlock()
	l.sinks.list = append(l.sinks.list, s)
	l.sinkList.Store(&l.sinks.list)
}

// StartSinks starts the writers of the sinks (the app calls it after the
// privilege drop, so a log file is never created as root). Records logged
// before wait in the queues.
func (l *Log) StartSinks() {
	l.sinks.mu.Lock()
	defer l.sinks.mu.Unlock()
	if l.sinks.started {
		return
	}
	l.sinks.started = true
	ctx, cancel := context.WithCancel(context.Background())
	l.sinks.cancel = cancel
	for _, s := range l.sinks.list {
		l.sinks.wg.Go(func() { s.run(ctx, s) })
	}
}

// CloseSinks stops the writers after they wrote what is queued, waiting
// at most d.
func (l *Log) CloseSinks(d time.Duration) {
	l.sinks.mu.Lock()
	cancel := l.sinks.cancel
	l.sinks.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	done := make(chan struct{})
	go func() { l.sinks.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
	}
}

// Sinks returns the state of every configured sink (empty without any).
func (l *Log) Sinks() []SinkState {
	out := []SinkState{}
	if p := l.sinkList.Load(); p != nil {
		for _, s := range *p {
			out = append(out, s.state())
		}
	}
	return out
}

// toSinks formats a record for every sink and queues it.
func (h *Handler) toSinks(r slog.Record, own []slog.Attr) {
	p := h.l.sinkList.Load()
	if p == nil || len(*p) == 0 {
		return
	}
	anon, hide := flag(&h.l.anon), flag(&h.l.hide)
	attrs := make([]slog.Attr, 0, len(h.attrs)+1)
	for _, a := range h.attrs {
		attrs = append(attrs, maskAttr(a, anon, hide))
	}
	for _, a := range prefixed(h.groups, own) {
		attrs = append(attrs, maskAttr(a, anon, hide))
	}
	rec := sinkRecord{t: r.Time, level: r.Level, msg: r.Message, component: h.component, attrs: attrs}
	for _, s := range *p {
		s.enqueue(s.format(rec))
	}
}

// maskAttr applies the ring's privacy masking to an attribute (any group
// depth) and redacts StderrOnly values.
func maskAttr(a slog.Attr, anon, hide bool) slog.Attr {
	if isStderrOnly(a.Value) {
		return slog.String(a.Key, redactedValue)
	}
	v := a.Value.Resolve()
	if v.Kind() == slog.KindGroup {
		g := v.Group()
		out := make([]slog.Attr, len(g))
		for i, x := range g {
			out[i] = maskAttr(x, anon, hide)
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(out...)}
	}
	switch {
	case anon && addrKeys[a.Key]:
		return slog.String(a.Key, maskAddr(valueString(v)))
	case hide && domainKeys[a.Key]:
		return slog.String(a.Key, hiddenValue)
	}
	return slog.Attr{Key: a.Key, Value: v}
}

// formatRecord formats a record with slog's text or JSON handler (one
// line).
func formatRecord(rec sinkRecord, json bool) []byte {
	var buf bytes.Buffer
	opts := &slog.HandlerOptions{Level: slog.LevelDebug}
	var h slog.Handler
	if json {
		h = slog.NewJSONHandler(&buf, opts)
	} else {
		h = slog.NewTextHandler(&buf, opts)
	}
	r := slog.NewRecord(rec.t, rec.level, rec.msg, 0)
	r.AddAttrs(rec.attrs...)
	_ = h.Handle(context.Background(), r)
	return buf.Bytes()
}

// --- syslog (RFC 5424) ---

// syslogSeverity maps a level: DEBUG 7, INFO 6, WARN 4, ERROR 3.
func syslogSeverity(l slog.Level) int {
	switch {
	case l < slog.LevelInfo:
		return 7
	case l < slog.LevelWarn:
		return 6
	case l < slog.LevelError:
		return 4
	}
	return 3
}

// formatSyslog formats "<PRI>1 TIMESTAMP HOSTNAME picache PROCID MSGID -
// MSG": facility daemon, MSGID the record's component ("-" without one),
// MSG the text format with every control character (newlines included)
// escaped as \uXXXX.
func formatSyslog(rec sinkRecord, host, pid string) []byte {
	msgid := "-"
	if c := syslogToken(rec.component, 32); c != "" {
		msgid = c
	}
	text := strings.TrimRight(string(formatRecord(rec, false)), "\n")
	var b strings.Builder
	fmt.Fprintf(&b, "<%d>1 %s %s picache %s %s - ", syslogFacility*8+syslogSeverity(rec.level),
		rec.t.UTC().Format("2006-01-02T15:04:05.000000Z07:00"), host, pid, msgid)
	b.WriteString(escapeControls(text))
	return []byte(b.String())
}

// escapeControls escapes C0 and C1 controls and DEL as \uXXXX.
func escapeControls(s string) string {
	var b strings.Builder
	for _, r := range strings.ToValidUTF8(s, "�") {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			fmt.Fprintf(&b, `\u%04x`, r)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// syslogToken returns s as a header field: printable ASCII without
// spaces, at most n characters ("" if nothing is left).
func syslogToken(s string, n int) string {
	var b strings.Builder
	for i := 0; i < len(s) && b.Len() < n; i++ {
		if c := s[i]; c > 0x20 && c < 0x7f {
			b.WriteByte(c)
		}
	}
	return b.String()
}

// syslogHostname returns the HOSTNAME field ("-" when unknown).
func syslogHostname(h string) string {
	if t := syslogToken(h, 255); t != "" {
		return t
	}
	return "-"
}

// syslogDial is the dialer of the syslog sink (replaced in tests).
var syslogDial = func(ctx context.Context, network, addr string) (net.Conn, error) {
	d := net.Dialer{Timeout: syslogWriteTimeout}
	return d.DialContext(ctx, network, addr)
}

// runSyslog sends the queued records: UDP one record per datagram (cut to
// 2048 bytes), TCP with octet-counting framing (RFC 6587); a failed
// connection or write reconnects with a backoff of 1 s doubling to 60 s,
// and the records meanwhile queue (and are dropped when the queue is full).
func runSyslog(ctx context.Context, s *sink, network, addr string) {
	var conn net.Conn
	backoff := time.Second
	defer func() {
		if conn != nil {
			conn.Close()
		}
	}()
	var pending []byte
	for {
		if pending == nil {
			select {
			case <-ctx.Done():
				drainSyslog(s, conn, network)
				return
			case pending = <-s.queue:
			}
		}
		if conn == nil {
			c, err := syslogDial(ctx, network, addr)
			if err != nil {
				s.setState(err)
				if !sleepCtx(ctx, backoff) {
					return
				}
				backoff = min(backoff*2, syslogMaxBackoff)
				continue
			}
			conn = c
		}
		if err := writeSyslog(conn, network, pending); err != nil {
			s.setState(err)
			conn.Close()
			conn = nil
			s.dropped.Add(1) // this record is lost; the connection is retried
			pending = nil
			if !sleepCtx(ctx, backoff) {
				return
			}
			backoff = min(backoff*2, syslogMaxBackoff)
			continue
		}
		s.setState(nil)
		backoff = time.Second
		pending = nil
	}
}

// drainSyslog writes what is queued at shutdown (best effort, while the
// connection works).
func drainSyslog(s *sink, conn net.Conn, network string) {
	for conn != nil {
		select {
		case b := <-s.queue:
			if writeSyslog(conn, network, b) != nil {
				return
			}
		default:
			return
		}
	}
}

func writeSyslog(conn net.Conn, network string, msg []byte) error {
	_ = conn.SetWriteDeadline(time.Now().Add(syslogWriteTimeout))
	if network == "udp" {
		if len(msg) > syslogMaxDatagram {
			msg = msg[:syslogMaxDatagram]
		}
		_, err := conn.Write(msg)
		return err
	}
	frame := append([]byte(strconv.Itoa(len(msg))+" "), msg...)
	_, err := conn.Write(frame)
	return err
}

// sleepCtx waits d; false when ctx ended.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// --- the log file ---

// errLink is the error of a symbolic link in place of the log file.
var errLink = errors.New("a symbolic link is in place of the log file")

// logFile is an open log file with its size.
type logFile struct {
	f    *os.File
	size int64
}

// runFile writes the queued records to the file: opened with
// O_WRONLY|O_APPEND|O_CREAT|O_NOFOLLOW (0640), rotated at 10 MiB; an open
// or write error drops the records (counted) and the file is opened again
// a minute later.
func runFile(ctx context.Context, s *sink, path string) {
	var lf *logFile
	var retryAt time.Time
	var compress sync.WaitGroup
	// rotateErr keeps a failed rotation visible while the file keeps
	// growing (cleared by the next rotation that works).
	var rotateErr error
	defer func() {
		if lf != nil {
			lf.f.Close()
		}
		compress.Wait()
	}()
	write := func(b []byte) {
		if lf == nil {
			if time.Now().Before(retryAt) {
				s.dropped.Add(1)
				return
			}
			f, size, err := openLogFile(path)
			if err != nil {
				s.setState(err)
				retryAt = time.Now().Add(fileRetry)
				s.dropped.Add(1)
				return
			}
			lf = &logFile{f: f, size: size}
		}
		if lf.size > 0 && lf.size+int64(len(b)) > fileRotateBytes {
			lf.f.Close()
			lf = nil
			compress.Wait()
			rotateErr = rotateLogFiles(path, &compress)
			f, size, err := openLogFile(path)
			if err != nil {
				s.setState(err)
				retryAt = time.Now().Add(fileRetry)
				s.dropped.Add(1)
				return
			}
			lf = &logFile{f: f, size: size}
		}
		n, err := lf.f.Write(b)
		lf.size += int64(n)
		if err != nil {
			s.setState(err)
			lf.f.Close()
			lf = nil
			retryAt = time.Now().Add(fileRetry)
			s.dropped.Add(1)
			return
		}
		if rotateErr != nil {
			s.setState(fmt.Errorf("rotation failed: %w", rotateErr))
			return
		}
		s.setState(nil)
	}
	for {
		select {
		case <-ctx.Done():
			for {
				select {
				case b := <-s.queue:
					write(b)
				default:
					return
				}
			}
		case b := <-s.queue:
			write(b)
		}
	}
}

// rotateLogFiles shifts the generations: <name>.4.gz → <name>.5.gz, …,
// <name>.1.gz → <name>.2.gz (the oldest beyond 5 removed), then <name> →
// <name>.1, which is gzip-compressed to <name>.1.gz in the background.
// Everything stays in the file's own directory.
func rotateLogFiles(path string, compress *sync.WaitGroup) error {
	gen := func(n int) string { return path + "." + strconv.Itoa(n) }
	_ = os.Remove(gen(fileGenerations) + ".gz")
	_ = os.Remove(gen(fileGenerations))
	for n := fileGenerations - 1; n >= 1; n-- {
		for _, sfx := range []string{".gz", ""} {
			if _, err := os.Lstat(gen(n) + sfx); err == nil {
				if err := os.Rename(gen(n)+sfx, gen(n+1)+sfx); err != nil {
					return err
				}
			}
		}
	}
	if err := os.Rename(path, gen(1)); err != nil {
		return err
	}
	compress.Go(func() { compressFiles(path) })
	return nil
}

// compressFiles gzips every uncompressed generation <name>.<n> to
// <name>.<n>.gz (through a temporary file) and removes it.
func compressFiles(path string) {
	for n := 1; n <= fileGenerations; n++ {
		src := path + "." + strconv.Itoa(n)
		if fi, err := os.Lstat(src); err != nil || !fi.Mode().IsRegular() {
			continue
		}
		if err := gzipFile(src, src+".gz"); err == nil {
			_ = os.Remove(src)
		}
	}
}

func gzipFile(src, dst string) error {
	in, err := os.OpenFile(src, os.O_RDONLY|oNoFollow, 0)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := filepath.Join(filepath.Dir(dst), "."+filepath.Base(dst)+".tmp")
	_ = os.Remove(tmp)
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|oNoFollow, 0o640)
	if err != nil {
		return err
	}
	zw := gzip.NewWriter(out)
	_, err = io.Copy(zw, in)
	if cerr := zw.Close(); err == nil {
		err = cerr
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
