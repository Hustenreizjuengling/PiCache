package netutil

import (
	"net/netip"
	"sync/atomic"
	"time"

	"github.com/hustenreizjuengling/picache/internal/settings"
)

// ACL decides which client addresses may use DNS, the cache proxy, the SNI
// pass-through and (by default) the web UI. It is immutable.
type ACL struct {
	allowAll bool
	prefixes []netip.Prefix
}

// NewACL builds an ACL from the private defaults, the directly connected
// subnets and extra user prefixes.
func NewACL(extra []netip.Prefix, allowAll bool) *ACL {
	ps := append([]netip.Prefix{}, PrivateLANPrefixes...)
	ps = append(ps, LocalSubnets()...)
	ps = append(ps, extra...)
	return &ACL{allowAll: allowAll, prefixes: ps}
}

// Allowed reports whether ip may use the service.
func (a *ACL) Allowed(ip netip.Addr) bool {
	if a == nil {
		return false
	}
	if a.allowAll {
		return true
	}
	return inAny(ip, a.prefixes)
}

// ACLWatcher keeps an ACL current with settings (dns.allowedNetworks,
// dns.allowAllNetworks) and periodically refreshes connected subnets.
type ACLWatcher struct {
	cur atomic.Pointer[ACL]
	set *settings.Store
}

// NewACLWatcher creates the watcher and subscribes to settings changes.
func NewACLWatcher(set *settings.Store) *ACLWatcher {
	w := &ACLWatcher{set: set}
	w.rebuild(set.Get())
	set.Subscribe(func(_, n *settings.All) { w.rebuild(n) })
	return w
}

func (w *ACLWatcher) rebuild(s *settings.All) {
	w.cur.Store(NewACL(settings.ParsePrefixes(s.DNS.AllowedNetworks), s.DNS.AllowAllNetworks))
}

// Get returns the current ACL.
func (w *ACLWatcher) Get() *ACL { return w.cur.Load() }

// Run refreshes connected subnets every interval until done is closed
// (interfaces can change, e.g. DHCP renumbering).
func (w *ACLWatcher) Run(done <-chan struct{}, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			w.rebuild(w.set.Get())
		}
	}
}
