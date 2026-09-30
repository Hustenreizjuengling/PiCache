package settings

import (
	"context"
	"encoding/json/jsontext"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
)

// CanonicalUpstream writes a valid host#port as host:port (IPv6 in
// brackets, the scheme as typed): a version before 1.0.0 ignores "#port"
// and would ask the default port, host:port means the same to every
// version. Anything else is returned unchanged.
func TestCanonicalUpstream(t *testing.T) {
	for in, want := range map[string]string{
		"127.0.0.1#5335":                  "127.0.0.1:5335",
		" 10.0.0.53#5353 ":                "10.0.0.53:5353",
		"10.0.0.53/#5353":                 "10.0.0.53:5353",
		"10.0.0.53#053":                   "10.0.0.53:53",
		"udp://10.0.0.53#5353":            "udp://10.0.0.53:5353",
		"UDP://10.0.0.53#5353":            "UDP://10.0.0.53:5353",
		"tcp://[fd00::53]#5335":           "tcp://[fd00::53]:5335",
		"[fd00::53]#5335":                 "[fd00::53]:5335",
		"[fe80::1%eth0]#5353":             "[fe80::1%eth0]:5353",
		"tls://dns.example#8853":          "tls://dns.example:8853",
		"quic://dns.example#8853":         "quic://dns.example:8853",
		"dns.example#5353":                "dns.example:5353",
		"9.9.9.9":                         "9.9.9.9",
		"9.9.9.9:53":                      "9.9.9.9:53",
		"https://dns.example/q":           "https://dns.example/q",
		"https://dns.example/q#443":       "https://dns.example/q#443", // refused by Validate
		"9.9.9.9#dns.quad9.net":           "9.9.9.9#dns.quad9.net",     // refused (new) or read by LegacyUpstream (stored)
		"10.0.0.53:53#5353":               "10.0.0.53:53#5353",
		"sdns://AgcAAAAAAAAAAAAHOS45Ljku": "sdns://AgcAAAAAAAAAAAAHOS45Ljku",
	} {
		got := CanonicalUpstream(in)
		if got != want {
			t.Errorf("CanonicalUpstream(%q) = %q, want %q", in, got, want)
			continue
		}
		if got == in || strings.TrimSpace(in) == got {
			continue
		}
		a, errA := ParseUpstream(in)
		b, errB := ParseUpstream(got)
		if errA != nil || errB != nil || a.Proto != b.Proto || a.Addr() != b.Addr() {
			t.Errorf("%q and %q differ: %s %s / %s %s (%v %v)", in, got, a.Proto, a.Addr(), b.Proto, b.Addr(), errA, errB)
		}
	}
}

// Every save of the settings (a write, the follower sync) stores a
// host#port of the upstreams, fallbacks and local PTR upstreams as
// host:port, duplicates removed. A document that 1.0.0-rc.2 stored with
// host#port loads with the same ports and is stored as host:port by the
// next save.
func TestSettingsStoreHostPort(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(filepath.Join(t.TempDir(), "s.db"), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.Migrate(ctx, "settings", migrations); err != nil {
		t.Fatal(err)
	}
	doc := `{"dns":{"upstreams":["127.0.0.1#5335","[fd00::53]#5335"],"fallbackUpstreams":["tls://9.9.9.9#853"],` +
		`"localPtrUpstreams":["192.168.1.1#5353"]}}`
	if _, err := d.W.ExecContext(ctx, `INSERT INTO settings (id, doc, updated_at) VALUES (1, ?, 0)`, doc); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, d, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	stored := func(member string) string {
		t.Helper()
		var v string
		if err := d.R.QueryRowContext(ctx, `SELECT json_extract(doc, '$.dns.`+member+`') FROM settings`).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	for _, u := range append(slices.Clone(s.Get().DNS.Upstreams), s.Get().DNS.LocalPTRUpstreams...) {
		if spec, err := ParseUpstream(u); err != nil || (spec.Port != 5335 && spec.Port != 5353) {
			t.Fatalf("stored #port %q read as port %d (%v)", u, spec.Port, err)
		}
	}
	if _, err := s.Update(ctx, func(a *All) error { a.Logs.QueryLogRetentionHours = 48; return nil }); err != nil {
		t.Fatal(err)
	}
	if u, f, p := stored("upstreams"), stored("fallbackUpstreams"), stored("localPtrUpstreams"); u != `["127.0.0.1:5335","[fd00::53]:5335"]` ||
		f != `["tls://9.9.9.9:853"]` || p != `["192.168.1.1:5353"]` {
		t.Fatalf("the next save stored %s %s %s", u, f, p)
	}

	// New input.
	got, err := s.Update(ctx, func(a *All) error {
		a.DNS.Upstreams = []string{"10.0.0.53#5353", "10.0.0.53:5353", "udp://[fd00::53]#5335"}
		a.DNS.FallbackUpstreams = []string{"quic://dns.example#8853"}
		a.DNS.LocalPTRUpstreams = []string{"192.168.1.1#53"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.DNS.Upstreams, []string{"10.0.0.53:5353", "udp://[fd00::53]:5335"}) {
		t.Fatalf("returned %v", got.DNS.Upstreams)
	}
	if u, f, p := stored("upstreams"), stored("fallbackUpstreams"), stored("localPtrUpstreams"); u != `["10.0.0.53:5353","udp://[fd00::53]:5335"]` ||
		f != `["quic://dns.example:8853"]` || p != `["192.168.1.1:53"]` {
		t.Fatalf("stored %s %s %s", u, f, p)
	}

	// The follower sync: the synced members are saved through Update.
	raw := jsontext.Value(`{"dns":{"upstreams":["127.0.0.1#5335"],"localPtrUpstreams":["192.168.1.1#5353"]}}`)
	if _, err := s.Update(ctx, func(a *All) error { _, err := ApplySyncable(a, raw); return err }); err != nil {
		t.Fatal(err)
	}
	if u, p := stored("upstreams"), stored("localPtrUpstreams"); u != `["127.0.0.1:5335"]` || p != `["192.168.1.1:5353"]` {
		t.Fatalf("synced %s %s", u, p)
	}
}

// New input with text after "#" in a local PTR upstream gets the message
// of the upstream syntax ("write the port as host:port"), not the generic
// one; an entry that is no plain DNS server IP keeps it.
func TestLocalPTRUpstreamMessages(t *testing.T) {
	for in, want := range map[string]string{
		"192.168.1.1#router":    "write the port as host:port",
		"192.168.1.1:53#5353":   "the port is given twice",
		"tls://192.168.1.1":     "must be a plain DNS server IP",
		"router.example":        "must be a plain DNS server IP",
		"192.168.1.1#abc/x#y#z": "write the port as host:port",
	} {
		a := Defaults()
		a.DNS.LocalPTRUpstreams = []string{in}
		a.normalize()
		err := a.Validate()
		if e, ok := apperr.As(err); !ok || e.Field != "dns.localPtrUpstreams[0]" || !strings.Contains(e.Message, want) {
			t.Errorf("%q: %v, want %q", in, err, want)
		}
	}
}
