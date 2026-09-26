package app

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/hustenreizjuengling/picache/internal/settings"
)

// testStamp builds a DNS stamp of typ with an address and host (DoH: the
// path /secret-profile; DNSCrypt: a provider key).
func testStamp(typ byte, addr, host string) string {
	b := []byte{typ, 0, 0, 0, 0, 0, 0, 0, 0, byte(len(addr))}
	b = append(b, addr...)
	if typ == 0x01 {
		b = append(b, 32)
		b = append(b, make([]byte, 32)...)
		b = append(b, byte(len(host)))
		b = append(b, host...)
		return "sdns://" + base64.RawURLEncoding.EncodeToString(b)
	}
	b = append(b, 0, byte(len(host)))
	b = append(b, host...)
	if typ == 0x02 {
		b = append(b, byte(len("/secret-profile")))
		b = append(b, "/secret-profile"...)
	}
	return "sdns://" + base64.RawURLEncoding.EncodeToString(b)
}

// The support bundle never contains a stamp, its address or path: a stamp
// becomes sdns:<protocol>:<host scrubbed>; quic:// and h3:// are reduced
// like tls:// and https://; the server name is a name; clientid: entries
// of dns.blockedClients are dropped like MACs.
func TestScrubEncryptedDNS(t *testing.T) {
	doh := testStamp(0x02, "203.0.113.44", "abc123.dns.nextdns.io")
	crypt := testStamp(0x01, "198.51.100.23:5443", "2.dnscrypt-cert.resolver.example.org")
	a := fullSettings()
	a.DNS.Upstreams = []string{doh, crypt, "quic://p1.dns.example.net:8853", "h3://dns.example.org/secret/path"}
	a.DNS.FallbackUpstreams = []string{}
	a.DNS.BlockedClients = []string{"clientid:kids-tablet", "192.168.1.9"}
	a.DNS.Encrypted = settings.EncryptedDNS{DoT: true, DoH: true, ServerName: "dns.smith.home"}
	a.DNS.PlainDNS = false
	sc := newScrubber(false)
	sc.registerSettingNames(&a)
	got := scrubbedSettings(t, sc, &a)
	want := map[string]any{
		"dns.upstreams": []any{"sdns:doh:*.nextdns.io", "sdns:dnscrypt:*.example.org", "quic://*.example.net:8853",
			"h3://*.example.org/…"},
		"dns.blockedClients":       []any{"192.168.1.9"},
		"dns.encrypted.dot":        true,
		"dns.encrypted.doh":        true,
		"dns.plainDns":             false,
		"dns.encrypted.serverName": sc.names["dns.smith.home"],
	}
	for k, w := range want {
		if g := got[k]; !jsonEqual(t, g, w) {
			t.Errorf("%s = %v, want %v", k, g, w)
		}
	}
	if !strings.HasPrefix(sc.names["dns.smith.home"], "name-") {
		t.Fatalf("server name placeholder %v", sc.names)
	}
	raw, err := sc.scrubSettings(&a)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"sdns://", doh[7:20], "203.0.113.44", "198.51.100", "secret-profile", "kids-tablet", "abc123", "smith.home"} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("settings.json contains %q: %s", leak, raw)
		}
	}
	// In other files: the configured stamp, an unknown stamp and the
	// display names of the log.
	text := sc.scrubText("upstream " + doh + " failed; also " + testStamp(0x03, "192.0.2.77", "dot.private.example") +
		"; upstream sdns:doh:abc123.dns.nextdns.io recovered")
	for _, leak := range []string{"sdns://", "203.0.113.44", "192.0.2.77", "abc123", "secret-profile"} {
		if strings.Contains(text, leak) {
			t.Fatalf("text contains %q: %s", leak, text)
		}
	}
	if !strings.Contains(text, "sdns:doh:*.nextdns.io") || !strings.Contains(text, "sdns:dot:*.private.example") {
		t.Fatalf("text %s", text)
	}
	if got := sc.scrubText("DoT for phone.dns.smith.home"); got != "DoT for phone."+sc.names["dns.smith.home"] {
		t.Fatalf("server name in text: %s", got)
	}
}
