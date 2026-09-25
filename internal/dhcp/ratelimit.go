package dhcp

import (
	"sync"
	"time"
)

// Rate limits (docs/ARCHITECTURE.md 18).
const (
	limitTotal  = 50 // DHCPv4 packets per second, all clients
	limitPerMAC = 5  // DHCPv4 packets per second and client MAC
	limitV6     = 50 // DHCPv6 packets per second
)

// windowLimiter admits at most total events per second, and at most
// perKey per key (0: no per-key limit), in fixed one-second windows.
// Denied events do not count. The per-key map holds at most total keys.
type windowLimiter struct {
	mu     sync.Mutex
	total  int
	perKey int
	sec    int64
	n      int
	keys   map[[6]byte]int
}

func newWindowLimiter(total, perKey int) *windowLimiter {
	return &windowLimiter{total: total, perKey: perKey, keys: map[[6]byte]int{}}
}

// allow reports whether one more event of key is admitted at now.
func (l *windowLimiter) allow(key [6]byte, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if sec := now.Unix(); sec != l.sec {
		l.sec, l.n = sec, 0
		clear(l.keys)
	}
	if l.n >= l.total || (l.perKey > 0 && l.keys[key] >= l.perKey) {
		return false
	}
	l.n++
	if l.perKey > 0 {
		l.keys[key]++
	}
	return true
}
