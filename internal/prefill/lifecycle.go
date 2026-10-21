package prefill

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"regexp"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/hustenreizjuengling/picache/internal/prefill/prefillapi"
)

// What the service leaves for a diagnosis (docs/ARCHITECTURE.md 20.3),
// all in the state directory: its log (log.txt, the previous part
// log.1.txt), Go's output of a fatal error (crash.log, through
// debug.SetCrashOutput) and a marker of the running process
// (running.json). A marker found at the start means the last run did not
// end by itself: with a crash.log it crashed, without one it was killed
// (the kernel's OOM killer at the unit's MemoryMax, or a hard stop). The
// last such ends are kept in exits.json and shown by the page.

const (
	fileRunning   = "running.json"
	fileCrash     = "crash.log"
	fileCrashPrev = "crash.prev"
	fileExits     = "exits.json"
	fileLog       = "log.txt"
	fileLogPrev   = "log.1.txt"

	maxLogFile   = 1 << 20 // then log.txt becomes log.1.txt
	maxExits     = 10
	maxCrashText = 6 << 10
	maxLogLines  = 1000
)

type runMarker struct {
	Started time.Time `json:"started"`
	PID     int       `json:"pid"`
	Version string    `json:"version"`
}

type exitsRec struct {
	Exits []prefillapi.Exit `json:"exits"`
}

// captureCrashes moves the crash output of the last run aside
// (crash.prev, read by previousRun) and has Go write a fatal error of
// this run into crash.log as well as to standard error.
func (s *store) captureCrashes() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.root.Rename(fileCrash, fileCrashPrev); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	f, err := s.root.OpenFile(fileCrash, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|oNoFollow, 0o600)
	if err != nil {
		return err
	}
	defer f.Close() // SetCrashOutput keeps its own descriptor
	return debug.SetCrashOutput(f, debug.CrashOptions{})
}

// readRaw reads a small file of the state directory ("" when missing).
func (s *store) readRaw(name string, limit int64) (string, time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.root.OpenFile(name, os.O_RDONLY|oNoFollow, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return "", time.Time{}, nil
	}
	if err != nil {
		return "", time.Time{}, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", time.Time{}, err
	}
	b, err := io.ReadAll(io.LimitReader(f, limit))
	return string(b), fi.ModTime(), err
}

func (s *store) modTime(name string) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	fi, err := s.root.Stat(name)
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}

// previousRun records how the last run ended when it left its marker, and
// writes the marker of this run.
func (d *Daemon) previousRun() {
	var m runMarker
	if err := d.st.load(fileRunning, &m); err != nil {
		d.log.Warn("running.json is unreadable", "err", err)
	}
	var ex exitsRec
	if err := d.st.load(fileExits, &ex); err != nil {
		ex = exitsRec{}
	}
	if !m.Started.IsZero() {
		e := prefillapi.Exit{Version: m.Version, Started: m.Started, Kind: prefillapi.ExitKilled}
		// The end: the crash output's time, else the last line of the log.
		crash, at, err := d.st.readRaw(fileCrashPrev, maxCrashText)
		if err == nil && strings.TrimSpace(crash) != "" && !at.Before(m.Started) {
			e.Kind, e.At, e.Detail = prefillapi.ExitCrash, at, redact(strings.TrimSpace(crash))
		} else {
			e.At = d.st.modTime(fileLog)
			if e.At.Before(m.Started) {
				e.At = m.Started
			}
		}
		ex.Exits = append([]prefillapi.Exit{e}, ex.Exits...)
		if len(ex.Exits) > maxExits {
			ex.Exits = ex.Exits[:maxExits]
		}
		if err := d.st.save(fileExits, ex); err != nil {
			d.log.Warn("exits.json could not be written", "err", err)
		}
		if e.Kind == prefillapi.ExitCrash {
			d.log.Error("the last run of the prefill service crashed", "started", m.Started, "version", m.Version,
				"crash", firstLine(e.Detail))
		} else {
			d.log.Error("the last run of the prefill service was ended from outside (killed, for example by the "+
				"kernel when it reached its memory limit)", "started", m.Started, "version", m.Version, "lastSignOfLife", e.At)
		}
	}
	_ = d.st.remove(fileCrashPrev)
	d.mu.Lock()
	d.exits = ex.Exits
	d.mu.Unlock()
	if err := d.st.save(fileRunning, runMarker{Started: d.started, PID: os.Getpid(), Version: d.cfg.Version}); err != nil {
		d.log.Warn("running.json could not be written", "err", err)
	}
}

// endRun removes the marker: this run ends by itself.
func (d *Daemon) endRun() {
	if err := d.st.remove(fileRunning); err != nil {
		d.log.Warn("running.json could not be removed", "err", err)
	}
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	return l
}

// Secrets never go into the log file: long token-like words are cut (the
// service never logs a token; an error text of a store might quote one).
var secretRE = regexp.MustCompile(`eg1~[A-Za-z0-9._~+/=-]+|eyJ[A-Za-z0-9._-]{20,}|[A-Za-z0-9+/_=~.-]{48,}`)

func redact(s string) string {
	return secretRE.ReplaceAllStringFunc(s, func(w string) string { return w[:min(len(w), 6)] + "…" })
}

// fileLog keeps the service's log records in log.txt (text lines, the
// previous part in log.1.txt), so they survive a crash and a restart.
type fileLogger struct {
	st *store
	mu sync.Mutex
	f  *os.File
	n  int64
}

func (s *store) openLog() (*fileLogger, error) {
	l := &fileLogger{st: s}
	return l, l.open()
}

func (l *fileLogger) open() error {
	l.st.mu.Lock()
	defer l.st.mu.Unlock()
	f, err := l.st.root.OpenFile(fileLog, os.O_WRONLY|os.O_CREATE|os.O_APPEND|oNoFollow, 0o600)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	l.f, l.n = f, fi.Size()
	return nil
}

func (l *fileLogger) write(line string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return
	}
	if l.n+int64(len(line)) > maxLogFile {
		l.f.Close()
		l.f = nil
		l.st.mu.Lock()
		_ = l.st.root.Rename(fileLog, fileLogPrev)
		l.st.mu.Unlock()
		if err := l.open(); err != nil {
			return
		}
	}
	n, _ := l.f.WriteString(line)
	l.n += int64(n)
}

func (l *fileLogger) close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		l.f.Close()
		l.f = nil
	}
}

// tail returns the last n lines of the log, oldest first.
func (l *fileLogger) tail(n int) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var lines []string
	for _, name := range []string{fileLogPrev, fileLog} {
		text, _, err := l.st.readRaw(name, maxLogFile+1)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(strings.NewReader(text))
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
		for sc.Scan() {
			lines = append(lines, sc.Text())
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

// logHandler writes records to the base handler and, as text lines, to
// the log file.
type logHandler struct {
	base  slog.Handler
	file  *fileLogger
	attrs []slog.Attr
	group string
}

func (h *logHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.base.Enabled(ctx, l)
}

func (h *logHandler) Handle(ctx context.Context, r slog.Record) error {
	var b bytes.Buffer
	b.WriteString(r.Time.UTC().Format(time.RFC3339))
	b.WriteByte(' ')
	b.WriteString(r.Level.String())
	b.WriteByte(' ')
	b.WriteString(r.Message)
	write := func(a slog.Attr) {
		if a.Key == "component" {
			return
		}
		key := a.Key
		if h.group != "" {
			key = h.group + "." + key
		}
		fmt.Fprintf(&b, " %s=%s", key, quote(a.Value.Resolve().String()))
	}
	for _, a := range h.attrs {
		write(a)
	}
	r.Attrs(func(a slog.Attr) bool { write(a); return true })
	b.WriteByte('\n')
	h.file.write(redact(b.String()))
	return h.base.Handle(ctx, r)
}

func quote(v string) string {
	if v == "" || strings.ContainsAny(v, " \"=\n") {
		return fmt.Sprintf("%q", v)
	}
	return v
}

func (h *logHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return &logHandler{base: h.base.WithAttrs(as), file: h.file, attrs: append(slices.Clone(h.attrs), as...), group: h.group}
}

func (h *logHandler) WithGroup(name string) slog.Handler {
	g := name
	if h.group != "" {
		g = h.group + "." + name
	}
	return &logHandler{base: h.base.WithGroup(name), file: h.file, attrs: h.attrs, group: g}
}
