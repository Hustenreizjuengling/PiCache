package upstream

import (
	"net/netip"
	"strings"

	"github.com/miekg/dns"
)

// Kinds of answers blocked by the upstream itself (docs/ARCHITECTURE.md
// 7.4), checked in this order.
const (
	BlockEDE          = "ede"            // EDE 15 Blocked, 16 Censored or 17 Filtered
	BlockNullIP       = "null-ip"        // every A is 0.0.0.0 and every AAAA ::
	BlockBlockPage    = "block-page"     // every address is a known block page
	BlockNXDomainNoRA = "nxdomain-no-ra" // Quad9's blocks: NXDOMAIN with RA=0
)

// BlockInfo describes an answer a default upstream blocked itself.
type BlockInfo struct {
	Kind string // BlockEDE, BlockNullIP, BlockBlockPage or BlockNXDomainNoRA
	Host string // host of the answering upstream (UpstreamSpec.Host: never the path, query or port of a DoH URL)
}

// Reason is the query-log reason: "<host>: <kind>".
func (b *BlockInfo) Reason() string { return b.Host + ": " + b.Kind }

// ClientText is the EDE text sent to clients: "blocked by upstream
// (<kind>)", without the host, which can carry an account or profile ID
// (e.g. tls://<id>.dns.example) that no LAN or guest device may learn.
func (b *BlockInfo) ClientText() string { return "blocked by upstream (" + b.Kind + ")" }

// blockPages are the block page addresses of Cisco Umbrella (OpenDNS),
// 146.112.61.104–146.112.61.110 and their IPv4-mapped IPv6 forms. Source:
// Cisco Umbrella, "What are the Cisco Umbrella Block Page IP Addresses".
var blockPages = func() map[netip.Addr]bool {
	m := map[netip.Addr]bool{}
	for last := byte(104); last <= 110; last++ {
		v4 := netip.AddrFrom4([4]byte{146, 112, 61, last})
		m[v4] = true
		m[netip.AddrFrom16(v4.As16())] = true // ::ffff:146.112.61.x
	}
	return m
}()

// quad9Filtering are the hosts of Quad9's filtering endpoints (DoH/DoT
// names and plain IPs, so the plain upstreams of the clock guard are
// covered). They answer blocked names with NXDOMAIN without RA.
var quad9Filtering = func() map[string]bool {
	m := map[string]bool{}
	for _, h := range []string{"dns.quad9.net", "dns9.quad9.net", "dns11.quad9.net", "9.9.9.9", "149.112.112.112",
		"9.9.9.11", "149.112.112.11", "2620:fe::fe", "2620:fe::9", "2620:fe::11", "2620:fe::fe:11"} {
		m[h] = true
	}
	return m
}()

// isQuad9Filtering reports whether host (UpstreamSpec.Host) is one of
// Quad9's filtering endpoints; IP literals are compared canonically.
func isQuad9Filtering(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if ip, err := netip.ParseAddr(host); err == nil {
		host = ip.Unmap().WithZone("").String()
	}
	return quad9Filtering[host]
}

// classify decides whether the reply m of a default-set upstream (host) to
// a query of type qtype is the upstream's own block; blocking is the
// reply's blocking EDE (parseEDE). The first matching kind wins.
func classify(m *dns.Msg, qtype uint16, host string, blocking *EDE) *BlockInfo {
	switch {
	case blocking != nil:
		return &BlockInfo{Kind: BlockEDE, Host: host}
	case addressAnswer(m, qtype, func(ip netip.Addr) bool { return ip.IsUnspecified() }):
		return &BlockInfo{Kind: BlockNullIP, Host: host}
	case addressAnswer(m, qtype, func(ip netip.Addr) bool { return blockPages[ip] }):
		return &BlockInfo{Kind: BlockBlockPage, Host: host}
	case m.Rcode == dns.RcodeNameError && !m.RecursionAvailable && isQuad9Filtering(host):
		return &BlockInfo{Kind: BlockNXDomainNoRA, Host: host}
	}
	return nil
}

// addressAnswer reports whether m is a NOERROR answer to an A or AAAA
// query with at least one record of qtype in which every A and AAAA record
// satisfies match (CNAMEs and other records may precede them).
func addressAnswer(m *dns.Msg, qtype uint16, match func(netip.Addr) bool) bool {
	if (qtype != dns.TypeA && qtype != dns.TypeAAAA) || m.Rcode != dns.RcodeSuccess {
		return false
	}
	n := 0
	for _, rr := range m.Answer {
		var ip netip.Addr
		switch v := rr.(type) {
		case *dns.A:
			ip, _ = netip.AddrFromSlice(v.A.To4())
		case *dns.AAAA:
			ip, _ = netip.AddrFromSlice(v.AAAA.To16())
		default:
			continue
		}
		if !ip.IsValid() || !match(ip) {
			return false
		}
		if rr.Header().Rrtype == qtype {
			n++
		}
	}
	return n > 0
}
