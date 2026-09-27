package hostinfo

import (
	"os"
	"slices"
	"strconv"
	"strings"
)

// Interface is the sysfs view of a network interface (the interfaces
// panel of the network check, api.NetworkInterfaces; the caller adds the
// addresses, routes and gateways).
type Interface struct {
	Name      string
	Index     int
	MAC       string // "" when none (all zero) or unreadable
	Up        bool   // IFF_UP
	OperState string // up | down | dormant | lowerlayerdown | notpresent | testing | unknown
	MTU       int
	SpeedMbps int    // 0 = unknown (−1, EINVAL or unreadable)
	Duplex    string // full | half | "" (unknown)
	Virtual   bool   // the sysfs node is below /sys/devices/virtual
	RxBytes   int64
	TxBytes   int64
	RxErrors  int64
	TxErrors  int64
}

// Bounds of the interface reads.
const (
	MaxInterfaces = 64
	maxNetRead    = 64 // bytes of one sysfs attribute
	iffUp         = 0x1
	iffLoopback   = 0x8
)

var operStates = []string{"up", "down", "dormant", "lowerlayerdown", "notpresent", "testing", "unknown"}

// Interfaces reads /sys/class/net below the root: every interface but
// loopback, sorted by name, at most 64. An attribute that cannot be read
// is left at its zero value.
func (s *Sampler) Interfaces() []Interface {
	entries, err := os.ReadDir(s.file("/sys/class/net"))
	if err != nil {
		return []Interface{}
	}
	var names []string
	for _, e := range entries {
		if n := e.Name(); validIfaceName(n) {
			names = append(names, n)
		}
	}
	slices.Sort(names)
	out := []Interface{}
	for _, n := range names {
		if len(out) == MaxInterfaces {
			break
		}
		dir := "/sys/class/net/" + n + "/"
		attr := func(a string) string { return strings.TrimSpace(string(s.read(dir+a, maxNetRead))) }
		num := func(a string) int64 {
			v, err := strconv.ParseInt(attr(a), 0, 64)
			if err != nil {
				return 0
			}
			return v
		}
		flags := num("flags")
		if flags&iffLoopback != 0 || attr("type") == "772" {
			continue
		}
		in := Interface{Name: n, Index: int(num("ifindex")), MAC: macAttr(attr("address")), Up: flags&iffUp != 0,
			OperState: attr("operstate"), MTU: int(num("mtu")),
			RxBytes: num("statistics/rx_bytes"), TxBytes: num("statistics/tx_bytes"),
			RxErrors: num("statistics/rx_errors"), TxErrors: num("statistics/tx_errors")}
		if !slices.Contains(operStates, in.OperState) {
			in.OperState = "unknown"
		}
		if v := num("speed"); v > 0 && v < 1<<31 {
			in.SpeedMbps = int(v)
		}
		if d := attr("duplex"); d == "full" || d == "half" {
			in.Duplex = d
		}
		if _, err := os.Stat(s.file("/sys/devices/virtual/net/" + n)); err == nil {
			in.Virtual = true
		}
		out = append(out, in)
	}
	return out
}

// validIfaceName reports a Linux interface name: 1–15 bytes, no "/", ":",
// white space or control characters, not "." or "..".
func validIfaceName(s string) bool {
	if len(s) == 0 || len(s) > 15 || s == "." || s == ".." {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; c <= ' ' || c >= 0x7f || c == '/' || c == ':' {
			return false
		}
	}
	return true
}

// macAttr returns a link-layer address of hex pairs separated by ":"
// (lower-case), "" when it is all zero or malformed.
func macAttr(s string) string {
	s = strings.ToLower(s)
	if len(s) < 2 || len(s) > 59 {
		return ""
	}
	zero := true
	for part := range strings.SplitSeq(s, ":") {
		if len(part) != 2 || strings.Trim(part, "0123456789abcdef") != "" {
			return ""
		}
		if part != "00" {
			zero = false
		}
	}
	if zero {
		return ""
	}
	return s
}
