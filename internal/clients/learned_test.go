package clients

import (
	"context"
	"maps"
	"net/netip"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// setNeighbours replaces the neighbour table source and reads it once.
func setNeighbours(r *Registry, m map[netip.Addr]string) {
	r.readARP = func() map[netip.Addr]string { return m }
	r.refreshARP()
}

// A device configured by its IPv4 address (or a CIDR) is recognised when it
// queries from its IPv6 addresses, privacy addresses included, as soon as
// they are in the neighbour table.
func TestLearnedMACCarriesIPClientToIPv6(t *testing.T) {
	r := newTestRegistry(t, false)
	r.gateways = func() []netip.Addr { return nil }
	kids := mustGroup(t, r, "Kids", true)
	laptop := mustClient(t, r, "laptop", []int64{kids.ID}, "192.168.1.5")
	lan := mustClient(t, r, "lan", nil, "192.168.2.0/24")
	temp := ip("2001:db8:1:0:1234:5678:9abc:def0") // temporary address
	if id := r.Identify(temp); id.ClientID != 0 {
		t.Fatalf("before the neighbour read: %+v", id)
	}
	setNeighbours(r, map[netip.Addr]string{
		ip("192.168.1.5"): "aa:00:00:00:00:05", ip("fd00::5"): "aa:00:00:00:00:05", temp: "aa:00:00:00:00:05",
		ip("fe80::5"):     "aa:00:00:00:00:05",
		ip("192.168.2.7"): "aa:00:00:00:00:07", ip("fd00::7"): "aa:00:00:00:00:07",
	})
	for _, a := range []string{"192.168.1.5", "fd00::5", temp.String(), "fe80::5"} {
		if id := r.Identify(ip(a)); id.ClientID != laptop.ID || id.Name != "laptop" || !slices.Equal(id.GroupIDs, []int64{kids.ID}) {
			t.Errorf("Identify(%s) = %+v, want the laptop in Kids", a, id)
		}
	}
	if id := r.Identify(ip("fd00::7")); id.ClientID != lan.ID {
		t.Errorf("CIDR client over IPv6: %+v", id)
	}
	// Known and Describe report the learned client too.
	if id, name, _ := r.Describe(ip("fd00::99"), "aa:00:00:00:00:05"); id != laptop.ID || name != "laptop" {
		t.Errorf("Describe by learned MAC = %d %q", id, name)
	}
	r.Seen(ip("fd00::5"))
	if known, err := r.Known(context.Background(), 0); err != nil || len(known) != 1 || known[0].ClientID != laptop.ID ||
		known[0].MAC != "aa:00:00:00:00:05" {
		t.Errorf("known %+v %v", known, err)
	}
	// Removing the IPv4 identifier drops the learned MAC.
	if _, err := r.UpdateClient(context.Background(), laptop.ID, ClientInput{Name: "laptop", Identifiers: []string{"192.168.9.9"}}); err != nil {
		t.Fatal(err)
	}
	if id := r.Identify(ip("fd00::5")); id.ClientID != 0 {
		t.Errorf("after the identifier changed: %+v", id)
	}
}

// Precedence: exact IP → CIDR → MAC identifier → learned MAC → Default.
// A MAC that maps to two clients, the gateways' MACs and a MAC behind
// which several IPv4 addresses belong to different devices are never
// learned.
func TestLearnedMACRules(t *testing.T) {
	r := newTestRegistry(t, false)
	r.gateways = func() []netip.Addr { return []netip.Addr{ip("192.168.1.1"), ip("fe80::1")} }
	laptop := mustClient(t, r, "laptop", nil, "192.168.1.5")
	phone := mustClient(t, r, "phone", nil, "192.168.1.6")
	tv := mustClient(t, r, "tv", nil, "aa:00:00:00:00:10")
	ula := mustClient(t, r, "ula net", nil, "fd00:1::/64")
	exact6 := mustClient(t, r, "exact v6", nil, "fd00::66")
	router := mustClient(t, r, "router", nil, "192.168.1.1")
	setNeighbours(r, map[netip.Addr]string{
		// the router: its MAC is never learned
		ip("192.168.1.1"): "aa:00:00:00:00:01", ip("fd00::1"): "aa:00:00:00:00:01",
		// one MAC, two configured clients: ambiguous
		ip("192.168.1.5"): "aa:00:00:00:00:05", ip("192.168.1.6"): "aa:00:00:00:00:05", ip("fd00::5"): "aa:00:00:00:00:05",
		// a repeater that rewrites MACs: one configured, one other IPv4 device
		ip("192.168.1.20"): "aa:00:00:00:00:20", ip("192.168.1.21"): "aa:00:00:00:00:20", ip("fd00::20"): "aa:00:00:00:00:20",
		// the TV by MAC identifier; its IPv4 address is inside no client
		ip("192.168.1.10"): "aa:00:00:00:00:10", ip("fd00::10"): "aa:00:00:00:00:10",
		// a device configured by exact IPv6 whose other address is in a CIDR
		ip("fd00::66"): "aa:00:00:00:00:66", ip("fd00:1::66"): "aa:00:00:00:00:66", ip("2001:db8::66"): "aa:00:00:00:00:66",
	})
	mustClient(t, r, "one behind the repeater", nil, "192.168.1.20")
	for _, tc := range []struct {
		addr string
		want int64
	}{
		{"fd00::1", 0},                    // gateway MAC not learned
		{"fd00::5", 0},                    // ambiguous
		{"192.168.1.5", laptop.ID},        // exact IP still wins
		{"192.168.1.6", phone.ID},         // exact IP still wins
		{"fd00::20", 0},                   // several IPv4 devices behind one MAC
		{"fd00::10", tv.ID},               // MAC identifier
		{"fd00:1::66", ula.ID},            // CIDR beats the learned MAC
		{"fd00::66", exact6.ID},           // exact IPv6
		{"2001:db8::66", 0},               // its MAC maps to two clients (exact v6 and the ULA CIDR)
		{"192.168.1.99", 0},               // unknown
		{"fd00::10", tv.ID},               // cached
		{"fd00:1::77", ula.ID},            // CIDR without neighbour entry
		{"2001:db8::10", 0},               // not in the neighbour table
		{"fe80::1", 0},                    // not in the neighbour table
		{"::ffff:192.168.1.5", laptop.ID}, // canonical
		{"fd00::5", 0},                    // still ambiguous
		{"192.168.1.1", router.ID},        // the router's own IPv4 identifier
	} {
		if id := r.Identify(ip(tc.addr)); id.ClientID != tc.want {
			t.Errorf("Identify(%s) = client %d (%q), want %d", tc.addr, id.ClientID, id.Name, tc.want)
		}
	}
}

// An on-link source without a MAC is cached for at most 2 s and requests an
// early neighbour-table read; once the MAC is known the identity is cached
// normally.
func TestShortIdentityWithoutMAC(t *testing.T) {
	r := newTestRegistry(t, false)
	r.gateways = func() []netip.Addr { return nil }
	r.onLink = func(a netip.Addr) bool { return a.Is6() || a == ip("192.168.1.5") }
	mustClient(t, r, "laptop", nil, "192.168.1.5")
	var table atomic.Pointer[map[netip.Addr]string]
	empty := map[netip.Addr]string{}
	table.Store(&empty)
	r.readARP = func() map[netip.Addr]string { return *table.Load() }

	before := time.Now()
	if id := r.Identify(ip("fd00::5")); id.ClientID != 0 || id.MAC != "" {
		t.Fatalf("first identity %+v", id)
	}
	if len(r.arpKick) != 1 {
		t.Fatal("an early neighbour read must be requested")
	}
	e, _ := r.cache.peek(ip("fd00::5"))
	if e.expires == 0 || e.expires > before.Add(shortIdentityTTL+time.Second).UnixNano() {
		t.Fatalf("expiry %d: want at most 2 s", e.expires)
	}
	// Expired entries are resolved again.
	r.cacheMu.Lock()
	e.expires = time.Now().Add(-time.Millisecond).UnixNano()
	r.cache.put(ip("fd00::5"), e)
	r.cacheMu.Unlock()
	next := map[netip.Addr]string{ip("192.168.1.5"): "aa:00:00:00:00:05", ip("fd00::5"): "aa:00:00:00:00:05"}
	table.Store(&next)
	r.refreshARP()
	id := r.Identify(ip("fd00::5"))
	if id.Name != "laptop" || id.MAC != "aa:00:00:00:00:05" {
		t.Fatalf("after the read: %+v", id)
	}
	if e, _ := r.cache.peek(ip("fd00::5")); e.expires != 0 {
		t.Errorf("an identity with MAC is cached until invalidated, expiry %d", e.expires)
	}
	// Off-link sources (routed) never ask for a read.
	<-r.arpKick
	r.Identify(ip("10.9.9.9"))
	if len(r.arpKick) != 0 {
		t.Error("an off-link source must not request a neighbour read")
	}
}

// Early neighbour reads are coalesced: at most one per second, however
// many are requested.
func TestEarlyNeighbourReadsCoalesced(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var reads atomic.Int32
		r := &Registry{
			readARP:  func() map[netip.Addr]string { reads.Add(1); return map[netip.Addr]string{} },
			gateways: func() []netip.Addr { return nil },
			arpKick:  make(chan struct{}, 1),
			cache:    newLRU[netip.Addr, cachedIdentity](8),
		}
		empty := map[netip.Addr]string{}
		r.arp.Store(&empty)
		r.snap.Store(newSnapshot(nil, nil))
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { r.arpLoop(ctx); close(done) }()
		defer func() { cancel(); <-done }()
		synctest.Wait()
		if reads.Load() != 1 {
			t.Fatalf("initial reads %d", reads.Load())
		}
		for range 20 {
			r.kickARP()
		}
		synctest.Wait()
		if reads.Load() != 1 {
			t.Fatalf("a read within a second of the last one: %d", reads.Load())
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if reads.Load() != 2 {
			t.Fatalf("reads after a second: %d, want 2", reads.Load())
		}
		for range 20 {
			r.kickARP()
		}
		time.Sleep(500 * time.Millisecond)
		synctest.Wait()
		if reads.Load() != 2 {
			t.Fatalf("reads after 1.5 s: %d, want 2", reads.Load())
		}
		time.Sleep(600 * time.Millisecond)
		synctest.Wait()
		if reads.Load() != 3 {
			t.Fatalf("reads after 2.1 s: %d, want 3", reads.Load())
		}
		// A request that arrived during the wait is served one second later;
		// then nothing happens until the 30 s tick.
		time.Sleep(10 * time.Second)
		synctest.Wait()
		if n := reads.Load(); n != 3 && n != 4 {
			t.Fatalf("reads without further requests: %d, want 3 or 4", n)
		}
	})
}

// Without a PTR name, an address takes the name of another address with
// the same MAC: an IPv4 address's name first, then the most recently
// resolved one. Identify, Describe and Known use it.
func TestNameFallbackByMAC(t *testing.T) {
	r := newTestRegistry(t, true)
	r.gateways = func() []netip.Addr { return nil }
	setNeighbours(r, map[netip.Addr]string{
		ip("192.168.1.30"): "aa:00:00:00:00:30", ip("fd00::30"): "aa:00:00:00:00:30",
		ip("2001:db8::30"): "aa:00:00:00:00:30", ip("2001:db8::31"): "aa:00:00:00:00:30",
		ip("fd00::40"): "aa:00:00:00:00:40", ip("2001:db8::40"): "aa:00:00:00:00:40", ip("2001:db8::41"): "aa:00:00:00:00:40",
	})
	now := time.Now()
	r.namesMu.Lock()
	r.names.put(ip("fd00::30"), hostName{name: "phone-v6.lan", at: now})
	r.names.put(ip("192.168.1.30"), hostName{name: "phone.lan", at: now.Add(-time.Hour)})
	r.names.put(ip("2001:db8::31"), hostName{name: "own.lan", at: now})
	r.names.put(ip("fd00::40"), hostName{name: "old.lan", at: now.Add(-time.Hour)})
	r.names.put(ip("2001:db8::40"), hostName{name: "new.lan", at: now})
	r.namesMu.Unlock()
	r.invalidate()
	for addr, want := range map[string]string{
		"2001:db8::30": "phone.lan",    // the IPv4 address's name wins over a newer IPv6 name
		"2001:db8::31": "own.lan",      // its own PTR name first
		"2001:db8::41": "new.lan",      // no IPv4 name: the most recently resolved one
		"192.168.1.99": "",             // not in the neighbour table
		"fd00::30":     "phone-v6.lan", // own name
	} {
		if got := r.DisplayName(ip(addr)); got != want {
			t.Errorf("DisplayName(%s) = %q, want %q", addr, got, want)
		}
	}
	if _, _, hn := r.Describe(ip("2001:db8::99"), "aa:00:00:00:00:30"); hn != "phone.lan" {
		t.Errorf("Describe host name %q", hn)
	}
	// Known: from the live names, else from other rows with the same MAC.
	r.Seen(ip("2001:db8::30"))
	r.flush(context.Background())
	if _, err := r.ldb.W.Exec(`INSERT INTO clients_seen (ip, mac, hostname, first_seen, last_seen, queries)
		VALUES ('192.168.7.7', 'aa:00:00:00:00:77', 'tablet.lan', 1, ?, 1), ('fd00::77', 'aa:00:00:00:00:77', '', 1, ?, 1)`,
		now.UnixMilli(), now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	known, err := r.Known(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]string{}
	for _, k := range known {
		names[k.IP] = k.Hostname
	}
	if names["2001:db8::30"] != "phone.lan" || names["fd00::77"] != "tablet.lan" {
		t.Errorf("known host names %v", names)
	}
	// A new name of the IPv4 address reaches the identities of its IPv6
	// addresses (their cache entries are dropped).
	r.SetPTRResolver(func(context.Context, netip.Addr) (string, error) { return "renamed.lan", nil })
	r.lookupName(context.Background(), ip("192.168.1.30"))
	if got := r.DisplayName(ip("2001:db8::30")); got != "renamed.lan" {
		t.Errorf("after the rename: %q", got)
	}
}

// Devices keys addresses by configured client, else MAC (neighbour table,
// else the seen data), else address.
func TestDevices(t *testing.T) {
	r := newTestRegistry(t, true)
	r.gateways = func() []netip.Addr { return nil }
	laptop := mustClient(t, r, "laptop", nil, "192.168.1.5")
	setNeighbours(r, map[netip.Addr]string{
		ip("192.168.1.5"): "aa:00:00:00:00:05", ip("fd00::5"): "aa:00:00:00:00:05",
		ip("192.168.1.8"): "aa:00:00:00:00:08", ip("fd00::8"): "aa:00:00:00:00:08",
	})
	r.namesMu.Lock()
	r.names.put(ip("192.168.1.8"), hostName{name: "tv.lan", at: time.Now()})
	r.namesMu.Unlock()
	if _, err := r.ldb.W.Exec(`INSERT INTO clients_seen (ip, mac, hostname, first_seen, last_seen, queries)
		VALUES ('fd00::9', 'aa:00:00:00:00:09', 'nas.lan', 1, 2, 1)`); err != nil {
		t.Fatal(err)
	}
	got, err := r.Devices(context.Background(), []string{"192.168.1.5", "fd00::5", "fd00::8", "fd00::9", "10.0.0.1", "garbage"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]Device{
		"192.168.1.5": {Key: "client:" + strconv.FormatInt(laptop.ID, 10), ClientID: laptop.ID, MAC: "aa:00:00:00:00:05", Name: "laptop"},
		"fd00::5":     {Key: "client:" + strconv.FormatInt(laptop.ID, 10), ClientID: laptop.ID, MAC: "aa:00:00:00:00:05", Name: "laptop"},
		"fd00::8":     {Key: "mac:aa:00:00:00:00:08", MAC: "aa:00:00:00:00:08", Name: "tv.lan"},
		"fd00::9":     {Key: "mac:aa:00:00:00:00:09", MAC: "aa:00:00:00:00:09", Name: "nas.lan"},
		"10.0.0.1":    {Key: "ip:10.0.0.1"},
		"garbage":     {Key: "ip:garbage"},
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("Devices[%s] = %+v, want %+v", k, got[k], w)
		}
	}
}

// The first query of a device's new on-link address already gets the
// device's client: Identify makes the kernel resolve the address and waits
// for the neighbour entry. Without MAC-based clients, for link-local
// addresses and beyond the rate limit nothing is sent.
func TestPrimeNewAddress(t *testing.T) {
	r := newTestRegistry(t, false)
	r.gateways = func() []netip.Addr { return nil }
	r.onLink = func(a netip.Addr) bool { return true }
	const mac = "aa:00:00:00:00:05"
	var table atomic.Pointer[map[netip.Addr]string]
	setTable := func(m map[netip.Addr]string) { table.Store(&m) }
	setTable(map[netip.Addr]string{})
	r.readARP = func() map[netip.Addr]string { return *table.Load() }
	var probed []netip.Addr
	answer := true
	r.probe = func(a netip.Addr) {
		probed = append(probed, a)
		if answer {
			m := maps.Clone(*table.Load())
			m[a] = mac
			setTable(m)
		}
	}

	// No client configured: a MAC decides nothing, nothing is sent.
	if id := r.Identify(ip("fd00::77")); id.ClientID != 0 || len(probed) != 0 {
		t.Fatalf("without clients: %+v, probed %v", id, probed)
	}

	// The laptop is configured by IPv4 and has just queried over IPv4, but
	// no regular neighbour read has learned its MAC yet: the read of the
	// first query from a new temporary address learns it.
	laptop := mustClient(t, r, "laptop", nil, "192.168.1.5")
	setTable(map[netip.Addr]string{ip("192.168.1.5"): mac})
	if id := r.Identify(ip("fd00::78")); id.ClientID != laptop.ID || id.MAC != mac {
		t.Fatalf("new address: %+v", id)
	}
	if !slices.Equal(probed, []netip.Addr{ip("fd00::78")}) {
		t.Fatalf("probed %v", probed)
	}

	// Link-local addresses are not probed.
	r.Identify(ip("fe80::79"))
	if len(probed) != 1 {
		t.Errorf("link-local probed: %v", probed)
	}

	// No answer: Default for now, cached briefly, early read requested.
	answer = false
	if id := r.Identify(ip("fd00::7a")); id.ClientID != 0 {
		t.Errorf("unanswered probe: %+v", id)
	}
	if e, _ := r.cache.peek(ip("fd00::7a")); e.expires == 0 {
		t.Error("an unresolved address must be cached briefly")
	}

	// Rate limit: beyond primePerSecond nothing is sent.
	answer = true
	n := len(probed)
	r.primeSecond.Store(time.Now().Unix())
	r.primeCount.Store(primePerSecond)
	if id := r.Identify(ip("fd00::7b")); id.ClientID != 0 || len(probed) != n {
		t.Errorf("rate limited: %+v, probed %v", id, probed[n:])
	}
}

// A DHCP lease name of PiCache is the first name source of an address (it
// wins over the PTR name) and names the device's other addresses; a
// change reaches cached identities with LeaseNamesChanged.
func TestLeaseNames(t *testing.T) {
	r := newTestRegistry(t, false)
	r.gateways = func() []netip.Addr { return nil }
	setNeighbours(r, map[netip.Addr]string{
		ip("192.168.1.30"): "aa:00:00:00:00:30", ip("fd00::30"): "aa:00:00:00:00:30",
	})
	r.namesMu.Lock()
	r.names.put(ip("192.168.1.30"), hostName{name: "router-name.fritz.box", at: time.Now()})
	r.namesMu.Unlock()
	if got := r.DisplayName(ip("192.168.1.30")); got != "router-name.fritz.box" {
		t.Fatalf("before: %q", got)
	}
	var lease atomic.Value
	lease.Store("laptop.lan")
	r.SetLeaseNames(func(a netip.Addr) string {
		if a == ip("192.168.1.30") {
			return lease.Load().(string)
		}
		return ""
	})
	r.LeaseNamesChanged([]netip.Addr{ip("192.168.1.30")})
	for _, a := range []string{"192.168.1.30", "fd00::30"} {
		if got := r.DisplayName(ip(a)); got != "laptop.lan" {
			t.Errorf("DisplayName(%s) = %q, want the lease name", a, got)
		}
	}
	if _, _, hn := r.Describe(ip("fd00::99"), "aa:00:00:00:00:30"); hn != "laptop.lan" {
		t.Errorf("Describe %q", hn)
	}
	lease.Store("Bad Name!")
	r.LeaseNamesChanged([]netip.Addr{ip("192.168.1.30")})
	if got := r.DisplayName(ip("192.168.1.30")); got != "router-name.fritz.box" {
		t.Errorf("an invalid lease name is not shown: %q", got)
	}
	if mac, ok := r.NeighbourMAC(ip("fd00::30")); !ok || mac != "aa:00:00:00:00:30" {
		t.Errorf("NeighbourMAC %q %v", mac, ok)
	}
}
