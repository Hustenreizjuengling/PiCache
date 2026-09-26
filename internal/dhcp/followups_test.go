package dhcp

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// lease gives mac the address want through DISCOVER and REQUEST.
func (ts *testSvc) lease(t *testing.T, mac [6]byte, want string, opts ...func(*req)) *message {
	t.Helper()
	d := reqFrom(msgDiscover, mac)
	r := reqFrom(msgRequest, mac).addr(optRequestedIP, want).addr(optServerID, "192.168.1.10")
	for _, o := range opts {
		o(d)
		o(r)
	}
	ts.reply(t, d)
	ack, _ := ts.reply(t, r)
	if ack.msgType() != msgAck || ack.yiaddr != ip(want) {
		t.Fatalf("lease %v: %v %v", want, ack.msgType(), ack.yiaddr)
	}
	return ack
}

func withHost(h string) func(*req) {
	return func(r *req) { r.opt(optHostName, []byte(h)...) }
}

func withClientID(b ...byte) func(*req) {
	return func(r *req) { r.opt(optClientID, b...) }
}

func mustStatic(t *testing.T, ts *testSvc, in StaticInput) StaticLease {
	t.Helper()
	st, err := ts.CreateStatic(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func wantField(t *testing.T, err error, field string) {
	t.Helper()
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Field != field {
		t.Fatalf("err = %v, want field %s", err, field)
	}
}

// K2: a lease without a usable host name, or whose name another client
// holds, answers the generated name of its address, only in DNS: never in
// option 12, never as the lease's host name or display name.
func TestGeneratedNames(t *testing.T) {
	ts := newTestSvc(t, enabledDHCP)
	ack := ts.lease(t, macN(1), "192.168.1.100")
	if ack.opts[optHostName] != nil {
		t.Fatal("generated name sent in option 12")
	}
	if a, ttl, ok := ts.LeaseAddr("192-168-1-100.lan"); !ok || a != ip("192.168.1.100") || ttl != 300 {
		t.Fatalf("A %v %d %v", a, ttl, ok)
	}
	if n, _, ok := ts.LeasePTR(ip("192.168.1.100")); !ok || n != "192-168-1-100.lan" {
		t.Fatalf("PTR %q", n)
	}
	if ts.LeaseName(ip("192.168.1.100")) != "" {
		t.Fatalf("display %q", ts.LeaseName(ip("192.168.1.100")))
	}
	if l := ts.Leases()[0]; l.Hostname != "" || l.DNSName != "192-168-1-100.lan" || !l.NameGenerated {
		t.Fatalf("lease %+v", l)
	}
	// Names a client may not take: the generated form (another address's
	// name), wpad and localhost.
	for i, h := range []string{"192-168-1-100", "10-0-0-1", "WPAD", "localhost"} {
		mac := macN(byte(10 + i))
		want := fmt.Sprintf("192.168.1.%d", 110+i)
		if ack := ts.lease(t, mac, want, withHost(h)); ack.opts[optHostName] != nil {
			t.Errorf("%s: option 12 %q", h, ack.opts[optHostName])
		}
		if _, _, ok := ts.LeaseAddr(strings.ToLower(h) + ".lan"); ok && !generatedForm(h) {
			t.Errorf("%s answered", h)
		}
		if l := leaseOf(ts, macStr(mac)); l.Hostname != "" || l.DNSName != generatedName(ip(want))+".lan" {
			t.Errorf("%s: lease %+v", h, l)
		}
	}
	if a, _, ok := ts.LeaseAddr("192-168-1-100.lan"); !ok || a != ip("192.168.1.100") {
		t.Fatal("a client took another address's generated name")
	}
	// A reservation may name wpad explicitly; a name of the generated form
	// must be its own address's.
	mustStatic(t, ts, StaticInput{MAC: "02:00:00:00:00:30", IP: "192.168.1.30", Hostname: "wpad"})
	mustStatic(t, ts, StaticInput{MAC: "02:00:00:00:00:31", IP: "192.168.1.31", Hostname: "192-168-1-31"})
	_, err := ts.CreateStatic(context.Background(), StaticInput{MAC: "02:00:00:00:00:32", IP: "192.168.1.32", Hostname: "192-168-1-33"})
	wantField(t, err, "hostname")
	// Switched off: no generated names.
	if _, err := ts.set.Update(context.Background(), func(a *settings.All) error { a.DHCP.GenerateNames = false; return nil }); err != nil {
		t.Fatal(err)
	}
	ts.refreshNames()
	if _, _, ok := ts.LeaseAddr("192-168-1-100.lan"); ok {
		t.Fatal("generated name without generateNames")
	}
}

// K3: NTP servers, MTU and WPAD URL are sent only when requested, in OFFER,
// ACK and the INFORM ACK; option 119 carries the domain first and drops
// extras from the end when the reply is too small, never the domain.
func TestTypedOptions(t *testing.T) {
	ts := newTestSvc(t, func(a *settings.All) {
		enabledDHCP(a)
		a.DHCP.Options = settings.DHCPOptions{NTPServers: []string{"192.168.1.2", "192.168.1.3"}, MTU: 1492,
			WPADURL: "http://proxy.lan/wpad.dat", ExtraSearchDomains: []string{"corp.example", "lab.example"}}
	})
	prl := []byte{optSubnetMask, optNTPServers, optInterfaceMTU, optWPAD, optDomainSearch}
	off, _ := ts.reply(t, reqFrom(msgDiscover, macN(1)).opt(optParamRequest, prl...))
	want := map[byte]string{
		optNTPServers:   "\xc0\xa8\x01\x02\xc0\xa8\x01\x03",
		optInterfaceMTU: "\x05\xd4",
		optWPAD:         "http://proxy.lan/wpad.dat",
		optDomainSearch: "\x03lan\x00\x04corp\x07example\x00\x03lab\x07example\x00",
	}
	for code, w := range want {
		if got := string(off.opts[code]); got != w {
			t.Errorf("OFFER option %d = %q, want %q", code, got, w)
		}
	}
	plain, _ := ts.reply(t, reqFrom(msgDiscover, macN(2)))
	for code := range want {
		if plain.opts[code] != nil {
			t.Errorf("option %d sent without being requested", code)
		}
	}
	inform := reqFrom(msgInform, macN(3)).opt(optParamRequest, prl...)
	inform.ciaddr = ip("192.168.1.50")
	ack, _ := ts.reply(t, inform)
	for code, w := range want {
		if got := string(ack.opts[code]); got != w {
			t.Errorf("INFORM ACK option %d = %q", code, got)
		}
	}
	// Extras that do not fit are dropped from the end, the domain stays.
	lists := searchLists("lan", []string{strings.Repeat("a", 60) + ".example", strings.Repeat("b", 60) + ".example"})
	if len(lists) != 3 || string(lists[2]) != "\x03lan\x00" {
		t.Fatalf("lists %q", lists)
	}
	req := reqFrom(msgInform, macN(4)).opt(optParamRequest, optDomainSearch)
	m, _ := parseMessage(req.bytes())
	b := buildReply(m, replyFields{typ: msgAck}, testSelf, []option{
		{code: optDNS, data: make([]byte, 200)},
		{code: optDomainName, data: make([]byte, 200)},
		{code: optDomainSearch, data: lists[0], alts: lists[1:]},
	})
	r, _ := parseMessage(b)
	if got := string(r.opts[optDomainSearch]); !strings.HasPrefix(got, "\x03lan\x00") || len(b) > maxReply {
		t.Fatalf("truncated search list %q (%d bytes)", got, len(b))
	}
	// Extras equal to the domain, and a list over 255 bytes, never.
	if l := searchLists("lan", []string{"lan"}); len(l) != 1 || string(l[0]) != "\x03lan\x00" {
		t.Fatalf("duplicate %q", l)
	}
	// The live check: an NTP server is not the subnet's network or
	// broadcast address.
	a := ts.set.Get().Clone()
	a.DHCP.Options.NTPServers = []string{"192.168.1.2", " ", "192.168.1.255"}
	wantField(t, ts.CheckSettings(a), "dhcp.options.ntpServers[1]")
}

// K4: DELETE /dhcp/leases ends every lease while serving (table, names,
// offers, quarantine); POST /dhcp/reset puts the settings back through
// settings.Update and empties both tables, but keeps what was observed of
// the network.
func TestResetLeasesAndDHCP(t *testing.T) {
	ts, op, dir := liveSvc(t, enabledDHCP, &Sockets{bindCapable: true})
	ctx := context.Background()
	ts.lease(t, macN(1), "192.168.1.100", withHost("pc"))
	mustStatic(t, ts, StaticInput{MAC: "02:00:00:00:00:02", IP: "192.168.1.50", Hostname: "printer"})
	ts.lease(t, macN(2), "192.168.1.50")
	ts.reply(t, reqFrom(msgDiscover, macN(3))) // a pending offer
	ts.mu.Lock()
	ts.t.quarantine[ip("192.168.1.150")] = t0.Add(quarantine)
	ts.mu.Unlock()
	n, err := ts.DeleteLeases(ctx)
	if err != nil || n != 2 || len(ts.Leases()) != 0 {
		t.Fatalf("deleted %d %v, leases %+v", n, err, ts.Leases())
	}
	ts.mu.Lock()
	offers, quar, statics := len(ts.t.offers), len(ts.t.quarantine), len(ts.t.statics)
	ts.mu.Unlock()
	if offers != 0 || quar != 0 || statics != 1 {
		t.Fatalf("offers %d quarantine %d statics %d", offers, quar, statics)
	}
	if _, _, ok := ts.LeaseAddr("pc.lan"); ok {
		t.Fatal("name of a deleted lease answered")
	}
	var rows int
	ts.d.R.QueryRow(`SELECT COUNT(*) FROM dhcp_leases`).Scan(&rows)
	if rows != 0 {
		t.Fatalf("rows %d", rows)
	}
	// Serving goes on: the device gets its address again on renewal.
	ts.lease(t, macN(1), "192.168.1.100", withHost("pc"))

	// Observations that survive the reset.
	ts.packet(reqFrom(msgRequest, macN(9)).addr(optRequestedIP, "192.168.1.20").addr(optServerID, "192.168.1.1"))
	ts.ann.recordRA("eth0", ip("fe80::1"), raWithDNS("fd00::1", 1800), ts.now())
	logged := len(ts.Log(MaxLog))
	ts.update(t, func(a *settings.All) { a.DHCP.IPv6.RouterAdvertisements = true; a.DHCP.Options.MTU = 1500 })
	rr, err := ts.Reset(ctx)
	if err != nil || !rr.Settings || rr.Leases != 1 || rr.Statics != 1 {
		t.Fatalf("reset %+v %v", rr, err)
	}
	if h := ts.set.Get().DHCP; !h.Equal(settings.Defaults().DHCP) {
		t.Fatalf("settings not reset: %+v", h)
	}
	if len(ts.Leases()) != 0 || len(ts.Statics()) != 0 {
		t.Fatal("tables not emptied")
	}
	ts.evaluate(ctx, true) // the switch-off path (the control loop does this after the kick)
	if st := ts.d2().state(); st.v4Open || st.v6Open {
		t.Fatalf("sockets after the reset: %+v", st)
	}
	if s, r := markers(dir); s || r {
		t.Fatal("markers after the reset")
	}
	st := ts.Status()
	if len(st.OtherServers) != 0 && st.OtherServers[0].ServerID != "192.168.1.1" {
		t.Fatalf("other servers %+v", st.OtherServers)
	}
	ts.mu.Lock()
	others := len(ts.others)
	ts.mu.Unlock()
	if others != 1 || st.LastProbe == nil || len(ts.Log(MaxLog)) < logged || len(st.IPv6.OtherAnnouncers) != 1 {
		t.Fatalf("observations lost: others %d, probe %v, log %d, announcers %d", others, st.LastProbe, len(ts.Log(MaxLog)),
			len(st.IPv6.OtherAnnouncers))
	}
	_ = op
}

// K4: when emptying the tables fails after the settings were reset, the
// result says the settings part took effect (the API audits it), and the
// tables are left as they were.
func TestResetWipeFails(t *testing.T) {
	ts := newTestSvc(t, enabledDHCP)
	ctx := context.Background()
	mustStatic(t, ts, StaticInput{MAC: "02:00:00:00:00:02", IP: "192.168.1.50"})
	if _, err := ts.d.W.Exec(`CREATE TRIGGER no_wipe BEFORE DELETE ON dhcp_static BEGIN SELECT RAISE(ABORT, 'disk full'); END`); err != nil {
		t.Fatal(err)
	}
	rr, err := ts.Reset(ctx)
	if err == nil || !rr.Settings || rr.Leases != 0 || rr.Statics != 0 {
		t.Fatalf("reset %+v %v", rr, err)
	}
	if !ts.set.Get().DHCP.Equal(settings.Defaults().DHCP) || len(ts.Statics()) != 1 {
		t.Fatalf("settings %+v, statics %+v", ts.set.Get().DHCP, ts.Statics())
	}
}

// K5: with onlyReserved a client without a reservation gets nothing: no
// OFFER, no ACK or NAK in any REQUEST state, a DECLINE is ignored; its
// RELEASE is processed and INFORM answered. Its lease is neither renewed
// nor ended; it expires, and its name is answered until then.
func TestOnlyReserved(t *testing.T) {
	ts := newTestSvc(t, enabledDHCP)
	ctx := context.Background()
	ts.lease(t, macN(1), "192.168.1.100", withHost("old"))
	mustStatic(t, ts, StaticInput{MAC: "02:00:00:00:00:02", IP: "192.168.1.50"})
	ts.update(t, func(a *settings.All) { a.DHCP.OnlyReserved = true })

	renew := reqFrom(msgRequest, macN(1))
	renew.ciaddr = ip("192.168.1.100")
	rebind := reqFrom(msgRequest, macN(1))
	rebind.ciaddr, rebind.flags = ip("192.168.1.100"), flagBcast
	for name, r := range map[string]*req{
		"discover":          reqFrom(msgDiscover, macN(1)),
		"selecting":         reqFrom(msgRequest, macN(1)).addr(optRequestedIP, "192.168.1.100").addr(optServerID, "192.168.1.10"),
		"init-reboot":       reqFrom(msgRequest, macN(1)).addr(optRequestedIP, "192.168.1.100"),
		"init-reboot other": reqFrom(msgRequest, macN(1)).addr(optRequestedIP, "10.0.0.9"),
		"renewing":          renew,
		"rebinding":         rebind,
		"decline":           reqFrom(msgDecline, macN(1)).addr(optRequestedIP, "192.168.1.100"),
	} {
		ts.clk.Add(time.Second) // not the rate limit
		before := len(ts.Log(MaxLog))
		if out := ts.packet(r); len(out) != 0 {
			t.Errorf("%s answered", name)
		}
		if e := ts.Log(1)[0]; len(ts.Log(MaxLog)) != before+1 || e.Reason != LogReasonNotReserved {
			t.Errorf("%s: log %+v", name, e)
		}
	}
	ts.clk.Add(-7 * time.Second)
	if l := ts.Leases(); len(l) != 1 || !l[0].Active || !l[0].Expires.Equal(t0.Add(24*time.Hour)) {
		t.Fatalf("lease touched: %+v", l)
	}
	if _, _, ok := ts.LeaseAddr("old.lan"); !ok {
		t.Fatal("name of the unreserved client's lease not answered until it expires")
	}
	log := ts.Log(1)[0]
	if log.Result != ResultIgnored || log.Reason != LogReasonNotReserved || log.MAC != "02:00:00:00:00:01" {
		t.Fatalf("log %+v", log)
	}
	ts.clk.Add(time.Second)
	inform := reqFrom(msgInform, macN(1))
	inform.ciaddr = ip("192.168.1.100")
	if out := ts.packet(inform); len(out) != 1 {
		t.Fatal("INFORM not answered")
	}
	rel := reqFrom(msgRelease, macN(1)).addr(optServerID, "192.168.1.10")
	rel.ciaddr = ip("192.168.1.100")
	ts.packet(rel)
	if l := ts.Leases(); l[0].Active {
		t.Fatal("RELEASE not processed")
	}
	// The reserved client is served.
	ts.lease(t, macN(2), "192.168.1.50")
	_ = ctx
}

// K6: a reservation's own lease time sets lease time and T1/T2 on its
// address; a client identifier matches a reservation of another MAC unless
// an active lease, a pending offer or another device holds the address; a
// MAC match always wins.
func TestReservationMatching(t *testing.T) {
	ts := newTestSvc(t, enabledDHCP)
	ctx := context.Background()
	mustStatic(t, ts, StaticInput{MAC: "02:00:00:00:00:01", IP: "192.168.1.50", Hostname: "tv", LeaseSeconds: 3600})
	off, _ := ts.reply(t, reqFrom(msgDiscover, macN(1)))
	if binary.BigEndian.Uint32(off.opts[optLeaseTime]) != 3600 || binary.BigEndian.Uint32(off.opts[optRenewal]) != 1800 ||
		binary.BigEndian.Uint32(off.opts[optRebinding]) != 3150 {
		t.Fatalf("lease times %v %v %v", off.opts[optLeaseTime], off.opts[optRenewal], off.opts[optRebinding])
	}
	ack := ts.lease(t, macN(1), "192.168.1.50")
	if binary.BigEndian.Uint32(ack.opts[optLeaseTime]) != 3600 {
		t.Fatal("ACK lease time")
	}
	if l := ts.Leases(); !l[0].Expires.Equal(t0.Add(time.Hour)) {
		t.Fatalf("expires %v", l[0].Expires)
	}

	// Client-ID match: MAC 02:..:0b with option 61 01:02:03 gets .60.
	st := mustStatic(t, ts, StaticInput{MAC: "02:00:00:00:00:0a", IP: "192.168.1.60", Hostname: "phone", ClientID: "01:02:03"})
	if st.ClientID != "01:02:03" {
		t.Fatalf("stored %+v", st)
	}
	_, err := ts.CreateStatic(ctx, StaticInput{MAC: "02:00:00:00:00:0c", IP: "192.168.1.61", ClientID: "01:02:03"})
	wantField(t, err, "clientId")
	_, err = ts.CreateStatic(ctx, StaticInput{MAC: "02:00:00:00:00:0c", IP: "192.168.1.61", ClientID: "zz"})
	wantField(t, err, "clientId")
	_, err = ts.CreateStatic(ctx, StaticInput{MAC: "02:00:00:00:00:0c", IP: "192.168.1.61", LeaseSeconds: 60})
	wantField(t, err, "leaseSeconds")
	ts.lease(t, macN(11), "192.168.1.60", withClientID(1, 2, 3))
	l := leaseOf(ts, "02:00:00:00:00:0b")
	if l.IP != "192.168.1.60" || !l.Static || l.DNSName != "phone.lan" || l.Hostname != "phone" {
		t.Fatalf("client-ID lease %+v", l)
	}
	if s := staticOf(ts, "02:00:00:00:00:0a"); !s.Active {
		t.Fatal("reservation not active")
	}
	// The MAC changes again (a private address): the old lease is active,
	// so the new MAC is served as unreserved.
	off, _ = ts.reply(t, reqFrom(msgDiscover, macN(12)).opt(optClientID, 1, 2, 3))
	if off.yiaddr == ip("192.168.1.60") {
		t.Fatal("reserved address offered while another MAC holds it")
	}
	if e := ts.Log(1)[0]; e.Reason != LogReasonClientIDConflict || e.Result != ResultAnswered {
		t.Fatalf("log %+v", e)
	}
	// After the old lease expired (kept 24 h) the new MAC gets the address;
	// the old row is deleted in the same write.
	ts.clk.Add(25 * time.Hour)
	ts.lease(t, macN(12), "192.168.1.60", withClientID(1, 2, 3))
	var rows int
	ts.d.R.QueryRow(`SELECT COUNT(*) FROM dhcp_leases WHERE ip = '192.168.1.60'`).Scan(&rows)
	if rows != 1 || leaseOf(ts, "02:00:00:00:00:0c").IP != "192.168.1.60" {
		t.Fatalf("rows %d", rows)
	}
	// A pending offer of another MAC blocks the match, and so does another
	// device on the address (the neighbour check).
	mustStatic(t, ts, StaticInput{MAC: "02:00:00:00:00:20", IP: "192.168.1.70", ClientID: "01:aa:bb"})
	ts.mu.Lock()
	ts.t.putOffer("02:00:00:00:00:21", ip("192.168.1.70"), ts.now().Add(offerHold))
	ts.mu.Unlock()
	if off, _ := ts.reply(t, reqFrom(msgDiscover, macN(0x22)).opt(optClientID, 1, 0xaa, 0xbb)); off.yiaddr == ip("192.168.1.70") {
		t.Fatal("offered while another MAC holds an offer")
	}
	ts.mu.Lock()
	ts.t.dropOffer("02:00:00:00:00:21")
	ts.mu.Unlock()
	ts.nmu.Lock()
	ts.neigh[ip("192.168.1.70")] = "02:99:00:00:00:01"
	ts.nmu.Unlock()
	if off, _ := ts.reply(t, reqFrom(msgDiscover, macN(0x23)).opt(optClientID, 1, 0xaa, 0xbb)); off.yiaddr == ip("192.168.1.70") {
		t.Fatal("offered while another device uses the address")
	}
	// A MAC match wins over a client-ID match.
	mustStatic(t, ts, StaticInput{MAC: "02:00:00:00:00:24", IP: "192.168.1.80"})
	if off, _ := ts.reply(t, reqFrom(msgDiscover, macN(0x24)).opt(optClientID, 1, 2, 3)); off.yiaddr != ip("192.168.1.80") {
		t.Fatalf("MAC match lost: %v", off.yiaddr)
	}
}

func macStr(m [6]byte) string { s, _ := macString(m); return s }

func leaseOf(ts *testSvc, mac string) Lease {
	for _, l := range ts.Leases() {
		if l.MAC == mac {
			return l
		}
	}
	return Lease{}
}

func staticOf(ts *testSvc, mac string) StaticLease {
	for _, s := range ts.Statics() {
		if s.MAC == mac {
			return s
		}
	}
	return StaticLease{}
}

// A reservation's client identifier and lease time survive a restart; the
// migration added the columns.
func TestReservationColumnsPersist(t *testing.T) {
	ts := newTestSvc(t, enabledDHCP)
	mustStatic(t, ts, StaticInput{MAC: "02:00:00:00:00:01", IP: "192.168.1.50", ClientID: "FF:00:01", LeaseSeconds: 7200})
	ts2 := openTestSvc(t, ts.d, nil)
	if s := staticOf(ts2, "02:00:00:00:00:01"); s.ClientID != "ff:00:01" || s.LeaseSeconds != 7200 {
		t.Fatalf("after a restart %+v", s)
	}
	// PUT replaces the reservation: members left out are cleared.
	s, err := ts2.UpdateStatic(context.Background(), "02:00:00:00:00:01", StaticUpdate{IP: "192.168.1.51"})
	if err != nil || s.ClientID != "" || s.LeaseSeconds != 0 {
		t.Fatalf("update %+v %v", s, err)
	}
}

// K7: the log keeps the last 200 handled exchanges, newest first; packets
// that are not handled are not recorded.
func TestExchangeLog(t *testing.T) {
	ts := newTestSvc(t, enabledDHCP)
	ts.lease(t, macN(1), "192.168.1.100", withHost("Laptop"))
	got := ts.Log(MaxLog)
	if len(got) != 2 {
		t.Fatalf("log %+v", got)
	}
	if e := got[0]; e.In != "REQUEST" || e.Out != "ACK" || e.Result != ResultAnswered || e.Address != "192.168.1.100" ||
		e.Hostname != "laptop" || e.MAC != "02:00:00:00:00:01" || e.Kind != LogDHCPv4 || !e.Time.Equal(t0) {
		t.Fatalf("newest %+v", e)
	}
	if e := got[1]; e.In != "DISCOVER" || e.Out != "OFFER" {
		t.Fatalf("oldest %+v", e)
	}
	// Not recorded: malformed, another interface, relayed.
	ts.handle4Out(reqFrom(msgDiscover, macN(1)).bytes()[:100], testIfIndex, "0.0.0.0")
	ts.handle4Out(reqFrom(msgDiscover, macN(1)).bytes(), 3, "0.0.0.0")
	relayed := reqFrom(msgDiscover, macN(1))
	relayed.giaddr = ip("192.168.1.2")
	ts.handle4Out(relayed.bytes(), testIfIndex, "192.168.1.2")
	if n := len(ts.Log(MaxLog)); n != 2 {
		t.Fatalf("unhandled packets recorded: %d", n)
	}
	// A NAK names why; a REQUEST for another server is ignored.
	ts.packet(reqFrom(msgRequest, macN(2)).addr(optRequestedIP, "10.0.0.7"))
	if e := ts.Log(1)[0]; e.Result != ResultNak || e.Out != "NAK" || e.Reason != LogReasonAddressUnavailable || e.Address != "10.0.0.7" {
		t.Fatalf("nak %+v", e)
	}
	ts.packet(reqFrom(msgRequest, macN(3)).addr(optRequestedIP, "192.168.1.150").addr(optServerID, "192.168.1.1"))
	if e := ts.Log(1)[0]; e.Result != ResultIgnored || e.Reason != LogReasonOtherServer {
		t.Fatalf("other server %+v", e)
	}
	// Bounded: 200 entries.
	for i := range 250 {
		ts.clk.Add(time.Second)
		inform := reqFrom(msgInform, macN(byte(i)))
		inform.ciaddr = ip("192.168.1.50")
		ts.packet(inform)
	}
	if got := ts.Log(MaxLog); len(got) != MaxLog || got[0].Time.Before(got[1].Time) || got[0].In != "INFORM" {
		t.Fatalf("bounded log: %d", len(got))
	}
	if len(ts.Log(5)) != 5 {
		t.Fatal("limit")
	}
}

// Rapid commit (option 80): off by default; when on, a DISCOVER with the
// option is acknowledged at once (with option 80, the lease written) while
// no other server counts and ignoreOtherServers is off.
func TestRapidCommit(t *testing.T) {
	ts := newTestSvc(t, enabledDHCP)
	disc := func(mac [6]byte) *req { return reqFrom(msgDiscover, mac).opt(optRapidCommit) }
	if m, _ := ts.reply(t, disc(macN(1))); m.msgType() != msgOffer || m.rapidCommit() {
		t.Fatalf("default: %v", m.msgType())
	}
	ts.update(t, func(a *settings.All) { a.DHCP.RapidCommit = true })
	m, _ := ts.reply(t, disc(macN(2)))
	if v, ok := m.opts[optRapidCommit]; m.msgType() != msgAck || !ok || len(v) != 0 {
		t.Fatalf("rapid: %v %v", m.msgType(), m.opts)
	}
	if l := leaseOf(ts, "02:00:00:00:00:02"); !l.Active || l.IP != m.yiaddr.String() || l.DNSName == "" {
		t.Fatalf("lease %+v", l)
	}
	if e := ts.Log(1)[0]; e.In != "DISCOVER" || e.Out != "ACK" || e.Reason != LogReasonRapidCommit {
		t.Fatalf("log %+v", e)
	}
	// Without option 80: the normal OFFER.
	if m, _ := ts.reply(t, reqFrom(msgDiscover, macN(3))); m.msgType() != msgOffer {
		t.Fatal("rapid commit without the option")
	}
	// The neighbour check applies: an address in use is skipped.
	ts.nmu.Lock()
	next := netip.MustParseAddr("192.168.1.103")
	for a := ip("192.168.1.100"); a.Less(next); a = a.Next() {
		if _, used := ts.t.byIP[a]; !used {
			ts.neigh[a] = "02:99:00:00:00:01"
		}
	}
	ts.nmu.Unlock()
	if m, _ := ts.reply(t, disc(macN(4))); m.msgType() != msgAck || m.yiaddr.Less(next) {
		t.Fatalf("neighbour conflict: %v %v", m.msgType(), m.yiaddr)
	}
	// Another server counts: the normal OFFER.
	ts.recordOther(otherServer{iface: "eth0", address: ip("192.168.1.1"), serverID: ip("192.168.1.1"), source: SourceRequest, lastSeen: ts.now()})
	if m, _ := ts.reply(t, disc(macN(5))); m.msgType() != msgOffer {
		t.Fatal("rapid commit while another server counts")
	}
	ts.update(t, func(a *settings.All) { a.DHCP.IgnoreOtherServers = true })
	if m, _ := ts.reply(t, disc(macN(6))); m.msgType() != msgOffer {
		t.Fatal("rapid commit with ignoreOtherServers")
	}
}

// K7: when the pool holds only addresses retained for other clients'
// expired leases, a rapid-commit DISCOVER gets the oldest one acknowledged,
// as the DISCOVER → OFFER → REQUEST path would (not silence).
func TestRapidCommitRetainedAddress(t *testing.T) {
	ts := newTestSvc(t, func(a *settings.All) { enabledDHCP(a); a.DHCP.RangeEnd = "192.168.1.101"; a.DHCP.RapidCommit = true })
	ts.lease(t, macN(1), "192.168.1.100")
	ts.clk.Add(time.Minute)
	ts.lease(t, macN(2), "192.168.1.101")
	ts.clk.Add(24*time.Hour + time.Second) // both expired, both kept for their clients
	m, _ := ts.reply(t, reqFrom(msgDiscover, macN(3)).opt(optRapidCommit))
	if m.msgType() != msgAck || !m.rapidCommit() || m.yiaddr != ip("192.168.1.100") {
		t.Fatalf("rapid commit on a retained address: %v %v", m.msgType(), m.yiaddr)
	}
	if l := leaseOf(ts, "02:00:00:00:00:03"); !l.Active || l.IP != "192.168.1.100" {
		t.Fatalf("lease %+v", l)
	}
	if l := leaseOf(ts, "02:00:00:00:00:01"); l.MAC != "" {
		t.Fatalf("the expired lease on the address is kept: %+v", l)
	}
}

// K1: export as CSV (formula prefix) and hosts (named reservations only).
func TestExport(t *testing.T) {
	ts := newTestSvc(t, enabledDHCP)
	mustStatic(t, ts, StaticInput{MAC: "02:00:00:00:00:02", IP: "192.168.1.60", Hostname: "tv", Comment: "=cmd|' /c calc'!A1", ClientID: "01:02",
		LeaseSeconds: 3600})
	mustStatic(t, ts, StaticInput{MAC: "02:00:00:00:00:01", IP: "192.168.1.50", Comment: "office, 2nd \"floor\""})
	b, err := ts.ExportStatics(FormatCSV)
	want := "mac,ip,hostname,comment,clientId,leaseSeconds\n" +
		"02:00:00:00:00:01,192.168.1.50,,\"office, 2nd \"\"floor\"\"\",,0\n" +
		"02:00:00:00:00:02,192.168.1.60,tv,'=cmd|' /c calc'!A1,01:02,3600\n"
	if err != nil || string(b) != want {
		t.Fatalf("csv %v\n%s", err, b)
	}
	b, _ = ts.ExportStatics(FormatHosts)
	if string(b) != "# PiCache reserved addresses\n192.168.1.60\ttv\t# 02:00:00:00:00:02\n" {
		t.Fatalf("hosts %q", b)
	}
	_, err = ts.ExportStatics("xml")
	wantField(t, err, "format")
	// The CSV imports back unchanged (the formula prefix is removed).
	res, err := ts.ImportStatics(context.Background(), ImportInput{Format: FormatCSV, Text: string(mustExport(t, ts, FormatCSV))})
	if err != nil || !res.Applied || res.Unchanged != 2 || res.Added+res.Updated+res.Removed != 0 {
		t.Fatalf("round trip %+v %v", res, err)
	}
	if staticOf(ts, "02:00:00:00:00:02").Comment != "=cmd|' /c calc'!A1" {
		t.Fatal("comment changed")
	}
}

func mustExport(t *testing.T, ts *testSvc, f string) []byte {
	b, err := ts.ExportStatics(f)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// K1: imports of every format, absent versus empty columns, swaps,
// replace, the batch rules and all-or-nothing.
func TestImport(t *testing.T) {
	ts := newTestSvc(t, enabledDHCP)
	ctx := context.Background()
	mustStatic(t, ts, StaticInput{MAC: "02:00:00:00:00:01", IP: "192.168.1.50", Hostname: "a", Comment: "keep", ClientID: "01:01", LeaseSeconds: 600})
	mustStatic(t, ts, StaticInput{MAC: "02:00:00:00:00:02", IP: "192.168.1.51", Hostname: "b"})
	imp := func(in ImportInput) ImportResult {
		t.Helper()
		res, err := ts.ImportStatics(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	// CSV without the optional columns keeps them; an empty field clears;
	// the two swap their addresses; CRLF and quoting.
	res := imp(ImportInput{Format: FormatCSV, Text: "MAC,ip,hostname,comment\r\n02:00:00:00:00:01,192.168.1.51,a2,\r\n\"02-00-00-00-00-02\",192.168.1.50,b,\"x, y\"\r\n"})
	if !res.Applied || res.Updated != 2 || len(res.Errors) != 0 {
		t.Fatalf("csv %+v", res)
	}
	if s := staticOf(ts, "02:00:00:00:00:01"); s.IP != "192.168.1.51" || s.Hostname != "a2" || s.Comment != "" || s.ClientID != "01:01" || s.LeaseSeconds != 600 {
		t.Fatalf("absent kept, empty cleared: %+v", s)
	}
	if s := staticOf(ts, "02:00:00:00:00:02"); s.IP != "192.168.1.50" || s.Comment != "x, y" {
		t.Fatalf("swap %+v", s)
	}
	// Six columns: empty clears the client identifier and the lease time.
	imp(ImportInput{Format: FormatCSV, Text: "02:00:00:00:00:01,192.168.1.51,a2,,,\n"})
	if s := staticOf(ts, "02:00:00:00:00:01"); s.ClientID != "" || s.LeaseSeconds != 0 {
		t.Fatalf("cleared %+v", s)
	}
	// lines without a host name keep it; hosts sets it and keeps the rest.
	res = imp(ImportInput{Format: FormatLines, Text: "# comment\n\n02:00:00:00:00:01 192.168.1.52\n02:00:00:00:00:03 192.168.1.53 c\n"})
	if res.Updated != 1 || res.Added != 1 || staticOf(ts, "02:00:00:00:00:01").Hostname != "a2" {
		t.Fatalf("lines %+v", res)
	}
	res = imp(ImportInput{Format: FormatHosts, Text: "# PiCache reserved addresses\n192.168.1.50 bee bee.lan # 02:00:00:00:00:02 old tv\n"})
	if res.Updated != 1 || staticOf(ts, "02:00:00:00:00:02").Hostname != "bee" || staticOf(ts, "02:00:00:00:00:02").Comment != "x, y" {
		t.Fatalf("hosts %+v %+v", res, staticOf(ts, "02:00:00:00:00:02"))
	}
	// Replace removes what the batch does not name.
	res = imp(ImportInput{Format: FormatLines, Text: "02:00:00:00:00:03 192.168.1.53 c\n", Replace: true, DryRun: true})
	if res.Applied || res.Removed != 2 || res.Unchanged != 1 || len(ts.Statics()) != 3 {
		t.Fatalf("replace dry run %+v", res)
	}
	res = imp(ImportInput{Format: FormatLines, Text: "02:00:00:00:00:03 192.168.1.53 c\n", Replace: true})
	if !res.Applied || res.Removed != 2 || len(ts.Statics()) != 1 {
		t.Fatalf("replace %+v", res)
	}
	// All or nothing: line numbers count every line; one error per line;
	// in-batch duplicates are reported on the later line.
	text := "mac,ip,hostname,comment,clientId,leaseSeconds\n" +
		"02:00:00:00:00:10,192.168.1.60,d,,,\n" + // 2: fine
		"02:00:00:00:00:11,192.168.1.60,e,,,\n" + // 3: address twice
		"02:00:00:00:00:12,192.168.1.61,d,,,\n" + // 4: host name twice
		"02:00:00:00:00:10,192.168.1.62,,,,\n" + // 5: MAC twice
		"02:00:00:00:00:13,192.168.1.63,f,,zz,\n" + // 6: client ID
		"02:00:00:00:00:14,192.168.1.64,g,,,99\n" + // 7: lease time
		"02:00:00:00:00:15,8.8.8.8,h,,,\n" + // 8: address
		"02:00:00:00:00:16,192.168.1.53,i,,,\n" + // 9: address of a reservation that stays
		"nope,192.168.1.66\n" + // 10: fields
		"02:00:00:00:00:17,192.168.1.67,bad_name,,,\n" + // 11: host name
		"02:00:00:00:00:18,192.168.1.68,j,,,x\n" + // 12: lease time not a number
		"zz,192.168.1.69,k,,,\n" // 13: MAC
	res = imp(ImportInput{Format: FormatCSV, Text: text})
	want := []ImportError{{3, "ip", ""}, {4, "hostname", ""}, {5, "mac", ""}, {6, "clientId", ""}, {7, "leaseSeconds", ""},
		{8, "ip", ""}, {9, "ip", ""}, {10, "row", ""}, {11, "hostname", ""}, {12, "leaseSeconds", ""}, {13, "mac", ""}}
	if res.Applied || res.Added != 1 || len(res.Errors) != len(want) {
		t.Fatalf("errors %+v", res)
	}
	for i, e := range res.Errors {
		if e.Line != want[i].Line || e.Field != want[i].Field || e.Message == "" {
			t.Errorf("error %d: %+v, want line %d field %s", i, e, want[i].Line, want[i].Field)
		}
		if strings.HasPrefix(e.Message, e.Field+":") { // the UI shows the field next to the message
			t.Errorf("error %d: the message repeats the field: %q", i, e.Message)
		}
	}
	if len(ts.Statics()) != 1 {
		t.Fatal("a failed import wrote something")
	}
	// Quoting errors, the hosts comment, and the lines format.
	res = imp(ImportInput{Format: FormatCSV, Text: "02:00:00:00:00:20,\"192.168.1.70,x,y\n"})
	if len(res.Errors) != 1 || res.Errors[0].Field != "row" {
		t.Fatalf("quote %+v", res)
	}
	res = imp(ImportInput{Format: FormatHosts, Text: "192.168.1.70 tv\n192.168.1.71\n"})
	if len(res.Errors) != 2 || res.Errors[0].Field != "mac" || res.Errors[1].Field != "row" {
		t.Fatalf("hosts errors %+v", res)
	}
	res = imp(ImportInput{Format: FormatLines, Text: "02:00:00:00:00:21\n02:00:00:00:00:22 192.168.1.72 a b\n"})
	if len(res.Errors) != 2 || res.Errors[0].Line != 1 || res.Errors[1].Line != 2 {
		t.Fatalf("lines errors %+v", res)
	}
	// Request errors.
	for _, tc := range []struct {
		in    ImportInput
		field string
	}{
		{ImportInput{Format: "xml"}, "format"},
		{ImportInput{Format: FormatLines, Text: strings.Repeat("x", maxImportText+1)}, "text"},
		{ImportInput{Format: FormatLines, Text: strings.Repeat("02:00:00:00:00:30 192.168.1.80\n", maxImportRows+1)}, "text"},
		// Erroneous rows count too (a request error, not 131072 row errors).
		{ImportInput{Format: FormatCSV, Text: strings.Repeat("a\n", maxImportText/2), DryRun: true}, "text"},
		{ImportInput{Format: FormatHosts, Text: strings.Repeat("a\n", maxImportText/2), DryRun: true}, "text"},
		{ImportInput{Format: FormatLines, Text: strings.Repeat("a\n", maxImportText/2), DryRun: true}, "text"},
	} {
		_, err := ts.ImportStatics(ctx, tc.in)
		wantField(t, err, tc.field)
	}
	// The parsers stop as soon as the bound is passed: neither their work
	// nor the error map grows with the text.
	for name, parse := range map[string]func(string, importErrors) ([]importRow, error){
		"csv": parseCSV, "hosts": parseHosts, "lines": parseLines,
	} {
		errs := importErrors{}
		if rows, err := parse(strings.Repeat("a\n", maxImportText/2), errs); err != errTooManyRows || rows != nil || len(errs) > maxImportRows+1 {
			t.Fatalf("%s: %v, %d rows, %d errors", name, err, len(rows), len(errs))
		}
		errs = importErrors{}
		if rows, err := parse(strings.Repeat("a\n", maxImportRows), errs); err != nil || len(rows) != 0 || len(errs) != maxImportRows {
			t.Fatalf("%s at the bound: %v, %d rows, %d errors", name, err, len(rows), len(errs))
		}
	}
	// At most 1024 reservations in the end (line 0, field text).
	var b strings.Builder
	for i := range maxImportRows {
		fmt.Fprintf(&b, "02:00:00:%02x:%02x:01 10.1.%d.%d\n", i>>8, i&0xff, i/250, i%250+1)
	}
	ts.set.Update(ctx, func(a *settings.All) error { a.DHCP.Interface = ""; a.DHCP.Enabled = false; return nil })
	res = imp(ImportInput{Format: FormatLines, Text: b.String()})
	if res.Applied || len(res.Errors) != 1 || res.Errors[0].Line != 0 || res.Errors[0].Field != "text" || res.Added != maxImportRows {
		t.Fatalf("bound %+v", res.Errors)
	}
	res = imp(ImportInput{Format: FormatLines, Text: b.String(), Replace: true})
	if !res.Applied || len(ts.Statics()) != maxImportRows {
		t.Fatalf("1024 with replace %+v", res.Errors)
	}
}

// The option list order and slices stay comparable with Equal.
func TestSettingsEqualUsed(t *testing.T) {
	a, b := settings.Defaults().DHCP, settings.Defaults().DHCP
	b.Options.NTPServers = []string{"192.168.1.2"}
	if a.Equal(b) || !a.Equal(settings.Defaults().DHCP) || !slices.Equal(a.Options.ExtraSearchDomains, []string{}) {
		t.Fatal("Equal")
	}
}

// The import parsers never panic and keep line numbers within the text.
func FuzzImportParsers(f *testing.F) {
	f.Add("mac,ip,hostname,comment\n02:00:00:00:00:01,192.168.1.50,a,\"x\"\"y\"\n")
	f.Add("192.168.1.50 a b # 02:00:00:00:00:01 text\n# c\n")
	f.Add("02:00:00:00:00:01 192.168.1.50 a\r\n\"\n")
	f.Fuzz(func(t *testing.T, text string) {
		lines := strings.Count(text, "\n") + 1
		for _, parse := range []func(string, importErrors) ([]importRow, error){parseCSV, parseHosts, parseLines} {
			errs := importErrors{}
			rows, err := parse(text, errs)
			if err != nil && (err != errTooManyRows || rows != nil || len(errs) > maxImportRows+1) {
				t.Fatalf("parse: %v, %d rows, %d errors", err, len(rows), len(errs))
			}
			for _, r := range rows {
				if r.line < 1 || r.line > lines {
					t.Fatalf("row line %d of %d", r.line, lines)
				}
			}
			for _, e := range errs.list() {
				if e.Line < 0 || e.Line > lines || e.Field == "" {
					t.Fatalf("error %+v", e)
				}
			}
		}
	})
}
