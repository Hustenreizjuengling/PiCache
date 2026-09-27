package clients

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/netutil"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// withConfig applies a configuration derived from the defaults.
func withConfig(r *Registry, fn func(c *Config)) {
	c := *r.config()
	fn(&c)
	r.ApplyConfig(c)
}

func TestParseIfaceAndHostIdentifiers(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"iface:wg0", "iface:wg0"},
		{"IFACE:eth0.100", "iface:eth0.100"},
		{"iface:Br-Guest", "iface:Br-Guest"}, // the kernel's spelling is kept
		{"iface:" + strings.Repeat("a", 15), "iface:" + strings.Repeat("a", 15)},
		{"host:Kids-Tablet.", "host:kids-tablet"},
		{"HOST:tablet.lan", "host:tablet.lan"},
		{"host:" + strings.Repeat("a", 63) + "." + strings.Repeat("b", 63), "host:" + strings.Repeat("a", 63) + "." + strings.Repeat("b", 63)},
	} {
		id, ok := parseIdentifier(tc.in)
		if !ok || id.value != tc.want {
			t.Errorf("%q: %+v %v", tc.in, id, ok)
		}
	}
	for _, bad := range []string{"iface:", "iface:" + strings.Repeat("a", 16), "iface:a/b", "iface:a:b", "iface:a b", "iface:.",
		"iface:..", "iface:a\x01", "host:", "host:a_b", "host:a..b", "host:-", "host:" + strings.Repeat("a", 64),
		"host:" + strings.Repeat("a.", 127) + "aa", "host:ä"} {
		if id, ok := parseIdentifier(bad); ok {
			t.Errorf("%q accepted as %+v", bad, id)
		}
	}
}

// iface: comes after IP and CIDR and before the MAC identifiers; a source
// of the interface's networks stays in its client even with a trusted
// device's MAC; derived and forwarded identities never match.
func TestIfaceIdentification(t *testing.T) {
	r := newTestRegistry(t, false)
	r.gateways = func() []netip.Addr { return nil }
	guests := mustGroup(t, r, "Guests", true)
	trusted := mustGroup(t, r, "Trusted", true)
	mustClient(t, r, "Guest VLAN", []int64{guests.ID}, "iface:br-guest")
	mustClient(t, r, "Admin laptop", []int64{trusted.ID}, "aa:00:00:00:00:01", "192.168.50.7")
	routes := map[netip.Addr]string{}
	r.ifaceOf = func(a netip.Addr) string {
		if a.Is4() && a.As4()[2] == 50 {
			return "br-guest"
		}
		return routes[a]
	}
	// A guest copying the admin laptop's MAC stays a guest.
	setNeighbours(r, map[netip.Addr]string{ip("192.168.50.20"): "aa:00:00:00:00:01", ip("192.168.1.9"): "aa:00:00:00:00:01"})
	if id := r.Identify(ip("192.168.50.20")); id.Name != "Guest VLAN" {
		t.Fatalf("MAC spoof on the guest network: %+v", id)
	}
	// The exact IP of the guest network wins over iface:.
	if id := r.Identify(ip("192.168.50.7")); id.Name != "Admin laptop" {
		t.Fatalf("exact IP: %+v", id)
	}
	// Another network: the MAC decides.
	if id := r.Identify(ip("192.168.1.9")); id.Name != "Admin laptop" {
		t.Fatalf("main LAN: %+v", id)
	}
	// Derived (EDNS) and forwarded (DoH through a trusted proxy): never iface:.
	if id := r.IdentifyDerived(ip("192.168.50.21"), ""); id.ClientID != 0 {
		t.Fatalf("derived: %+v", id)
	}
	if id := r.IdentifyForwarded(ip("192.168.50.21")); id.ClientID != 0 {
		t.Fatalf("forwarded: %+v", id)
	}
	if id := r.Identify(ip("192.168.50.21")); id.Name != "Guest VLAN" {
		t.Fatalf("transport source after the others: %+v", id)
	}
	// A changed route snapshot drops the cached identities.
	routes[ip("10.8.0.2")] = "br-guest"
	r.Identify(ip("10.8.0.2")) // cached before the change? (route was set already)
	delete(routes, ip("10.8.0.2"))
	r.InterfacesChanged()
	if id := r.Identify(ip("10.8.0.2")); id.ClientID != 0 {
		t.Fatalf("after the change: %+v", id)
	}
	// An interface belongs to one client.
	_, err := r.CreateClient(context.Background(), ClientInput{Name: "Other", Identifiers: []string{"iface:br-guest"}})
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Kind != apperr.KindConflict || ae.Message != "iface:br-guest belongs to client Guest VLAN" {
		t.Fatalf("duplicate interface: %v", err)
	}
	// Known shows the interface of each address.
	r.SeenTransient(ip("192.168.50.20"))
	rows, _ := r.Known(context.Background(), 0)
	if len(rows) != 1 || rows[0].Interface != "br-guest" || rows[0].Name != "Guest VLAN" {
		t.Fatalf("known %+v", rows)
	}
}

// An IPv6 link-local source: the routes cannot name its interface (every
// interface has fe80::/64), its zone can (the interface it arrived on).
// Without a zone it never reaches the MAC steps while iface: identifiers
// exist, so a guest that copied a trusted MAC cannot leave its client by
// querying from fe80::.
func TestIfaceLinkLocalSource(t *testing.T) {
	r := newTestRegistry(t, false)
	r.gateways = func() []netip.Addr { return nil }
	r.onLink = nil
	r.ifaceOf = func(netip.Addr) string { return "" }
	r.zoneIface = func(zone string) string { return zone }
	guests := mustGroup(t, r, "Guests", true)
	trusted := mustGroup(t, r, "Trusted", true)
	mustClient(t, r, "Admin laptop", []int64{trusted.ID}, "aa:00:00:00:00:01")
	setNeighbours(r, map[netip.Addr]string{ip("fe80::5"): "aa:00:00:00:00:01", ip("fe80::9"): "aa:00:00:00:00:01"})
	// Without iface: identifiers the MAC decides, zone or not.
	if id := r.Identify(ip("fe80::9")); id.Name != "Admin laptop" {
		t.Fatalf("no iface: identifiers: %+v", id)
	}
	mustClient(t, r, "Guest VLAN", []int64{guests.ID}, "iface:vlan20")
	if id := r.Identify(ip("fe80::5%vlan20")); id.Name != "Guest VLAN" || id.IP != ip("fe80::5") {
		t.Fatalf("MAC spoof from a link-local address of the guest network: %+v", id)
	}
	// The same address on another interface is another device: its MAC
	// decides (the cache keeps the zone apart).
	if id := r.Identify(ip("fe80::5%eth0")); id.Name != "Admin laptop" {
		t.Fatalf("main LAN: %+v", id)
	}
	if id := r.Identify(ip("fe80::5%vlan20")); id.Name != "Guest VLAN" {
		t.Fatalf("cached across zones: %+v", id)
	}
	// Unknown interface: the address alone (here: nothing).
	if id := r.Identify(ip("fe80::9")); id.ClientID != 0 {
		t.Fatalf("link-local source without a zone reached the MAC steps: %+v", id)
	}
	// Forwarded and derived identities never use the zone.
	if id := r.IdentifyForwarded(ip("fe80::9%vlan20")); id.Name != "Admin laptop" {
		t.Fatalf("forwarded: %+v", id)
	}
	// Other systems (no zoneIface): iface: never applies, the address decides.
	r.zoneIface = nil
	r.invalidate()
	if id := r.Identify(ip("fe80::5%vlan20")); id.ClientID != 0 {
		t.Fatalf("without zone support: %+v", id)
	}
}

// host: identifiers: the address's own cached names from the enabled
// sources, single labels also with the local domain; last, after the
// ClientID (ByHost); a rename drops the cached identity.
func TestHostIdentification(t *testing.T) {
	r := newTestRegistry(t, false)
	r.gateways = func() []netip.Addr { return nil }
	kids := mustGroup(t, r, "Kids", true)
	mustClient(t, r, "Tablet", []int64{kids.ID}, "host:kids-tablet")
	mustClient(t, r, "Console", []int64{kids.ID}, "host:console.fritz.box")
	withConfig(r, func(c *Config) { c.LocalDomain = "lan" })
	var lease atomic.Value
	lease.Store("")
	r.SetLeaseNames(func(a netip.Addr) string {
		if a == ip("192.168.1.40") {
			return lease.Load().(string)
		}
		return ""
	})
	// Not named yet: no match (the query path never looks a name up).
	if id := r.Identify(ip("192.168.1.40")); id.ClientID != 0 {
		t.Fatalf("unnamed: %+v", id)
	}
	lease.Store("kids-tablet.lan")
	r.LeaseNamesChanged([]netip.Addr{ip("192.168.1.40")})
	if id := r.Identify(ip("192.168.1.40")); id.Name != "Tablet" || !id.ByHost {
		t.Fatalf("lease name with the local domain: %+v", id)
	}
	lease.Store("Kids-Tablet")
	r.LeaseNamesChanged([]netip.Addr{ip("192.168.1.40")})
	if id := r.Identify(ip("192.168.1.40")); id.Name != "Tablet" {
		t.Fatalf("lease name: %+v", id)
	}
	// A rename drops the match at once.
	lease.Store("laptop")
	r.LeaseNamesChanged([]netip.Addr{ip("192.168.1.40")})
	if id := r.Identify(ip("192.168.1.40")); id.ClientID != 0 {
		t.Fatalf("after a rename: %+v", id)
	}
	// PTR names; a multi-label identifier needs the full name.
	r.namesMu.Lock()
	r.names.put(ip("192.168.1.41"), hostName{name: "console.fritz.box", at: time.Now()})
	r.names.put(ip("192.168.1.42"), hostName{name: "console.lan", at: time.Now()})
	r.namesMu.Unlock()
	r.invalidate()
	if id := r.Identify(ip("192.168.1.41")); id.Name != "Console" {
		t.Fatalf("PTR name: %+v", id)
	}
	if id := r.Identify(ip("192.168.1.42")); id.ClientID != 0 {
		t.Fatalf("console.lan must not match host:console.fritz.box: %+v", id)
	}
	// Never the name of another address with the same MAC.
	setNeighbours(r, map[netip.Addr]string{ip("192.168.1.41"): "aa:00:00:00:00:41", ip("fd00::41"): "aa:00:00:00:00:41"})
	if id := r.Identify(ip("fd00::41")); id.ClientID != 0 || id.Name != "console.fritz.box" {
		t.Fatalf("another address's name: %+v", id)
	}
	// A disabled source stops matching.
	withConfig(r, func(c *Config) { c.Sources.PTR = false })
	if id := r.Identify(ip("192.168.1.41")); id.ClientID != 0 {
		t.Fatalf("PTR off: %+v", id)
	}
	// The PTR name of a public source is set by whoever holds its reverse
	// zone: it never matches, unless the address is on a connected network
	// (the LAN's public IPv6 prefix).
	withConfig(r, func(c *Config) { c.Sources.PTR = true })
	r.namesMu.Lock()
	r.names.put(ip("9.9.9.41"), hostName{name: "console.fritz.box", at: time.Now()})
	r.names.put(ip("2a00:1450::41"), hostName{name: "console.fritz.box", at: time.Now()})
	r.namesMu.Unlock()
	r.invalidate()
	if id := r.Identify(ip("9.9.9.41")); id.ClientID != 0 || id.Name != "console.fritz.box" {
		t.Fatalf("PTR name of a public address: %+v", id)
	}
	r.onLink = func(a netip.Addr) bool { return a == ip("2a00:1450::41") }
	if id := r.Identify(ip("2a00:1450::41")); id.Name != "Console" {
		t.Fatalf("PTR name of an address of a connected network: %+v", id)
	}
	r.onLink = nil
	// Precedence over nothing but Default: an IP identifier wins.
	r.namesMu.Lock()
	r.names.put(ip("192.168.1.43"), hostName{name: "kids-tablet", at: time.Now()})
	r.namesMu.Unlock()
	mustClient(t, r, "By IP", nil, "192.168.1.43")
	if id := r.Identify(ip("192.168.1.43")); id.Name != "By IP" || id.ByHost {
		t.Fatalf("an IP identifier must win: %+v", id)
	}
	_, err := r.CreateClient(context.Background(), ClientInput{Name: "Twin", Identifiers: []string{"host:KIDS-TABLET"}})
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Kind != apperr.KindConflict || ae.Message != "host:kids-tablet belongs to client Tablet" {
		t.Fatalf("duplicate host: %v", err)
	}
}

// Each switch of clients.nameSources.
func TestNameSourceSwitches(t *testing.T) {
	ctx := context.Background()
	t.Run("ptr", func(t *testing.T) {
		r := newTestRegistry(t, false)
		var calls atomic.Int32
		r.SetPTRResolver(func(context.Context, netip.Addr) (string, error) { calls.Add(1); return "pc.lan", nil })
		r.lookupName(ctx, ip("192.168.1.2"))
		if r.DisplayName(ip("192.168.1.2")) != "pc.lan" {
			t.Fatal("PTR on")
		}
		withConfig(r, func(c *Config) { c.Sources.PTR = false })
		if n := r.DisplayName(ip("192.168.1.2")); n != "" {
			t.Fatalf("cached PTR names must be dropped: %q", n)
		}
		r.enqueueName(ip("192.168.1.3"))
		r.lookupName(ctx, ip("192.168.1.3"))
		r.LookupNames([]netip.Addr{ip("192.168.1.4")})
		if calls.Load() != 1 || len(r.queue) != 0 {
			t.Fatalf("no PTR lookups while off: %d calls, %d queued", calls.Load(), len(r.queue))
		}
	})
	t.Run("dhcp", func(t *testing.T) {
		r := newTestRegistry(t, false)
		r.SetLeaseNames(func(netip.Addr) string { return "phone" })
		if r.DisplayName(ip("192.168.1.5")) != "phone" {
			t.Fatal("dhcp on")
		}
		withConfig(r, func(c *Config) { c.Sources.DHCP = false })
		if n := r.DisplayName(ip("192.168.1.5")); n != "" {
			t.Fatalf("lease names must not be used: %q", n)
		}
	})
	t.Run("hostsFile", func(t *testing.T) {
		r := newTestRegistry(t, false)
		r.hostsPath = filepath.Join(t.TempDir(), "hosts")
		os.WriteFile(r.hostsPath, []byte("192.168.1.6 nas nas-alias\n"), 0o644)
		warned := false
		r.refreshHosts(&warned)
		if n := r.DisplayName(ip("192.168.1.6")); n != "" {
			t.Fatalf("off by default: %q", n)
		}
		withConfig(r, func(c *Config) { c.Sources.HostsFile = true })
		r.refreshHosts(&warned)
		if n := r.DisplayName(ip("192.168.1.6")); n != "nas" {
			t.Fatalf("hosts on: %q", n)
		}
		// Changed file (size and time): read again.
		os.WriteFile(r.hostsPath, []byte("192.168.1.6 storage\n"), 0o644)
		future := time.Now().Add(time.Hour)
		os.Chtimes(r.hostsPath, future, future)
		r.refreshHosts(&warned)
		if n := r.DisplayName(ip("192.168.1.6")); n != "storage" {
			t.Fatalf("changed file: %q", n)
		}
		withConfig(r, func(c *Config) { c.Sources.HostsFile = false })
		if n := r.DisplayName(ip("192.168.1.6")); n != "" || len(r.hosts.Load().names) != 0 {
			t.Fatalf("hosts off: %q", n)
		}
	})
	t.Run("whois", func(t *testing.T) {
		r := newTestRegistry(t, false)
		r.SetWhoisClient(http.DefaultClient, "PiCache/test")
		r.Seen(ip("8.8.8.8"))
		if len(r.whois.queue) != 0 {
			t.Fatal("WHOIS is off by default")
		}
		withConfig(r, func(c *Config) { c.Sources.WHOIS = true })
		r.Seen(ip("8.8.4.4"))
		if len(r.whois.queue) != 1 {
			t.Fatal("WHOIS on: the new public address is queued")
		}
		withConfig(r, func(c *Config) { c.Anonymize = true })
		if len(r.whois.queue) != 0 {
			t.Fatal("anonymising client addresses drops the queue")
		}
	})
}

func TestParseHostsFile(t *testing.T) {
	text := strings.Join([]string{
		"# comment",
		"127.0.0.1 localhost",
		"::1 localhost ip6-localhost ip6-loopback",
		"0.0.0.0 blocked.example",
		"ff02::1 ip6-allnodes",
		"192.168.1.10 NAS.lan nas   # inline",
		"192.168.1.10 other-name",
		"192.168.1.11 broadcasthost ip6-foo bad_name! printer",
		"192.168.1.12 <script>",
		"fe80::1%eth0 zoned",
		"not-an-ip host",
		"192.168.1.13 " + strings.Repeat("a", hostsMaxLine),
		"192.168.1.14\tTAB-host",
		"::ffff:192.168.1.15 mapped",
	}, "\n")
	got, truncated := parseHostsFile(strings.NewReader(text))
	want := map[netip.Addr]string{ip("192.168.1.10"): "nas.lan", ip("192.168.1.11"): "printer",
		ip("192.168.1.14"): "tab-host", ip("192.168.1.15"): "mapped"}
	if truncated || len(got) != len(want) {
		t.Fatalf("%v %v", got, truncated)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s = %q, want %q", k, got[k], v)
		}
	}
	// 10 000 lines at most, 1 MiB at most.
	var b strings.Builder
	for i := range hostsMaxLines + 5 {
		fmt.Fprintf(&b, "10.%d.%d.1 h%d\n", i/256, i%256, i)
	}
	got, truncated = parseHostsFile(strings.NewReader(b.String()))
	if !truncated || len(got) != hostsMaxLines {
		t.Fatalf("lines: %d %v", len(got), truncated)
	}
	big := strings.Repeat("# padding padding padding padding padding padding padding\n", (hostsMaxBytes/58)+10) + "10.0.0.1 late\n"
	got, truncated = parseHostsFile(strings.NewReader(big))
	if !truncated || len(got) != 0 {
		t.Fatalf("bytes: %d %v", len(got), truncated)
	}
}

func FuzzParseHostsFile(f *testing.F) {
	f.Add("192.168.1.10 nas.lan nas # c\n::1 localhost\n")
	f.Add("fe80::1%lo0 localhost\n10.0.0.1\tx\r\n")
	f.Fuzz(func(t *testing.T, s string) {
		got, _ := parseHostsFile(strings.NewReader(s))
		for a, n := range got {
			if !a.IsValid() || a.IsLoopback() || n == "" || sanitizeHostname(n) != n {
				t.Fatalf("%v %q", a, n)
			}
		}
	})
}

func seenRows(t *testing.T, r *Registry) []string {
	t.Helper()
	rows, err := r.ldb.R.Query(`SELECT ip FROM clients_seen ORDER BY ip`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		rows.Scan(&s)
		out = append(out, s)
	}
	return out
}

func TestForgetAndFlush(t *testing.T) {
	ctx := context.Background()
	r := newTestRegistry(t, true)
	r.gateways = func() []netip.Addr { return nil }
	for _, a := range []string{"192.168.1.2", "192.168.1.3", "fd00::3", "192.168.1.4"} {
		r.Seen(ip(a))
	}
	r.flush(ctx)
	r.SeenDNSClientID(ip("192.168.1.2"), "phone")
	r.namesMu.Lock()
	r.names.put(ip("192.168.1.2"), hostName{name: "phone.lan", at: time.Now()})
	r.namesMu.Unlock()
	if n, err := r.ForgetKnown(ctx, ip("192.168.1.2"), ""); err != nil || n != 1 {
		t.Fatalf("forget by ip: %d %v", n, err)
	}
	if got := seenRows(t, r); strings.Join(got, " ") != "192.168.1.3 192.168.1.4 fd00::3" {
		t.Fatalf("rows %v", got)
	}
	if len(r.DNSClientIDs()) != 0 || r.DisplayName(ip("192.168.1.2")) != "" {
		t.Fatal("the ClientID and the name of the address must be forgotten")
	}
	if n, _ := r.ForgetKnown(ctx, ip("192.168.1.2"), ""); n != 0 {
		t.Fatalf("again: %d", n)
	}
	// By MAC: the neighbour table and the stored MAC.
	setNeighbours(r, map[netip.Addr]string{ip("192.168.1.3"): "aa:00:00:00:00:03", ip("fd00::3"): "aa:00:00:00:00:03"})
	r.Seen(ip("192.168.1.3"))
	r.flush(ctx) // stores the MAC with the rows
	setNeighbours(r, map[netip.Addr]string{ip("fd00::3"): "aa:00:00:00:00:03"})
	if n, err := r.ForgetKnown(ctx, netip.Addr{}, "aa:00:00:00:00:03"); err != nil || n != 2 {
		t.Fatalf("forget by mac: %d %v", n, err)
	}
	if got := seenRows(t, r); strings.Join(got, " ") != "192.168.1.4" {
		t.Fatalf("rows %v", got)
	}
	r.Seen(ip("192.168.1.9")) // memory only so far
	if n, err := r.FlushKnown(ctx); err != nil || n != 2 {
		t.Fatalf("flush: %d %v", n, err)
	}
	known, _ := r.Known(ctx, 0)
	if len(seenRows(t, r)) != 0 || len(known) != 0 {
		t.Fatalf("after flush: %v %v", seenRows(t, r), known)
	}
}

// A forget that races a flush: the flush copied the pending counts, the
// forget waits until its write returned and then deletes the row.
func TestForgetRacingFlush(t *testing.T) {
	ctx := context.Background()
	r := newTestRegistry(t, true)
	r.Seen(ip("192.168.1.2"))
	copied := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	r.flushHook = func() { once.Do(func() { close(copied); <-release }) }
	done := make(chan struct{})
	go func() { r.flush(ctx); close(done) }()
	<-copied
	forgot := make(chan int)
	go func() { n, _ := r.ForgetKnown(ctx, ip("192.168.1.2"), ""); forgot <- n }()
	time.Sleep(20 * time.Millisecond)
	close(release)
	<-done
	if n := <-forgot; n != 1 {
		t.Fatalf("forgotten %d", n)
	}
	r.flushHook = nil
	r.flush(ctx)
	if got := seenRows(t, r); len(got) != 0 {
		t.Fatalf("the flush re-created %v", got)
	}
}

// Forgetting never holds seenMu while it waits for logs.db: DNS queries
// (Seen) go on while the logs.db writer is busy.
func TestForgetDoesNotBlockSeen(t *testing.T) {
	ctx := context.Background()
	r := newTestRegistry(t, true)
	r.Seen(ip("192.168.1.2"))
	r.flush(ctx)
	for i, forget := range []func() error{
		func() error { _, err := r.ForgetKnown(ctx, ip("192.168.1.2"), ""); return err },
		func() error { _, err := r.FlushKnown(ctx); return err },
	} {
		conn, err := r.ldb.W.Conn(ctx) // the only write connection: logs.db is busy
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- forget() }()
		time.Sleep(20 * time.Millisecond) // the forget waits for the writer now
		seen := make(chan struct{})
		go func() { r.Seen(ip("192.168.1.5")); close(seen) }()
		select {
		case <-seen:
		case <-time.After(2 * time.Second):
			t.Errorf("%d: Seen blocked while a forget waited for logs.db", i)
		}
		conn.Close()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		<-seen
	}
}

// The retention of logs.seenRetentionDays prunes memory and rows; a
// lowered value is applied at once (pruneKick).
func TestSeenRetention(t *testing.T) {
	ctx := context.Background()
	r := newTestRegistry(t, true)
	r.Seen(ip("192.168.1.2"))
	r.Seen(ip("192.168.1.3"))
	r.flush(ctx)
	old := time.Now().Add(-10 * 24 * time.Hour).UnixMilli()
	if _, err := r.ldb.W.Exec(`UPDATE clients_seen SET last_seen = ? WHERE ip = '192.168.1.2'`, old); err != nil {
		t.Fatal(err)
	}
	r.seenMu.Lock()
	e, _ := r.seen.peek(ip("192.168.1.2"))
	e.last = time.Now().Add(-10 * 24 * time.Hour)
	r.seenMu.Unlock()
	r.prune(ctx) // 30 days: kept
	if got := seenRows(t, r); len(got) != 2 {
		t.Fatalf("rows %v", got)
	}
	withConfig(r, func(c *Config) { c.SeenRetention = 7 * 24 * time.Hour })
	if len(r.pruneKick) != 1 {
		t.Fatal("a lowered retention must prune at once")
	}
	<-r.pruneKick
	r.prune(ctx)
	if got := seenRows(t, r); strings.Join(got, " ") != "192.168.1.3" {
		t.Fatalf("rows %v", got)
	}
	if _, ok := r.seen.peek(ip("192.168.1.2")); ok {
		t.Fatal("memory not pruned")
	}
	// Known never reaches back further than the retention.
	known, _ := r.Known(ctx, 365*24*time.Hour)
	if len(known) != 1 {
		t.Fatalf("known %v", known)
	}
	cfg := ConfigFrom(&settings.All{Logs: settings.Logs{SeenRetentionDays: 7}})
	if cfg.SeenRetention != 7*24*time.Hour {
		t.Fatalf("ConfigFrom %v", cfg.SeenRetention)
	}
}

// A name source switched off while running drops the host names stored
// with the seen data too (a stored name does not say which source it came
// from): Known no longer shows them. The configuration of the start never
// drops them, and switching a source on keeps them.
func TestNameSourceOffDropsStoredNames(t *testing.T) {
	ctx := context.Background()
	r := newTestRegistry(t, true)
	r.gateways = func() []netip.Addr { return nil }
	withConfig(r, func(*Config) {}) // the start
	if len(r.namesKick) != 0 {
		t.Fatal("the start must not drop the stored names")
	}
	r.SetPTRResolver(func(context.Context, netip.Addr) (string, error) { return "pc.lan", nil })
	r.Seen(ip("192.168.1.2"))
	r.lookupName(ctx, ip("192.168.1.2"))
	r.flush(ctx)
	var stored string
	if err := r.ldb.R.QueryRow(`SELECT hostname FROM clients_seen WHERE ip = '192.168.1.2'`).Scan(&stored); err != nil ||
		stored != "pc.lan" {
		t.Fatalf("stored name %q %v", stored, err)
	}
	withConfig(r, func(c *Config) { c.Sources.PTR = false })
	if len(r.namesKick) != 1 {
		t.Fatal("switching ptr off must drop the stored names")
	}
	<-r.namesKick
	r.clearStoredNames(ctx)
	known, err := r.Known(ctx, 0)
	if err != nil || len(known) != 1 || known[0].Hostname != "" {
		t.Fatalf("known %+v %v", known, err)
	}
	for _, on := range []func(c *Config){
		func(c *Config) { c.Sources.PTR = true },
		func(c *Config) { c.Sources.HostsFile = true },
	} {
		withConfig(r, on)
		if len(r.namesKick) != 0 {
			t.Fatal("switching a source on keeps the stored names")
		}
	}
	for _, off := range []func(c *Config){
		func(c *Config) { c.Sources.DHCP = false },
		func(c *Config) { c.Sources.HostsFile = false },
	} {
		withConfig(r, off)
		if len(r.namesKick) != 1 {
			t.Fatal("switching dhcp or hostsFile off must drop the stored names")
		}
		<-r.namesKick
	}
	// A registry whose first configuration has sources off (the start).
	r2 := newTestRegistry(t, true)
	withConfig(r2, func(c *Config) { c.Sources.PTR, c.Sources.DHCP = false, false })
	if len(r2.namesKick) != 0 {
		t.Fatal("the configuration of the start must not drop the stored names")
	}
}

func TestKnownVendor(t *testing.T) {
	r := newTestRegistry(t, false)
	r.gateways = func() []netip.Addr { return nil }
	setNeighbours(r, map[netip.Addr]string{ip("192.168.1.2"): "b8:27:eb:00:00:01", ip("192.168.1.3"): "da:a6:32:00:00:01"})
	r.SeenTransient(ip("192.168.1.2"))
	r.SeenTransient(ip("192.168.1.3"))
	rows, _ := r.Known(context.Background(), 0)
	got := map[string]Known{}
	for _, k := range rows {
		got[k.IP] = k
	}
	if k := got["192.168.1.2"]; k.Vendor != "Raspberry Pi Foundation" || k.MACRandomized {
		t.Fatalf("%+v", k)
	}
	if k := got["192.168.1.3"]; k.Vendor != "" || !k.MACRandomized {
		t.Fatalf("%+v", k)
	}
}

// whoisServer is an RDAP server with its own bootstrap file.
func whoisServer(t *testing.T, redirects int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case req.URL.Path == "/ipv4.json":
			fmt.Fprintf(w, `{"services":[[["8.0.0.0/8","203.0.113.0/24"],["http://ignored/","%s/rdap/"]]]}`, srv.URL)
		case req.URL.Path == "/ipv6.json":
			fmt.Fprintf(w, `{"services":[[["2001:4860::/32"],["%s/rdap6/"]]]}`, srv.URL)
		case strings.HasPrefix(req.URL.Path, "/hop/"):
			n := 0
			fmt.Sscanf(strings.TrimPrefix(req.URL.Path, "/hop/"), "%d", &n)
			if n < redirects {
				http.Redirect(w, req, fmt.Sprintf("/hop/%d", n+1), http.StatusFound)
				return
			}
			fallthrough
		case strings.HasPrefix(req.URL.Path, "/rdap/ip/8.8.8.0"), strings.HasPrefix(req.URL.Path, "/rdap6/ip/2001:4860:4860::"):
			hits.Add(1)
			if redirects > 0 && !strings.HasPrefix(req.URL.Path, "/hop/") {
				http.Redirect(w, req, "/hop/1", http.StatusFound)
				return
			}
			w.Write([]byte(`{"objectClassName":"ip network","name":"LVLT-GOGL-8-8-8","country":"us",
				"entities":[{"roles":["technical"],"vcardArray":["vcard",[["fn",{},"text","NOC"]]]},
				{"roles":["registrant"],"vcardArray":["vcard",[["version",{},"text","4.0"],["fn",{},"text","Google‮LLC  \u0007"]]]}]}`))
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func newWhoisRegistry(t *testing.T, srv *httptest.Server) *Registry {
	r := newTestRegistry(t, false)
	r.gateways = func() []netip.Addr { return nil }
	r.SetWhoisClient(srv.Client(), "PiCache/test")
	r.whois.bootstrapURL = func(v6 bool) string {
		if v6 {
			return srv.URL + "/ipv6.json"
		}
		return srv.URL + "/ipv4.json"
	}
	r.whois.connected = func() []netip.Prefix { return []netip.Prefix{netip.MustParsePrefix("192.168.1.0/24")} }
	r.whois.hostAddrs = func() []netutil.HostAddr {
		return []netutil.HostAddr{{Iface: "eth0", Prefix: netip.MustParsePrefix("2a01:1:2:3::10/64"), Up: true}}
	}
	withConfig(r, func(c *Config) {
		c.Sources.WHOIS = true
		c.AllowedNetworks = []netip.Prefix{netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("100.0.0.0/8")}
		c.TrustedForwarders = []netip.Addr{ip("9.9.9.9")}
	})
	return r
}

func TestWhoisLookup(t *testing.T) {
	ctx := context.Background()
	srv, hits := whoisServer(t, 0)
	r := newWhoisRegistry(t, srv)
	var slept []time.Duration
	now := time.Now()
	r.whois.now = func() time.Time { return now }
	r.whois.sleep = func(_ context.Context, d time.Duration) bool { slept = append(slept, d); now = now.Add(d); return true }
	r.whois.process(ctx, whoisItem{ip: ip("8.8.8.8"), network: whoisNetwork(ip("8.8.8.8"))})
	if got := r.whois.info(ip("8.8.8.77")); got == nil || got.Org != "GoogleLLC" || got.Country != "US" {
		t.Fatalf("info %+v", got)
	}
	// Cached: the same network is not looked up again.
	r.whois.process(ctx, whoisItem{ip: ip("8.8.8.9"), network: whoisNetwork(ip("8.8.8.9"))})
	if hits.Load() != 1 {
		t.Fatalf("%d lookups", hits.Load())
	}
	// Rate: the next lookup waits 10 s.
	r.whois.process(ctx, whoisItem{ip: ip("2001:4860:4860::8888"), network: whoisNetwork(ip("2001:4860:4860::8888"))})
	if len(slept) != 1 || slept[0] != whoisGap || hits.Load() != 2 {
		t.Fatalf("slept %v, %d lookups", slept, hits.Load())
	}
	// Not eligible: never looked up (connected network, neighbour, a narrow
	// allowed network, a trusted forwarder, the /48 of this machine's
	// global IPv6 address, private addresses).
	setNeighbours(r, map[netip.Addr]string{ip("8.8.4.4"): "aa:00:00:00:00:44"})
	for _, a := range []string{"192.168.1.20", "8.8.4.4", "203.0.113.5", "9.9.9.9", "2a01:1:2:ff::1", "10.0.0.1", "fd00::1"} {
		before := r.whois.lookups.Load()
		r.whois.process(ctx, whoisItem{ip: ip(a), network: whoisNetwork(ip(a))})
		if r.whois.lookups.Load() != before {
			t.Errorf("%s was looked up", a)
		}
	}
	// A broad allowed network (a carrier's range) stays eligible.
	if !r.whois.eligible(ip("100.1.2.3")) {
		t.Error("an address of a broad allowed network must stay eligible")
	}
	// 100 a day.
	r.whois.mu.Lock()
	r.whois.dayCount = whoisPerDay
	r.whois.mu.Unlock()
	before := r.whois.lookups.Load()
	r.whois.cache.clear()
	r.whois.process(ctx, whoisItem{ip: ip("8.8.8.8"), network: whoisNetwork(ip("8.8.8.8"))})
	if r.whois.lookups.Load() != before {
		t.Fatal("more than 100 lookups a day")
	}
	// Anonymised: nothing shown.
	withConfig(r, func(c *Config) { c.Anonymize = true })
	if r.whois.info(ip("8.8.8.8")) != nil {
		t.Fatal("shown while anonymised")
	}
}

func TestWhoisRedirects(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		redirects int
		found     bool
	}{{3, true}, {4, false}} {
		srv, _ := whoisServer(t, tc.redirects)
		r := newWhoisRegistry(t, srv)
		r.whois.process(ctx, whoisItem{ip: ip("8.8.8.8"), network: whoisNetwork(ip("8.8.8.8"))})
		r.whois.mu.Lock()
		e, ok := r.whois.cache.peek(whoisNetwork(ip("8.8.8.8")))
		r.whois.mu.Unlock()
		if !ok || (e.info != nil) != tc.found {
			t.Errorf("%d redirects: %+v %v", tc.redirects, e, ok)
		}
	}
}

func FuzzParseRDAP(f *testing.F) {
	f.Add([]byte(`{"name":"NET","country":"DE","entities":[{"roles":["registrant"],"vcardArray":["vcard",[["fn",{},"text","ACME"]]]}]}`))
	f.Add([]byte(`{"name":1,"entities":[{"roles":[1],"vcardArray":[1,2]}]}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		info, err := parseRDAP(b)
		if err != nil {
			return
		}
		if info.Org == "" || len([]rune(info.Org)) > maxWhoisText || (info.Country != "" && len(info.Country) != 2) {
			t.Fatalf("%+v", info)
		}
		_, _ = parseBootstrap(b)
	})
}
