package filter

import (
	"encoding/binary"
	"net/netip"
	"slices"
	"sort"
	"strings"
)

// Address tiers of lists of format ips (and of the user IP rules): an
// exception ("@@" lines, allowlists) beats a block of the same kind of
// source.
const (
	ipTierAllow = iota
	ipTierBlock
	numIPTiers
)

// ipGuardV4 and ipGuardV6 are the networks an address block may not
// overlap (ARCHITECTURE 7.2, IP guard): this network, private, CGNAT,
// loopback, link-local, multicast and reserved IPv4 space; the IPv6
// unspecified and loopback addresses, ULA, link-local and multicast.
var (
	ipGuardV4 = []netip.Prefix{
		netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
		netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("172.16.0.0/12"),
		netip.MustParsePrefix("192.168.0.0/16"), netip.MustParsePrefix("224.0.0.0/4"), netip.MustParsePrefix("240.0.0.0/4"),
	}
	ipGuardV6 = []netip.Prefix{
		netip.MustParsePrefix("::/128"), netip.MustParsePrefix("::1/128"), netip.MustParsePrefix("fc00::/7"),
		netip.MustParsePrefix("fe80::/10"), netip.MustParsePrefix("ff00::/8"),
	}
	// ipEmbedV6 are the IPv6 prefixes that embed an IPv4 address: a block
	// inside one must be written as the IPv4 address, and a block around one
	// would block every address it carries (every NAT64/DNS64 answer).
	ipEmbedV6 = []netip.Prefix{
		netip.MustParsePrefix("::ffff:0:0/96"), netip.MustParsePrefix("::/96"), netip.MustParsePrefix("64:ff9b::/96"),
		netip.MustParsePrefix("64:ff9b:1::/48"), netip.MustParsePrefix("2002::/16"),
	}
)

// Narrowest-allowed bounds of address blocks of lists (the IP guard) and of
// IP rules.
const (
	minListBitsV4 = 16
	minListBitsV6 = 32
	minRuleBitsV4 = 8
	minRuleBitsV6 = 32
)

// canonicalPrefix returns an address or CIDR in its stored form: an
// IPv4-mapped address or network as IPv4, masked, never with a zone.
func canonicalPrefix(s string) (netip.Prefix, bool) {
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		if err != nil || p.Addr().Zone() != "" {
			return netip.Prefix{}, false
		}
		addr, bits := p.Addr(), p.Bits()
		if addr.Is4In6() && bits >= 96 {
			addr, bits = addr.Unmap(), bits-96
		}
		return netip.PrefixFrom(addr, bits).Masked(), true
	}
	ip, err := netip.ParseAddr(s)
	if err != nil || ip.Zone() != "" {
		return netip.Prefix{}, false
	}
	ip = ip.Unmap()
	return netip.PrefixFrom(ip, ip.BitLen()), true
}

// prefixText returns the stored text of a canonical prefix: a single
// address without "/len".
func prefixText(p netip.Prefix) string {
	if p.IsSingleIP() {
		return p.Addr().String()
	}
	return p.String()
}

// ipGuarded reports whether the IP guard refuses a block of p: broader than
// /16 (IPv4) or /32 (IPv6), overlapping one of the guarded networks, or an
// IPv6 block overlapping a prefix that embeds IPv4 (inside it, or around it
// like 64:ff9b::/32).
func ipGuarded(p netip.Prefix) bool {
	if p.Addr().Is4() {
		if p.Bits() < minListBitsV4 {
			return true
		}
		return slices.ContainsFunc(ipGuardV4, p.Overlaps)
	}
	if p.Bits() < minListBitsV6 || slices.ContainsFunc(ipGuardV6, p.Overlaps) {
		return true
	}
	return slices.ContainsFunc(ipEmbedV6, p.Overlaps)
}

// ipEntry is one line of a list of format ips.
type ipEntry struct {
	prefix netip.Prefix
	allow  bool
}

// parseIPLine parses a line of an address list: "<address>", "<CIDR>" or
// "||<address>^", each also with "@@" (an exception). Everything else
// (hosts lines, domains, modifiers, "||<CIDR>^") is invalid. The IP guard
// refuses broad and private blocks (lineBroad).
func parseIPLine(line string) (ipEntry, lineStatus) {
	s := strings.TrimSpace(line)
	if s == "" {
		return ipEntry{}, lineSkip
	}
	switch s[0] {
	case '!', '#', ';', '[':
		return ipEntry{}, lineSkip
	}
	if i := inlineComment(s); i >= 0 {
		if s = strings.TrimSpace(s[:i]); s == "" {
			return ipEntry{}, lineSkip
		}
	}
	var e ipEntry
	if rest, ok := strings.CutPrefix(s, "@@"); ok {
		e.allow, s = true, rest
	}
	abp := false
	if rest, ok := strings.CutPrefix(s, "||"); ok {
		body, sep := strings.CutSuffix(rest, "^")
		if !sep || strings.Contains(body, "/") {
			return ipEntry{}, lineInvalid
		}
		s, abp = body, true
	}
	if s == "" || strings.ContainsAny(s, " \t$|^") {
		return ipEntry{}, lineInvalid
	}
	p, ok := canonicalPrefix(s)
	if !ok || (abp && !p.IsSingleIP()) {
		return ipEntry{}, lineInvalid
	}
	e.prefix = p
	return e, lineOK
}

// ipKey returns the hash-set key of the network of ip with the given
// prefix length: IPv4 addr<<8 | bits exactly, IPv6 the 64-bit FNV-1a of
// the 16 masked bytes and the length byte.
func ipKey(ip netip.Addr, bits int) uint64 {
	if ip.Is4() {
		a := ip.As4()
		v := binary.BigEndian.Uint32(a[:])
		if bits < 32 {
			v &= ^uint32(0) << (32 - bits) // 0 for bits 0
		}
		return uint64(v)<<8 | uint64(bits)
	}
	a := ip.As16()
	h := uint64(fnvOffset64)
	for i := range 16 {
		b := a[i]
		if rem := bits - i*8; rem <= 0 {
			b = 0
		} else if rem < 8 {
			b &= ^byte(0) << (8 - rem)
		}
		h ^= uint64(b)
		h *= fnvPrime64
	}
	h ^= uint64(bits)
	h *= fnvPrime64
	return h
}

// ipTier holds the address entries of one tier per family: a hash set of
// the masked networks and the prefix lengths present, longest first.
type ipTier struct {
	v4, v6         hashSet
	v4Bits, v6Bits []uint8
}

// lookup returns the first source whose network contains ip (canonical:
// unmapped, no zone), longest prefix first, sharing a group with groups.
func (t *ipTier) lookup(ip netip.Addr, groups []int64, srcGroups [][]int64) (uint32, bool) {
	set, bits := &t.v6, t.v6Bits
	if ip.Is4() {
		set, bits = &t.v4, t.v4Bits
	}
	if set.Len() == 0 {
		return 0, false
	}
	for _, b := range bits {
		if src, ok := set.lookup(ipKey(ip, int(b)), groups, srcGroups, nil); ok {
			return src, true
		}
	}
	return 0, false
}

// ipBitsOf returns the prefix lengths of prefixes, unique, longest first.
func ipBitsOf(bits []uint8) []uint8 {
	out := slices.Clone(bits)
	slices.Sort(out)
	out = slices.Compact(out)
	slices.Reverse(out)
	return out
}

// mergeIPTier merges the address entries of one tier of the parse results
// (source index = result index).
func mergeIPTier(results []*parsed, tier int) ipTier {
	var t ipTier
	for fam := range 2 {
		parts := make([][]uint64, len(results))
		var bits []uint8
		for i, r := range results {
			parts[i] = r.ips[tier][fam]
			bits = append(bits, r.ipBits[tier][fam]...)
		}
		if fam == 0 {
			t.v4, t.v4Bits = mergeHashes(parts), ipBitsOf(bits)
		} else {
			t.v6, t.v6Bits = mergeHashes(parts), ipBitsOf(bits)
		}
	}
	return t
}

// ipRuleEntry is a compiled user IP rule.
type ipRuleEntry struct {
	id      int64
	action  Action
	pattern string
	groups  []int64
}

// ipRuleMatcher is the compiled, immutable form of the enabled IP rules.
type ipRuleMatcher struct {
	tiers  [numIPTiers]ipTier
	rules  []ipRuleEntry
	groups [][]int64
	memory int64
}

// buildIPRuleMatcher compiles the enabled IP rules (patterns canonical).
func buildIPRuleMatcher(rules []ipRuleEntry) *ipRuleMatcher {
	m := &ipRuleMatcher{rules: rules, groups: make([][]int64, len(rules))}
	var bits [numIPTiers][2][]uint8
	for i, r := range rules {
		m.groups[i] = r.groups
		p, ok := canonicalPrefix(r.pattern)
		if !ok {
			continue
		}
		tier := ipTierBlock
		if r.action == ActionAllow {
			tier = ipTierAllow
		}
		t := &m.tiers[tier]
		set, fam := &t.v6, 1
		if p.Addr().Is4() {
			set, fam = &t.v4, 0
		}
		set.h = append(set.h, ipKey(p.Addr(), p.Bits()))
		set.src = append(set.src, uint32(i))
		bits[tier][fam] = append(bits[tier][fam], uint8(p.Bits()))
		m.memory += int64(len(r.pattern)) + 64
	}
	for tier := range numIPTiers {
		t := &m.tiers[tier]
		sortHashSet(&t.v4)
		sortHashSet(&t.v6)
		t.v4Bits, t.v6Bits = ipBitsOf(bits[tier][0]), ipBitsOf(bits[tier][1])
		m.memory += t.v4.memory() + t.v6.memory()
	}
	return m
}

func sortHashSet(s *hashSet) {
	if s.Len() > 1 {
		sort.Sort(s)
	}
}

// checkIP returns the decision for an answer address (ARCHITECTURE 7.2,
// response addresses): user IP allow, user IP block, list IP allow, list
// IP block; the first tier with an entry that shares a group with groups
// wins. Lock-free and allocation-free; at once when no address entry or
// IP rule exists.
func (s *snapshot) checkIP(ip netip.Addr, groups []int64) Decision {
	if !s.hasIP || len(groups) == 0 || !ip.IsValid() {
		return Decision{}
	}
	ip = ip.Unmap().WithZone("")
	r := s.ipRules
	for tier := range numIPTiers {
		if src, ok := r.tiers[tier].lookup(ip, groups, r.groups); ok {
			e := &r.rules[src]
			return Decision{Action: e.action, Source: "ip-rule", Kind: "ip", RuleID: e.id, Name: e.pattern}
		}
	}
	for tier := range numIPTiers {
		if src, ok := s.lists.ip[tier].lookup(ip, groups, s.listGroups); ok {
			d := Decision{Action: ActionBlock, Source: "list", Kind: "ip"}
			if tier == ipTierAllow {
				d.Action = ActionAllow
			}
			s.nameList(&d, src)
			return d
		}
	}
	return Decision{}
}
