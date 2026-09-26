package dhcp

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"time"

	"github.com/hustenreizjuengling/picache/internal/dhcp/dhcpv6"
	"github.com/hustenreizjuengling/picache/internal/dhcp/ra"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// Multicast groups.
var (
	allNodes      = netip.MustParseAddr("ff02::1")
	allRouters    = netip.MustParseAddr("ff02::2")   // router solicitations are sent here
	allDHCPAgents = netip.MustParseAddr("ff02::1:2") // All_DHCP_Relay_Agents_and_Servers
)

// evaluateIPv6 decides the state of the IPv6 announcements. They need DHCP
// enabled and available, a usable interface and a stable ULA; the IPv4
// blockers (dynamic address, other server, range) do not stop them.
func (s *Service) evaluateIPv6(set *settings.All, v *view) {
	h := set.DHCP
	st := s.d.Sockets.state()
	g := v.gate
	ifaceOK := g.iface.problem == "" && g.iface.index != 0

	v.raEnabled = h.IPv6.RouterAdvertisements
	v.raAvailable, v.raReasonCode, v.raReason = raAvailability(v, st, h.Enabled && h.IPv6.RouterAdvertisements)
	switch {
	case !v.raEnabled || !h.Enabled:
		v.raState = StateOff
	case !v.available:
		v.raState = StateBlocked
	case !st.icmpOpen:
		v.raState, v.raBlockers = StateBlocked, []string{BlockerNoRawSocket}
	case !ifaceOK:
		v.raState, v.raBlockers = StateBlocked, []string{BlockerNoInterface}
	case !g.iface.ula.IsValid():
		v.raState, v.raBlockers = StateBlocked, []string{BlockerNoULA}
	default:
		v.raState = StateSending
	}

	v.v6Enabled = h.IPv6.DHCPv6
	switch {
	case !v.v6Enabled || !h.Enabled:
		v.v6State = StateOff
	case !v.available:
		v.v6State = StateBlocked
	case !st.v6Open:
		v.v6State, v.v6Blockers, v.v6Err = StateBlocked, []string{BlockerNoSocket}, st.v6Err
	case !ifaceOK:
		v.v6State, v.v6Blockers = StateBlocked, []string{BlockerNoInterface}
	case !g.iface.ula.IsValid():
		v.v6State, v.v6Blockers = StateBlocked, []string{BlockerNoULA}
	default:
		v.v6State = StateServing
	}
	if ifaceOK && g.iface.ula.IsValid() {
		v.duid = dhcpv6.DUIDLL(g.iface.mac)
		v.domainWire = ra.EncodeDomain(g.domain)
	}
	if v.raState == StateSending {
		v.adv = ra.Advertisement{MAC: g.iface.mac, DNS: g.iface.ula, Domain: g.domain, OtherConfig: v.v6State == StateServing}
	}
}

// raAvailability decides whether router advertisements can be sent in
// this process (on is the option together with dhcp.enabled): the raw
// socket is open, or it is not needed and PiCache held CAP_NET_RAW at this
// start (switching them on then needs one restart). Otherwise the reason
// code says why: DHCP itself is unavailable; dropping CAP_NET_RAW could not
// be verified; the process had no CAP_NET_RAW at this start (reported
// while the option is off too); opening the socket at start failed; or
// the socket opens at the next start (restart-required).
func raAvailability(v *view, st sockState, on bool) (bool, string, string) {
	switch {
	case !v.available:
		return false, RAReasonDHCPUnavailable, v.reason
	case st.dropUnverified != "":
		return false, RAReasonDropUnverified, st.dropUnverified
	case st.icmpOpen:
		return true, "", ""
	case !st.rawCapable:
		return false, RAReasonNoCapNetRaw, noCapNetRaw
	case !on:
		return true, "", ""
	case st.icmpStartErr != "":
		return false, RAReasonSocket, st.icmpStartErr
	}
	return false, RAReasonRestart, raRestart
}

// applyIPv6 joins and leaves the multicast groups and (re)starts or stops
// the router advertisements for a new view. A withdrawn announcement (RAs
// switched off, another interface or another address or domain) is sent
// once with lifetime 0.
func (s *Service) applyIPv6(v *view) {
	_, v6, icmp, _, _, _ := s.d.Sockets.get()
	if v6 != nil {
		want := 0
		if v.v6State == StateServing {
			want = v.gate.iface.index
		}
		s.mu.Lock()
		joined := s.v6Joined
		s.mu.Unlock()
		if joined != want {
			if joined != 0 {
				_ = v6.LeaveGroup(joined, allDHCPAgents)
			}
			var err error
			if want != 0 {
				err = v6.JoinGroup(want, allDHCPAgents)
			}
			s.mu.Lock()
			s.v6Joined = want
			if err != nil {
				s.v6Joined = 0
			}
			s.mu.Unlock()
			if err != nil {
				msg := "joining " + allDHCPAgents.String() + " failed: " + err.Error()
				s.v6SendErr.Store(&msg)
			} else if want != 0 {
				s.v6SendErr.Store(nil)
			}
		}
	}
	want := v.raState == StateSending
	s.raMu.Lock()
	defer s.raMu.Unlock()
	r := &s.ra
	if icmp == nil {
		if r.active { // the socket was closed (drop-unverified)
			r.active, r.joined = false, 0
			r.sched.Stop()
		}
		return
	}
	if r.active {
		moved := !want || r.ifIndex != v.gate.iface.index || r.adv.DNS != v.adv.DNS || r.adv.Domain != v.adv.Domain || r.adv.MAC != v.adv.MAC
		if moved {
			_ = icmp.WriteTo(r.adv.Marshal(0), r.ifIndex, allNodes) // withdraw, best effort
		}
		if !want || r.ifIndex != v.gate.iface.index {
			if r.joined != 0 {
				_ = icmp.LeaveGroup(r.joined, allRouters)
				r.joined = 0
			}
		}
		if !want {
			r.active = false
			r.sched.Stop()
			s.kickRA()
			return
		}
		if !moved && r.adv == v.adv {
			return // unchanged
		}
	}
	if !want {
		return
	}
	r.active, r.ifIndex, r.adv, r.err = true, v.gate.iface.index, v.adv, ""
	if r.joined != r.ifIndex {
		if err := icmp.JoinGroup(r.ifIndex, allRouters); err != nil {
			r.err = "joining " + allRouters.String() + " failed (solicitations are not answered): " + err.Error()
		} else {
			r.joined = r.ifIndex
		}
	}
	r.sched.Start(s.now())
	s.kickRA()
}

func (s *Service) kickRA() {
	select {
	case s.raKick <- struct{}{}:
	default:
	}
}

// raLoop sends the router advertisements when they are due; when ctx ends
// it withdraws the announcement (best effort).
func (s *Service) raLoop(ctx context.Context) {
	t := time.NewTimer(time.Hour)
	defer t.Stop()
	for {
		t.Reset(s.raTick())
		select {
		case <-ctx.Done():
			_, _, icmp, _, _, _ := s.d.Sockets.get()
			s.raMu.Lock()
			if s.ra.active && icmp != nil {
				_ = icmp.WriteTo(s.ra.adv.Marshal(0), s.ra.ifIndex, allNodes)
			}
			s.ra.active = false
			s.ra.sched.Stop()
			s.raMu.Unlock()
			return
		case <-t.C:
		case <-s.raKick:
		}
	}
}

// raTick sends the advertisement that is due (if any) and returns how long
// to wait for the next one.
func (s *Service) raTick() time.Duration {
	_, _, icmp, _, _, _ := s.d.Sockets.get()
	s.raMu.Lock()
	defer s.raMu.Unlock()
	r := &s.ra
	if now := s.now(); r.active && icmp != nil && r.sched.Due(now) {
		err := icmp.WriteTo(r.adv.Marshal(ra.Lifetime), r.ifIndex, allNodes)
		r.sched.Sent(now)
		if err != nil {
			r.err = "sending a router advertisement failed: " + err.Error()
		} else {
			r.err, r.lastSent = "", now
			r.sent++
		}
	}
	if next := r.sched.Next(); r.active && !next.IsZero() {
		return max(next.Sub(s.now()), time.Millisecond)
	}
	return time.Hour
}

// readLoopICMP reads router solicitations (answered) and other routers'
// advertisements (recorded, K8) until the socket is closed.
func (s *Service) readLoopICMP(ctx context.Context, c icmpConn) {
	buf := make([]byte, 1500)
	for {
		n, ifIndex, hops, src, err := c.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			s.logRepeated("readRS", "reading the ICMPv6 socket failed", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
			continue
		}
		if n == 0 {
			continue
		}
		switch buf[0] {
		case ra.TypeRouterSolicitation:
			s.solicitation(buf[:n], ifIndex, hops, src)
		case ra.TypeRouterAdvertisement:
			s.advertisement(buf[:n], ifIndex, hops, src)
		}
	}
}

// solicitation schedules an advertisement for a valid router solicitation
// received on the announcing interface. PiCache's own (the search for
// other routers, reflected back by some bridges) is ignored.
func (s *Service) solicitation(b []byte, ifIndex, hops int, src netip.Addr) {
	if !ra.ValidSolicitation(b, hops, src) || s.ann.isOwn(src) {
		return
	}
	s.raMu.Lock()
	scheduled := s.ra.active && ifIndex == s.ra.ifIndex && s.ra.sched.Solicit(s.now())
	if scheduled {
		s.ra.solicits++
	}
	s.raMu.Unlock()
	if scheduled {
		s.kickRA()
		e := LogEntry{Kind: LogRA, Address: src.String(), In: "RS", Out: "RA", Result: ResultAnswered}
		if mac, ok := ra.SolicitationSource(b); ok {
			e.MAC, _ = macString(mac)
		}
		s.logExchange(e)
	}
}

// readLoop6 answers DHCPv6 information requests while DHCPv6 answers and
// hands the answers to PiCache's relayed search (K8) to it, until the
// socket is closed.
func (s *Service) readLoop6(ctx context.Context, c v6Conn) {
	buf := make([]byte, dhcpv6.MaxMessage+1)
	for {
		n, ifIndex, src, dst, err := c.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			s.logRepeated("read6", "reading the DHCPv6 socket failed", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
			continue
		}
		if n > 0 && buf[0] == dhcpv6.MsgRelayReply {
			s.relayReply(buf[:n], ifIndex, src)
			continue
		}
		if reply := s.handle6(buf[:n], ifIndex, src, dst); reply != nil {
			if err := c.WriteTo(reply, ifIndex, src); err != nil {
				msg := "sending a DHCPv6 reply failed: " + err.Error()
				s.v6SendErr.Store(&msg)
			} else {
				s.v6Replies.Add(1)
				s.v6SendErr.Store(nil)
			}
		}
	}
}

// handle6 returns the REPLY to an information request that arrived on the
// served interface from a link-local client address (to ff02::1:2 or a
// link-local address of this machine), or nil. At most 50 packets per
// second are handled.
func (s *Service) handle6(b []byte, ifIndex int, src netip.AddrPort, dst netip.Addr) (reply []byte) {
	defer func() {
		if r := recover(); r != nil {
			reply = nil
		}
	}()
	v := s.view.Load()
	if v.v6State != StateServing || ifIndex != v.gate.iface.index || len(b) > dhcpv6.MaxMessage {
		return nil
	}
	if !src.Addr().IsLinkLocalUnicast() || src.Port() == 0 || (dst != allDHCPAgents && !dst.IsLinkLocalUnicast()) {
		return nil
	}
	if !s.limit6.allow([6]byte{}, s.now()) {
		return nil
	}
	req, err := dhcpv6.Parse(b, v.duid)
	if err != nil {
		s.v6Ignored.Add(1) // other message types (no address leases) and malformed packets
		return nil
	}
	s.logExchange(LogEntry{Kind: LogDHCPv6, DUID: hexColon(req.ClientID), Address: src.Addr().WithZone("").String(),
		In: "INFORMATION-REQUEST", Out: "REPLY", Result: ResultAnswered})
	return dhcpv6.Reply(req, v.duid, []netip.Addr{v.gate.iface.ula}, v.domainWire)
}
