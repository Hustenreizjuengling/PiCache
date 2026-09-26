package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/db"
	dnsserver "github.com/hustenreizjuengling/picache/internal/dns/server"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// ownIPv4 returns a non-loopback IPv4 address of an interface that is up.
func ownIPv4(t *testing.T) netip.Addr {
	t.Helper()
	ifs, err := net.Interfaces()
	if err != nil {
		t.Skip(err)
	}
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if pn, ok := a.(*net.IPNet); ok {
				if ip, ok := netip.AddrFromSlice(pn.IP); ok && ip.Unmap().Is4() && !ip.IsLinkLocalUnicast() {
					return ip.Unmap()
				}
			}
		}
	}
	t.Skip("no non-loopback IPv4 address")
	return netip.Addr{}
}

// With plain DNS switched off and DoT serving, the health probe of
// `picache healthcheck` (the Docker HEALTHCHECK) still passes against a DNS
// listener bound to one of this machine's LAN addresses, and this machine
// is exempt from the plain-DNS gate: a query from that address for a name
// that is neither the health probe nor a bootstrap name (localhost) is
// answered, while the same query from another address would be refused.
func TestDNSCheckWithPlainDNSOff(t *testing.T) {
	t.Setenv("PICACHE_ENV_FILE", "")
	addr := ownIPv4(t)
	ctx, cancel := context.WithCancel(context.Background())
	d, err := db.Open(filepath.Join(t.TempDir(), "picache.db"), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	set, err := settings.Open(ctx, d, log)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := set.Update(ctx, func(a *settings.All) error {
		a.DNS.Encrypted = settings.EncryptedDNS{DoT: true, ServerName: "dns.lan"}
		a.DNS.PlainDNS = false
		a.DNS.RouterResolver = ""
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	st := &dnsserver.EncryptedState{DoT: true, DoTPorts: []uint16{853}}
	srv, err := dnsserver.New(ctx, dnsserver.Deps{DB: d, Settings: set, Log: log,
		Encrypted: func() *dnsserver.EncryptedState { return st }})
	if err != nil {
		t.Fatal(err)
	}
	pc, err := net.ListenPacket("udp", netip.AddrPortFrom(addr, 0).String())
	if err != nil {
		t.Skip(err)
	}
	ln, err := net.Listen("tcp", pc.LocalAddr().String())
	if err != nil {
		t.Skip(err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, []net.PacketConn{pc}, []net.Listener{ln}, nil) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	}()
	t.Setenv("PICACHE_DNS_LISTEN", pc.LocalAddr().String())
	if err := dnsCheck(); err != nil {
		t.Fatalf("dnsCheck: %v", err)
	}
	// Other queries from this machine are answered too: not REFUSED, no EDE
	// 18 (the gate would refuse localhost, which is answered only at step 6).
	m := new(dns.Msg).SetQuestion("localhost.", dns.TypeA)
	m.SetEdns0(1232, false)
	c := &dns.Client{Timeout: 2 * time.Second}
	r, _, err := c.Exchange(m, pc.LocalAddr().String())
	if err != nil || r.Rcode != dns.RcodeSuccess || len(r.Answer) != 1 {
		t.Fatalf("own query: %v %v", r, err)
	}
	if opt := r.IsEdns0(); opt != nil {
		for _, o := range opt.Option {
			if e, ok := o.(*dns.EDNS0_EDE); ok && e.InfoCode == dns.ExtendedErrorCodeProhibited {
				t.Fatalf("own query: EDE %+v", e)
			}
		}
	}
	// The gate is closed for other addresses (the lookup notes it for them,
	// not for this machine's address).
	const note = "plain DNS is closed"
	for _, tc := range []struct {
		client string
		noted  bool
	}{{"192.0.2.10", true}, {addr.String(), false}} {
		res, err := srv.Lookup(ctx, dnsserver.LookupRequest{Name: "localhost", ClientIP: tc.client}, netip.IPv6Loopback())
		if err != nil {
			t.Fatal(err)
		}
		if got := slices.ContainsFunc(res.Steps, func(s string) bool { return strings.Contains(s, note) }); got != tc.noted {
			t.Fatalf("lookup from %s: gate noted %v, want %v (%q)", tc.client, got, tc.noted, res.Steps)
		}
	}
}
