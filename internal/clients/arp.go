package clients

import (
	"bufio"
	"context"
	"encoding/binary"
	"io"
	"maps"
	"net"
	"net/netip"
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
	nudFailed     = 0x20
	nudNoARP      = 0x40
)

// arpLoop refreshes the neighbour table now and every 30 s.
func (r *Registry) arpLoop(ctx context.Context) {
	r.refreshARP()
	t := time.NewTicker(arpInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.refreshARP()
		}
	}
}

// refreshARP re-reads the neighbour table and invalidates the identities of
// addresses whose MAC changed.
func (r *Registry) refreshARP() {
	next := r.readARP()
	if next == nil {
		next = map[netip.Addr]string{}
	}
	old := *r.arp.Load()
	if maps.Equal(old, next) {
		return
	}
	r.arp.Store(&next)
	for ip, mac := range next {
		if old[ip] != mac {
			r.invalidateIP(ip)
		}
	}
	for ip := range old {
		if _, ok := next[ip]; !ok {
			r.invalidateIP(ip)
		}
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
	for len(b) >= nlmsgHdrLen && len(out) < maxARPEntries {
		l := int(binary.NativeEndian.Uint32(b[0:4]))
		typ := binary.NativeEndian.Uint16(b[4:6])
		if l < nlmsgHdrLen || l > len(b) || typ == nlmsgDone {
			break
		}
		if typ == rtmNewNeigh {
			if ip, mac, ok := parseNeighMsg(b[nlmsgHdrLen:l]); ok {
				out[ip] = mac
			}
		}
		n := align4(l)
		if n >= len(b) {
			break
		}
		b = b[n:]
	}
	return out
}

// parseNeighMsg parses one ndmsg with its attributes.
func parseNeighMsg(m []byte) (netip.Addr, string, bool) {
	if len(m) < ndMsgLen {
		return netip.Addr{}, "", false
	}
	if state := binary.NativeEndian.Uint16(m[8:10]); state&(nudIncomplete|nudFailed|nudNoARP) != 0 {
		return netip.Addr{}, "", false
	}
	var ip netip.Addr
	var mac string
	for a := m[ndMsgLen:]; len(a) >= 4; {
		l := int(binary.NativeEndian.Uint16(a[0:2]))
		if l < 4 || l > len(a) {
			return netip.Addr{}, "", false
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
		return netip.Addr{}, "", false
	}
	return ip, mac, true
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
