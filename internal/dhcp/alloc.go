package dhcp

import (
	"net/netip"
	"slices"
	"time"
)

// pool is the served subnet and the address range handed out.
type pool struct {
	subnet     netip.Prefix // masked, e.g. 192.168.1.0/24
	start, end netip.Addr
	// skip are addresses never handed out dynamically: the network and
	// broadcast addresses, PiCache's own, the router's and the DNS
	// server's.
	skip []netip.Addr
}

func newPool(subnet netip.Prefix, start, end netip.Addr, skip ...netip.Addr) *pool {
	p := &pool{subnet: subnet.Masked(), start: start, end: end}
	p.skip = append(p.skip, p.subnet.Addr(), lastAddr(p.subnet))
	for _, a := range skip {
		if a.IsValid() && !slices.Contains(p.skip, a) {
			p.skip = append(p.skip, a)
		}
	}
	return p
}

// lastAddr returns the broadcast address of an IPv4 prefix.
func lastAddr(p netip.Prefix) netip.Addr {
	a := p.Masked().Addr().As4()
	host := uint32(1)<<(32-p.Bits()) - 1
	v := uint32(a[0])<<24 | uint32(a[1])<<16 | uint32(a[2])<<8 | uint32(a[3])
	v |= host
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}

// usable reports whether ip may be given to a client at all (static
// entries too): inside the subnet and not one of the skipped addresses.
func (p *pool) usable(ip netip.Addr) bool {
	return p.subnet.Contains(ip) && !slices.Contains(p.skip, ip)
}

// dynamic reports whether ip is handed out dynamically: usable and inside
// the range.
func (p *pool) dynamic(ip netip.Addr) bool {
	return p.usable(ip) && !ip.Less(p.start) && !p.end.Less(ip)
}

// size returns the number of dynamic addresses.
func (p *pool) size() int {
	n := 0
	for ip := p.start; ip.IsValid() && !p.end.Less(ip); ip = ip.Next() {
		if p.usable(ip) {
			n++
		}
	}
	return n
}

// freeFor reports whether ip may be offered to mac now: no static entry
// of another client, no active lease of another client, no pending offer
// to another client and not quarantined.
func (t *table) freeFor(ip netip.Addr, mac string, now time.Time) bool {
	if st := t.staticIP[ip]; st != nil && st.mac != mac {
		return false
	}
	if l := t.byIP[ip]; l != nil && l.mac != mac && l.active(now) {
		return false
	}
	if other, ok := t.offerIP[ip]; ok && other != mac && now.Before(t.offers[other].until) {
		return false
	}
	return !t.quarantined(ip, now)
}

// retainedByOther reports whether ip has an (expired) lease of another
// client that is kept to hand it back to that client.
func (t *table) retainedByOther(ip netip.Addr, mac string) bool {
	l := t.byIP[ip]
	return l != nil && l.mac != mac
}

// candidate returns the next address to offer mac for a DISCOVER, in this
// order: its static entry; its current or previous lease; the requested
// address (option 50) if free and in the range; the lowest free address;
// finally the address whose lease of another client expired first. check
// is true when nothing proves the address free (no lease of this client):
// the neighbour table must be consulted before offering it. Addresses in
// tried are skipped. ok is false when the pool is exhausted.
func (t *table) candidate(p *pool, mac string, requested netip.Addr, tried map[netip.Addr]bool, now time.Time) (ip netip.Addr, check, ok bool) {
	if st := t.statics[mac]; st != nil && !tried[st.ip] && p.usable(st.ip) && !t.quarantined(st.ip, now) {
		if l := t.byIP[st.ip]; l == nil || l.mac == mac || !l.active(now) {
			return st.ip, false, true
		}
	}
	if l := t.leases[mac]; l != nil && !tried[l.ip] && p.dynamic(l.ip) && t.freeFor(l.ip, mac, now) {
		return l.ip, false, true
	}
	if requested.Is4() && !tried[requested] && p.dynamic(requested) && t.freeFor(requested, mac, now) && !t.retainedByOther(requested, mac) {
		return requested, true, true
	}
	for a := p.start; a.IsValid() && !p.end.Less(a); a = a.Next() {
		if !tried[a] && p.dynamic(a) && t.freeFor(a, mac, now) && !t.retainedByOther(a, mac) {
			return a, true, true
		}
	}
	var oldest *lease
	for a := p.start; a.IsValid() && !p.end.Less(a); a = a.Next() {
		l := t.byIP[a]
		if l == nil || tried[a] || !p.dynamic(a) || !t.freeFor(a, mac, now) {
			continue
		}
		if oldest == nil || l.expires.Before(oldest.expires) {
			oldest = l
		}
	}
	if oldest != nil {
		return oldest.ip, true, true
	}
	return netip.Addr{}, false, false
}

// requestVerdict is the answer to a REQUEST.
type requestVerdict int

const (
	verdictNak   requestVerdict = iota
	verdictAck                  // the address is the client's
	verdictCheck                // no record of the address: acknowledge if the neighbour table shows no other device
)

// decideRequest decides a REQUEST of mac for the address want (option 50
// in the selecting and init-reboot states, ciaddr when renewing or
// rebinding). PiCache is authoritative for its subnet: it naks addresses
// outside the subnet, reserved addresses, addresses of other clients
// (static entries, active leases, pending offers), quarantined ones, a
// dynamic address when the client has a free static one, and addresses
// outside the range that are not the client's static address. An address
// of the range without any record is acknowledged after the neighbour
// check (a client that got it from the previous DHCP server keeps it).
func (t *table) decideRequest(p *pool, mac string, want netip.Addr, now time.Time) requestVerdict {
	if !want.Is4() || !p.usable(want) {
		return verdictNak
	}
	if st := t.staticIP[want]; st != nil && st.mac != mac {
		return verdictNak
	}
	if l := t.byIP[want]; l != nil && l.mac != mac && l.active(now) {
		return verdictNak
	}
	if other, ok := t.offerIP[want]; ok && other != mac && now.Before(t.offers[other].until) {
		return verdictNak
	}
	if t.quarantined(want, now) {
		return verdictNak
	}
	if st := t.statics[mac]; st != nil {
		if st.ip == want {
			return verdictAck
		}
		if l := t.byIP[st.ip]; p.usable(st.ip) && (l == nil || l.mac == mac || !l.active(now)) {
			return verdictNak // move the client to its static address
		}
	}
	if !p.dynamic(want) {
		return verdictNak
	}
	if l := t.leases[mac]; l != nil && l.ip == want {
		return verdictAck
	}
	if o, ok := t.offers[mac]; ok && o.ip == want && now.Before(o.until) {
		return verdictAck // offered after the neighbour check
	}
	if t.retainedByOther(want, mac) {
		return verdictNak
	}
	return verdictCheck
}
