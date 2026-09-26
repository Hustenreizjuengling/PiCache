package netutil

import (
	"net/netip"
	"strings"

	"github.com/hustenreizjuengling/picache/internal/settings"
)

// ClientList matches client addresses and MAC addresses against a list of
// IP addresses, CIDRs and MAC addresses (the entries of dns.blockedClients,
// settings.ParseBlockedClient). It is immutable and safe for concurrent
// use; a match reports the first entry of the list (in list order) that
// equals or contains the address, or equals the MAC.
type ClientList struct {
	ips   map[netip.Addr]int // single addresses → index
	macs  map[string]int     // MAC → index
	cidrs []clientCIDR       // in list order
	raw   []string           // the entries (normalised)
}

type clientCIDR struct {
	prefix netip.Prefix
	index  int
}

// NewClientList compiles entries; entries that do not parse are skipped
// (settings.Validate rejects them earlier).
func NewClientList(entries []string) *ClientList {
	l := &ClientList{ips: map[netip.Addr]int{}, macs: map[string]int{}}
	for _, s := range entries {
		e, ok := settings.ParseBlockedClient(s)
		if !ok {
			continue
		}
		i := len(l.raw)
		l.raw = append(l.raw, e)
		switch p, err := netip.ParsePrefix(e); {
		case err == nil:
			l.cidrs = append(l.cidrs, clientCIDR{prefix: p, index: i})
		default:
			if ip, err := netip.ParseAddr(e); err == nil {
				if _, dup := l.ips[ip]; !dup {
					l.ips[ip] = i
				}
			} else if _, dup := l.macs[e]; !dup {
				l.macs[e] = i
			}
		}
	}
	return l
}

// Len returns the number of entries.
func (l *ClientList) Len() int {
	if l == nil {
		return 0
	}
	return len(l.raw)
}

// Entries returns the compiled entries in list order (read-only).
func (l *ClientList) Entries() []string {
	if l == nil {
		return nil
	}
	return l.raw
}

// MatchAddr returns the first entry that equals or contains ip.
func (l *ClientList) MatchAddr(ip netip.Addr) (string, bool) {
	i := l.addrIndex(Canon(ip))
	if i < 0 {
		return "", false
	}
	return l.raw[i], true
}

// MatchMAC returns the entry that equals mac ("" never matches).
func (l *ClientList) MatchMAC(mac string) (string, bool) {
	if l == nil || mac == "" {
		return "", false
	}
	i, ok := l.macs[strings.ToLower(mac)]
	if !ok {
		return "", false
	}
	return l.raw[i], true
}

// Match returns the first entry that matches ip (invalid: none) or mac
// ("": none).
func (l *ClientList) Match(ip netip.Addr, mac string) (string, bool) {
	if l == nil {
		return "", false
	}
	best := l.addrIndex(Canon(ip))
	if i, ok := l.macs[strings.ToLower(mac)]; ok && mac != "" && (best < 0 || i < best) {
		best = i
	}
	if best < 0 {
		return "", false
	}
	return l.raw[best], true
}

// addrIndex returns the index of the first entry that equals or contains
// ip, -1 if none.
func (l *ClientList) addrIndex(ip netip.Addr) int {
	if l == nil || !ip.IsValid() {
		return -1
	}
	best := -1
	if i, ok := l.ips[ip]; ok {
		best = i
	}
	for _, c := range l.cidrs {
		if best >= 0 && c.index > best {
			break // later entries cannot win
		}
		if c.prefix.Contains(ip) {
			return c.index
		}
	}
	return best
}
