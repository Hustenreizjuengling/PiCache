package settings

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
)

func TestDefaultsValid(t *testing.T) {
	d := Defaults()
	if err := d.Validate(); err != nil {
		t.Fatalf("defaults invalid: %v", err)
	}
}

func TestUpdatePersistsAndNotifies(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "s.db")
	d, err := db.Open(path, 1)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := Open(ctx, d, log)
	if err != nil {
		t.Fatal(err)
	}
	var got *All
	unsub := s.Subscribe(func(_, n *All) { got = n })
	if _, err := s.Update(ctx, func(a *All) error { a.DNS.RateLimitQPS = 0; return nil }); err != nil {
		t.Fatal(err)
	}
	if got == nil || got.DNS.RateLimitQPS != 0 {
		t.Fatal("listener not called with new value")
	}
	unsub()
	_, err = s.Update(ctx, func(a *All) error { a.DNS.Upstreams = nil; return nil })
	if apperr.KindOf(err) != apperr.KindInvalid {
		t.Fatalf("want invalid, got %v", err)
	}
	if len(s.Get().DNS.Upstreams) == 0 {
		t.Fatal("failed update must not change state")
	}
	until := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	if _, err := s.Update(ctx, func(a *All) error { a.Filter.PausedUntil = &until; return nil }); err != nil {
		t.Fatal(err)
	}
	d.Close()

	d2, err := db.Open(path, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	s2, err := Open(ctx, d2, log)
	if err != nil {
		t.Fatal(err)
	}
	if s2.Get().DNS.RateLimitQPS != 0 {
		t.Fatal("update not persisted")
	}
	if p := s2.Get().Filter.PausedUntil; p == nil || !p.Equal(until) {
		t.Fatalf("pausedUntil not persisted: %v", p)
	}
	if s2.Get().Filter.BlockingActive(time.Now()) {
		t.Fatal("blocking should be paused")
	}
}

func TestParseUpstream(t *testing.T) {
	cases := []struct {
		in    string
		proto string
		addr  string
		ok    bool
	}{
		{"9.9.9.9", "udp", "9.9.9.9:53", true},
		{"tcp://1.1.1.1:5353", "tcp", "1.1.1.1:5353", true},
		{"[2620:fe::fe]:53", "udp", "[2620:fe::fe]:53", true},
		{"tls://dns.quad9.net", "tls", "dns.quad9.net:853", true},
		{"https://dns.quad9.net/dns-query", "https", "dns.quad9.net:443", true},
		{"https://dns.quad9.net", "https", "dns.quad9.net:443", true},
		{"dns.quad9.net", "", "", false},
		{"ftp://x", "", "", false},
		{"https://user:pw@x.example/dns-query", "", "", false},
	}
	for _, c := range cases {
		spec, err := ParseUpstream(c.in)
		if (err == nil) != c.ok {
			t.Errorf("%q: err=%v, want ok=%v", c.in, err, c.ok)
			continue
		}
		if c.ok && (spec.Proto != c.proto || spec.Addr() != c.addr) {
			t.Errorf("%q: got %s %s", c.in, spec.Proto, spec.Addr())
		}
	}
}
