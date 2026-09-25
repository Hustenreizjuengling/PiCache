package dhcp

import (
	"net/netip"
	"slices"
	"strings"
	"time"
)

// maxNameTTL caps the TTL of lease names; it shrinks to the remaining
// lease time.
const maxNameTTL = 300

// nameRec is a DNS name of an active lease.
type nameRec struct {
	name    string // <host>.<domain> (lower-case) for byIP, the address owner for byName
	ip      netip.Addr
	expires time.Time
}

// names is the immutable snapshot of lease names read by the DNS server
// and the clients registry.
type names struct {
	byName  map[string]nameRec     // <host>.<domain> → address
	byIP    map[netip.Addr]nameRec // address → <host>.<domain>
	display map[netip.Addr]string  // address of an active lease → its DNS name, else its host name
}

var emptyNames = &names{byName: map[string]nameRec{}, byIP: map[netip.Addr]nameRec{}, display: map[netip.Addr]string{}}

// effectiveName returns the host name of a lease: its static entry's name
// if it has one, else the name the client sent.
func (t *table) effectiveName(l *lease) string {
	if st := t.statics[l.mac]; st != nil && st.hostname != "" {
		return st.hostname
	}
	return l.hostname
}

// assignNames decides which client holds each host name. Static entries
// hold their names always; a name stays with the active lease that held
// it before; free names go to active leases in MAC order. A lease whose
// name another client holds gets no DNS name (a name conflict).
func (t *table) assignNames(prev map[string]string, now time.Time) map[string]string {
	next := map[string]string{}
	statics := make([]*static, 0, len(t.statics))
	for _, st := range t.statics {
		statics = append(statics, st)
	}
	slices.SortFunc(statics, func(a, b *static) int { return strings.Compare(a.mac, b.mac) })
	for _, st := range statics {
		if _, taken := next[st.hostname]; st.hostname != "" && !taken {
			next[st.hostname] = st.mac
		}
	}
	for name, mac := range prev {
		if _, taken := next[name]; taken {
			continue
		}
		if l := t.leases[mac]; l != nil && l.active(now) && t.effectiveName(l) == name {
			next[name] = mac
		}
	}
	for _, l := range t.sortedLeases() {
		name := t.effectiveName(l)
		if _, taken := next[name]; name != "" && !taken && l.active(now) {
			next[name] = l.mac
		}
	}
	return next
}

// buildNames builds the snapshot for the holders: DNS names only with
// register and a domain, names of at most 253 characters.
func (t *table) buildNames(holders map[string]string, domain string, register bool, now time.Time) *names {
	n := &names{byName: map[string]nameRec{}, byIP: map[netip.Addr]nameRec{}, display: map[netip.Addr]string{}}
	for _, l := range t.leases {
		if !l.active(now) {
			continue
		}
		host := t.effectiveName(l)
		if host == "" {
			continue
		}
		n.display[l.ip] = host
		if !register || domain == "" || holders[host] != l.mac {
			continue
		}
		fq := host + "." + domain
		if len(fq) > 253 {
			continue
		}
		n.byName[fq] = nameRec{name: fq, ip: l.ip, expires: l.expires}
		n.byIP[l.ip] = nameRec{name: fq, ip: l.ip, expires: l.expires}
		n.display[l.ip] = fq
	}
	return n
}

// changedDisplay returns the addresses whose display name differs.
func changedDisplay(a, b *names) []netip.Addr {
	var out []netip.Addr
	for ip, n := range b.display {
		if a.display[ip] != n {
			out = append(out, ip)
		}
	}
	for ip := range a.display {
		if _, ok := b.display[ip]; !ok {
			out = append(out, ip)
		}
	}
	return out
}

// ttl returns min(300 s, remaining lease) in seconds; ok is false when the
// lease has ended.
func (r nameRec) ttl(now time.Time) (uint32, bool) {
	rem := r.expires.Sub(now)
	if rem < time.Second {
		return 0, false
	}
	return uint32(min(rem/time.Second, maxNameTTL)), true
}

// LeaseAddr returns the address of an active lease whose DNS name is name
// (lower-case, without trailing dot) and the TTL to answer with
// (min(300 s, remaining lease)). It is called by the DNS server for every
// query that reaches the local records (hot path: one map lookup).
func (s *Service) LeaseAddr(name string) (netip.Addr, uint32, bool) {
	r, ok := s.names.Load().byName[name]
	if !ok {
		return netip.Addr{}, 0, false
	}
	ttl, ok := r.ttl(s.now())
	return r.ip, ttl, ok
}

// LeasePTR returns the DNS name of the active lease of ip and the TTL.
func (s *Service) LeasePTR(ip netip.Addr) (string, uint32, bool) {
	r, ok := s.names.Load().byIP[ip.Unmap()]
	if !ok {
		return "", 0, false
	}
	ttl, ok := r.ttl(s.now())
	return r.name, ttl, ok
}

// LeaseName returns the name of the active lease of ip for client lists
// (its DNS name, else its host name; "" if none).
func (s *Service) LeaseName(ip netip.Addr) string {
	return s.names.Load().display[ip.Unmap()]
}
