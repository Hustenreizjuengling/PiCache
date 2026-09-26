package settings

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"slices"
	"strings"
	"testing"
)

// stampLP encodes one length-prefixed field.
func stampLP(s []byte) []byte { return append([]byte{byte(len(s))}, s...) }

// stampVLP encodes a set of length-prefixed fields.
func stampVLP(items ...[]byte) []byte {
	if len(items) == 0 {
		return []byte{0}
	}
	var out []byte
	for i, it := range items {
		n := byte(len(it))
		if i < len(items)-1 {
			n |= 0x80
		}
		out = append(append(out, n), it...)
	}
	return out
}

// buildStamp returns "sdns://" + base64url of the fields after the type
// and the 8 bytes of properties.
func buildStamp(typ byte, fields ...[]byte) string {
	b := []byte{typ, 1, 0, 0, 0, 0, 0, 0, 0}
	for _, f := range fields {
		b = append(b, f...)
	}
	return "sdns://" + base64.RawURLEncoding.EncodeToString(b)
}

func TestParseStampTypes(t *testing.T) {
	hash := sha256.Sum256([]byte("tbs"))
	pk := bytes.Repeat([]byte{7}, 32)
	cases := []struct {
		name, stamp       string
		proto, host, url  string
		port              int
		dial              string
		pins              int
		ipLit, needsBoot  bool
		display, provider string
	}{
		{name: "doh with address and hash",
			stamp: buildStamp(0x02, stampLP([]byte("1.1.1.1")), stampVLP(hash[:]), stampLP([]byte("Dns.Example")), stampLP([]byte("/q"))),
			proto: "https", host: "dns.example", url: "https://dns.example/q", port: 443, dial: "1.1.1.1:443", pins: 1,
			display: "sdns:doh:dns.example"},
		{name: "doh without address uses the bootstrap servers",
			stamp: buildStamp(0x02, stampLP(nil), stampVLP(), stampLP([]byte("dns.example:8443")), stampLP(nil)),
			proto: "https", host: "dns.example", url: "https://dns.example:8443/dns-query", port: 8443, needsBoot: true,
			display: "sdns:doh:dns.example"},
		{name: "doh address port wins",
			stamp: buildStamp(0x02, stampLP([]byte("[2001:db8::1]:444")), stampVLP(), stampLP([]byte("dns.example:8443")), stampLP([]byte("/dns-query"))),
			proto: "https", host: "dns.example", url: "https://dns.example:444/dns-query", port: 444, dial: "[2001:db8::1]:444",
			display: "sdns:doh:dns.example"},
		{name: "doh with bootstrap addresses (ignored)",
			stamp: buildStamp(0x02, stampLP(nil), stampVLP(), stampLP([]byte("dns.example")), stampLP(nil), stampVLP([]byte("9.9.9.9"))),
			proto: "https", host: "dns.example", url: "https://dns.example/dns-query", port: 443, needsBoot: true,
			display: "sdns:doh:dns.example"},
		{name: "dot",
			stamp: buildStamp(0x03, stampLP([]byte("9.9.9.9")), stampVLP(), stampLP([]byte("dns.quad9.net"))),
			proto: "tls", host: "dns.quad9.net", port: 853, dial: "9.9.9.9:853", display: "sdns:dot:dns.quad9.net"},
		{name: "dot only a port",
			stamp: buildStamp(0x03, stampLP([]byte(":8853")), stampVLP(), stampLP([]byte("dns.quad9.net"))),
			proto: "tls", host: "dns.quad9.net", port: 8853, needsBoot: true, display: "sdns:dot:dns.quad9.net"},
		{name: "doq with an IP literal host",
			stamp: buildStamp(0x04, stampLP(nil), stampVLP(hash[:], hash[:]), stampLP([]byte("[2001:db8::53]"))),
			proto: "quic", host: "2001:db8::53", port: 853, dial: "[2001:db8::53]:853", pins: 2, ipLit: true,
			display: "sdns:doq:2001:db8::53"},
		{name: "dnscrypt",
			stamp: buildStamp(0x01, stampLP([]byte("203.0.113.5:5443")), stampLP(pk), stampLP([]byte("2.dnscrypt-cert.Example.org"))),
			proto: "dnscrypt", host: "2.dnscrypt-cert.example.org", port: 5443, dial: "203.0.113.5:5443",
			display: "sdns:dnscrypt:2.dnscrypt-cert.example.org", provider: "2.dnscrypt-cert.example.org"},
		{name: "dnscrypt default port",
			stamp: buildStamp(0x01, stampLP([]byte("203.0.113.5")), stampLP(pk), stampLP([]byte("2.dnscrypt-cert.example.org"))),
			proto: "dnscrypt", host: "2.dnscrypt-cert.example.org", port: 443, dial: "203.0.113.5:443",
			display: "sdns:dnscrypt:2.dnscrypt-cert.example.org", provider: "2.dnscrypt-cert.example.org"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := ParseUpstream(tc.stamp)
			if err != nil {
				t.Fatalf("ParseUpstream: %v", err)
			}
			if !spec.Stamp || spec.Proto != tc.proto || spec.Host != tc.host || spec.Port != tc.port || spec.URL != tc.url {
				t.Fatalf("spec = %+v", spec)
			}
			if spec.Raw != tc.stamp {
				t.Fatalf("Raw changed: %q", spec.Raw)
			}
			if got := spec.DialAddr; (tc.dial == "") == got.IsValid() || (got.IsValid() && got.String() != tc.dial) {
				t.Fatalf("DialAddr = %v, want %q", got, tc.dial)
			}
			if len(spec.Pins) != tc.pins || spec.IsIPLit != tc.ipLit || spec.NeedsBootstrap() != tc.needsBoot {
				t.Fatalf("pins %d, ipLit %v, needsBootstrap %v", len(spec.Pins), spec.IsIPLit, spec.NeedsBootstrap())
			}
			if spec.Display() != tc.display || spec.ProviderName != tc.provider {
				t.Fatalf("Display %q, provider %q", spec.Display(), spec.ProviderName)
			}
			if strings.Contains(spec.Display(), "203.0.113") || strings.Contains(spec.Display(), "sdns://") {
				t.Fatalf("Display leaks the stamp: %q", spec.Display())
			}
		})
	}
	if spec, _ := ParseUpstream(cases[len(cases)-1].stamp); spec.ProviderKey != [32]byte(pk) {
		t.Fatal("provider key not kept")
	}
}

func TestParseStampErrors(t *testing.T) {
	pk := bytes.Repeat([]byte{7}, 32)
	hash := make([]byte, 32)
	long := "sdns://" + strings.Repeat("A", MaxStampLen)
	cases := []struct{ name, stamp, want string }{
		{"too long", long, "a DNS stamp is at most 1024 characters"},
		{"padding", buildStamp(0x03, stampLP(nil), stampVLP(), stampLP([]byte("a.example"))) + "==", "invalid DNS stamp: base64url without padding expected"},
		{"not base64", "sdns://!!!", "invalid DNS stamp: not base64url"},
		{"empty", "sdns://", "invalid DNS stamp:"},
		{"plain dns", buildStamp(0x00, stampLP([]byte("1.1.1.1"))), "unsupported DNS stamp type"},
		{"odoh", buildStamp(0x05, stampLP([]byte("a.example")), stampLP([]byte("/q"))), "unsupported DNS stamp type"},
		{"dnscrypt relay", "sdns://" + base64.RawURLEncoding.EncodeToString(append([]byte{0x81}, stampLP([]byte("1.1.1.1"))...)), "unsupported DNS stamp type"},
		{"odoh relay", "sdns://" + base64.RawURLEncoding.EncodeToString([]byte{0x85, 0, 0, 0, 0, 0, 0, 0, 0}), "unsupported DNS stamp type"},
		{"unknown type", buildStamp(0x42), "unsupported DNS stamp type"},
		{"short properties", "sdns://" + base64.RawURLEncoding.EncodeToString([]byte{0x03, 0, 0}), "invalid DNS stamp: properties"},
		{"length beyond the end", buildStamp(0x03, []byte{40, 'a'}), "invalid DNS stamp: address"},
		{"missing host", buildStamp(0x03, stampLP(nil), stampVLP()), "invalid DNS stamp: host name"},
		{"trailing bytes dnscrypt", buildStamp(0x01, stampLP([]byte("1.2.3.4")), stampLP(pk), stampLP([]byte("2.dnscrypt-cert.a.example")), []byte{1}), "unexpected bytes"},
		{"trailing bytes dot", buildStamp(0x03, stampLP(nil), stampVLP(), stampLP([]byte("a.example")), stampVLP([]byte("1.1.1.1")), []byte{0}), "unexpected bytes"},
		{"short hash", buildStamp(0x03, stampLP(nil), stampVLP([]byte{1, 2, 3}), stampLP([]byte("a.example"))), "32 bytes"},
		{"hash set runs off the end", buildStamp(0x03, stampLP(nil), []byte{0x80 | 32}), "certificate hashes"},
		{"dnscrypt without address", buildStamp(0x01, stampLP(nil), stampLP(pk), stampLP([]byte("2.dnscrypt-cert.a.example"))), "needs an IP address"},
		{"dnscrypt short key", buildStamp(0x01, stampLP([]byte("1.2.3.4")), stampLP(pk[:31]), stampLP([]byte("2.dnscrypt-cert.a.example"))), "provider key"},
		{"dnscrypt bad name", buildStamp(0x01, stampLP([]byte("1.2.3.4")), stampLP(pk), stampLP([]byte("bad name"))), "provider name"},
		{"bad address", buildStamp(0x03, stampLP([]byte("dns.example")), stampVLP(), stampLP([]byte("a.example"))), "invalid address"},
		{"ipv6 without brackets", buildStamp(0x03, stampLP([]byte("2001:db8::1")), stampVLP(), stampLP([]byte("a.example"))), "invalid address"},
		{"bad port", buildStamp(0x03, stampLP([]byte("1.2.3.4:70000")), stampVLP(), stampLP([]byte("a.example"))), "invalid port"},
		{"bad host", buildStamp(0x03, stampLP(nil), stampVLP(), stampLP([]byte("a b.example"))), "invalid host name"},
		{"bad path", buildStamp(0x02, stampLP(nil), stampVLP(), stampLP([]byte("a.example")), stampLP([]byte("q"))), "invalid path"},
		{"too many hashes", buildStamp(0x03, stampLP(nil), stampVLP(slices.Repeat([][]byte{hash}, 17)...), stampLP([]byte("a.example"))), "more than 16"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseUpstream(tc.stamp)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestParseUpstreamNewSchemes(t *testing.T) {
	cases := []struct {
		in, proto, url string
		port           int
		boot           bool
	}{
		{"quic://dns.example", "quic", "", 853, true},
		{"QUIC://dns.example:8853", "quic", "", 8853, true},
		{"quic://9.9.9.9", "quic", "", 853, false},
		{"h3://dns.example", "h3", "https://dns.example/dns-query", 443, true},
		{"h3://dns.example:8443/q", "h3", "https://dns.example:8443/q", 8443, true},
		{"h3://[2001:db8::1]/dns-query", "h3", "https://[2001:db8::1]/dns-query", 443, false},
	}
	for _, tc := range cases {
		spec, err := ParseUpstream(tc.in)
		if err != nil {
			t.Fatalf("%s: %v", tc.in, err)
		}
		if spec.Proto != tc.proto || spec.URL != tc.url || spec.Port != tc.port || spec.NeedsBootstrap() != tc.boot {
			t.Fatalf("%s: %+v", tc.in, spec)
		}
		if spec.Display() != tc.in {
			t.Fatalf("%s: Display %q", tc.in, spec.Display())
		}
	}
	for in, want := range map[string]string{
		"quic://dns.example/path": "DoQ upstreams take no path",
		"gopher://dns.example":    "scheme must be udp, tcp, tls, https, quic, h3 or sdns",
		"h3://u:p@dns.example/q":  "credentials",
	} {
		if _, err := ParseUpstream(in); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: err = %v, want %q", in, err, want)
		}
	}
	// A stamp keeps its case (base64 is case-sensitive); the scheme is
	// recognised in any case.
	st := buildStamp(0x03, stampLP([]byte("9.9.9.9")), stampVLP(), stampLP([]byte("dns.quad9.net")))
	mixed := "SDNS://" + st[len("sdns://"):]
	spec, err := ParseUpstream(" " + mixed + " ")
	if err != nil || spec.Raw != mixed {
		t.Fatalf("mixed-case scheme: %+v %v", spec, err)
	}
	var s All = Defaults()
	s.DNS.Upstreams = []string{st}
	s.normalize()
	if s.DNS.Upstreams[0] != st {
		t.Fatalf("normalize changed the stamp: %q", s.DNS.Upstreams[0])
	}
}

// TestStampBootstrapRule: names in quic:// and h3:// and stamps without an
// address need dns.bootstrap; IP literals and stamps with one do not.
func TestStampBootstrapRule(t *testing.T) {
	withAddr := buildStamp(0x02, stampLP([]byte("1.1.1.1")), stampVLP(), stampLP([]byte("dns.example")), stampLP(nil))
	withoutAddr := buildStamp(0x02, stampLP(nil), stampVLP(), stampLP([]byte("dns.example")), stampLP(nil))
	for _, tc := range []struct {
		up string
		ok bool
	}{{withAddr, true}, {"quic://9.9.9.9", true}, {"h3://[2001:db8::1]/q", true},
		{withoutAddr, false}, {"quic://dns.example", false}, {"h3://dns.example/q", false}} {
		s := Defaults()
		s.DNS.Upstreams = []string{tc.up}
		s.DNS.FallbackUpstreams = []string{}
		s.DNS.Bootstrap = []string{}
		err := s.Validate()
		if (err == nil) != tc.ok {
			t.Fatalf("%s: err = %v", tc.up, err)
		}
		if err != nil && !strings.Contains(err.Error(), "dns.bootstrap") {
			t.Fatalf("%s: %v", tc.up, err)
		}
	}
}

func TestUpstreamDisplay(t *testing.T) {
	st := buildStamp(0x01, stampLP([]byte("203.0.113.5")), stampLP(bytes.Repeat([]byte{1}, 32)), stampLP([]byte("2.dnscrypt-cert.example.org")))
	if got := UpstreamDisplay(st); got != "sdns:dnscrypt:2.dnscrypt-cert.example.org" {
		t.Fatal(got)
	}
	if got := UpstreamDisplay("sdns://%%%"); got != "sdns:invalid" {
		t.Fatal(got)
	}
	if got := UpstreamDisplay("https://dns.example/abc"); got != "https://dns.example/abc" {
		t.Fatal(got)
	}
}

// FuzzParseStamp: no input panics, and an accepted stamp is consistent.
func FuzzParseStamp(f *testing.F) {
	f.Add(buildStamp(0x02, stampLP([]byte("1.1.1.1")), stampVLP(make([]byte, 32)), stampLP([]byte("dns.example")), stampLP([]byte("/q"))))
	f.Add(buildStamp(0x01, stampLP([]byte("[2001:db8::1]:443")), stampLP(make([]byte, 32)), stampLP([]byte("2.dnscrypt-cert.a.example"))))
	f.Add(buildStamp(0x03, stampLP(nil), stampVLP(), stampLP([]byte("a.example:853")), stampVLP([]byte("1.1.1.1"))))
	f.Add(buildStamp(0x04, stampLP([]byte(":853")), []byte{0x80, 0}, stampLP([]byte("a.example"))))
	f.Add("sdns://AQ")
	f.Fuzz(func(t *testing.T, s string) {
		if !strings.HasPrefix(strings.ToLower(s), "sdns://") {
			s = "sdns://" + s
		}
		spec, err := ParseUpstream(s)
		if err != nil {
			return
		}
		if !spec.Stamp || spec.Port < 1 || spec.Port > 65535 || spec.Host == "" {
			t.Fatalf("inconsistent spec %+v", spec)
		}
		if spec.Proto == "dnscrypt" && !spec.DialAddr.IsValid() {
			t.Fatal("DNSCrypt without an address")
		}
		if spec.DialAddr.IsValid() && spec.DialAddr.Addr() != spec.DialAddr.Addr().Unmap() {
			t.Fatal("mapped dial address")
		}
		if d := spec.Display(); strings.Contains(d, "sdns://") || d != "sdns:"+strings.Split(d, ":")[1]+":"+spec.Host {
			t.Fatalf("Display %q leaks the stamp", d)
		}
	})
}
