package netutil

import (
	"errors"
	"net/netip"
	"strings"
	"testing"
)

func trustedSet(ps ...string) func(netip.Addr) bool {
	var set []netip.Prefix
	for _, p := range ps {
		set = append(set, netip.MustParsePrefix(p))
	}
	return func(ip netip.Addr) bool { return inAny(ip, set) }
}

func TestForwardedClient(t *testing.T) {
	proxy := netip.MustParseAddr("192.168.1.5")
	trusted := trustedSet("192.168.1.5/32", "10.9.0.0/24", "::1/128")
	many := func(n int, s string) string { return strings.TrimSuffix(strings.Repeat(s+",", n), ",") }
	for _, tc := range []struct {
		name   string
		peer   string
		header []string
		want   string // "" = malformed
	}{
		{"untrusted peer ignores the header", "203.0.113.9", []string{"198.51.100.1"}, "203.0.113.9"},
		{"untrusted peer with garbage", "203.0.113.9", []string{"unknown"}, "203.0.113.9"},
		{"no header", "192.168.1.5", nil, "192.168.1.5"},
		{"empty header", "192.168.1.5", []string{" , ,"}, "192.168.1.5"},
		{"one entry", "192.168.1.5", []string{"198.51.100.1"}, "198.51.100.1"},
		{"right-most untrusted wins", "192.168.1.5", []string{"1.1.1.1, 198.51.100.1"}, "198.51.100.1"},
		{"trusted hops skipped", "192.168.1.5", []string{"198.51.100.1, 10.9.0.7, 192.168.1.5"}, "198.51.100.1"},
		{"several lines in order", "192.168.1.5", []string{"198.51.100.1", "10.9.0.7"}, "198.51.100.1"},
		{"spoofed left entry ignored", "192.168.1.5", []string{"127.0.0.1", "198.51.100.1"}, "198.51.100.1"},
		{"ipv4 with port", "192.168.1.5", []string{"198.51.100.1:4711"}, "198.51.100.1"},
		{"ipv6", "192.168.1.5", []string{"2001:db8::7"}, "2001:db8::7"},
		{"ipv6 brackets", "192.168.1.5", []string{"[2001:db8::7]"}, "2001:db8::7"},
		{"ipv6 brackets port", "192.168.1.5", []string{"[2001:db8::7]:443"}, "2001:db8::7"},
		{"zone removed", "192.168.1.5", []string{"[fe80::1%eth0]:80"}, "fe80::1"},
		{"bare zone removed", "192.168.1.5", []string{"fe80::1%eth0"}, "fe80::1"},
		{"mapped address", "192.168.1.5", []string{"::ffff:198.51.100.1"}, "198.51.100.1"},
		{"mapped peer", "::ffff:192.168.1.5", []string{"198.51.100.1"}, "198.51.100.1"},
		{"all trusted: left-most inspected", "192.168.1.5", []string{"10.9.0.1, 10.9.0.2"}, "10.9.0.1"},
		{"16-entry window", "192.168.1.5", []string{"198.51.100.1, " + many(16, "10.9.0.3")}, "10.9.0.3"},
		{"15 trusted then the client", "192.168.1.5", []string{"198.51.100.1, " + many(15, "10.9.0.3")}, "198.51.100.1"},
		{"garbage beyond the window", "192.168.1.5", []string{"unknown, " + many(16, "10.9.0.3")}, "10.9.0.3"},
		{"garbage left of the client", "192.168.1.5", []string{"unknown, 198.51.100.1"}, "198.51.100.1"},
		{"malformed right-most", "192.168.1.5", []string{"198.51.100.1, unknown"}, ""},
		{"obfuscated", "192.168.1.5", []string{"_hidden"}, ""},
		{"name", "192.168.1.5", []string{"client.example"}, ""},
		{"ipv4 in brackets", "192.168.1.5", []string{"[198.51.100.1]"}, ""},
		{"bad port", "192.168.1.5", []string{"198.51.100.1:99999"}, ""},
		{"empty port", "192.168.1.5", []string{"198.51.100.1:"}, ""},
		{"unclosed bracket", "192.168.1.5", []string{"[2001:db8::1"}, ""},
		{"trailing junk", "192.168.1.5", []string{"[2001:db8::1]x"}, ""},
		{"loopback proxy", "::1", []string{"192.168.1.44"}, "192.168.1.44"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ForwardedClient(netip.MustParseAddr(tc.peer), tc.header, trusted)
			if tc.want == "" {
				if !errors.Is(err, ErrForwardedFor) {
					t.Fatalf("got %v %v, want malformed", got, err)
				}
				return
			}
			if err != nil || got != netip.MustParseAddr(tc.want) {
				t.Fatalf("got %v %v, want %s", got, err, tc.want)
			}
		})
	}
	if got, err := ForwardedClient(proxy, []string{"198.51.100.1"}, nil); err != nil || got != proxy {
		t.Fatalf("no trusted set: %v %v", got, err)
	}
}

func TestForwardedHTTPS(t *testing.T) {
	for _, tc := range []struct {
		header []string
		want   bool
	}{
		{nil, false},
		{[]string{"https"}, true},
		{[]string{"HTTPS"}, true},
		{[]string{" https "}, true},
		{[]string{"http"}, false},
		{[]string{"https, http"}, false},
		{[]string{"http, https"}, true},
		{[]string{"http", "https"}, true},
		{[]string{"https", "http"}, false},
		{[]string{"https,"}, false},
		{[]string{"httpsx"}, false},
	} {
		if got := ForwardedHTTPS(tc.header); got != tc.want {
			t.Errorf("ForwardedHTTPS(%q) = %v, want %v", tc.header, got, tc.want)
		}
	}
}

// FuzzForwardedClient: never panics; an untrusted peer is always the
// answer; a result is canonical and either the peer or an address that
// appears in the header.
func FuzzForwardedClient(f *testing.F) {
	for _, s := range []string{"198.51.100.1", "1.1.1.1, 10.9.0.7", "[2001:db8::1]:80", "unknown", "fe80::1%eth0",
		"::ffff:1.2.3.4", ",,,", "[::1]", "10.9.0.1:65535, 10.9.0.2"} {
		f.Add("192.168.1.5", s, "")
		f.Add("203.0.113.9", s, "10.9.0.3")
	}
	trusted := trustedSet("192.168.1.5/32", "10.9.0.0/24", "::1/128")
	f.Fuzz(func(t *testing.T, peerS, line1, line2 string) {
		peer, err := netip.ParseAddr(peerS)
		if err != nil {
			peer = netip.Addr{}
		}
		header := []string{line1}
		if line2 != "" {
			header = append(header, line2)
		}
		got, err := ForwardedClient(peer, header, trusted)
		if !trusted(Canon(peer)) || !peer.IsValid() {
			if err != nil || got != Canon(peer) {
				t.Fatalf("untrusted peer %v: %v %v", peer, got, err)
			}
			return
		}
		if err != nil {
			if !errors.Is(err, ErrForwardedFor) || got.IsValid() {
				t.Fatalf("error %v with %v", err, got)
			}
			return
		}
		if got != Canon(got) || !got.IsValid() {
			t.Fatalf("not canonical: %v", got)
		}
		if got != Canon(peer) && !strings.Contains(strings.ToLower(line1+","+line2), strings.ToLower(strings.Split(got.String(), "%")[0])) {
			// Mapped and compressed forms differ textually; parse every
			// entry instead.
			found := false
			for _, l := range header {
				for e := range strings.SplitSeq(l, ",") {
					if ip, ok := ParseForwardedAddr(strings.TrimSpace(e)); ok && ip == got {
						found = true
					}
				}
			}
			if !found {
				t.Fatalf("result %v is neither the peer nor in the header %q", got, header)
			}
		}
	})
}

// FuzzParseForwardedAddr: never panics; a parsed address is canonical and
// survives a round trip through its string form.
func FuzzParseForwardedAddr(f *testing.F) {
	for _, s := range []string{"1.2.3.4", "1.2.3.4:80", "[::1]:443", "[fe80::1%eth0]", "::ffff:1.2.3.4", "unknown", "[", "]:", ":80"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		ip, ok := ParseForwardedAddr(s)
		if !ok {
			return
		}
		if !ip.IsValid() || ip != Canon(ip) {
			t.Fatalf("%q → %v not canonical", s, ip)
		}
		if again, ok := ParseForwardedAddr(ip.String()); !ok || again != ip {
			t.Fatalf("%q → %v does not round-trip", s, ip)
		}
	})
}
