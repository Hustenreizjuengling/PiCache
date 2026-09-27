package applog

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// waitFor polls cond for up to 5 s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timeout waiting for %s", what)
}

// The file gets what stderr gets (level, redaction) plus the ring's
// privacy masking; the setup token (StderrOnly) is redacted.
func TestFileSinkContentAndMasking(t *testing.T) {
	log, l, stderr := newTest(slog.LevelInfo)
	path := filepath.Join(t.TempDir(), "picache.log")
	l.AddFileSink(path, "text")
	anon, hide := true, true
	l.SetPrivacy(func() bool { return anon }, func() bool { return hide })
	l.StartSinks()
	defer l.CloseSinks(time.Second)
	dns := log.With(slog.String("component", "dns"))
	dns.Debug("not at info level")
	dns.Info("query", slog.String("client", "192.168.178.23"), slog.String("qname", "secret.example"),
		slog.String("password", "hunter2"), slog.String("url", "https://u:p@h.example/x?token=abc"),
		slog.Group("g", slog.String("ip", "2001:db8:1:2::5")))
	dns.Info("setup", StderrOnly("setupToken", "tok-123"))
	waitFor(t, "two lines", func() bool {
		b, _ := os.ReadFile(path)
		return strings.Count(string(b), "\n") == 2
	})
	b, _ := os.ReadFile(path)
	s := string(b)
	for _, bad := range []string{"not at info level", "192.168.178.23", "secret.example", "hunter2", "token=abc", "u:p@", "2001:db8:1:2::5", "tok-123"} {
		if strings.Contains(s, bad) {
			t.Fatalf("file contains %q:\n%s", bad, s)
		}
	}
	for _, want := range []string{"client=192.168.0.0", "qname=[hidden]", "password=[redacted]", "url=https://h.example/x", "g.ip=2001:db8:1::", "setupToken=[redacted]"} {
		if !strings.Contains(s, want) {
			t.Fatalf("file lacks %q:\n%s", want, s)
		}
	}
	// stderr is unchanged by the sinks (not masked, the token visible).
	if !strings.Contains(stderr.String(), "192.168.178.23") || !strings.Contains(stderr.String(), "tok-123") {
		t.Fatalf("stderr %s", stderr.String())
	}
	st := l.Sinks()
	if len(st) != 1 || st[0].Kind != SinkFile || st[0].Target != path || !st[0].OK || st[0].Dropped != 0 {
		t.Fatalf("state %+v", st)
	}
	fi, err := os.Stat(path)
	if err != nil || (filepath.Separator == '/' && fi.Mode().Perm() != 0o640) {
		t.Fatalf("mode %v %v", fi, err)
	}
}

func TestFileSinkRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "picache.log")
	// A leftover uncompressed generation and five compressed ones.
	for n := 1; n <= fileGenerations; n++ {
		os.WriteFile(path+"."+strconv.Itoa(n)+".gz", []byte("old"+strconv.Itoa(n)), 0o640)
	}
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), fileRotateBytes-10), 0o640); err != nil {
		t.Fatal(err)
	}
	log, l, _ := newTest(slog.LevelInfo)
	l.AddFileSink(path, "json")
	l.StartSinks()
	log.Info("after the rotation")
	// CloseSinks writes what is queued and waits for the compression (no
	// reader meanwhile: it would keep the rename from working on Windows).
	l.CloseSinks(5 * time.Second)
	b, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(b), `{"time":`) {
		t.Fatalf("json format: %.80s (%d bytes; state %+v)", b, len(b), l.Sinks())
	}
	f, err := os.Open(path + ".1.gz")
	if err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	plain, _ := io.ReadAll(zr)
	f.Close()
	if len(plain) != fileRotateBytes-10 {
		t.Fatalf("generation 1 holds %d bytes", len(plain))
	}
	for n := 2; n <= fileGenerations; n++ {
		b, err := os.ReadFile(path + "." + strconv.Itoa(n) + ".gz")
		if err != nil || string(b) != "old"+strconv.Itoa(n-1) {
			t.Fatalf("generation %d: %q %v", n, b, err)
		}
	}
	if _, err := os.Stat(path + ".6.gz"); err == nil {
		t.Fatal("more than 5 generations")
	}
	if _, err := os.Stat(path + ".1"); err == nil {
		t.Fatal("the uncompressed generation is left")
	}
}

// A symbolic link in place of the file is refused (retried a minute
// later); the records are dropped and counted; stderr continues.
func TestFileSinkRefusesLink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	os.WriteFile(target, nil, 0o640)
	path := filepath.Join(dir, "picache.log")
	if err := os.Symlink(target, path); err != nil {
		t.Skip("no symbolic links:", err)
	}
	log, l, stderr := newTest(slog.LevelInfo)
	l.AddFileSink(path, "text")
	l.StartSinks()
	defer l.CloseSinks(time.Second)
	log.Info("one")
	log.Info("two")
	waitFor(t, "the error", func() bool { st := l.Sinks(); return !st[0].OK && st[0].Dropped == 2 })
	if st := l.Sinks(); !strings.Contains(st[0].Error, "symbolic link") {
		t.Fatalf("state %+v", st)
	}
	if b, _ := os.ReadFile(target); len(b) != 0 {
		t.Fatal("written through the link")
	}
	if !strings.Contains(stderr.String(), "two") {
		t.Fatal("stderr must continue")
	}
}

// A full queue drops records and counts them; the slog call never blocks.
func TestSinkQueueOverflow(t *testing.T) {
	log, l, _ := newTest(slog.LevelInfo)
	l.AddSyslogSink("udp", "127.0.0.1:9", "host")
	// Not started: nothing drains the queue.
	start := time.Now()
	for range sinkQueue + 10 {
		log.Info("x")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("slog calls blocked")
	}
	if st := l.Sinks(); len(st) != 1 || st[0].Dropped != 10 || st[0].Kind != SinkSyslog || st[0].Target != "127.0.0.1:9" {
		t.Fatalf("state %+v", st)
	}
}

func TestSyslogUDP(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	log, l, _ := newTest(slog.LevelDebug)
	l.AddSyslogSink("udp", pc.LocalAddr().String(), "pi host")
	l.StartSinks()
	defer l.CloseSinks(time.Second)
	log.With(slog.String("component", "dns")).Warn("line one\nline two\x1b[31m", slog.String("k", "v\u0085"))
	log.Error(strings.Repeat("y", 3000))
	buf := make([]byte, 65536)
	pc.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, _, err := pc.ReadFrom(buf)
	if err != nil {
		t.Fatal(err)
	}
	msg := string(buf[:n])
	pid := strconv.Itoa(os.Getpid())
	if !strings.HasPrefix(msg, "<28>1 ") || !strings.Contains(msg, " pihost picache "+pid+" dns - ") {
		t.Fatalf("header: %q", msg)
	}
	for _, c := range msg {
		if c < 0x20 || c == 0x7f || (c >= 0x80 && c <= 0x9f) {
			t.Fatalf("raw control character in %q", msg)
		}
	}
	if !strings.Contains(msg, `\u000a`) && !strings.Contains(msg, `\n`) {
		t.Fatalf("the newline must be escaped: %q", msg)
	}
	ts := strings.Fields(msg)[1]
	if _, err := time.Parse(time.RFC3339Nano, ts); err != nil {
		t.Fatalf("timestamp %q: %v", ts, err)
	}
	n, _, err = pc.ReadFrom(buf)
	if err != nil {
		t.Fatal(err)
	}
	if n != syslogMaxDatagram || !strings.HasPrefix(string(buf[:n]), "<27>1 ") || !strings.Contains(string(buf[:n]), " - - ") {
		t.Fatalf("second datagram (%d bytes): %q", n, buf[:40])
	}
}

// TCP: octet-counting framing; an unreachable server is retried and
// reported.
func TestSyslogTCP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	got := make(chan string, 4)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		br := bufio.NewReader(c)
		for {
			l, err := br.ReadString(' ')
			if err != nil {
				return
			}
			n, _ := strconv.Atoi(strings.TrimSpace(l))
			b := make([]byte, n)
			if _, err := io.ReadFull(br, b); err != nil {
				return
			}
			got <- string(b)
		}
	}()
	log, l, _ := newTest(slog.LevelInfo)
	l.AddSyslogSink("tcp", addr, "host")
	l.StartSinks()
	defer l.CloseSinks(time.Second)
	log.Info("first")
	log.Info("second")
	for _, want := range []string{"first", "second"} {
		select {
		case m := <-got:
			if !strings.HasPrefix(m, "<30>1 ") || !strings.Contains(m, "msg="+want) {
				t.Fatalf("frame %q", m)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no frame")
		}
	}
	waitFor(t, "ok", func() bool { return l.Sinks()[0].OK })

	// Unreachable.
	ln2, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := ln2.Addr().String()
	ln2.Close()
	log2, l2, _ := newTest(slog.LevelInfo)
	l2.AddSyslogSink("tcp", dead, "host")
	l2.StartSinks()
	defer l2.CloseSinks(time.Second)
	log2.Info("lost")
	waitFor(t, "the error", func() bool { st := l2.Sinks()[0]; return !st.OK && st.Error != "" })
}

func TestFormatSyslogFields(t *testing.T) {
	rec := sinkRecord{t: time.Date(2026, 9, 27, 10, 0, 0, 123456000, time.UTC), level: slog.LevelDebug, msg: "m",
		component: "we b\x00"}
	b := string(formatSyslog(rec, syslogHostname(""), "42"))
	if !strings.HasPrefix(b, "<31>1 2026-09-27T10:00:00.123456Z - picache 42 web - ") {
		t.Fatalf("%q", b)
	}
	_ = context.Background()
}
