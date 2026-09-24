package clients

import (
	"bufio"
	"context"
	"io"
	"maps"
	"net/netip"
	"strings"
	"time"

	"github.com/hustenreizjuengling/picache/internal/netutil"
)

const (
	maxARPEntries = 65536
	maxARPBytes   = 8 << 20
)

// arpLoop refreshes the ARP table now and every 30 s.
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
