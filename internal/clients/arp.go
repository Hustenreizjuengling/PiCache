package clients

import (
	"bufio"
	"context"
	"encoding/binary"
	"io"
	"maps"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/hustenreizjuengling/picache/internal/netutil"
)

const (
	maxARPEntries = 65536
	maxARPBytes   = 8 << 20
)

// Netlink neighbour dump layout (linux/netlink.h, linux/rtnetlink.h,
// linux/neighbour.h). Header fields are in host byte order.
const (
	nlmsgHdrLen   = 16 // nlmsghdr: len u32, type u16, flags u16, seq u32, pid u32
	nlmsgDone     = 3
	rtmNewNeigh   = 28
	ndMsgLen      = 12 // ndmsg: family u8, pad u8, pad u16, ifindex s32, state u16, flags u8, type u8
	ndaDst        = 1
	ndaLLAddr     = 2
	nudIncomplete = 0x01
	nudReachable  = 0x02
	nudStale      = 0x04
	nudDelay      = 0x08
	nudProbe      = 0x10
	nudFailed     = 0x20
	nudNoARP      = 0x40
	nudPermanent  = 0x80
	// nudUsable are the states in which a neighbour has answered recently
	// enough to be listed by Neighbours.
	nudUsable = nudReachable | nudStale | nudDelay | nudProbe | nudPermanent
)

// Neighbour is an entry of the kernel's neighbour table (IPv4 ARP or IPv6
// NDP).
type Neighbour struct {
	IP    netip.Addr
	MAC   string // lower-case, colon-separated
	Iface string // interface name ("" if unknown)
}

// Neighbours reads the kernel's neighbour table now: entries with a
// link-layer address in state REACHABLE, STALE, DELAY, PROBE or PERMANENT
// (never INCOMPLETE or FAILED), without all-zero, broadcast and multicast
// MACs, at most 65 536. Empty on systems other than Linux.
func (r *Registry) Neighbours(ctx context.Context) ([]Neighbour, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out, err := r.readNeighbours()
	if out == nil {
		out = []Neighbour{}
	}
	return out, err
}

// NeighbourMAC reads the kernel's neighbour table now and returns the MAC
// of ip (entries that are not incomplete or failed). ok is false when ip
// is not listed (always on systems other than Linux).
func (r *Registry) NeighbourMAC(ip netip.Addr) (mac string, ok bool) {
	mac = r.readARP()[netutil.Canon(ip)]
	return mac, mac != ""
}

// arpLoop refreshes the neighbour table now, every 30 s and when an early
// read is requested (kickARP), but then at most once per second.
func (r *Registry) arpLoop(ctx context.Context) {
	r.refreshARP()
	last := time.Now()
	t := time.NewTicker(arpInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-r.arpKick:
			if wait := arpEarlyGap - time.Since(last); wait > 0 {
				w := time.NewTimer(wait)
				select {
				case <-ctx.Done():
					w.Stop()
					return
				case <-w.C:
				}
			}
		}
		r.refreshARP()
		last = time.Now()
	}
}

// refreshARP re-reads the neighbour table, rebuilds the learned MACs and
// invalidates the identities of addresses whose MAC changed and of the
// other addresses of those MACs (their name fallback may change), or all
// identities when the learned MACs changed.
func (r *Registry) refreshARP() {
	r.arpMu.Lock()
	defer r.arpMu.Unlock()
	r.applyARP(r.readARP())
}

// refreshARPFor re-reads the neighbour table and applies it if it lists ip
// (the other reads are left to the regular refresh). It reports whether
// ip has a MAC now.
func (r *Registry) refreshARPFor(ip netip.Addr) bool {
	r.arpMu.Lock()
	defer r.arpMu.Unlock()
	if (*r.arp.Load())[ip] != "" {
		return true // another query applied it meanwhile
	}
	next := r.readARP()
	if next[ip] == "" {
		return false
	}
	r.applyARP(next)
	return true
}

// applyARP stores a neighbour table read (arpMu held), rebuilds the learned
// MACs and invalidates the affected identities.
func (r *Registry) applyARP(next map[netip.Addr]string) {
	if next == nil {
		next = map[netip.Addr]string{}
	}
	old := *r.arp.Load()
	if maps.Equal(old, next) {
		return
	}
	r.arp.Store(&next)
	if r.rebuildLearned() {
		r.invalidate()
		return
	}
	macs := map[string]bool{}
	for ip, mac := range next {
		if prev := old[ip]; prev != mac {
			r.invalidateIP(ip)
			macs[mac], macs[prev] = true, true
		}
	}
	for ip, mac := range old {
		if _, ok := next[ip]; !ok {
			r.invalidateIP(ip)
			macs[mac] = true
		}
	}
	delete(macs, "")
	for mac := range macs {
		r.invalidateMAC(mac)
	}
}

// parseARP parses /proc/net/arp:
//
//	IP address       HW type     Flags       HW address            Mask     Device
//	192.168.1.1      0x1         0x2         aa:bb:cc:dd:ee:ff     *        eth0
//
// Incomplete entries (flags 0x0) and all-zero MACs are skipped.
func parseARP(rd io.Reader) map[netip.Addr]string {
	out := map[netip.Addr]string{}
	sc := bufio.NewScanner(io.LimitReader(rd, maxARPBytes))
	first := true
	for sc.Scan() {
		if first {
			first = false
			continue
		}
		f := strings.Fields(sc.Text())
		if len(f) < 4 || f[2] == "0x0" {
			continue
		}
		ip, err := netip.ParseAddr(f[0])
		if err != nil {
			continue
		}
		mac, ok := normalizeMAC(f[3])
		if !ok {
			continue
		}
		out[netutil.Canon(ip)] = mac
		if len(out) >= maxARPEntries {
			break
		}
	}
	return out
}

// parseNeighDump parses the reply of a netlink RTM_GETNEIGH dump (IPv4 ARP
// and IPv6 NDP neighbours) into address → MAC. Incomplete, failed and NOARP
// entries and non-Ethernet or all-zero link-layer addresses are skipped;
// malformed data ends the parse.
func parseNeighDump(b []byte) map[netip.Addr]string {
	out := map[netip.Addr]string{}
	walkNeighDump(b, func(n neighMsg) bool {
		if n.state&(nudIncomplete|nudFailed|nudNoARP) == 0 {
			out[n.ip] = n.mac
		}
		return len(out) < maxARPEntries
	})
	return out
}

// parseNeighbours parses a netlink neighbour dump for Neighbours: entries
// in a usable state with a unicast MAC, each address once. ifname names an
// interface index ("" if unknown).
func parseNeighbours(b []byte, ifname func(int) string) []Neighbour {
	var out []Neighbour
	seen := map[netip.Addr]bool{}
	walkNeighDump(b, func(n neighMsg) bool {
		if n.state&nudUsable != 0 && !groupMAC(n.mac) && !seen[n.ip] {
			seen[n.ip] = true
			out = append(out, Neighbour{IP: n.ip, MAC: n.mac, Iface: ifname(int(n.ifindex))})
		}
		return len(out) < maxARPEntries
	})
	return out
}

// parseARPNeighbours parses /proc/net/arp for Neighbours (the fallback when
// netlink is unavailable; IPv4 only): complete or permanent entries
// (flags ATF_COM 0x2, ATF_PERM 0x4) with a unicast MAC.
func parseARPNeighbours(rd io.Reader) []Neighbour {
	var out []Neighbour
	seen := map[netip.Addr]bool{}
	sc := bufio.NewScanner(io.LimitReader(rd, maxARPBytes))
	first := true
	for sc.Scan() && len(out) < maxARPEntries {
		if first {
			first = false
			continue
		}
		f := strings.Fields(sc.Text())
		if len(f) < 6 {
			continue
		}
		flags, err := strconv.ParseUint(strings.TrimPrefix(f[2], "0x"), 16, 32)
		if err != nil || flags&0x6 == 0 {
			continue
		}
		ip, err := netip.ParseAddr(f[0])
		if err != nil {
			continue
		}
		ip = netutil.Canon(ip)
		mac, ok := normalizeMAC(f[3])
		if !ok || groupMAC(mac) || seen[ip] {
			continue
		}
		seen[ip] = true
		out = append(out, Neighbour{IP: ip, MAC: mac, Iface: f[5]})
	}
	return out
}

// groupMAC reports whether a normalised MAC is a multicast or broadcast
// address (the group bit of the first octet).
func groupMAC(mac string) bool {
	b, err := strconv.ParseUint(mac[:2], 16, 8)
	return err != nil || b&1 != 0
}

// neighMsg is one neighbour of a netlink dump.
type neighMsg struct {
	ifindex int32
	state   uint16
	ip      netip.Addr
	mac     string
}

// walkNeighDump calls fn for every well-formed RTM_NEWNEIGH message with an
// address and an Ethernet MAC until fn returns false; malformed data ends
// the walk.
func walkNeighDump(b []byte, fn func(neighMsg) bool) {
	for len(b) >= nlmsgHdrLen {
		l := int(binary.NativeEndian.Uint32(b[0:4]))
		typ := binary.NativeEndian.Uint16(b[4:6])
		if l < nlmsgHdrLen || l > len(b) || typ == nlmsgDone {
			return
		}
		if typ == rtmNewNeigh {
			if n, ok := parseNeighMsg(b[nlmsgHdrLen:l]); ok && !fn(n) {
				return
			}
		}
		n := align4(l)
		if n >= len(b) {
			return
		}
		b = b[n:]
	}
}

// parseNeighMsg parses one ndmsg with its attributes. It fails for
// entries without a usable address (unspecified, multicast) or without an
// Ethernet MAC.
func parseNeighMsg(m []byte) (neighMsg, bool) {
	if len(m) < ndMsgLen {
		return neighMsg{}, false
	}
	msg := neighMsg{
		ifindex: int32(binary.NativeEndian.Uint32(m[4:8])),
		state:   binary.NativeEndian.Uint16(m[8:10]),
	}
	var ip netip.Addr
	var mac string
	for a := m[ndMsgLen:]; len(a) >= 4; {
		l := int(binary.NativeEndian.Uint16(a[0:2]))
		if l < 4 || l > len(a) {
			return neighMsg{}, false
		}
		v := a[4:l]
		switch binary.NativeEndian.Uint16(a[2:4]) & 0x3fff { // without NLA_F_NESTED / NLA_F_NET_BYTEORDER
		case ndaDst:
			ip, _ = netip.AddrFromSlice(v)
		case ndaLLAddr:
			if len(v) == 6 {
				mac, _ = normalizeMAC(net.HardwareAddr(v).String())
			}
		}
		n := align4(l)
		if n >= len(a) {
			break
		}
		a = a[n:]
	}
	ip = netutil.Canon(ip)
	if !ip.IsValid() || ip.IsUnspecified() || ip.IsMulticast() || mac == "" {
		return neighMsg{}, false
	}
	msg.ip, msg.mac = ip, mac
	return msg, true
}

func align4(n int) int { return (n + 3) &^ 3 }

// mergeNeighbours adds the entries of from to into (from wins), keeping
// into below maxARPEntries.
func mergeNeighbours(into, from map[netip.Addr]string) {
	for ip, mac := range from {
		if _, ok := into[ip]; !ok && len(into) >= maxARPEntries {
			continue
		}
		into[ip] = mac
	}
}
