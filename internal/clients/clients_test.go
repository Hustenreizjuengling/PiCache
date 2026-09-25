package clients

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// newTestRegistry opens a registry on temp databases (withLogs: also logs.db).
func newTestRegistry(t *testing.T, withLogs bool) *Registry {
	t.Helper()
	dir := t.TempDir()
	cdb, err := db.Open(filepath.Join(dir, "picache.db"), 2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cdb.Close() })
	var ldb *db.DB
	if withLogs {
		if ldb, err = db.Open(filepath.Join(dir, "logs.db"), 2); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { ldb.Close() })
	}
	r, err := New(context.Background(), cdb, ldb, quiet())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func mustGroup(t *testing.T, r *Registry, name string, enabled bool) Group {
	t.Helper()
	g, err := r.CreateGroup(context.Background(), GroupInput{Name: name, Enabled: enabled})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func mustClient(t *testing.T, r *Registry, name string, groups []int64, ids ...string) Client {
	t.Helper()
	c, err := r.CreateClient(context.Background(), ClientInput{Name: name, Identifiers: ids, GroupIDs: groups})
	if err != nil {
		t.Fatalf("create client %s: %v", name, err)
	}
	return c
}

func ip(s string) netip.Addr { return netip.MustParseAddr(s) }

func TestIdentifyPrecedence(t *testing.T) {
	r := newTestRegistry(t, false)
	ctx := context.Background()
	kids := mustGroup(t, r, "Kids", true)
	servers := mustGroup(t, r, "Servers", true)
	off := mustGroup(t, r, "Off", false)
	mustClient(t, r, "lan", []int64{kids.ID}, "192.168.1.0/24")
	mustClient(t, r, "lower half", []int64{servers.ID}, "192.168.1.0/25")
	exact := mustClient(t, r, "laptop", []int64{kids.ID, off.ID}, "192.168.1.5")
	mustClient(t, r, "tv", []int64{servers.ID}, "AA-BB-CC-DD-EE-FF")
	arp := map[netip.Addr]string{ip("192.168.2.9"): "aa:bb:cc:dd:ee:ff", ip("192.168.1.5"): "11:22:33:44:55:66"}
	r.arp.Store(&arp)
	r.invalidate()

	tests := []struct {
		ip     string
		client string
		groups []int64
	}{
		{"192.168.1.5", "laptop", []int64{kids.ID}}, // exact IP; disabled group removed
		{"::ffff:192.168.1.5", "laptop", []int64{kids.ID}},
		{"192.168.1.6", "lower half", []int64{servers.ID}}, // longest prefix
		{"192.168.1.200", "lan", []int64{kids.ID}},
		{"192.168.2.9", "tv", []int64{servers.ID}}, // MAC from ARP
		{"10.9.9.9", "", []int64{DefaultGroupID}},  // unknown → Default
	}
	for _, tc := range tests {
		id := r.Identify(ip(tc.ip))
		if id.Name != tc.client || !slices.Equal(id.GroupIDs, tc.groups) {
			t.Errorf("Identify(%s) = %q %v, want %q %v", tc.ip, id.Name, id.GroupIDs, tc.client, tc.groups)
		}
	}
	if id := r.Identify(ip("192.168.1.5")); id.MAC != "11:22:33:44:55:66" || id.ClientID != exact.ID {
		t.Errorf("identity %+v", id)
	}

	// Changes invalidate the cache: the exact identifier moves elsewhere.
	if _, err := r.UpdateClient(ctx, exact.ID, ClientInput{Name: "laptop", Identifiers: []string{"192.168.3.5"}, GroupIDs: []int64{kids.ID}}); err != nil {
		t.Fatal(err)
	}
	if id := r.Identify(ip("192.168.1.5")); id.Name != "lower half" {
		t.Errorf("after update: %q", id.Name)
	}
	// Disabling the Default group leaves unknown clients without groups.
	if _, err := r.UpdateGroup(ctx, DefaultGroupID, GroupInput{Name: "Default", Enabled: false}); err != nil {
		t.Fatal(err)
	}
	if id := r.Identify(ip("10.9.9.9")); len(id.GroupIDs) != 0 || id.GroupIDs == nil {
		t.Errorf("unknown client with disabled Default: %v", id.GroupIDs)
	}
}

func TestSnapshotCIDRTieHighestID(t *testing.T) {
	s := newSnapshot([]Group{{ID: 1, Enabled: true}}, []Client{
		{ID: 3, Name: "older", Identifiers: []string{"10.0.0.0/8"}, GroupIDs: []int64{1}},
		{ID: 7, Name: "newer", Identifiers: []string{"10.0.0.0/8"}, GroupIDs: []int64{1}},
		{ID: 5, Name: "narrow", Identifiers: []string{"10.1.0.0/16"}, GroupIDs: []int64{1}},
	})
	if c := s.match(ip("10.2.0.1"), ""); c == nil || c.name != "newer" {
		t.Errorf("tie must pick the highest ID, got %+v", c)
	}
	if c := s.match(ip("10.1.0.1"), ""); c == nil || c.name != "narrow" {
		t.Errorf("longest prefix must win, got %+v", c)
	}
}

func TestGroups(t *testing.T) {
	r := newTestRegistry(t, false)
	ctx := context.Background()
	gs, err := r.Groups(ctx)
	if err != nil || len(gs) != 1 || gs[0].ID != DefaultGroupID || gs[0].Name != "Default" || !gs[0].Enabled {
		t.Fatalf("default group: %+v %v", gs, err)
	}
	if err := r.DeleteGroup(ctx, DefaultGroupID); apperr.KindOf(err) != apperr.KindForbidden {
		t.Errorf("deleting Default: %v", err)
	}
	g := mustGroup(t, r, "Kids", true)
	if _, err := r.CreateGroup(ctx, GroupInput{Name: " kids "}); apperr.KindOf(err) != apperr.KindConflict {
		t.Errorf("duplicate name: %v", err)
	}
	if _, err := r.CreateGroup(ctx, GroupInput{Name: "  "}); apperr.KindOf(err) != apperr.KindInvalid {
		t.Errorf("empty name: %v", err)
	}
	if _, err := r.CreateGroup(ctx, GroupInput{Name: "bad\nname"}); apperr.KindOf(err) != apperr.KindInvalid {
		t.Errorf("control characters: %v", err)
	}
	if _, err := r.UpdateGroup(ctx, 999, GroupInput{Name: "x"}); apperr.KindOf(err) != apperr.KindNotFound {
		t.Errorf("update missing: %v", err)
	}
	mustClient(t, r, "only kids", []int64{g.ID}, "192.168.1.7")
	mustClient(t, r, "both", []int64{g.ID, DefaultGroupID}, "192.168.1.8")
	if gs, _ := r.Groups(ctx); gs[1].ClientCount != 2 {
		t.Errorf("client count %+v", gs)
	}
	if err := r.DeleteGroup(ctx, g.ID); err != nil {
		t.Fatal(err)
	}
	cl, _ := r.Clients(ctx)
	for _, c := range cl {
		if !slices.Equal(c.GroupIDs, []int64{DefaultGroupID}) {
			t.Errorf("client %d groups %v after group delete, want Default", c.ID, c.GroupIDs)
		}
	}
	if err := r.DeleteGroup(ctx, g.ID); apperr.KindOf(err) != apperr.KindNotFound {
		t.Errorf("delete twice: %v", err)
	}
}

func TestClientValidationAndNormalisation(t *testing.T) {
	r := newTestRegistry(t, false)
	ctx := context.Background()
	var changes atomic.Int32
	r.OnChange(func() { changes.Add(1) })

	c, err := r.CreateClient(ctx, ClientInput{Name: " TV ", Identifiers: []string{"AA-BB-CC-DD-EE-FF", "192.168.1.77/24", "fd00::1%eth0", "192.168.1.9/32", "192.168.1.9"}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"aa:bb:cc:dd:ee:ff", "192.168.1.0/24", "fd00::1", "192.168.1.9"}
	if c.Name != "TV" || !slices.Equal(c.Identifiers, want) || !slices.Equal(c.GroupIDs, []int64{DefaultGroupID}) {
		t.Errorf("not normalised: %+v", c)
	}
	if changes.Load() != 1 {
		t.Errorf("OnChange called %d times", changes.Load())
	}

	tests := []struct {
		name string
		in   ClientInput
		kind apperr.Kind
	}{
		{"no identifiers", ClientInput{Name: "x"}, apperr.KindInvalid},
		{"no name", ClientInput{Identifiers: []string{"10.0.0.1"}}, apperr.KindInvalid},
		{"hostname identifier", ClientInput{Name: "x", Identifiers: []string{"laptop.lan"}}, apperr.KindInvalid},
		{"zero MAC", ClientInput{Name: "x", Identifiers: []string{"00:00:00:00:00:00"}}, apperr.KindInvalid},
		{"EUI-64", ClientInput{Name: "x", Identifiers: []string{"02-00-00-00-fe-80-00-01"}}, apperr.KindInvalid},
		{"unknown group", ClientInput{Name: "x", Identifiers: []string{"10.0.0.1"}, GroupIDs: []int64{42}}, apperr.KindInvalid},
		{"identifier in use", ClientInput{Name: "x", Identifiers: []string{"aa:bb:cc:dd:ee:ff"}}, apperr.KindConflict},
		{"too many identifiers", ClientInput{Name: "x", Identifiers: manyIPs(33)}, apperr.KindInvalid},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := r.CreateClient(ctx, tc.in); apperr.KindOf(err) != tc.kind {
				t.Errorf("kind %v, want %v (%v)", apperr.KindOf(err), tc.kind, err)
			}
		})
	}
	// Re-saving a client with its own identifiers is not a conflict.
	if _, err := r.UpdateClient(ctx, c.ID, ClientInput{Name: "TV", Identifiers: c.Identifiers, IgnoreLogs: true}); err != nil {
		t.Fatal(err)
	}
	if id := r.Identify(ip("192.168.1.9")); !id.IgnoreLogs || id.ClientID != c.ID {
		t.Errorf("identity %+v", id)
	}
	if err := r.DeleteClient(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if id := r.Identify(ip("192.168.1.9")); id.ClientID != 0 {
		t.Errorf("deleted client still identified: %+v", id)
	}
	if err := r.DeleteClient(ctx, c.ID); apperr.KindOf(err) != apperr.KindNotFound {
		t.Errorf("delete twice: %v", err)
	}
}

func manyIPs(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = netip.AddrFrom4([4]byte{10, 0, byte(i / 256), byte(i % 256)}).String()
	}
	return out
}

func TestParseIdentifier(t *testing.T) {
	tests := []struct {
		in, kind, value string
	}{
		{"192.168.1.5", kindIP, "192.168.1.5"},
		{"::ffff:192.168.1.5", kindIP, "192.168.1.5"},
		{"fe80::1%eth0", kindIP, "fe80::1"},
		{"10.1.2.3/8", kindCIDR, "10.0.0.0/8"},
		{"::ffff:10.0.0.0/104", kindCIDR, "10.0.0.0/8"},
		{"2001:db8::1/64", kindCIDR, "2001:db8::/64"},
		{"10.1.2.3/32", kindIP, "10.1.2.3"},
		{"aabb.ccdd.eeff", kindMAC, "aa:bb:cc:dd:ee:ff"},
		{"AA:BB:CC:DD:EE:FF", kindMAC, "aa:bb:cc:dd:ee:ff"},
	}
	for _, tc := range tests {
		id, ok := parseIdentifier(tc.in)
		if !ok || id.kind != tc.kind || id.value != tc.value {
			t.Errorf("parseIdentifier(%q) = %+v %v", tc.in, id, ok)
		}
	}
	for _, bad := range []string{"", "laptop", "10.0.0.0/33", "fe80::/64%eth0", strings.Repeat("1", 65)} {
		if _, ok := parseIdentifier(bad); ok {
			t.Errorf("parseIdentifier(%q) must fail", bad)
		}
	}
}

func TestSeenAndKnownMemoryOnly(t *testing.T) {
	r := newTestRegistry(t, false)
	mustClient(t, r, "laptop", nil, "192.168.1.5")
	r.Seen(ip("192.168.1.5"))
	r.Seen(ip("::ffff:192.168.1.5"))
	r.Seen(ip("192.168.1.6"))
	known, err := r.Known(context.Background(), 0)
	if err != nil || len(known) != 2 {
		t.Fatalf("known %+v %v", known, err)
	}
	byIP := map[string]Known{}
	for _, k := range known {
		byIP[k.IP] = k
	}
	if k := byIP["192.168.1.5"]; k.Queries != 2 || k.Name != "laptop" || k.ClientID == 0 {
		t.Errorf("known %+v", k)
	}
	// Entries older than the window are not listed; pruning removes old ones.
	r.seenMu.Lock()
	e, _ := r.seen.peek(ip("192.168.1.6"))
	e.last = time.Now().Add(-40 * 24 * time.Hour)
	e.pending = 0
	r.seenMu.Unlock()
	if known, _ := r.Known(context.Background(), time.Hour); len(known) != 1 {
		t.Errorf("window: %+v", known)
	}
	r.prune(context.Background())
	if r.seen.len() != 1 {
		t.Errorf("prune kept %d entries", r.seen.len())
	}
}

func TestSeenFlushToLogsDB(t *testing.T) {
	r := newTestRegistry(t, true)
	ctx := context.Background()
	arp := map[netip.Addr]string{ip("192.168.1.5"): "aa:bb:cc:dd:ee:01"}
	r.arp.Store(&arp)
	for range 3 {
		r.Seen(ip("192.168.1.5"))
	}
	r.flush(ctx)
	r.Seen(ip("192.168.1.5"))
	known, err := r.Known(ctx, 0)
	if err != nil || len(known) != 1 || known[0].Queries != 4 || known[0].MAC != "aa:bb:cc:dd:ee:01" {
		t.Fatalf("known %+v %v", known, err)
	}
	r.flush(ctx)
	var q int64
	var mac string
	if err := r.ldb.R.QueryRowContext(ctx, `SELECT queries, mac FROM clients_seen WHERE ip = ?`, "192.168.1.5").Scan(&q, &mac); err != nil || q != 4 || mac != "aa:bb:cc:dd:ee:01" {
		t.Fatalf("stored %d %q %v", q, mac, err)
	}
	// A fresh registry (restart) still knows the client from logs.db.
	r2, err := New(ctx, r.db, r.ldb, quiet())
	if err != nil {
		t.Fatal(err)
	}
	if known, _ := r2.Known(ctx, 0); len(known) != 1 || known[0].Queries != 4 {
		t.Errorf("after restart %+v", known)
	}
	if _, err := r.ldb.W.ExecContext(ctx, `UPDATE clients_seen SET last_seen = ?`, db.Ms(time.Now().Add(-31*24*time.Hour))); err != nil {
		t.Fatal(err)
	}
	r2.prune(ctx)
	if known, _ := r2.Known(ctx, 0); len(known) != 0 {
		t.Errorf("pruned rows still listed: %+v", known)
	}
}

func TestHostnameWorker(t *testing.T) {
	r := newTestRegistry(t, false)
	var calls atomic.Int32
	r.SetPTRResolver(func(_ context.Context, a netip.Addr) (string, error) {
		calls.Add(1)
		switch a {
		case ip("192.168.1.20"):
			return "Laptop.lan.", nil
		case ip("192.168.1.21"):
			return "<script>", nil
		}
		return "", errors.New("no PTR")
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.ptrWorker(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	r.Seen(ip("192.168.1.20"))
	r.Seen(ip("192.168.1.20")) // de-duplicated: only new addresses are queued
	r.Seen(ip("192.168.1.21"))
	r.Seen(ip("127.0.0.1")) // loopback is never resolved
	deadline := time.Now().Add(3 * time.Second)
	for r.DisplayName(ip("192.168.1.20")) != "laptop.lan" {
		if time.Now().After(deadline) {
			t.Fatal("hostname not resolved")
		}
		time.Sleep(5 * time.Millisecond)
	}
	for calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if calls.Load() != 2 {
		t.Errorf("resolver called %d times, want 2", calls.Load())
	}
	if n := r.DisplayName(ip("192.168.1.21")); n != "" {
		t.Errorf("unsafe PTR names are dropped, got %q", n)
	}
	// Configured names win over PTR names.
	mustClient(t, r, "My Laptop", nil, "192.168.1.20")
	if n := r.DisplayName(ip("192.168.1.20")); n != "My Laptop" {
		t.Errorf("display name %q", n)
	}
}

func TestPTRQueueBounded(t *testing.T) {
	r := newTestRegistry(t, false)
	for i := range maxPTRQueue + 100 {
		r.enqueueName(netip.AddrFrom4([4]byte{10, 1, byte(i / 256), byte(i % 256)}))
	}
	if len(r.queue) != maxPTRQueue || len(r.queued) != maxPTRQueue {
		t.Errorf("queue %d, queued %d", len(r.queue), len(r.queued))
	}
}

func TestParseARP(t *testing.T) {
	const table = `IP address       HW type     Flags       HW address            Mask     Device
192.168.1.1      0x1         0x2         AA:BB:CC:DD:EE:01     *        eth0
192.168.1.50     0x1         0x0         00:00:00:00:00:00     *        eth0
192.168.1.51     0x1         0x2         00:00:00:00:00:00     *        eth0
192.168.1.52     0x1         0x6         aa:bb:cc:dd:ee:02     *        eth0
garbage
`
	got := parseARP(strings.NewReader(table))
	want := map[netip.Addr]string{ip("192.168.1.1"): "aa:bb:cc:dd:ee:01", ip("192.168.1.52"): "aa:bb:cc:dd:ee:02"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

func TestRefreshARPInvalidatesChangedEntries(t *testing.T) {
	r := newTestRegistry(t, false)
	mustClient(t, r, "tv", nil, "aa:bb:cc:dd:ee:ff")
	if id := r.Identify(ip("192.168.1.30")); id.ClientID != 0 {
		t.Fatalf("no ARP entry yet: %+v", id)
	}
	r.readARP = func() map[netip.Addr]string { return map[netip.Addr]string{ip("192.168.1.30"): "aa:bb:cc:dd:ee:ff"} }
	r.refreshARP()
	if id := r.Identify(ip("192.168.1.30")); id.Name != "tv" {
		t.Errorf("MAC match after ARP change: %+v", id)
	}
}

func TestLRU(t *testing.T) {
	c := newLRU[int, string](3)
	c.put(1, "a")
	c.put(2, "b")
	c.put(3, "c")
	c.get(1) // 1 becomes most recent; 2 is now the oldest
	if !c.put(4, "d") {
		t.Error("new key must report insertion")
	}
	if _, ok := c.peek(2); ok {
		t.Error("least recently used entry must be evicted")
	}
	if c.put(4, "e") {
		t.Error("replacing a key is not an insertion")
	}
	var order []int
	c.each(func(k int, _ string) bool { order = append(order, k); return true })
	if !slices.Equal(order, []int{4, 1, 3}) || c.len() != 3 {
		t.Errorf("order %v", order)
	}
	c.delete(1)
	c.clear()
	if c.len() != 0 {
		t.Error("clear")
	}
}

// nlNeigh encodes one RTM_NEWNEIGH message (host byte order) as the kernel
// sends it in a neighbour dump.
func nlNeigh(typ uint16, state uint16, dst netip.Addr, lladdr []byte) []byte {
	attr := func(t uint16, v []byte) []byte {
		b := make([]byte, align4(4+len(v)))
		binary.NativeEndian.PutUint16(b[0:2], uint16(4+len(v)))
		binary.NativeEndian.PutUint16(b[2:4], t)
		copy(b[4:], v)
		return b
	}
	body := make([]byte, ndMsgLen)
	if dst.Is4() {
		body[0] = 2 // AF_INET
	} else {
		body[0] = 10 // AF_INET6
	}
	binary.NativeEndian.PutUint32(body[4:8], 2) // ifindex
	binary.NativeEndian.PutUint16(body[8:10], state)
	if dst.IsValid() {
		body = append(body, attr(ndaDst, dst.AsSlice())...)
	}
	if lladdr != nil {
		body = append(body, attr(ndaLLAddr, lladdr)...)
	}
	msg := make([]byte, nlmsgHdrLen, nlmsgHdrLen+len(body))
	binary.NativeEndian.PutUint32(msg[0:4], uint32(nlmsgHdrLen+len(body)))
	binary.NativeEndian.PutUint16(msg[4:6], typ)
	return append(msg, body...)
}

func TestParseNeighDump(t *testing.T) {
	mac1 := []byte{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0x01}
	mac2 := []byte{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0x02}
	var dump []byte
	dump = append(dump, nlNeigh(rtmNewNeigh, 0x02, ip("192.168.1.30"), mac1)...)              // REACHABLE
	dump = append(dump, nlNeigh(rtmNewNeigh, 0x04, ip("fd00::1234:5678"), mac2)...)           // STALE
	dump = append(dump, nlNeigh(rtmNewNeigh, 0x04, ip("fe80::a8bb:ccff:fedd:ee02"), mac2)...) // link-local
	dump = append(dump, nlNeigh(rtmNewNeigh, nudIncomplete, ip("fd00::99"), nil)...)
	dump = append(dump, nlNeigh(rtmNewNeigh, nudFailed, ip("fd00::98"), mac1)...)
	dump = append(dump, nlNeigh(rtmNewNeigh, 0x02, ip("fd00::97"), make([]byte, 6))...)                // zero MAC
	dump = append(dump, nlNeigh(rtmNewNeigh, 0x02, ip("fd00::96"), []byte{1, 2, 3, 4, 5, 6, 7, 8})...) // not Ethernet
	dump = append(dump, nlNeigh(rtmNewNeigh, nudNoARP, ip("ff02::1"), mac1)...)
	dump = append(dump, nlNeigh(nlmsgDone, 0, netip.Addr{}, nil)...)
	dump = append(dump, nlNeigh(rtmNewNeigh, 0x02, ip("fd00::95"), mac1)...) // after DONE: ignored
	got := parseNeighDump(dump)
	want := map[netip.Addr]string{
		ip("192.168.1.30"):              "aa:bb:cc:dd:ee:01",
		ip("fd00::1234:5678"):           "aa:bb:cc:dd:ee:02",
		ip("fe80::a8bb:ccff:fedd:ee02"): "aa:bb:cc:dd:ee:02",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	// Truncated or garbage input never panics.
	for i := range dump {
		parseNeighDump(dump[:i])
	}
	merged := map[netip.Addr]string{ip("192.168.1.31"): "aa:bb:cc:dd:ee:03"}
	mergeNeighbours(merged, got)
	if len(merged) != 4 {
		t.Errorf("merged %v", merged)
	}
}

// A device configured by MAC is identified when it queries over IPv6 too.
func TestIdentifyByMACOverIPv6(t *testing.T) {
	r := newTestRegistry(t, false)
	kids := mustGroup(t, r, "Kids", true)
	mustClient(t, r, "tablet", []int64{kids.ID}, "aa:bb:cc:dd:ee:02")
	dump := append(nlNeigh(rtmNewNeigh, 0x02, ip("192.168.1.30"), []byte{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0x02}),
		nlNeigh(rtmNewNeigh, 0x04, ip("fd00::1234:5678"), []byte{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0x02})...)
	r.readARP = func() map[netip.Addr]string { return parseNeighDump(dump) }
	r.refreshARP()
	for _, a := range []string{"192.168.1.30", "fd00::1234:5678"} {
		if id := r.Identify(ip(a)); id.Name != "tablet" || !slices.Equal(id.GroupIDs, []int64{kids.ID}) {
			t.Errorf("Identify(%s) = %q %v, want the MAC client in Kids", a, id.Name, id.GroupIDs)
		}
	}
}

// SeenTransient keeps activity in memory only: nothing reaches logs.db.
func TestSeenTransientNotPersisted(t *testing.T) {
	r := newTestRegistry(t, true)
	ctx := context.Background()
	arp := map[netip.Addr]string{ip("192.168.1.5"): "aa:bb:cc:dd:ee:01"}
	r.arp.Store(&arp)
	r.SeenTransient(ip("192.168.1.5"))
	r.SeenTransient(ip("192.168.1.5"))
	r.flush(ctx)
	var n int
	if err := r.ldb.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM clients_seen`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("transient activity written to logs.db: %d rows (%v)", n, err)
	}
	known, err := r.Known(ctx, 0)
	if err != nil || len(known) != 1 || known[0].Queries != 2 {
		t.Fatalf("known %+v %v", known, err)
	}
	// Persisted activity, then anonymisation: the unwritten part is dropped.
	r.Seen(ip("192.168.1.6"))
	r.SeenTransient(ip("192.168.1.6"))
	r.flush(ctx)
	if err := r.ldb.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM clients_seen`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("activity recorded before anonymisation but not yet written must be dropped: %d rows (%v)", n, err)
	}
	r.Seen(ip("192.168.1.6"))
	r.flush(ctx)
	if err := r.ldb.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM clients_seen`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("Seen must persist again: %d rows (%v)", n, err)
	}
}

// Clients of 0.1.x (clients schema v1) keep the bypass flag in a column of
// the old name. Clients v2 renames it once and keeps the values; the rename
// also runs on a restored older backup at the start that applies it.
func TestMigrateBypassColumn(t *testing.T) {
	const oldColumn = "lancache_bypass" // column name of 0.1.x
	ctx := context.Background()
	cdb, err := db.Open(filepath.Join(t.TempDir(), "picache.db"), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer cdb.Close()
	if err := cdb.Migrate(ctx, "clients", migrations[:1]); err != nil {
		t.Fatal(err)
	}
	if _, err := cdb.W.ExecContext(ctx, `INSERT INTO client_clients (name, comment, `+oldColumn+`, ignore_logs, created_at, updated_at)
		VALUES ('Console', '', 1, 0, 1, 1), ('Laptop', '', 0, 1, 1, 1)`); err != nil {
		t.Fatal(err)
	}
	for range 2 { // the second start finds the renamed column
		r, err := New(ctx, cdb, nil, quiet())
		if err != nil {
			t.Fatal(err)
		}
		list, err := r.Clients(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(list) != 2 || list[0].Name != "Console" || !list[0].DownloadCacheBypass || list[0].IgnoreLogs ||
			list[1].DownloadCacheBypass || !list[1].IgnoreLogs {
			t.Fatalf("clients after the migration: %+v", list)
		}
		var cols []string
		rows, err := cdb.R.QueryContext(ctx, `SELECT name FROM pragma_table_info('client_clients')`)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var c string
			if err := rows.Scan(&c); err != nil {
				t.Fatal(err)
			}
			cols = append(cols, c)
		}
		rows.Close()
		if !slices.Contains(cols, "download_cache_bypass") || slices.Contains(cols, oldColumn) {
			t.Fatalf("columns %v", cols)
		}
	}
}
