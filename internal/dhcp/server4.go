package dhcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"time"
)

// Conflict check before an address without a lease is offered: at most 4
// addresses per DISCOVER, the neighbour table read after these pauses
// (about 50 ms in total; a device on the LAN answers ARP within a
// millisecond).
var conflictWaits = []time.Duration{5 * time.Millisecond, 10 * time.Millisecond, 15 * time.Millisecond, 20 * time.Millisecond}

const maxConflictChecks = 4

// readLoop4 reads the DHCPv4 socket until it is closed.
func (s *Service) readLoop4(ctx context.Context, c v4Conn) {
	buf := make([]byte, maxPacket+1) // a longer datagram fills it and is dropped
	for {
		n, ifIndex, src, err := c.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			s.logRepeated("read4", "reading the DHCP socket failed", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
			continue
		}
		s.handle4(buf[:n], ifIndex, src)
	}
}

// handle4 handles one DHCPv4 datagram. A failure while handling it drops
// the packet and is logged; it never ends the loop.
func (s *Service) handle4(b []byte, ifIndex int, src netip.AddrPort) {
	defer func() {
		if r := recover(); r != nil {
			s.c.dropped.Add(1)
			s.log.Error("DHCP packet dropped after an internal error", slog.Any("panic", r))
		}
	}()
	s.c.received.Add(1)
	now := s.now()
	if len(b) < minPacket || len(b) > maxPacket {
		s.c.dropped.Add(1)
		return
	}
	var key [6]byte
	copy(key[:], b[chaddrOff:chaddrOff+6])
	if !s.limit4.allow(key, now) {
		s.c.dropped.Add(1)
		return
	}
	m, err := parseMessage(b)
	if err != nil {
		s.c.dropped.Add(1)
		return
	}
	if m.op == opReply {
		s.probeReply(m, ifIndex, src)
		return
	}
	if m.op != opRequest || m.htype != hwEther || m.hlen != hwEtherLen {
		s.c.dropped.Add(1)
		return
	}
	if !m.giaddr.IsUnspecified() {
		return // relayed requests (our own probe included) are never answered
	}
	mac, ok := macString(m.mac())
	typ := m.msgType()
	if !ok || typ == 0 {
		s.c.dropped.Add(1)
		return
	}
	v := s.view.Load()
	iface := v.gate.iface
	if iface.problem != "" || iface.index == 0 || ifIndex != iface.index {
		return // another interface
	}
	if from := src.Addr(); !from.IsUnspecified() && !iface.self.Masked().Contains(from) {
		s.c.dropped.Add(1) // clients send from 0.0.0.0 or their address in the subnet
		return
	}
	self := iface.self.Addr()
	sid := m.addrOpt(optServerID)
	forOther := sid.IsValid() && sid != self && !sid.IsUnspecified()
	if typ == msgRequest && forOther && v.enabled {
		// A client chose another server's offer: passive detection (while
		// DHCP is switched on).
		s.recordOther(otherServer{iface: iface.name, address: sid, serverID: sid, source: SourceRequest, lastSeen: now})
	}
	// The settings are read too: a reset (or a switch-off) takes effect
	// before the next evaluation publishes a new view.
	if !v.serving || !s.d.Settings.Get().DHCP.Enabled {
		return
	}
	switch typ {
	case msgDiscover, msgRequest, msgDecline:
		if forOther {
			if typ == msgRequest { // its offer was chosen
				s.mu.Lock()
				s.t.dropOffer(mac)
				s.mu.Unlock()
				e := entry4(m, mac, "REQUEST")
				e.Address, e.Result, e.Reason = addrString(m.addrOpt(optRequestedIP)), ResultIgnored, LogReasonOtherServer
				s.logExchange(e)
			}
			return
		}
		res, cidConflict := s.reservation(m, mac, now)
		if v.onlyReserved && res == nil {
			e := entry4(m, mac, messageName(typ))
			e.Address, e.Result, e.Reason = addrString(m.addrOpt(optRequestedIP)), ResultIgnored, LogReasonNotReserved
			if typ == msgRequest && !m.ciaddr.IsUnspecified() {
				e.Address = m.ciaddr.String()
			}
			s.logExchange(e)
			return
		}
		switch typ {
		case msgDiscover:
			s.discover(v, m, mac, res, cidConflict, now)
		case msgRequest:
			s.request(v, m, mac, res, cidConflict, now)
		default:
			s.decline(v, m, mac, now)
		}
	case msgRelease:
		if !forOther {
			s.release(m, mac, now)
		}
	case msgInform:
		s.inform(v, m, mac)
	default:
		s.c.dropped.Add(1)
	}
}

// reservation decides which reservation applies to a client
// (docs/ARCHITECTURE.md 18.4): the one of its MAC (a MAC match always
// wins); else the one whose client identifier equals option 61, but only
// while no active lease and no pending offer of another MAC holds its
// address and the neighbour check finds no other device on it. conflict is
// true when such a client-ID match was refused: the client is served as
// unreserved.
func (s *Service) reservation(m *message, mac string, now time.Time) (res *static, conflict bool) {
	cid := m.clientID()
	s.mu.Lock()
	if st := s.t.statics[mac]; st != nil {
		s.mu.Unlock()
		return st, false
	}
	st := s.t.staticCID[cid]
	if cid == "" || st == nil {
		s.mu.Unlock()
		return nil, false
	}
	held := false // the client holds the address itself: no neighbour check needed
	blocked := false
	if l := s.t.byIP[st.ip]; l != nil && l.active(now) {
		held, blocked = l.mac == mac, l.mac != mac
	}
	if other, ok := s.t.offerIP[st.ip]; ok && other != mac && now.Before(s.t.offers[other].until) {
		blocked = true
	}
	s.mu.Unlock()
	if blocked || (!held && s.conflict(st.ip, mac)) {
		return nil, true
	}
	return st, false
}

// discover answers a DISCOVER with an OFFER (no answer when the pool is
// exhausted), or with rapid commit (option 80) with an ACK.
func (s *Service) discover(v *view, m *message, mac string, res *static, cidConflict bool, now time.Time) {
	e := entry4(m, mac, "DISCOVER")
	if cidConflict {
		e.Reason = LogReasonClientIDConflict
	}
	p := v.gate.pool
	requested := m.addrOpt(optRequestedIP)
	rapid := v.rapidCommit && m.rapidCommit() && !v.ignoreOthers && !s.othersCount(v.gate.iface.name, now)
	tried := map[netip.Addr]bool{}
	checks := 0
	for {
		s.mu.Lock()
		ip, check, ok := s.t.candidate(p, mac, res, requested, tried, now)
		s.mu.Unlock()
		if !ok {
			s.logRepeated("exhausted", "DHCP pool exhausted: no address to offer", fmt.Errorf("range %s-%s", p.start, p.end))
			e.Result, e.Reason = ResultIgnored, LogReasonPoolExhausted
			s.logExchange(e)
			return
		}
		tried[ip] = true
		if check {
			if checks == maxConflictChecks {
				e.Result, e.Reason = ResultIgnored, LogReasonAddressUnavailable
				s.logExchange(e)
				return
			}
			checks++
			if s.conflict(ip, mac) {
				s.quarantine(p, ip, now)
				continue
			}
		}
		s.mu.Lock()
		if !s.t.freeFor(ip, mac, res, now) { // changed meanwhile (a static lease was added)
			s.mu.Unlock()
			continue
		}
		s.t.putOffer(mac, ip, now.Add(offerHold))
		if rapid {
			s.mu.Unlock()
			// The same path as the REQUEST that follows this OFFER (RFC
			// 4039): decideRequest acknowledges the address through the
			// pending offer, also when it was retained for another
			// client's expired lease.
			e.Reason = LogReasonRapidCommit
			s.commit(v, m, mac, res, ip, now, e, true)
			return
		}
		host := s.hostFor(m, mac, res)
		s.mu.Unlock()
		e.Address, e.Out, e.Result = ip.String(), "OFFER", ResultAnswered
		if s.send(v, m, replyFields{typ: msgOffer, yiaddr: ip}, leaseOptions(host, s.leaseTimeFor(v, res, ip), v.opts)) {
			s.c.offers.Add(1)
			s.logExchange(e)
		}
		return
	}
}

// othersCount reports whether another DHCP server counts on iface.
func (s *Service) othersCount(iface string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.otherServers(iface, now)) > 0
}

// hostFor returns the host name an OFFER carries: the reservation's, else
// the one the client sent, else its previous lease's (s.mu held).
func (s *Service) hostFor(m *message, mac string, res *static) string {
	if res != nil && res.hostname != "" {
		return res.hostname
	}
	host := m.hostName()
	if l := s.t.leases[mac]; host == "" && l != nil {
		host = l.hostname
	}
	return host
}

// leaseTimeFor returns the lease time of ip for a client: its
// reservation's own lease time on the reserved address, else the global
// one.
func (s *Service) leaseTimeFor(v *view, res *static, ip netip.Addr) time.Duration {
	if res != nil && res.leaseSeconds > 0 && res.ip == ip {
		return time.Duration(res.leaseSeconds) * time.Second
	}
	return v.leaseTime
}

// request answers a REQUEST for this server with ACK or NAK (one naming
// another server was handled by the caller).
func (s *Service) request(v *view, m *message, mac string, res *static, cidConflict bool, now time.Time) {
	want := m.ciaddr // renewing, rebinding
	if want.IsUnspecified() {
		want = m.addrOpt(optRequestedIP) // selecting, init-reboot
	}
	if !want.IsValid() || want.IsUnspecified() {
		s.c.dropped.Add(1)
		return
	}
	e := entry4(m, mac, "REQUEST")
	if cidConflict {
		e.Reason = LogReasonClientIDConflict
	}
	s.commit(v, m, mac, res, want, now, e, false)
}

// commit acknowledges want for mac or naks it: the REQUEST path, which a
// DISCOVER with rapid commit takes too (rapid: an ACK with option 80, and
// no answer instead of a NAK).
func (s *Service) commit(v *view, m *message, mac string, res *static, want netip.Addr, now time.Time, e LogEntry, rapid bool) {
	p := v.gate.pool
	e.Address = want.String()
	s.mu.Lock()
	verdict := s.t.decideRequest(p, mac, res, want, now)
	s.mu.Unlock()
	if verdict == verdictCheck && s.conflict(want, mac) {
		s.quarantine(p, want, now)
		verdict = verdictNak
	}
	s.mu.Lock()
	if verdict == verdictAck || verdict == verdictCheck {
		// Changed meanwhile?
		if d := s.t.decideRequest(p, mac, res, want, now); d == verdictNak || d == verdictMove {
			verdict = d
		}
	}
	if verdict == verdictNak || verdict == verdictMove {
		s.mu.Unlock()
		if rapid {
			e.Result, e.Reason = ResultIgnored, LogReasonAddressUnavailable
			s.logExchange(e)
			return
		}
		e.Out, e.Result = "NAK", ResultNak
		switch {
		case verdict == verdictMove:
			e.Reason = LogReasonMoveToReservation
		case e.Reason == "":
			e.Reason = LogReasonAddressUnavailable
		}
		if s.send(v, m, replyFields{typ: msgNak}, nil) {
			s.c.naks.Add(1)
			s.logExchange(e)
		}
		return
	}
	old := s.t.leases[mac]
	var dropped string
	if old == nil {
		var ok bool
		if dropped, ok = s.t.makeRoom(now); !ok {
			s.mu.Unlock()
			s.logRepeated("full", "DHCP lease table full: request not answered", fmt.Errorf("%d leases", maxLeases))
			e.Result, e.Reason = ResultIgnored, LogReasonPoolExhausted
			s.logExchange(e)
			return
		}
	}
	dur := s.leaseTimeFor(v, res, want)
	l := &lease{mac: mac, ip: want, hostname: m.hostName(), clientID: m.clientID(), expires: now.Add(dur), updated: now}
	if old != nil {
		if l.hostname == "" {
			l.hostname = old.hostname
		}
		if l.clientID == "" {
			l.clientID = old.clientID
		}
	}
	// A client-ID match takes the reserved address from an expired lease of
	// another MAC: saveLease deletes that row in the same transaction.
	s.t.putLease(l)
	host := s.t.effectiveName(l)
	err := deleteLeases(context.Background(), s.d.DB, nonEmpty(dropped)...)
	if err == nil {
		err = saveLease(context.Background(), s.d.DB, l)
	}
	s.mu.Unlock()
	if err != nil {
		s.logRepeated("save", "saving a DHCP lease failed (it is kept in memory)", err)
	}
	s.refreshNames()
	opts := leaseOptions(host, dur, v.opts)
	if rapid {
		opts = append(opts, option{code: optRapidCommit, data: []byte{}})
	}
	e.Out, e.Result = "ACK", ResultAnswered
	if s.send(v, m, replyFields{typ: msgAck, yiaddr: want, ciaddr: m.ciaddr}, opts) {
		s.c.acks.Add(1)
		s.logExchange(e)
	}
}

// decline handles a DECLINE of the address the client holds or was
// offered: the lease ends and the address is quarantined for 10 minutes.
func (s *Service) decline(v *view, m *message, mac string, now time.Time) {
	want := m.addrOpt(optRequestedIP)
	s.mu.Lock()
	l := s.t.leases[mac]
	o, offered := s.t.offers[mac]
	if !want.IsValid() || !((l != nil && l.ip == want) || (offered && o.ip == want)) {
		s.mu.Unlock()
		s.c.dropped.Add(1)
		return
	}
	var err error
	if l != nil && l.ip == want {
		s.t.dropLease(mac)
		err = deleteLeases(context.Background(), s.d.DB, mac)
	}
	s.t.dropOffer(mac)
	if v.gate.pool.usable(want) {
		s.t.quarantine[want] = now.Add(quarantine)
	}
	s.mu.Unlock()
	if err != nil {
		s.logRepeated("save", "saving a DHCP lease failed (it is kept in memory)", err)
	}
	s.c.declines.Add(1)
	s.log.Info("a DHCP client declined its address (in use by another device?); quarantined for 10 minutes",
		slog.String("ip", want.String()), slog.String("mac", mac))
	e := entry4(m, mac, "DECLINE")
	e.Address, e.Result = want.String(), ResultProcessed
	s.logExchange(e)
	s.refreshNames()
}

// release ends the client's lease on ciaddr; the address stays reserved
// for it for 24 hours.
func (s *Service) release(m *message, mac string, now time.Time) {
	s.mu.Lock()
	l := s.t.leases[mac]
	if l == nil || l.ip != m.ciaddr || !l.active(now) {
		s.mu.Unlock()
		s.c.dropped.Add(1)
		return
	}
	l.expires, l.updated = now, now
	err := saveLease(context.Background(), s.d.DB, l)
	s.mu.Unlock()
	if err != nil {
		s.logRepeated("save", "saving a DHCP lease failed (it is kept in memory)", err)
	}
	s.c.releases.Add(1)
	e := entry4(m, mac, "RELEASE")
	e.Address, e.Result = m.ciaddr.String(), ResultProcessed
	s.logExchange(e)
	s.refreshNames()
}

// inform answers an INFORM from an address of the subnet with the options
// (no lease).
func (s *Service) inform(v *view, m *message, mac string) {
	ci := m.ciaddr
	if !v.gate.pool.usable(ci) {
		s.c.dropped.Add(1)
		return
	}
	if s.send(v, m, replyFields{typ: msgAck, ciaddr: ci}, slicesClone(v.opts)) {
		s.c.informs.Add(1)
		e := entry4(m, mac, "INFORM")
		e.Address, e.Out, e.Result = ci.String(), "ACK", ResultAnswered
		s.logExchange(e)
	}
}

// quarantine keeps an address in use by another device from being offered
// for 10 minutes.
func (s *Service) quarantine(p *pool, ip netip.Addr, now time.Time) {
	s.mu.Lock()
	if p.usable(ip) {
		s.t.quarantine[ip] = now.Add(quarantine)
	}
	s.mu.Unlock()
	s.log.Info("an address of the DHCP range is used by another device; quarantined for 10 minutes", slog.String("ip", ip.String()))
}

// conflict reports whether the neighbour table shows another device (a
// different MAC) on ip after making the kernel resolve it.
func (s *Service) conflict(ip netip.Addr, mac string) bool {
	if s.neighbour == nil {
		return false
	}
	if s.prime != nil {
		s.prime(ip)
	}
	for _, d := range conflictWaits {
		s.sleep(d)
		if other, ok := s.neighbour(ip); ok {
			return other != mac
		}
	}
	return false
}

// leaseOptions are the options of an OFFER or ACK: lease time d, T1
// (50 %), T2 (87.5 %), the view's options and the host name.
func leaseOptions(host string, d time.Duration, viewOpts []option) []option {
	secs := uint32(d / time.Second)
	opts := make([]option, 0, len(viewOpts)+5)
	opts = append(opts, option{code: optLeaseTime, data: u32(secs)},
		option{code: optRenewal, data: u32(secs / 2)}, option{code: optRebinding, data: u32(secs / 8 * 7)})
	opts = append(opts, viewOpts...)
	if host != "" {
		opts = append(opts, option{code: optHostName, data: []byte(host)})
	}
	return opts
}

// send delivers a reply: NAKs by broadcast, replies to a client with an
// address (ciaddr) by unicast, everything else by broadcast to
// 255.255.255.255:68 out of the interface.
func (s *Service) send(v *view, req *message, f replyFields, opts []option) bool {
	v4, _, _, _, _, _ := s.d.Sockets.get()
	if v4 == nil {
		return false
	}
	self := v.gate.iface.self.Addr()
	b := buildReply(req, f, self, opts)
	dst := netip.AddrPortFrom(broadcast4, clientPort)
	if f.typ != msgNak && req.ciaddr.Is4() && !req.ciaddr.IsUnspecified() {
		dst = netip.AddrPortFrom(req.ciaddr, clientPort)
	}
	if err := v4.WriteTo(b, v.gate.iface.index, self, dst); err != nil {
		s.logRepeated("send4", "sending a DHCP reply failed", err)
		return false
	}
	return true
}

func nonEmpty(s ...string) []string {
	var out []string
	for _, x := range s {
		if x != "" {
			out = append(out, x)
		}
	}
	return out
}

func slicesClone(o []option) []option { return append([]option(nil), o...) }
