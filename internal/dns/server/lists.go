package dnsserver

import (
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/netutil"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// dnsLists are the lists of the dns settings section compiled for the
// query path (rebuilt on every settings change).
type dnsLists struct {
	blocked  *netutil.ClientList       // dns.blockedClients
	dropped  map[string][]droppedEntry // dns.droppedDomains by domain
	ignored  map[string]bool           // logs.ignoredDomains
	bogus    []netip.Prefix            // dns.bogusNxdomain
	revZones map[string]bool           // reverse zones of dns.privateReverseNetworks
	trusted  []netip.Addr              // dns.ednsClientTrusted
	ecs      netip.Prefix              // dns.ecs.customSubnet if public (invalid otherwise)
	// serverV4 and serverV6 are dns.serverNameAddresses (empty =
	// automatic).
	serverV4, serverV6 []netip.Addr
}

// isServerAddr reports whether ip is one of dns.serverNameAddresses.
func (l *dnsLists) isServerAddr(ip netip.Addr) bool {
	ip = netutil.Canon(ip)
	return slices.Contains(l.serverV4, ip) || slices.Contains(l.serverV6, ip)
}

// droppedEntry is one entry of dns.droppedDomains for its domain.
type droppedEntry struct {
	qtype uint16 // 0 = every type
	entry string // the normalised entry (traces)
}

func newDNSLists(set *settings.All) *dnsLists {
	d := &set.DNS
	l := &dnsLists{blocked: netutil.NewClientList(d.BlockedClients), dropped: map[string][]droppedEntry{}, revZones: map[string]bool{},
		ignored: make(map[string]bool, len(set.Logs.IgnoredDomains))}
	for _, s := range set.Logs.IgnoredDomains {
		if settings.ValidIgnoredDomain(s) {
			l.ignored[s] = true
		}
	}
	for _, s := range d.DroppedDomains {
		if e, err := settings.ParseDroppedDomain(s); err == nil {
			l.dropped[e.Domain] = append(l.dropped[e.Domain], droppedEntry{qtype: e.Type, entry: e.String()})
		}
	}
	for _, s := range d.BogusNXDomain {
		if p, err := settings.ParsePrefix(s); err == nil {
			l.bogus = append(l.bogus, p)
		}
	}
	for _, z := range d.PrivateReverseZones() {
		l.revZones[z] = true
	}
	for _, s := range d.EDNSClientTrusted {
		if ip, err := netip.ParseAddr(s); err == nil {
			l.trusted = append(l.trusted, netutil.Canon(ip))
		}
	}
	if p := d.ECSSubnet(); p.IsValid() && netutil.IsPublicUnicast(p.Addr()) {
		l.ecs = p
	}
	for _, v := range d.ServerNameAddresses.IPv4 {
		if ip, err := netip.ParseAddr(v); err == nil && ip.Unmap().Is4() {
			l.serverV4 = append(l.serverV4, ip.Unmap())
		}
	}
	for _, v := range d.ServerNameAddresses.IPv6 {
		if ip, err := netip.ParseAddr(v); err == nil && ip.Is6() && !ip.Is4In6() && ip.Zone() == "" {
			l.serverV6 = append(l.serverV6, ip)
		}
	}
	return l
}

func (l *dnsLists) isTrusted(ip netip.Addr) bool {
	return slices.Contains(l.trusted, netutil.Canon(ip))
}

// --- 2a and 5: blocked clients (dns.blockedClients) ---

// blockedSource returns the dns.blockedClients entry that matches the
// source address of a query (step 2a: IP and CIDR entries); protected
// sources never match (safety net).
func (s *Server) blockedSource(ip netip.Addr) (string, bool) {
	l := s.lists.Load()
	if l.blocked.Len() == 0 {
		return "", false
	}
	entry, ok := l.blocked.MatchAddr(ip)
	if !ok || s.protectedAddr(l, entry, ip) {
		return "", false
	}
	return entry, true
}

// blockedIdentity returns the dns.blockedClients entry that matches the
// identity of a query (end of step 5): MAC entries against the identity's
// MAC (neighbour table or EDNS), IP and CIDR entries against an address
// derived from EDNS. Protected MACs and addresses never match: a MAC from
// the neighbour table is the MAC of qc.client, so it never drops a
// protected address either (e.g. a trusted forwarder's own queries or a
// second address of the router).
func (s *Server) blockedIdentity(qc *qctx) (string, bool) {
	l := s.lists.Load()
	if l.blocked.Len() == 0 {
		return "", false
	}
	if mac := qc.id.MAC; mac != "" {
		if entry, ok := l.blocked.MatchMAC(mac); ok && !s.protectedMAC(entry, mac) && (qc.ednsMAC || !s.protectedAddr(l, entry, qc.client)) {
			return entry, true
		}
	}
	if qc.derived {
		if entry, ok := l.blocked.MatchAddr(qc.client); ok && !s.protectedAddr(l, entry, qc.client) {
			return entry, true
		}
	}
	return "", false
}

// protectedAddr reports whether ip must never be dropped whatever the list
// says: loopback, this machine's addresses, the router's addresses and the
// trusted EDNS forwarders. A matching entry is logged (at most once per
// entry and hour).
func (s *Server) protectedAddr(l *dnsLists, entry string, ip netip.Addr) bool {
	ip = netutil.Canon(ip)
	var what string
	switch {
	case ip.IsLoopback():
		what = "a loopback address"
	case s.host.Load().isOwn(ip):
		what = "an address of this machine"
	case slices.Contains(s.router.Load().protect, ip):
		what = "an address of the router"
	case l.isTrusted(ip):
		what = "a trusted EDNS forwarder (dns.ednsClientTrusted)"
	default:
		return false
	}
	s.warnProtected(entry, ip.String(), what)
	return true
}

// protectedMAC reports whether mac is the router's (of any default
// gateway), one of this machine's or a trusted EDNS forwarder's.
func (s *Server) protectedMAC(entry, mac string) bool {
	rs := s.router.Load()
	var what string
	switch {
	case slices.Contains(rs.macs, mac):
		what = "the router's MAC address"
	case slices.Contains(s.host.Load().macs, mac):
		what = "a MAC address of this machine"
	case slices.Contains(rs.trustedMACs, mac):
		what = "the MAC address of a trusted EDNS forwarder (dns.ednsClientTrusted)"
	default:
		return false
	}
	s.warnProtected(entry, mac, what)
	return true
}

// protectWarnings limits the WARN about a protected client matched by a
// blocked-client entry to once per entry and hour (at most one entry per
// list entry, dns.blockedClients has at most 256).
type protectWarnings struct {
	mu   sync.Mutex
	last map[string]time.Time
}

func (s *Server) warnProtected(entry, who, what string) {
	now := time.Now()
	w := &s.protectWarn
	w.mu.Lock()
	if t, ok := w.last[entry]; ok && now.Sub(t) < time.Hour {
		w.mu.Unlock()
		return
	}
	if w.last == nil || len(w.last) >= 2*settings.MaxBlockedClients {
		w.last = map[string]time.Time{}
	}
	w.last[entry] = now
	w.mu.Unlock()
	s.log.Warn("a dns.blockedClients entry matches "+what+"; its queries are answered anyway (remove the entry)",
		slog.String("entry", entry), slog.String("client", who))
}

// ProtectedClient is an address or MAC address that a blocked-client entry
// must never match (docs/ARCHITECTURE.md 6.1): What names it in errors.
type ProtectedClient struct {
	Addr netip.Addr // valid for addresses
	MAC  string     // set for MAC addresses
	What string     // e.g. "the router"
}

// ProtectedClients lists what dns.blockedClients must not contain: the
// loopback addresses, this machine's addresses and MACs, the router's
// addresses and MACs (every default gateway), the container network's
// gateway in a bridge network and the trusted EDNS forwarders with their
// MACs (trusted: the entries of the settings being checked; macOf reads
// the neighbour table now, nil = MACs unknown). A MAC entry of a trusted
// forwarder would drop its own queries and with them its clients'.
func (s *Server) ProtectedClients(trusted []string, macOf func(netip.Addr) (string, bool)) []ProtectedClient {
	out := []ProtectedClient{{Addr: netip.MustParseAddr("127.0.0.1"), What: "the loopback address"},
		{Addr: netip.IPv6Loopback(), What: "the loopback address"}}
	for _, ip := range netutil.LocalAddrs() {
		if !ip.IsLoopback() {
			out = append(out, ProtectedClient{Addr: ip, What: "this machine's address"})
		}
	}
	rs := s.router.Load()
	for _, ip := range rs.protect {
		out = append(out, ProtectedClient{Addr: ip, What: "the router"})
	}
	if s.cacheIPs.Load().bridge && s.env.gateway != nil {
		if gw, err := s.env.gateway(); err == nil && gw.IsValid() {
			out = append(out, ProtectedClient{Addr: netutil.Canon(gw), What: "the container network's gateway"})
		}
	}
	for _, t := range trusted {
		ip, err := netip.ParseAddr(strings.TrimSpace(t))
		if err != nil {
			p, err := netip.ParsePrefix(strings.TrimSpace(t))
			if err != nil || !p.IsSingleIP() {
				continue
			}
			ip = p.Addr()
		}
		ip = netutil.Canon(ip)
		out = append(out, ProtectedClient{Addr: ip, What: "the trusted EDNS forwarder"})
		if macOf == nil {
			continue
		}
		if mac, ok := macOf(ip); ok {
			if m, ok := settings.NormalizeMAC(mac); ok {
				out = append(out, ProtectedClient{MAC: m, What: "the MAC address of the trusted EDNS forwarder " + ip.String()})
			}
		}
	}
	for _, mac := range rs.macs {
		out = append(out, ProtectedClient{MAC: mac, What: "the router's MAC address"})
	}
	for _, mac := range s.host.Load().macs {
		out = append(out, ProtectedClient{MAC: mac, What: "a MAC address of this machine"})
	}
	return out
}

// BlockedClientLockout returns why a (normalised) dns.blockedClients entry
// would block a client PiCache depends on ("" if it does not), e.g.
// "192.168.178.0/24 contains the router 192.168.178.1".
func BlockedClientLockout(entry string, protected []ProtectedClient) string {
	loopback := []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")}
	if p, err := settings.ParsePrefix(entry); err == nil {
		for _, lo := range loopback {
			if p.Overlaps(lo) {
				if p.IsSingleIP() {
					return entry + " is a loopback address"
				}
				return fmt.Sprintf("%s contains loopback addresses (%s)", entry, lo)
			}
		}
		for _, c := range protected {
			if !c.Addr.IsValid() || !p.Contains(netutil.Canon(c.Addr)) {
				continue
			}
			if p.IsSingleIP() {
				return fmt.Sprintf("%s is %s", entry, c.What)
			}
			return fmt.Sprintf("%s contains %s %s", entry, c.What, c.Addr)
		}
		return ""
	}
	for _, c := range protected {
		if c.MAC != "" && strings.EqualFold(c.MAC, entry) {
			return fmt.Sprintf("%s is %s", entry, c.What)
		}
	}
	return ""
}

// localMACs returns the MAC addresses of this machine's interfaces
// (lower-case with colons; loopback and interfaces without one skipped).
func localMACs() []string {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []string
	for _, ifc := range ifs {
		if mac, ok := settings.NormalizeMAC(ifc.HardwareAddr.String()); ok && ifc.Flags&net.FlagLoopback == 0 && !slices.Contains(out, mac) {
			out = append(out, mac)
		}
	}
	return out
}

// --- 4a: the client identity of trusted forwarders ---

// ednsMACOption is the option that carries a client's MAC address in 6
// bytes (used by dnsmasq's --add-mac).
const ednsMACOption = 65001

// ednsClientIdentity parses the client options of a query from a trusted
// forwarder (untrusted data): the address from exactly one client subnet
// option of family 1 with source prefix 32 or family 2 with 128, whose
// address is unicast and neither loopback nor unspecified; the MAC from
// exactly one option 65001 of exactly 6 bytes that is not all-zero and not
// a group address (lower-case with colons). Several options of a kind
// leave that part out; the text or base64 MAC option (65073) and the
// CPE-ID (65074) are not used.
func ednsClientIdentity(opts []dns.EDNS0) (addr netip.Addr, mac string) {
	var subnets, macs int
	var subnet *dns.EDNS0_SUBNET
	var macData []byte
	for _, o := range opts {
		switch v := o.(type) {
		case *dns.EDNS0_SUBNET:
			subnets++
			subnet = v
		case *dns.EDNS0_LOCAL:
			if v.Code == ednsMACOption {
				macs++
				macData = v.Data
			}
		}
	}
	if subnets == 1 {
		var ip netip.Addr
		switch {
		case subnet.Family == 1 && subnet.SourceNetmask == 32:
			if v4 := subnet.Address.To4(); v4 != nil {
				ip = netip.AddrFrom4([4]byte(v4))
			}
		case subnet.Family == 2 && subnet.SourceNetmask == 128:
			if v6 := subnet.Address.To16(); v6 != nil {
				ip = netutil.Canon(netip.AddrFrom16([16]byte(v6)))
			}
		}
		if ip.IsValid() && !ip.IsMulticast() && !ip.IsLoopback() && !ip.IsUnspecified() && ip != netip.AddrFrom4([4]byte{255, 255, 255, 255}) {
			addr = ip
		}
	}
	if macs == 1 && len(macData) == 6 {
		if m, ok := settings.NormalizeMAC(net.HardwareAddr(macData).String()); ok {
			mac = m
		}
	}
	return addr, mac
}

// clientSubnet returns the client's first client subnet option as a
// masked prefix string ("203.0.113.0/24") for the query log: family 1 with
// a source prefix of at most 32 or family 2 with at most 128; "" otherwise.
func clientSubnet(req *dns.Msg) string {
	opt := req.IsEdns0()
	if opt == nil {
		return ""
	}
	for _, o := range opt.Option {
		e, ok := o.(*dns.EDNS0_SUBNET)
		if !ok {
			continue
		}
		var ip netip.Addr
		switch {
		case e.Family == 1 && e.SourceNetmask <= 32:
			if v4 := e.Address.To4(); v4 != nil {
				ip = netip.AddrFrom4([4]byte(v4))
			}
		case e.Family == 2 && e.SourceNetmask <= 128:
			if v6 := e.Address.To16(); v6 != nil {
				ip = netip.AddrFrom16([16]byte(v6))
			}
		}
		if !ip.IsValid() {
			return ""
		}
		p, err := ip.Prefix(int(e.SourceNetmask))
		if err != nil {
			return ""
		}
		return p.String()
	}
	return ""
}

// identifyClient runs steps 4a and 5 for a query: a trusted source may
// name its client by EDNS (address and/or MAC); without a derived address
// the client is the source.
func (s *Server) identifyClient(qc *qctx) {
	var addr netip.Addr
	var mac string
	if l := s.lists.Load(); len(l.trusted) > 0 && l.isTrusted(qc.source) {
		if opt := qc.req.IsEdns0(); opt != nil {
			addr, mac = ednsClientIdentity(opt.Option)
		}
	}
	switch {
	case (addr.IsValid() || mac != "") && s.d.Clients != nil:
		if addr.IsValid() {
			qc.client, qc.derived = addr, true
		}
		qc.id = s.d.Clients.IdentifyDerived(addr, mac)
		qc.ednsMAC = mac != ""
	case addr.IsValid():
		qc.client, qc.derived = addr, true
		qc.id = s.identify(addr)
	default:
		qc.id = s.identify(qc.source)
	}
}

// ecsFor returns the client subnet sent to the default upstreams for qc
// (dns.ecs): "client" = the /24 or /56 of the source when the source is a
// public unicast address, "custom" = the configured subnet if it is
// public; none otherwise.
func (s *Server) ecsFor(qc *qctx) netip.Prefix {
	switch qc.set.DNS.ECS.Mode {
	case settings.ECSClient:
		src := netutil.Canon(qc.source)
		if !netutil.IsPublicUnicast(src) {
			return netip.Prefix{}
		}
		bits := 24
		if src.Is6() {
			bits = 56
		}
		p, _ := src.Prefix(bits)
		return p
	case settings.ECSCustom:
		return s.lists.Load().ecs
	}
	return netip.Prefix{}
}

// --- logs.ignoredDomains ---

// ignoredDomain reports whether the query name is in logs.ignoredDomains
// (the domain or one of its subdomains): such queries are answered and
// filtered as usual but never logged or counted in the statistics.
func (s *Server) ignoredDomain(qname string) bool {
	l := s.lists.Load()
	if len(l.ignored) == 0 {
		return false
	}
	for n := qname; n != ""; n = parent(n) {
		if l.ignored[n] {
			return true
		}
	}
	return false
}

// --- 7b: dropped domains ---

// droppedDomain returns the dns.droppedDomains entry that matches the
// query name (the domain and its subdomains) and type.
func (s *Server) droppedDomain(qc *qctx) (string, bool) {
	l := s.lists.Load()
	if len(l.dropped) == 0 {
		return "", false
	}
	for n := qc.qname; n != ""; n = parent(n) {
		for _, e := range l.dropped[n] {
			if e.qtype == 0 || e.qtype == qc.qtype {
				return e.entry, true
			}
		}
	}
	return "", false
}
