package netutil

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"net/netip"
	"os"
	"strconv"
	"strings"
)

// DefaultGatewayIPv4 returns the IPv4 default gateway from /proc/net/route
// (Linux). It is used to auto-detect the router resolver.
func DefaultGatewayIPv4() (netip.Addr, error) {
	f, err := os.Open("/proc/net/route")
	if err != nil {
		return netip.Addr{}, err
	}
	defer f.Close()
	ip, _, err := parseProcRoute(f)
	return ip, err
}

// parseProcRoute returns the gateway of the first IPv4 default route with a
// gateway and the interface of the first IPv4 default route ("" if none;
// a point-to-point default route has no gateway).
func parseProcRoute(r io.Reader) (netip.Addr, string, error) {
	sc := bufio.NewScanner(io.LimitReader(r, 1<<20))
	first := true
	iface := ""
	for sc.Scan() {
		if first {
			first = false
			continue
		}
		f := strings.Fields(sc.Text())
		if len(f) < 4 || f[1] != "00000000" {
			continue
		}
		if iface == "" {
			iface = f[0]
		}
		b, err := hex.DecodeString(f[2])
		if err != nil || len(b) != 4 {
			continue
		}
		// /proc/net/route stores addresses in host (little-endian) order.
		v := binary.LittleEndian.Uint32(b)
		var a [4]byte
		binary.BigEndian.PutUint32(a[:], v)
		ip := netip.AddrFrom4(a)
		if ip.IsUnspecified() {
			continue
		}
		return ip, f[0], nil
	}
	return netip.Addr{}, iface, errors.New("no IPv4 default route")
}

// DefaultGatewayIPv6 returns the IPv6 default gateway from
// /proc/net/ipv6_route (Linux): the next hop of the default route (::/0)
// with the lowest metric, usually the router's link-local address, which
// then carries the route's interface as zone (fe80::1%eth0).
func DefaultGatewayIPv6() (netip.Addr, error) {
	f, err := os.Open("/proc/net/ipv6_route")
	if err != nil {
		return netip.Addr{}, err
	}
	defer f.Close()
	return parseIPv6Route(f)
}

// Route flags (linux/route.h).
const (
	rtfUp     = 0x0001
	rtfReject = 0x0200
)

// parseIPv6Route parses /proc/net/ipv6_route: destination, prefix length,
// source, source prefix length, next hop, metric, refcount, use, flags and
// device per line, addresses as 32 hex digits.
func parseIPv6Route(r io.Reader) (netip.Addr, error) {
	sc := bufio.NewScanner(io.LimitReader(r, 1<<20))
	var best netip.Addr
	var bestMetric uint64
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 10 || f[0] != strings.Repeat("0", 32) || f[1] != "00" {
			continue
		}
		b, err := hex.DecodeString(f[4])
		if err != nil || len(b) != 16 {
			continue
		}
		gw := netip.AddrFrom16([16]byte(b))
		metric, err1 := strconv.ParseUint(f[5], 16, 32)
		flags, err2 := strconv.ParseUint(f[8], 16, 32)
		if err1 != nil || err2 != nil || gw.IsUnspecified() || flags&rtfUp == 0 || flags&rtfReject != 0 {
			continue
		}
		if gw.IsLinkLocalUnicast() && validIfaceName(f[9]) {
			gw = gw.WithZone(f[9])
		}
		if !best.IsValid() || metric < bestMetric {
			best, bestMetric = gw, metric
		}
	}
	if !best.IsValid() {
		return netip.Addr{}, errors.New("no IPv6 default route")
	}
	return best, nil
}

// DefaultRouteInterface returns the interface of the IPv4 default route,
// else the zone (interface) of the IPv6 default gateway; "" if there is
// none or on systems other than Linux.
func DefaultRouteInterface() string {
	if f, err := os.Open("/proc/net/route"); err == nil {
		_, iface, _ := parseProcRoute(f)
		f.Close()
		if validIfaceName(iface) {
			return iface
		}
	}
	if gw, err := DefaultGatewayIPv6(); err == nil {
		return gw.Zone()
	}
	return ""
}

// IgnoresRouterAdvertisements reports whether the kernel ignores IPv6
// router advertisements on iface (Linux sysctl net.ipv6.conf.<iface>:
// accept_ra 0, or 1 while forwarding is on). ok is false when the values
// cannot be read (other systems, IPv6 disabled, unknown interface).
func IgnoresRouterAdvertisements(iface string) (ignores, ok bool) {
	if !validIfaceName(iface) {
		return false, false
	}
	dir := ipv6ConfDir + iface + "/"
	acceptRA, err := readSysctlInt(dir + "accept_ra")
	if err != nil {
		return false, false
	}
	forwarding, err := readSysctlInt(dir + "forwarding")
	if err != nil {
		return false, false
	}
	return acceptRA == 0 || (acceptRA == 1 && forwarding != 0), true
}

// ipv6ConfDir holds the per-interface IPv6 sysctls (replaced in tests).
var ipv6ConfDir = "/proc/sys/net/ipv6/conf/"

// validIfaceName reports whether s can be an interface name in a /proc or
// /sys path (never "", ".", ".." or containing a slash).
func validIfaceName(s string) bool {
	return s != "" && len(s) <= 64 && s != "." && s != ".." && !strings.ContainsAny(s, "/\x00")
}

func readSysctlInt(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 64))
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(b)))
}

// ResolvConfSearch returns the search/domain entries of /etc/resolv.conf
// (lower-case, without trailing dot). Empty on error or non-Linux systems.
func ResolvConfSearch() []string {
	f, err := os.Open("/etc/resolv.conf")
	if err != nil {
		return nil
	}
	defer f.Close()
	return parseResolvConfSearch(f)
}

func parseResolvConfSearch(r io.Reader) []string {
	var out []string
	sc := bufio.NewScanner(io.LimitReader(r, 64<<10))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 || (f[0] != "search" && f[0] != "domain") {
			continue
		}
		for _, d := range f[1:] {
			d = strings.ToLower(strings.TrimSuffix(d, "."))
			if d == "" || strings.HasPrefix(d, "#") || strings.HasPrefix(d, ";") {
				break
			}
			if _, _, ok := NormalizeHost(d); ok {
				dup := false
				for _, o := range out {
					dup = dup || o == d
				}
				if !dup {
					out = append(out, d)
				}
			}
		}
	}
	return out
}
