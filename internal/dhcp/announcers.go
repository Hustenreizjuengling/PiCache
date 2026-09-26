package dhcp

import (
	"context"
	cryptorand "crypto/rand"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/hustenreizjuengling/picache/internal/dhcp/dhcpv6"
	"github.com/hustenreizjuengling/picache/internal/dhcp/ra"
)

// Other IPv6 announcers (docs/ARCHITECTURE.md 18.5): routers that announce
// their own DNS server or DHCPv6 (their router advertisements, seen while
// PiCache's own are on) and DHCPv6 servers that answer PiCache's relayed
// information request. A search runs when PiCache's IPv6 announcements
// start (without delaying them) and every 10 minutes while they run.
const (
	searchEvery     = 10 * time.Minute
	maxAnnouncers   = 32
	maxAnnounceLife = time.Hour        // a router's record lives at most this long after its last RA
	dhcpv6Life      = 10 * time.Minute // a search answer is kept until the next search, at most this long
	limitRA         = 50               // other routers' advertisements parsed per second
)

// searchListen is how long a search collects answers (a variable for
// tests).
var searchListen = 3 * time.Second

// annRecord is another announcer on an interface.
type annRecord struct {
	kind, iface string
	src         netip.Addr
	serverID    string
	dns         []annDNS
	managed     bool
	other       bool
	lifetime    time.Duration // router lifetime
	first, last time.Time
	until       time.Time // the record expires
	seen        int       // RAs, or searches that got an answer in a row
}

// annDNS is an announced DNS server and until when it counts.
type annDNS struct {
	addr  netip.Addr
	until time.Time
}

// searchRun is a running DHCPv6 search.
type searchRun struct {
	txid    [3]byte
	peer    netip.Addr // PiCache's link-local address: the answers name it
	ifIndex int
	iface   string
	found   []*annRecord
}

// announcers holds the records (at most 32, least recently seen evicted),
// the running search and the last search.
type announcers struct {
	mu      sync.Mutex
	recs    []*annRecord
	run     *searchRun
	last    *LastSearch
	own     map[netip.Addr]bool // this machine's addresses (conflicts)
	ownMACs map[[6]byte]bool
	limit   *windowLimiter
	// answered counts, per interface/server/address, the searches in a row
	// that got an answer (at most one entry per record).
	answered map[string]int
}

func newAnnouncers() *announcers {
	return &announcers{own: map[netip.Addr]bool{}, ownMACs: map[[6]byte]bool{}, limit: newWindowLimiter(limitRA, 0)}
}

func (a *announcers) setOwn(addrs map[netip.Addr]bool, macs map[[6]byte]bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.own, a.ownMACs = addrs, macs
}

func (a *announcers) isOwn(ip netip.Addr) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.own[ip.WithZone("")]
}

// expireLocked drops expired records and DNS entries (a.mu held).
func (a *announcers) expireLocked(now time.Time) {
	a.recs = slices.DeleteFunc(a.recs, func(r *annRecord) bool { return !now.Before(r.until) })
	for _, r := range a.recs {
		r.dns = slices.DeleteFunc(r.dns, func(d annDNS) bool { return !now.Before(d.until) })
	}
}

// putLocked adds r, evicting the least recently seen record when full.
func (a *announcers) putLocked(r *annRecord) {
	if len(a.recs) >= maxAnnouncers {
		oldest := 0
		for i, x := range a.recs {
			if x.last.Before(a.recs[oldest].last) {
				oldest = i
			}
		}
		a.recs = slices.Delete(a.recs, oldest, oldest+1)
	}
	a.recs = append(a.recs, r)
}

// recordRA updates the record of a router from its advertisement: an RA
// that announces DNS (an RDNSS lifetime > 0) or DHCPv6 (M or O) creates or
// updates it (RDNSS addresses with lifetime 0 are removed at once), one
// that announces neither removes it. The record expires max(router
// lifetime, longest RDNSS lifetime) after the RA, at most after 1 h.
func (a *announcers) recordRA(iface string, src netip.Addr, adv ra.Received, now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.expireLocked(now)
	src = src.WithZone("")
	i := slices.IndexFunc(a.recs, func(r *annRecord) bool { return r.kind == AnnouncerRA && r.iface == iface && r.src == src })
	var r *annRecord
	if i >= 0 {
		r = a.recs[i]
	}
	life, dnsOn := adv.RouterLifetime, false
	for _, d := range adv.DNS {
		if d.Lifetime > 0 {
			dnsOn = true
			life = max(life, d.Lifetime)
		}
	}
	if !dnsOn && !adv.Managed && !adv.Other {
		if i >= 0 {
			a.recs = slices.Delete(a.recs, i, i+1)
		}
		return
	}
	if r == nil {
		r = &annRecord{kind: AnnouncerRA, iface: iface, src: src, first: now}
		a.putLocked(r)
	}
	r.managed, r.other, r.lifetime, r.last = adv.Managed, adv.Other, adv.RouterLifetime, now
	r.until = now.Add(min(life, maxAnnounceLife))
	r.seen++
	for _, d := range adv.DNS {
		j := slices.IndexFunc(r.dns, func(x annDNS) bool { return x.addr == d.Addr })
		switch {
		case d.Lifetime == 0 && j >= 0:
			r.dns = slices.Delete(r.dns, j, j+1)
		case d.Lifetime == 0:
		case j >= 0:
			r.dns[j].until = now.Add(min(d.Lifetime, maxAnnounceLife))
		case len(r.dns) < ra.MaxDNS:
			r.dns = append(r.dns, annDNS{addr: d.Addr, until: now.Add(min(d.Lifetime, maxAnnounceLife))})
		}
	}
}

// finishSearch replaces the DHCPv6 records of the interface by the answers
// of a search; a server that answered the previous search too counts as
// seen once more.
func (a *announcers) finishSearch(run *searchRun, st LastSearch, now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.expireLocked(now)
	if a.run == run {
		a.run = nil
	}
	a.last = &st
	if !st.DHCPv6 {
		return
	}
	a.recs = slices.DeleteFunc(a.recs, func(r *annRecord) bool { return r.kind == AnnouncerDHCPv6 && r.iface == run.iface })
	answered := map[string]int{}
	for k, n := range a.answered {
		if !strings.HasPrefix(k, run.iface+"/") {
			answered[k] = n // another interface's
		}
	}
	for _, r := range run.found {
		key := run.iface + "/" + r.src.String() + "/" + r.serverID
		r.seen = a.answered[key] + 1
		answered[key] = r.seen
		r.last, r.until = now, now.Add(dhcpv6Life)
		a.putLocked(r)
	}
	if len(answered) > maxAnnouncers {
		clear(answered)
	}
	a.answered = answered
}

// answer hands a Relay-Reply to the running search: one per server, at
// most 32.
func (a *announcers) answer(ifIndex int, src netip.Addr, rr dhcpv6.RelayReply, now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	run := a.run
	if run == nil || ifIndex != run.ifIndex || rr.TxID != run.txid || rr.Peer.WithZone("") != run.peer {
		return false
	}
	src = src.WithZone("")
	sid := hexColon(rr.ServerID)
	if len(run.found) >= maxAnnouncers || slices.ContainsFunc(run.found, func(r *annRecord) bool { return r.src == src && r.serverID == sid }) {
		return true
	}
	r := &annRecord{kind: AnnouncerDHCPv6, iface: run.iface, src: src, serverID: sid, first: now, last: now}
	for _, d := range rr.DNS {
		r.dns = append(r.dns, annDNS{addr: d.WithZone(""), until: now.Add(dhcpv6Life)})
	}
	run.found = append(run.found, r)
	return true
}

// conflictLocked reports whether r warns (a.mu held): while PiCache
// announces itself, a record seen at least twice that carries a DNS server
// that is not an address of this machine. M and O flags never warn.
func (a *announcers) conflictLocked(r *annRecord, announcing bool) bool {
	if !announcing || r.seen < 2 {
		return false
	}
	return slices.ContainsFunc(r.dns, func(d annDNS) bool { return !a.own[d.addr] })
}

// status returns the records, newest first, and the last search.
func (a *announcers) status(now time.Time, announcing bool) ([]Announcer, *LastSearch) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.expireLocked(now)
	out := make([]Announcer, 0, len(a.recs))
	for _, r := range a.recs {
		e := Announcer{Kind: r.kind, Address: r.src.String(), Interface: r.iface, DNS: []string{}, OwnDNS: []string{},
			FirstSeen: r.first.UTC(), LastSeen: r.last.UTC(), Conflict: a.conflictLocked(r, announcing)}
		for _, d := range r.dns {
			e.DNS = append(e.DNS, d.addr.String())
			if a.own[d.addr] {
				e.OwnDNS = append(e.OwnDNS, d.addr.String())
			}
		}
		if r.kind == AnnouncerRA {
			m, o, l := r.managed, r.other, int(r.lifetime/time.Second)
			e.Managed, e.Other, e.RouterLifetime = &m, &o, &l
		} else {
			e.ServerID = r.serverID
		}
		out = append(out, e)
	}
	slices.SortStableFunc(out, func(x, y Announcer) int { return y.LastSeen.Compare(x.LastSeen) })
	var last *LastSearch
	if a.last != nil {
		l := *a.last
		l.Time = l.Time.UTC()
		last = &l
	}
	return out, last
}

// conflicts returns the DNS servers of the records that warn.
func (a *announcers) conflicts(now time.Time, announcing bool) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.expireLocked(now)
	var out []string
	for _, r := range a.recs {
		if !a.conflictLocked(r, announcing) {
			continue
		}
		for _, d := range r.dns {
			if s := d.addr.String(); !a.own[d.addr] && !slices.Contains(out, s) {
				out = append(out, s)
			}
		}
	}
	return out
}

// routerRDNSS returns the DNS servers of the routers' records with a router
// lifetime above 0, by source address.
func (a *announcers) routerRDNSS(now time.Time) map[netip.Addr][]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.expireLocked(now)
	out := map[netip.Addr][]string{}
	for _, r := range a.recs {
		if r.kind != AnnouncerRA || r.lifetime == 0 {
			continue
		}
		dns := []string{}
		for _, d := range r.dns {
			dns = append(dns, d.addr.String())
		}
		out[r.src] = dns
	}
	return out
}

// RouterRDNSS returns, by source address, the DNS servers announced in the
// router advertisements of routers (router lifetime above 0; the network
// check picks the default router's). ok is false while PiCache does not
// record advertisements (its own router advertisements are off).
func (s *Service) RouterRDNSS() (bySource map[netip.Addr][]string, ok bool) {
	if !s.d.Sockets.HasRaw() {
		return nil, false
	}
	return s.ann.routerRDNSS(s.now()), true
}

// advertisement records another router's advertisement received on the
// served interface (at most 50 per second are parsed; PiCache's own and
// those from this machine's addresses or MACs are ignored).
func (s *Service) advertisement(b []byte, ifIndex, hops int, src netip.Addr) {
	v := s.view.Load()
	if v.gate.iface.problem != "" || v.gate.iface.index == 0 || ifIndex != v.gate.iface.index {
		return
	}
	now := s.now()
	if !s.ann.limit.allow([6]byte{}, now) {
		return
	}
	adv, err := ra.ParseAdvertisement(b, hops, src)
	if err != nil || s.ann.isOwn(src) {
		return
	}
	s.ann.mu.Lock()
	ownMAC := adv.HasMAC && s.ann.ownMACs[adv.SourceMAC]
	s.ann.mu.Unlock()
	if ownMAC {
		return
	}
	s.ann.recordRA(v.gate.iface.name, src, adv, now)
}

// relayReply hands a DHCPv6 server's answer to the running search; every
// other Relay-Reply is ignored and counted.
func (s *Service) relayReply(b []byte, ifIndex int, src netip.AddrPort) {
	v := s.view.Load()
	rr, err := dhcpv6.ParseRelayReply(b)
	if err != nil || ifIndex != v.gate.iface.index || s.ann.isOwn(src.Addr()) ||
		(v.duid != nil && string(rr.ServerID) == string(v.duid)) || !s.ann.answer(ifIndex, src.Addr(), rr, s.now()) {
		s.v6Ignored.Add(1)
	}
}

func (s *Service) kickSearch() {
	select {
	case s.annKick <- struct{}{}:
	default:
	}
}

// searchLoop searches for other IPv6 announcers when the announcements
// start and every 10 minutes while they run.
func (s *Service) searchLoop(ctx context.Context) {
	t := time.NewTicker(searchEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.annKick:
		case <-t.C:
		}
		if s.view.Load().announcing() {
			s.search(ctx)
			t.Reset(searchEvery)
		}
	}
}

// search runs one search (3 s): a router solicitation to ff02::2 while the
// raw socket is open (the answers arrive as advertisements, which are
// recorded anyway), and from UDP 547 a Relay-Forward with an
// Information-Request to ff02::1:2 (servers answer the relay, i.e.
// PiCache's port 547). One search at a time.
func (s *Service) search(ctx context.Context) {
	if !s.searchMu.TryLock() {
		return
	}
	defer s.searchMu.Unlock()
	v := s.view.Load()
	iface := v.gate.iface
	if !v.announcing() || iface.problem != "" || iface.index == 0 {
		return
	}
	now := s.now()
	st := LastSearch{Time: now}
	_, v6, icmp, _, _, _ := s.d.Sockets.get()
	if icmp != nil {
		if err := icmp.WriteTo(ra.Solicitation(iface.mac), iface.index, allRouters); err != nil {
			s.logRepeated("searchRS", "sending a router solicitation failed", err)
		} else {
			st.RA = true
		}
	}
	var run *searchRun
	if v6 != nil && iface.ll.IsValid() && iface.ula.IsValid() {
		run = &searchRun{peer: iface.ll, ifIndex: iface.index, iface: iface.name}
		_, _ = cryptorand.Read(run.txid[:])
		s.ann.mu.Lock()
		s.ann.run = run
		s.ann.mu.Unlock()
		pkt := dhcpv6.RelayForward(run.txid, dhcpv6.DUIDLL(iface.mac), iface.ula, iface.ll, []byte(iface.name))
		if err := v6.WriteTo(pkt, iface.index, netip.AddrPortFrom(allDHCPAgents, 547)); err != nil {
			s.logRepeated("search6", "sending the search for other DHCPv6 servers failed", err)
		} else {
			st.DHCPv6 = true
		}
	}
	t := time.NewTimer(searchListen)
	select {
	case <-ctx.Done():
	case <-t.C:
	}
	t.Stop()
	if run == nil {
		run = &searchRun{iface: iface.name}
	}
	s.ann.finishSearch(run, st, s.now())
}
