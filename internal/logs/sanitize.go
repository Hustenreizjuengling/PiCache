package logs

import (
	"net/netip"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hustenreizjuengling/picache/internal/netutil"
)

// Field length limits for stored events (bytes, cut at a rune boundary).
const (
	maxNameLen   = 255 // host names, client names, SNI, qnames
	maxShortLen  = 32  // qtype, status, rcode, method, protocol, cache status, reason codes
	maxIDLen     = 64  // service, store and object IDs, client IPs
	maxTextLen   = 256 // reasons, answers, labels, group keys, user agents, upstreams
	maxPathLen   = 2048
	maxRangeLen  = 128
	maxEventSpan = 25 * time.Hour // longer durations are clamped (SNI lifetime is 24 h)
	maxClockSkew = time.Minute    // tolerated lead of an event timestamp over the writer's clock
)

// anonymizeIP zeroes the host part of ip: IPv4 is kept to /16, IPv6 to /48.
func anonymizeIP(ip netip.Addr) netip.Addr {
	ip = netutil.Canon(ip)
	if ip.Is4() {
		b := ip.As4()
		b[2], b[3] = 0, 0
		return netip.AddrFrom4(b)
	}
	b := ip.As16()
	clear(b[6:])
	return netip.AddrFrom16(b)
}

// cleanClientIP canonicalises a client address and anonymises it if anon is
// set. An unparsable value is kept (bounded) unless anonymising, where it is
// dropped because it cannot be masked.
func cleanClientIP(s string, anon bool) string {
	ip, err := netip.ParseAddr(strings.TrimSpace(s))
	if err != nil {
		if anon {
			return ""
		}
		return clean(s, maxIDLen)
	}
	if anon {
		return anonymizeIP(ip).String()
	}
	return netutil.Canon(ip).String()
}

// clean makes s valid UTF-8 (JSON encoding rejects invalid UTF-8) and cuts
// it to at most n bytes at a rune boundary.
func clean(s string, n int) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "�")
	}
	if len(s) <= n {
		return s
	}
	i := n
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i]
}

// maxEDETextLen bounds the text of an upstream Extended DNS Error.
const maxEDETextLen = 200

// cleanEDEText bounds the text of an upstream EDE like the resolver does
// (it comes from the upstream, untrusted): cut to 200 bytes at a rune
// boundary, valid UTF-8, without C0 controls, DEL, C1 controls and the
// bidi controls U+061C, U+200E, U+200F, U+202A–U+202E, U+2066–U+2069.
func cleanEDEText(s string) string {
	return stripControls(clean(strings.ToValidUTF8(s, ""), maxEDETextLen))
}

// maxUpstreamAnswerLen bounds QueryEvent.UpstreamAnswer.
const maxUpstreamAnswerLen = 512

// cleanUpstreamAnswer bounds the upstream's answer (untrusted data): valid
// UTF-8 without control and bidi characters (as cleanEDEText), cut to 512
// bytes at a rune boundary.
func cleanUpstreamAnswer(s string) string {
	return clean(stripControls(strings.ToValidUTF8(s, "")), maxUpstreamAnswerLen)
}

// stripControls removes C0 controls, DEL, C1 controls and the bidi
// controls U+061C, U+200E, U+200F, U+202A–U+202E, U+2066–U+2069.
func stripControls(s string) string {
	return strings.Map(func(r rune) rune {
		if isControlOrBidi(r) {
			return -1
		}
		return r
	}, s)
}

// isControlOrBidi reports the characters stripControls removes.
func isControlOrBidi(r rune) bool {
	switch {
	case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f,
		r == 0x061c, r == 0x200e, r == 0x200f, r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
		return true
	}
	return false
}

// hiddenName replaces the query name while logs.hideDomains is on.
const hiddenName = "hidden"

// reasonKeptWhenHidden are the statuses whose reason names a list, a
// service, a schedule, a special-use name, an upstream or a search engine,
// never the queried domain: kept while domains are hidden.
var reasonKeptWhenHidden = []string{
	"blocked-list", "blocked-service", "blocked-schedule", "blocked-special", "blocked-upstream", statusSafeSearch,
}

// hideDomain applies logs.hideDomains to a cleaned event: the query name
// becomes "hidden", the answers are removed and the reason is kept only
// when it cannot be a domain name (reasonKeptWhenHidden, or neither "."
// nor ":" in it). The text of the upstream's EDE often names the query
// ("validation failure <example.org. A IN>"): it is removed when it
// contains "." or ":" or the query name (a single label); the code stays.
func hideDomain(e *QueryEvent) {
	name := e.QName
	e.QName = hiddenName
	e.Answer, e.UpstreamAnswer = "", ""
	if !slices.Contains(reasonKeptWhenHidden, e.Status) && strings.ContainsAny(e.Reason, ".:") {
		e.Reason = ""
	}
	if ede := e.UpstreamEDE; ede != nil && ede.Text != "" &&
		(strings.ContainsAny(ede.Text, ".:") || (name != "" && strings.Contains(strings.ToLower(ede.Text), name))) {
		c := *ede // never modify the producer's value
		c.Text = ""
		e.UpstreamEDE = &c
	}
}

// cleanECS returns the canonical masked form of a client subnet ("" if it
// does not parse); with anon, IPv4 is kept to at most /16 and IPv6 to at
// most /48 (like anonymised client addresses).
func cleanECS(s string, anon bool) string {
	p, err := netip.ParsePrefix(strings.TrimSpace(s))
	if err != nil || p.Addr().Zone() != "" {
		return ""
	}
	addr, bits := p.Addr(), p.Bits()
	if addr.Is4In6() && bits >= 96 {
		addr, bits = addr.Unmap(), bits-96
	}
	if anon {
		if addr.Is4() {
			bits = min(bits, 16)
		} else {
			bits = min(bits, 48)
		}
	}
	return netip.PrefixFrom(addr, bits).Masked().String()
}

// cleanName lower-cases a DNS/host name and strips the trailing dot.
func cleanName(s string) string {
	return clean(strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), "."), maxNameLen)
}

// cleanPath strips any query string or fragment (CDN query strings carry
// tokens and are never stored).
func cleanPath(p string) string {
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p = p[:i]
	}
	return clean(p, maxPathLen)
}

// eventTime normalises an event timestamp to UTC millisecond precision (the
// storage resolution) so live and stored events are identical. Missing or
// future timestamps (producers stamp events with the current time) become now.
func eventTime(t, now time.Time) time.Time {
	if t.IsZero() || t.After(now.Add(maxClockSkew)) {
		t = now
	}
	return time.UnixMilli(t.UnixMilli()).UTC()
}

func nonNeg(v int64) int64 { return max(v, 0) }

// spanMs bounds a duration in milliseconds to [0, maxEventSpan].
func spanMs(d int64) int64 { return min(max(d, 0), maxEventSpan.Milliseconds()) }

// cleanQuery bounds and normalises a query event before it is counted,
// stored or published; anon masks the client (logs.anonymizeClientIps),
// hide the domain names (logs.hideDomains, hideDomain).
func cleanQuery(e QueryEvent, anon, hide bool, now time.Time) QueryEvent {
	e.ID = 0
	e.Time = eventTime(e.Time, now)
	e.ClientIP = cleanClientIP(e.ClientIP, anon)
	e.ClientName = clean(e.ClientName, maxNameLen)
	if anon {
		e.ClientName = ""
	}
	e.QName = cleanName(e.QName)
	e.QType = strings.ToUpper(clean(e.QType, maxShortLen))
	e.Status = strings.ToLower(clean(e.Status, maxShortLen))
	e.RCode = strings.ToUpper(clean(e.RCode, maxShortLen))
	e.Reason = clean(e.Reason, maxTextLen)
	e.Service = clean(e.Service, maxIDLen)
	e.Upstream = clean(e.Upstream, maxTextLen)
	e.DurationUs = min(nonNeg(e.DurationUs), maxEventSpan.Microseconds())
	e.Answer = clean(e.Answer, maxTextLen)
	e.Protocol = strings.ToLower(clean(e.Protocol, maxShortLen))
	if e.UpstreamEDE != nil {
		ede := *e.UpstreamEDE // the producer's value is not modified
		ede.Code = min(max(ede.Code, 0), 65535)
		ede.Text = cleanEDEText(ede.Text)
		e.UpstreamEDE = &ede
	}
	e.ECS = cleanECS(e.ECS, anon)
	e.UpstreamAnswer = cleanUpstreamAnswer(e.UpstreamAnswer)
	e.Purpose = strings.ToLower(clean(e.Purpose, maxShortLen))
	if hide {
		hideDomain(&e)
	}
	return e
}

func cleanCache(e CacheEvent, anon bool, now time.Time) CacheEvent {
	e.ID = 0
	e.Time = eventTime(e.Time, now)
	e.ClientIP = cleanClientIP(e.ClientIP, anon)
	e.ClientName = clean(e.ClientName, maxNameLen)
	if anon {
		e.ClientName = ""
	}
	e.Service = clean(e.Service, maxIDLen)
	e.Host = cleanName(e.Host)
	e.Path = cleanPath(e.Path)
	e.Method = strings.ToUpper(clean(e.Method, maxShortLen))
	e.Status = min(max(e.Status, 0), 999)
	e.CacheStatus = strings.ToUpper(clean(e.CacheStatus, maxShortLen))
	e.Range = clean(e.Range, maxRangeLen)
	e.BytesSent = nonNeg(e.BytesSent)
	e.BytesHit = nonNeg(e.BytesHit)
	e.BytesWAN = nonNeg(e.BytesWAN)
	e.BytesStored = nonNeg(e.BytesStored)
	e.DurationMs = spanMs(e.DurationMs)
	e.GroupKey = clean(e.GroupKey, maxTextLen)
	e.Label = clean(e.Label, maxTextLen)
	e.UserAgent = clean(e.UserAgent, maxTextLen)
	return e
}

func cleanSNI(e SNIEvent, anon bool, now time.Time) SNIEvent {
	e.ID = 0
	e.Time = eventTime(e.Time, now)
	e.ClientIP = cleanClientIP(e.ClientIP, anon)
	e.ClientName = clean(e.ClientName, maxNameLen)
	if anon {
		e.ClientName = ""
	}
	e.SNI = cleanName(e.SNI)
	e.Service = clean(e.Service, maxIDLen)
	e.BytesUp = nonNeg(e.BytesUp)
	e.BytesDown = nonNeg(e.BytesDown)
	e.DurationMs = spanMs(e.DurationMs)
	return e
}

func cleanEviction(e EvictionEvent, now time.Time) EvictionEvent {
	e.ID = 0
	e.Time = eventTime(e.Time, now)
	e.StoreID = clean(e.StoreID, maxIDLen)
	e.ObjectID = clean(e.ObjectID, maxIDLen)
	e.Service = clean(e.Service, maxIDLen)
	e.GroupKey = clean(e.GroupKey, maxTextLen)
	e.Bytes = nonNeg(e.Bytes)
	e.Reason = strings.ToLower(clean(e.Reason, maxShortLen))
	return e
}
