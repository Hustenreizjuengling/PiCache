package dhcp

import (
	"net/netip"
	"sync"
)

// MaxLog is the number of exchanges the log keeps (in memory only; lost
// on restart).
const MaxLog = 200

// exchangeLog is a ring of the last MaxLog handled exchanges: DHCPv4
// packets that passed parsing and the rate limits while serving and were
// answered, nak'ed, processed or ignored by a rule; DHCPv6 information
// requests answered; router solicitations answered. Malformed,
// rate-limited, foreign-interface and relayed packets, packets while not
// serving and periodic advertisements are only counted.
type exchangeLog struct {
	mu   sync.Mutex
	ring [MaxLog]LogEntry
	n    int // entries held (≤ MaxLog)
	next int // index of the next write
}

func (l *exchangeLog) add(e LogEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ring[l.next] = e
	l.next = (l.next + 1) % MaxLog
	l.n = min(l.n+1, MaxLog)
}

// list returns at most limit entries, newest first.
func (l *exchangeLog) list(limit int) []LogEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := min(max(limit, 0), l.n)
	out := make([]LogEntry, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, l.ring[(l.next-i+MaxLog)%MaxLog])
	}
	return out
}

// logExchange records one exchange at the current time.
func (s *Service) logExchange(e LogEntry) {
	e.Time = s.now().UTC()
	s.xlog.add(e)
}

// Log returns the last limit exchanges, newest first (GET /dhcp/log).
func (s *Service) Log(limit int) []LogEntry { return s.xlog.list(limit) }

// entry4 starts the log entry of a DHCPv4 message (in: its name).
func entry4(m *message, mac, in string) LogEntry {
	return LogEntry{Kind: LogDHCPv4, MAC: mac, Hostname: m.hostName(), In: in}
}

// messageName returns the name of a client's DHCPv4 message type.
func messageName(typ byte) string {
	switch typ {
	case msgDiscover:
		return "DISCOVER"
	case msgRequest:
		return "REQUEST"
	case msgDecline:
		return "DECLINE"
	case msgRelease:
		return "RELEASE"
	case msgInform:
		return "INFORM"
	}
	return "UNKNOWN"
}

// addrString formats an address for the log ("" when invalid or 0.0.0.0).
func addrString(a netip.Addr) string {
	if !a.IsValid() || a.IsUnspecified() {
		return ""
	}
	return a.String()
}
