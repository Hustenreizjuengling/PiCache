package applog

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// newTest returns a logger whose stderr sink writes JSON to buf.
func newTest(base slog.Level) (*slog.Logger, *Log, *bytes.Buffer) {
	var buf bytes.Buffer
	h := New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}), base)
	return slog.New(h), h.Log(), &buf
}

func TestRingKeepsLastRecords(t *testing.T) {
	log, l, _ := newTest(slog.LevelInfo)
	for i := range Capacity + 50 {
		log.Info("m", slog.Int("i", i))
	}
	log.Debug("not kept") // below the base level
	recs := l.Records(slog.LevelDebug, "", Capacity+100)
	if len(recs) != Capacity || recs[0].Attrs[0].Value != fmt.Sprint(Capacity+49) || recs[len(recs)-1].Attrs[0].Value != "50" {
		t.Fatalf("%d records, newest %v, oldest %v", len(recs), recs[0].Attrs, recs[len(recs)-1].Attrs)
	}
	if recs[0].Seq != Capacity+50 || recs[0].Level != "INFO" || recs[0].Component != AppComponent {
		t.Fatalf("newest %+v", recs[0])
	}
	if got := l.Records(slog.LevelDebug, "", 3); len(got) != 3 || got[0].Seq != recs[0].Seq {
		t.Fatalf("limit: %+v", got)
	}
}

func TestRecordBounds(t *testing.T) {
	log, l, _ := newTest(slog.LevelDebug)
	var attrs []any
	for i := range 40 {
		attrs = append(attrs, slog.String(fmt.Sprintf("k%02d", i), "v"))
	}
	log.Info(strings.Repeat("é", 400), attrs...) // 800-byte message
	r := l.Records(slog.LevelDebug, "", 1)[0]
	if len(r.Msg) > maxMessage || !strings.HasSuffix(r.Msg, cutMark) {
		t.Fatalf("message %d bytes", len(r.Msg))
	}
	if len(r.Attrs) != maxAttrs+1 || r.Attrs[maxAttrs] != (Attr{Key: cutMark, Value: "8 more"}) {
		t.Fatalf("attrs %d, last %+v", len(r.Attrs), r.Attrs[len(r.Attrs)-1])
	}
	log.Info("big", slog.String(strings.Repeat("k", 100), strings.Repeat("v", 1000)),
		slog.String("a", strings.Repeat("x", 600)), slog.String("b", strings.Repeat("y", 600)),
		slog.String("c", strings.Repeat("z", 600)), slog.String("d", "tail"))
	r = l.Records(slog.LevelDebug, "", 1)[0]
	total := len(r.Msg)
	for _, a := range r.Attrs {
		if a.Key == cutMark {
			continue
		}
		total += len(a.Key) + len(a.Value)
		if len(a.Key) > maxKey || len(a.Value) > maxValue {
			t.Fatalf("attr %d/%d bytes", len(a.Key), len(a.Value))
		}
	}
	if total > maxRecord || r.Attrs[0].Key != strings.Repeat("k", maxKey-len(cutMark))+cutMark {
		t.Fatalf("record %d bytes: %+v", total, r.Attrs)
	}
	if last := r.Attrs[len(r.Attrs)-1]; last.Key != cutMark {
		t.Fatalf("dropped attributes not counted: %+v", last)
	}
	// Groups are flattened; the component is a field.
	log.With("component", "dns").WithGroup("g").Info("grouped", slog.Group("h", slog.Int("x", 1)), slog.Int("y", 2))
	r = l.Records(slog.LevelDebug, "", 1)[0]
	if r.Component != "dns" || fmt.Sprint(r.Attrs) != "[{g.h.x 1} {g.y 2}]" {
		t.Fatalf("grouped %+v", r)
	}
}

// Secrets and URL parts are removed from both sinks, at any group depth.
func TestRedactionBothSinks(t *testing.T) {
	log, l, buf := newTest(slog.LevelDebug)
	log.With(slog.String("api_key", "k-secret")).Info("x",
		slog.String("password", "p-secret"), slog.String("Current-Password", "p2-secret"),
		slog.Group("outer", slog.Group("inner", slog.String("token", "t-secret"), slog.String("fine", "ok"))),
		slog.String("url", "https://user:u-secret@dns.example:8443/dns-query?q-secret=1#f-secret"),
		slog.String("plain", "https://dns.example/path"), slog.Any("u", mustURL("http://a.example/p?x=q-secret")))
	out := buf.String()
	r := l.Records(slog.LevelDebug, "", 1)[0]
	ring := fmt.Sprint(r.Attrs)
	for _, sink := range []string{out, ring} {
		for _, s := range []string{"k-secret", "p-secret", "p2-secret", "t-secret", "u-secret", "q-secret", "f-secret"} {
			if strings.Contains(sink, s) {
				t.Errorf("%q leaked: %s", s, sink)
			}
		}
		for _, s := range []string{"https://dns.example:8443/dns-query", "https://dns.example/path", "http://a.example/p", "[redacted]", "ok"} {
			if !strings.Contains(sink, s) {
				t.Errorf("%q missing: %s", s, sink)
			}
		}
	}
}

// A StderrOnly value (the setup token) reaches stderr unredacted although
// its key is a secret's name; the ring redacts it. The same key without
// StderrOnly is redacted in both sinks.
func TestStderrOnly(t *testing.T) {
	log, l, buf := newTest(slog.LevelDebug)
	log.Warn("first-run setup required", StderrOnly("setupToken", "TOKEN234567"), slog.String("file", "/data/setup-token"))
	if !strings.Contains(buf.String(), `"setupToken":"TOKEN234567"`) {
		t.Fatalf("stderr: %s", buf)
	}
	r := l.Records(slog.LevelDebug, "", 1)[0]
	if got := fmt.Sprint(r.Attrs); got != "[{setupToken [redacted]} {file /data/setup-token}]" {
		t.Fatalf("ring %s", got)
	}
	buf.Reset()
	log.With(slog.Group("g", StderrOnly("setupToken", "GROUPED2345"))).Info("x", slog.String("setupToken", "PLAIN234567"))
	if out := buf.String(); !strings.Contains(out, "GROUPED2345") || strings.Contains(out, "PLAIN234567") {
		t.Fatalf("stderr: %s", out)
	}
	if got := fmt.Sprint(l.Records(slog.LevelDebug, "", 1)[0].Attrs); strings.Contains(got, "2345") {
		t.Fatalf("ring %s", got)
	}
}

func mustURL(s string) any {
	u, err := urlParse(s)
	if err != nil {
		panic(err)
	}
	return u
}

// The ring masks client addresses while anonymised and hides domains while
// hidden; stderr keeps them (journald is not covered).
func TestPrivacyMasking(t *testing.T) {
	log, l, buf := newTest(slog.LevelDebug)
	anon, hide := true, true
	l.SetPrivacy(func() bool { return anon }, func() bool { return hide })
	log.Info("q", slog.String("client", "192.168.17.42"), slog.String("peer", "[2001:db8:1:2::5]:53"),
		slog.String("source", "10.1.2.0/24"), slog.String("ip", "fe80::1%eth0"), slog.String("addr", "nas.lan"),
		slog.String("qname", "secret.example"), slog.String("sni", "cdn.example"), slog.String("other", "192.168.17.42"))
	r := l.Records(slog.LevelDebug, "", 1)[0]
	want := "[{client 192.168.0.0} {peer [2001:db8:1::]:53} {source 10.1.0.0/16} {ip fe80::} {addr nas.lan} " +
		"{qname [hidden]} {sni [hidden]} {other 192.168.17.42}]"
	if got := fmt.Sprint(r.Attrs); got != want {
		t.Fatalf("ring %s\nwant %s", got, want)
	}
	if !strings.Contains(buf.String(), "192.168.17.42") || !strings.Contains(buf.String(), "secret.example") {
		t.Fatalf("stderr masked: %s", buf)
	}
	anon, hide = false, false
	log.Info("q", slog.String("client", "192.168.17.42"), slog.String("qname", "secret.example"))
	if got := fmt.Sprint(l.Records(slog.LevelDebug, "", 1)[0].Attrs); got != "[{client 192.168.17.42} {qname secret.example}]" {
		t.Fatalf("switches off: %s", got)
	}
}

// A slow subscriber never blocks logging; beyond 4 streams Subscribe fails.
func TestFanOut(t *testing.T) {
	log, l, _ := newTest(slog.LevelDebug)
	slow, cancelSlow, err := l.Subscribe(slog.LevelDebug, "")
	if err != nil {
		t.Fatal(err)
	}
	warn, cancelWarn, err := l.Subscribe(slog.LevelWarn, "dns")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		for i := range subBuffer + 44 {
			log.Info("x", slog.Int("i", i))
		}
		log.With("component", "dns").Warn("dns warning")
		log.With("component", "logs").Warn("logs warning")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("logging blocked on a slow subscriber")
	}
	if len(slow) != subBuffer || l.Dropped() != 46 {
		t.Fatalf("buffered %d, dropped %d", len(slow), l.Dropped())
	}
	if len(warn) != 1 || (<-warn).Msg != "dns warning" {
		t.Fatal("filtered stream")
	}
	var cancels []func()
	for range MaxSubs - 2 {
		_, c, err := l.Subscribe(slog.LevelDebug, "")
		if err != nil {
			t.Fatal(err)
		}
		cancels = append(cancels, c)
	}
	if _, _, err := l.Subscribe(slog.LevelDebug, ""); !errors.Is(err, ErrTooMany) {
		t.Fatalf("fifth stream: %v", err)
	}
	cancelSlow()
	cancelSlow() // idempotent
	if _, c, err := l.Subscribe(slog.LevelDebug, ""); err != nil {
		t.Fatalf("after a cancel: %v", err)
	} else {
		c()
	}
	cancelWarn()
	for _, c := range cancels {
		c()
	}
}

// The runtime debug level: debug or info, more verbose than the base
// level, for all components or one; it ends at its time or at once, with a
// warning at the start and an info line at the end.
func TestOverride(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		log, l, _ := newTest(slog.LevelInfo)
		dnsLog := log.With("component", "dns")
		for _, tc := range []struct {
			level, component string
			d                time.Duration
			want             error
		}{
			{"info", "", time.Minute, ErrLevel}, // not more verbose than info
			{"warn", "", time.Minute, ErrLevel},
			{"trace", "", time.Minute, ErrLevel},
			{"debug", "nope", time.Minute, ErrComponent},
			{"debug", "", 0, ErrMinutes},
			{"debug", "", 241 * time.Minute, ErrMinutes},
		} {
			if _, err := l.SetOverride(tc.level, tc.component, tc.d); !errors.Is(err, tc.want) {
				t.Errorf("%+v: %v", tc, err)
			}
		}
		o, err := l.SetOverride("debug", "dns", 10*time.Minute)
		if err != nil || o.Component != "dns" || !o.Until.Equal(time.Now().Add(10*time.Minute).UTC().Truncate(time.Second)) {
			t.Fatalf("override %+v, %v", o, err)
		}
		if r := l.Records(slog.LevelDebug, "", 1)[0]; r.Level != "WARN" || !strings.Contains(r.Msg, "debug logging on for dns until") {
			t.Fatalf("start line %+v", r)
		}
		dnsLog.Debug("dns debug")
		log.With("component", "logs").Debug("logs debug")
		if r := l.Records(slog.LevelDebug, "", 1)[0]; r.Msg != "dns debug" {
			t.Fatalf("latest %+v", r)
		}
		time.Sleep(10*time.Minute + time.Second)
		synctest.Wait()
		if l.CurrentOverride() != nil {
			t.Fatal("override not ended")
		}
		if r := l.Records(slog.LevelDebug, "", 1)[0]; r.Level != "INFO" || r.Msg != "debug logging ended" {
			t.Fatalf("end line %+v", r)
		}
		dnsLog.Debug("after")
		if r := l.Records(slog.LevelDebug, "", 1)[0]; r.Msg == "after" {
			t.Fatal("debug record kept after the end")
		}
		// All components, replaced, then ended at once.
		if _, err := l.SetOverride("debug", "", time.Hour); err != nil {
			t.Fatal(err)
		}
		if _, err := l.SetOverride("debug", "", 2*time.Hour); err != nil {
			t.Fatal(err)
		}
		log.With("component", "logs").Debug("logs debug 2")
		if r := l.Records(slog.LevelDebug, "", 1)[0]; r.Msg != "logs debug 2" {
			t.Fatalf("all components: %+v", r)
		}
		l.ClearOverride()
		l.ClearOverride() // without one: nothing
		if l.CurrentOverride() != nil || l.Records(slog.LevelDebug, "", 1)[0].Msg != "debug logging ended" {
			t.Fatal("clear")
		}
		time.Sleep(3 * time.Hour) // the replaced and cleared timers do nothing
		synctest.Wait()
		ends := 0
		for _, r := range l.Records(slog.LevelDebug, "", Capacity) {
			if r.Msg == "debug logging ended" {
				ends++
			}
		}
		if ends != 2 {
			t.Fatalf("%d end lines", ends)
		}
	})
	_, l, _ := newTest(slog.LevelWarn)
	if _, err := l.SetOverride("info", "", time.Minute); err != nil {
		t.Fatalf("info below warn: %v", err)
	}
	_, l, _ = newTest(slog.LevelDebug)
	if _, err := l.SetOverride("debug", "", time.Minute); !errors.Is(err, ErrLevel) {
		t.Fatalf("debug at base debug: %v", err)
	}
}

// Every "component" literal in the source is one of Components.
func TestComponentsComplete(t *testing.T) {
	re := regexp.MustCompile(`(?:slog\.String\(|With\()\s*"component",\s*"([^"]+)"`)
	found := 0
	for _, root := range []string{"../../internal", "../../cmd"} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, m := range re.FindAllStringSubmatch(string(b), -1) {
				found++
				if !ValidComponent(m[1]) {
					t.Errorf("%s: component %q is not in applog.Components", path, m[1])
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if found < 20 {
		t.Fatalf("only %d component literals found", found)
	}
}

func TestMaskAddr(t *testing.T) {
	for in, want := range map[string]string{
		"10.20.30.40": "10.20.0.0", "::ffff:10.20.30.40": "10.20.0.0", "2001:db8:aa:bb::1": "2001:db8:aa::",
		"10.20.30.40:53": "10.20.0.0:53", "10.0.0.0/8": "10.0.0.0/8", "2001:db8::/64": "2001:db8::/48",
		"printer": "printer", "": "",
	} {
		if got := maskAddr(in); got != want {
			t.Errorf("maskAddr(%q) = %q, want %q", in, got, want)
		}
	}
}

var urlParse = url.Parse
