package app

import (
	"net"
	"strings"
	"testing"

	"github.com/hustenreizjuengling/picache/internal/logs"
	"github.com/hustenreizjuengling/picache/internal/notify"
)

// The text scrubber reads untrusted text (log messages, names clients
// sent): it never panics, and without client names no MAC address and no
// configured name survives.
func FuzzScrubText(f *testing.F) {
	for _, s := range []string{"client 192.168.1.5 aa:bb:cc:dd:ee:ff", "[2001:db8::1]:53", "nas.lan.", "::::::",
		"1.2.3.4.5.6", "fe80::1%eth0/64", "x.nas.lan-y", "", "\x9cnAs.lAn", "\xffnAs.lAn0"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		sc := newScrubber(false)
		sc.addName("nas.lan")
		out := sc.scrubText(s)
		if m := macRE.FindString(out); m != "" {
			if _, err := net.ParseMAC(m); err == nil && !strings.Contains(s, "mac-") {
				t.Fatalf("MAC %q survived in %q (from %q)", m, out, s)
			}
		}
		if i := strings.Index(asciiLower(out), "nas.lan"); i >= 0 {
			before := i == 0 || !isLabelByte(out[i-1])
			after := i+7 == len(out) || !isLabelByte(out[i+7])
			if before && after {
				t.Fatalf("name survived in %q (from %q)", out, s)
			}
		}
	})
}

// emit records the event in the history and hands it on; unknown events
// and the notifier's own events are not recorded.
func TestEmitRecordsHistory(t *testing.T) {
	a := newTestApp(t)
	a.logs = logs.Discard("test", nil)
	a.emit(notify.Message{Event: notify.EventHealthWarning, Title: "Health check warning: host", Message: "load 9"})
	a.emit(notify.Message{Event: notify.EventHealthWarning, Title: "Health check warning: host", Message: "load 10"})
	a.emit(notify.Message{Event: notify.EventBackupFailed})
	a.emit(notify.Message{Event: notify.EventTest, Title: "test"})
	a.emit(notify.Message{Event: notify.EventDropped, Title: "dropped"})
	page, err := a.logs.Events(t.Context(), logs.EventQuery{Security: true})
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("history %+v, %v", page, err)
	}
	for _, e := range page.Items {
		switch e.Event {
		case notify.EventHealthWarning:
			if e.Count != 2 || e.Message != "load 10" || e.Severity != "warning" {
				t.Fatalf("merged %+v", e)
			}
		case notify.EventBackupFailed:
			if e.Title != "Scheduled backup failed" || e.Severity != "error" {
				t.Fatalf("defaults %+v", e)
			}
		default:
			t.Fatalf("recorded %+v", e)
		}
	}
	if c := a.logs.EventCounts(); c.All != 2 {
		t.Fatalf("counts %+v", c)
	}
}
