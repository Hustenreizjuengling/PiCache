package netutil

import (
	"slices"
	"testing"
)

// TestReadHostAddrsLinux reads the real address table through netlink and
// checks it against net.Interfaces: the same interfaces and addresses, all
// canonical.
func TestReadHostAddrsLinux(t *testing.T) {
	got := readHostAddrs()
	want := ifaceHostAddrs()
	key := func(a HostAddr) string { return a.Iface + " " + a.Prefix.String() }
	var gk, wk []string
	for _, a := range got {
		if ip := a.Prefix.Addr(); !ip.IsValid() || ip.Zone() != "" || ip.Is4In6() {
			t.Errorf("address %v is not canonical", a.Prefix)
		}
		if a.Temporary && a.Prefix.Addr().Is4() {
			t.Errorf("an IPv4 address cannot be temporary: %+v", a)
		}
		gk = append(gk, key(a))
	}
	for _, a := range want {
		wk = append(wk, key(a))
	}
	slices.Sort(gk)
	slices.Sort(wk)
	if !slices.Equal(gk, wk) {
		t.Errorf("netlink addresses %v, net.Interfaces %v", gk, wk)
	}
	if !slices.ContainsFunc(got, func(a HostAddr) bool { return a.Loopback && a.Up && a.Prefix.Addr().IsLoopback() }) {
		t.Errorf("no loopback address in %v", gk)
	}
	t.Logf("%d addresses: %v", len(got), gk)
}
