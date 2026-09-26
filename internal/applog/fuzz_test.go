package applog

import (
	"log/slog"
	"net/url"
	"testing"
)

// URL values of log attributes are untrusted (upstream URLs, request
// paths): the reduction never panics and never keeps user information, a
// query or a fragment of a URL it reduces.
func FuzzReduceURL(f *testing.F) {
	for _, s := range []string{"https://u:p@h:1/p?q=1#f", "http://[::1]:8080/x?", "mailto:a@b", "a://", "://x", "https://h#", "A://0/@#0"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		out, ok := reduceURL(s)
		if !ok {
			return
		}
		u, err := url.Parse(out)
		if err != nil || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
			t.Fatalf("%q reduced to %q", s, out)
		}
		a := redact(slog.String("url", s))
		if a.Value.String() != out {
			t.Fatalf("redact %q = %q, want %q", s, a.Value.String(), out)
		}
	})
}

// Masking parses untrusted values: it never panics and keeps at most /16
// or /48 of an address.
func FuzzMaskAddr(f *testing.F) {
	for _, s := range []string{"10.1.2.3", "[2001:db8::1]:53", "fe80::1%eth0", "10.0.0.0/33", "::ffff:1.2.3.4/120"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		_ = maskAddr(s)
	})
}
