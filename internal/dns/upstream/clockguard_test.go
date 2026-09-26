package upstream

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/settings"
)

func TestClockGuard(t *testing.T) {
	var plainQueries atomic.Int32
	boot := startDNS(t, func(w dns.ResponseWriter, q *dns.Msg) {
		plainQueries.Add(1)
		_ = w.WriteMsg(answerA(q, "192.0.2.53", 60))
	})
	const doh = "https://dns.example/dns-query"
	st := newStore(t, func(d *settings.DNS) {
		d.Upstreams = []string{doh}
		d.Bootstrap = []string{boot.Addr().String()}
		d.CacheEnabled = false
	})
	tests := []struct {
		name      string
		buildDate time.Time
		guard     bool
	}{
		{name: "clock before build date", buildDate: time.Now().Add(24 * time.Hour), guard: true},
		{name: "clock after build date", buildDate: time.Now().Add(-24 * time.Hour), guard: false},
		{name: "unknown build date", guard: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			plainQueries.Store(0)
			encrypted := &fakeTransport{fn: replyA("192.0.2.43", 60)}
			opts := testOptions()
			opts.buildDate = tc.buildDate
			opts.plainPort = int(boot.Port())
			r := newTestResolver(t, st, opts, map[string]*fakeTransport{doh: encrypted})
			defer r.Close()

			m, info, err := r.Resolve(context.Background(), query("guard.example.", dns.TypeA, 1, false), noECS)
			if err != nil {
				t.Fatal(err)
			}
			ip, _ := firstA(t, m)
			if got := r.ClockGuard(); got != tc.guard {
				t.Fatalf("ClockGuard = %v, want %v", got, tc.guard)
			}
			if tc.guard {
				if ip != "192.0.2.53" || encrypted.calls() != 0 || plainQueries.Load() != 1 || info.Upstream != boot.String() {
					t.Fatalf("guard: answer %s from %q, encrypted calls %d, plain %d", ip, info.Upstream, encrypted.calls(), plainQueries.Load())
				}
				if st := r.Stats(); len(st) != 1 || st[0].Upstream != boot.String() {
					t.Errorf("stats must show the upstreams in use: %+v", st)
				}
				return
			}
			if ip != "192.0.2.43" || encrypted.calls() != 1 || plainQueries.Load() != 0 {
				t.Fatalf("no guard: answer %s, encrypted calls %d, plain %d", ip, encrypted.calls(), plainQueries.Load())
			}
		})
	}
}

func TestParseBuildDate(t *testing.T) {
	for in, want := range map[string]time.Time{
		"2026-09-24T10:00:00Z": time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC),
		"2026-09-24":           time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC),
		"unknown":              {},
		"":                     {},
	} {
		if got := parseBuildDate(in); !got.Equal(want) {
			t.Errorf("parseBuildDate(%q) = %v, want %v", in, got, want)
		}
	}
}
