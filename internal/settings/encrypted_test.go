package settings

import (
	"encoding/json/v2"
	"errors"
	"strings"
	"testing"

	"github.com/hustenreizjuengling/picache/internal/apperr"
)

// fieldErr returns the field and message of an apperr.Invalid error.
func fieldErr(t *testing.T, err error) (string, string) {
	t.Helper()
	var ae *apperr.Error
	if !errors.As(err, &ae) {
		t.Fatalf("not an apperr: %v", err)
	}
	return ae.Field, ae.Message
}

func TestServerNameRules(t *testing.T) {
	long189 := strings.Repeat("a", 60) + "." + strings.Repeat("b", 60) + "." + strings.Repeat("c", 60) + "." + strings.Repeat("d", 6)
	if len(long189) != 189 {
		t.Fatalf("len %d", len(long189))
	}
	cases := []struct {
		name, server, local string
		dot, doh            bool
		msg                 string // "" = valid
	}{
		{"empty while off", "", "lan", false, false, ""},
		{"empty while DoT is on", "", "lan", true, false, "required while DoT or DoH is enabled"},
		{"empty while DoH is on", "", "lan", false, true, "required while DoT or DoH is enabled"},
		{"normalised", "  PiCache.LAN.  ", "home.example", true, false, ""},
		{"ipv4", "192.168.1.5", "lan", true, false, "must be a host name, not an IP address"},
		{"ipv6", "2001:db8::1", "lan", false, true, "must be a host name, not an IP address"},
		{"189 characters", long189, "lan", true, false, ""},
		{"190 characters", "x" + long189, "lan", true, false, "must be at most 189 characters"},
		{"single label", "picache", "lan", true, false, "must be a host name with at least two labels, e.g. dns.example.com"},
		{"invalid", "pi_cache.example", "lan", true, false, "must be a host name with at least two labels"},
		{"localhost", "dns.localhost", "lan", true, false, "must not be a special-use name (localhost, invalid, onion, arpa)"},
		{"invalid tld", "dns.invalid", "lan", true, false, "must not be a special-use name"},
		{"onion", "x.onion", "lan", true, false, "must not be a special-use name"},
		{"arpa", "in-addr.arpa", "lan", true, false, "must not be a special-use name"},
		{"home.arpa itself", "home.arpa", "lan", true, false, "must not be a special-use name"},
		{"below home.arpa", "picache.home.arpa", "lan", true, false, ""},
		{"local domain", "home.example", "home.example", true, false, "must not be the local domain or a parent of it"},
		{"parent of the local domain", "example.net", "home.example.net", true, false, "must not be the local domain or a parent of it"},
		{"below the local domain", "picache.home.example", "home.example", true, false, ""},
		{"not a label boundary", "me.example", "home.example", true, false, ""},
		{"set while both off", "dns.example.com", "lan", false, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := Defaults()
			s.DNS.LocalDomain = tc.local
			s.DNS.Encrypted = EncryptedDNS{DoT: tc.dot, DoH: tc.doh, ServerName: tc.server}
			s.normalize()
			err := s.Validate()
			if tc.msg == "" {
				if err != nil {
					t.Fatalf("unexpected error %v", err)
				}
				return
			}
			field, msg := fieldErr(t, err)
			if field != "dns.encrypted.serverName" || !strings.Contains(msg, tc.msg) {
				t.Fatalf("got %s %q, want %q", field, msg, tc.msg)
			}
		})
	}
	s := Defaults()
	s.DNS.Encrypted.ServerName = "  PiCache.LAN.  "
	s.normalize()
	if s.DNS.Encrypted.ServerName != "picache.lan" {
		t.Fatalf("normalised %q", s.DNS.Encrypted.ServerName)
	}
}

// TestPlainDNSRule: plain DNS can be off only while DoT or DoH is on, in
// both directions (switching plain DNS off, switching the last protocol
// off).
func TestPlainDNSRule(t *testing.T) {
	const msg = "plain DNS can only be switched off while DoT or DoH is enabled"
	for _, tc := range []struct {
		plain, dot, doh, ok bool
	}{{true, false, false, true}, {false, false, false, false}, {false, true, false, true}, {false, false, true, true}, {false, true, true, true}} {
		s := Defaults()
		s.DNS.PlainDNS = tc.plain
		s.DNS.Encrypted = EncryptedDNS{DoT: tc.dot, DoH: tc.doh, ServerName: "picache.lan"}
		err := s.Validate()
		if (err == nil) != tc.ok {
			t.Fatalf("%+v: %v", tc, err)
		}
		if err != nil {
			if f, m := fieldErr(t, err); f != "dns.plainDns" || m != msg {
				t.Fatalf("%s %q", f, m)
			}
		}
	}
}

func TestSelfUpstreamRule(t *testing.T) {
	base := func() All {
		s := Defaults()
		s.DNS.LocalDomain = "lan"
		s.DNS.ServerNames = []string{"picache", "dns.home.example"}
		s.DNS.Encrypted = EncryptedDNS{DoT: true, ServerName: "picache.home.example"}
		return s
	}
	for _, tc := range []struct {
		up   string
		self bool
	}{
		{"tls://picache.home.example", true},
		{"https://picache.home.example/dns-query", true},
		{"quic://phone.picache.home.example", true},
		{"h3://dns.home.example/dns-query", true},
		{"tls://picache.lan", true},
		{"tls://picache", true},
		{"tls://other.example", false},
		{"tls://a.b.picache.home.example", false},
		{"9.9.9.9", false},
		{buildStamp(0x03, stampLP([]byte("192.168.1.2")), stampVLP(), stampLP([]byte("picache.home.example"))), true},
	} {
		s := base()
		s.DNS.Upstreams = []string{"https://dns.quad9.net/dns-query", tc.up}
		err := s.Validate()
		if !tc.self {
			if err != nil {
				t.Fatalf("%s: %v", tc.up, err)
			}
			continue
		}
		if f, m := fieldErr(t, err); f != "dns.upstreams[1]" || m != "an upstream must not be PiCache itself" {
			t.Fatalf("%s: %s %q", tc.up, f, m)
		}
		s = base()
		s.DNS.FallbackUpstreams = []string{tc.up}
		if f, _ := fieldErr(t, s.Validate()); f != "dns.fallbackUpstreams[0]" {
			t.Fatalf("%s: fallback field %s", tc.up, f)
		}
	}
}

// TestDecode013Document: a document of 0.13 (without the new members)
// decodes with plain DNS on and encrypted DNS off.
func TestDecode013Document(t *testing.T) {
	d := Defaults()
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	delete(doc["dns"], "plainDns")
	delete(doc["dns"], "encrypted")
	raw, _ = json.Marshal(doc)
	got, err := DecodeStored(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !got.DNS.PlainDNS || got.DNS.Encrypted != (EncryptedDNS{}) {
		t.Fatalf("decoded %+v %v", got.DNS.Encrypted, got.DNS.PlainDNS)
	}
	if err := got.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestClientIDSyntax(t *testing.T) {
	for in, want := range map[string]string{
		"phone": "phone", "Kid-Tablet": "kid-tablet", "a": "a", "0": "0", strings.Repeat("x", 63): strings.Repeat("x", 63),
	} {
		if got, ok := NormalizeClientID(in); !ok || got != want {
			t.Fatalf("%q: %q %v", in, got, ok)
		}
	}
	for _, in := range []string{"", "-a", "a-", "a.b", "a_b", "ä", strings.Repeat("x", 64), "a b"} {
		if _, ok := NormalizeClientID(in); ok {
			t.Fatalf("%q accepted", in)
		}
	}
	if id, ok := ParseClientIDEntry(" ClientID:Phone "); !ok || id != "phone" {
		t.Fatal(id, ok)
	}
	if _, ok := ParseClientIDEntry("clientid:"); ok {
		t.Fatal("empty accepted")
	}
	for in, want := range map[string]string{"CLIENTID:Kid-1": "clientid:kid-1", "clientid:tv": "clientid:tv"} {
		if got, ok := ParseBlockedClient(in); !ok || got != want {
			t.Fatalf("%q: %q %v", in, got, ok)
		}
	}
	for _, in := range []string{"clientid:-x", "clientid:", "clientid:" + strings.Repeat("a", 64)} {
		if _, ok := ParseBlockedClient(in); ok {
			t.Fatalf("%q accepted", in)
		}
	}
	s := Defaults()
	s.DNS.BlockedClients = []string{"ClientID:TV", "clientid:bad_id"}
	s.normalize()
	if s.DNS.BlockedClients[0] != "clientid:tv" {
		t.Fatal(s.DNS.BlockedClients)
	}
	if f, m := fieldErr(t, s.Validate()); f != "dns.blockedClients[1]" || m != "must be an IP address, CIDR, MAC address or clientid:<ClientID>" {
		t.Fatalf("%s %q", f, m)
	}
	for name, want := range map[string]string{"picache.lan": "", "phone.picache.lan": "phone", "PHONE.picache.lan": "phone"} {
		id, ok := ServerNameMatch("picache.lan", name)
		if !ok || id != want {
			t.Fatalf("%s: %q %v", name, id, ok)
		}
	}
	for _, name := range []string{"x.phone.picache.lan", "-x.picache.lan", "picache.lan.example", "lan"} {
		if _, ok := ServerNameMatch("picache.lan", name); ok {
			t.Fatalf("%s matched", name)
		}
	}
	if _, ok := ServerNameMatch("", "picache.lan"); ok {
		t.Fatal("matched without a server name")
	}
}
