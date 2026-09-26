package settings

import (
	"encoding/json/v2"
	"net/netip"
	"net/url"
	"strings"
	"testing"

	"github.com/hustenreizjuengling/picache/internal/apperr"
)

func invalidField(err error) string {
	if ae, ok := apperr.As(err); ok && ae.Kind == apperr.KindInvalid {
		return ae.Field
	}
	return ""
}

// The local-record and server-name members of the DNS section: defaults,
// normalisation and validation.
func TestLocalRecordSettings(t *testing.T) {
	d := Defaults()
	if !d.DNS.LocalRecordsEnabled || d.DNS.LocalizeRecords != LocalizeFirst || d.DNS.ServerNameAddresses.IPv4 == nil ||
		d.DNS.ServerNameAddresses.IPv6 == nil {
		t.Fatalf("defaults %+v", d.DNS)
	}
	// A stored document of 0.12 decodes the new members from the defaults.
	a, err := DecodeStored([]byte(`{"dns":{"upstreams":["9.9.9.9"]}}`))
	if err != nil || !a.DNS.LocalRecordsEnabled || a.DNS.LocalizeRecords != LocalizeFirst {
		t.Fatalf("decoded %+v %v", a.DNS, err)
	}
	cases := []struct {
		mut   func(*All)
		field string
	}{
		{func(a *All) { a.DNS.LocalizeRecords = "nearest" }, "dns.localizeRecords"},
		{func(a *All) { a.DNS.LocalizeRecords = " ONLY " }, ""},
		{func(a *All) { a.DNS.LocalizeRecords = "" }, ""},
		{func(a *All) { a.DNS.ServerNameAddresses.IPv4 = []string{"192.168.1.2", "127.0.0.1"} }, "dns.serverNameAddresses.ipv4[1]"},
		{func(a *All) { a.DNS.ServerNameAddresses.IPv4 = []string{"169.254.1.1"} }, "dns.serverNameAddresses.ipv4[0]"},
		{func(a *All) { a.DNS.ServerNameAddresses.IPv4 = []string{"0.0.0.0"} }, "dns.serverNameAddresses.ipv4[0]"},
		{func(a *All) { a.DNS.ServerNameAddresses.IPv4 = []string{"224.0.0.1"} }, "dns.serverNameAddresses.ipv4[0]"},
		{func(a *All) { a.DNS.ServerNameAddresses.IPv4 = []string{"255.255.255.255"} }, "dns.serverNameAddresses.ipv4[0]"},
		{func(a *All) { a.DNS.ServerNameAddresses.IPv4 = []string{"fd00::1"} }, "dns.serverNameAddresses.ipv4[0]"},
		{func(a *All) { a.DNS.ServerNameAddresses.IPv4 = []string{"10.0.0.0/8"} }, "dns.serverNameAddresses.ipv4[0]"},
		{func(a *All) { a.DNS.ServerNameAddresses.IPv6 = []string{"fe80::1"} }, "dns.serverNameAddresses.ipv6[0]"},
		{func(a *All) { a.DNS.ServerNameAddresses.IPv6 = []string{"::ffff:192.168.1.2"} }, "dns.serverNameAddresses.ipv6[0]"},
		{func(a *All) { a.DNS.ServerNameAddresses.IPv6 = []string{"::1"} }, "dns.serverNameAddresses.ipv6[0]"},
		{func(a *All) { a.DNS.ServerNameAddresses.IPv6 = []string{"ff02::1"} }, "dns.serverNameAddresses.ipv6[0]"},
		{func(a *All) {
			a.DNS.ServerNameAddresses.IPv4 = strings.Split("10.0.0.1,10.0.0.2,10.0.0.3,10.0.0.4,10.0.0.5,10.0.0.6,10.0.0.7,10.0.0.8,10.0.0.9", ",")
		}, "dns.serverNameAddresses.ipv4"},
		{func(a *All) {
			a.DNS.ServerNameAddresses.IPv4 = []string{"::ffff:192.168.1.2", "203.0.113.5"}
			a.DNS.ServerNameAddresses.IPv6 = []string{"fd00::10", "2001:DB8::1"}
		}, ""},
		{func(a *All) { a.Filter.BlockingIPv4 = "Self" }, ""},
		{func(a *All) { a.Filter.BlockingIPv6 = "self" }, ""},
		{func(a *All) { a.Filter.BlockingIPv4 = "me" }, "filter.blockingIpv4"},
		{func(a *All) { a.Filter.BlockingIPv4 = "2001:db8::1" }, "filter.blockingIpv4"},
		{func(a *All) { a.Filter.BlockingIPv6 = "192.0.2.1" }, "filter.blockingIpv6"},
		{func(a *All) { a.Filter.BlockingIPv6 = "::ffff:192.0.2.1" }, "filter.blockingIpv6"},
		{func(a *All) { a.Filter.BlockingMode, a.Filter.BlockingIPv4 = "custom_ip", "self" }, ""},
		{func(a *All) { a.Filter.BlockingMode, a.Filter.BlockingIPv4 = "custom_ip", "" }, "filter.blockingIpv4"},
	}
	for i, c := range cases {
		a := Defaults()
		c.mut(&a)
		a.normalize()
		if got := invalidField(a.Validate()); got != c.field {
			t.Errorf("case %d: field %q, want %q", i, got, c.field)
		}
	}
	a2 := Defaults()
	a2.DNS.LocalizeRecords = " ONLY "
	a2.Filter.BlockingIPv4 = " SELF "
	a2.DNS.ServerNameAddresses.IPv4 = []string{"::ffff:192.168.1.2", " 192.168.1.2 "}
	a2.normalize()
	if a2.DNS.LocalizeRecords != LocalizeOnly || a2.Filter.BlockingIPv4 != SelfAddress || len(a2.DNS.ServerNameAddresses.IPv4) != 1 ||
		a2.DNS.ServerNameAddresses.IPv4[0] != "192.168.1.2" {
		t.Fatalf("normalised %+v %q", a2.DNS.ServerNameAddresses, a2.Filter.BlockingIPv4)
	}
	// JSON: the members and their names.
	b, _ := json.Marshal(Defaults().DNS)
	for _, want := range []string{`"localRecordsEnabled":true`, `"localizeRecords":"first"`, `"serverNameAddresses":{"ipv4":[],"ipv6":[]}`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("JSON lacks %s", want)
		}
	}
}

// Query-type names as dns.droppedDomains stores them.
func TestQTypeNames(t *testing.T) {
	for in, want := range map[string]string{"aaaa": "AAAA", " https ": "HTTPS", "TYPE1": "A", "TYPE65280": "TYPE65280", "type28": "AAAA"} {
		q, err := ParseQType(in)
		if err != nil || QTypeName(q) != want {
			t.Errorf("%q: %d %q %v", in, q, QTypeName(q), err)
		}
	}
	for _, bad := range []string{"", "NOPE", "TYPE0", "TYPE65536", "TYPE01", "NONE"} {
		if _, err := ParseQType(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// The preset table: unique keys, DoH upstreams by name, plain addresses of
// both families (the clock guard).
func TestUpstreamPresets(t *testing.T) {
	ps := UpstreamPresets()
	if len(ps) != 3 || ps[0].Key != "cloudflare-family" || ps[1].Key != "opendns-familyshield" || ps[2].Key != "cleanbrowsing-family" {
		t.Fatalf("presets %+v", ps)
	}
	for _, p := range ps {
		if p.Name == "" || len(p.Upstreams) != 1 || len(p.Plain) != 4 {
			t.Errorf("%s: %+v", p.Key, p)
		}
		spec, err := ParseUpstream(p.Upstreams[0])
		if u, perr := url.Parse(p.Upstreams[0]); err != nil || spec.Proto != "https" || spec.IsIPLit || perr != nil || u.Scheme != "https" {
			t.Errorf("%s: upstream %q %v", p.Key, p.Upstreams[0], err)
		}
		v4, v6 := 0, 0
		for _, s := range p.Plain {
			ip, err := netip.ParseAddr(s)
			switch {
			case err != nil:
				t.Errorf("%s: plain %q", p.Key, s)
			case ip.Is4():
				v4++
			default:
				v6++
			}
		}
		if v4 != 2 || v6 != 2 {
			t.Errorf("%s: %d IPv4 and %d IPv6 plain addresses", p.Key, v4, v6)
		}
		if got, ok := UpstreamPresetByKey(p.Key); !ok || got.Name != p.Name {
			t.Errorf("by key %s", p.Key)
		}
	}
	ps[0].Upstreams[0] = "changed"
	if UpstreamPresets()[0].Upstreams[0] == "changed" {
		t.Fatal("the table is shared")
	}
	if _, ok := UpstreamPresetByKey("unknown"); ok {
		t.Fatal("unknown key found")
	}
}
