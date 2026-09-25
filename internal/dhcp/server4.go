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
	if typ == msgRequest {
		if sid := m.addrOpt(optServerID); sid.IsValid() && sid != self && !sid.IsUnspecified() {
			// A client chose another server's offer: passive detection.
			s.recordOther(otherServer{iface: iface.name, address: sid, serverID: sid, source: SourceRequest, lastSeen: now})
		}
	}
	if !v.serving {
		return
	}
	switch typ {
	case msgDiscover:
		s.discover(v, m, mac, now)
	case msgRequest:
		s.request(v, m, mac, now)
	case msgDecline:
		s.decline(v, m, mac, now)
	case msgRelease:
		s.release(v, m, mac, now)
	case msgInform:
		s.inform(v, m)
	default:
		s.c.dropped.Add(1)
	}
}

// discover answers a DISCOVER with an OFFER (no answer when the pool is
// exhausted).
func (s *Service) discover(v *view, m *message, mac string, now time.Time) {
	p := v.gate.pool
	requested := m.addrOpt(optRequestedIP)
	tried := map[netip.Addr]bool{}
	checks := 0
	for {
		s.mu.Lock()
		ip, check, ok := s.t.candidate(p, mac, requested, tried, now)
		s.mu.Unlock()
		if !ok {
			s.logRepeated("exhausted", "DHCP pool exhausted: no address to offer", fmt.Errorf("range %s-%s", p.start, p.end))
			return
		}
		tried[ip] = true
		if check {
			if checks == maxConflictChecks {
				return
			}
			checks++
			if s.conflict(ip, mac) {
				s.quarantine(p, ip, now)
				continue
			}
		}
		s.mu.Lock()
		if !s.t.freeFor(ip, mac, now) { // changed meanwhile (a static lease was added)
			s.mu.Unlock()
			continue
		}
		s.t.putOffer(mac, ip, now.Add(offerHold))
		host := m.hostName()
		if st := s.t.statics[mac]; st != nil && st.hostname != "" {
			host = st.hostname
		} else if l := s.t.leases[mac]; host == "" && l != nil {
			host = l.hostname
		}
		s.mu.Unlock()
		if s.send(v, m, replyFields{typ: msgOffer, yiaddr: ip}, leaseOptions(v, host)) {
			s.c.offers.Add(1)
		}
		return
	}
}

// request answers a REQUEST with ACK or NAK; one naming another server is
// ignored (its offer was chosen).
func (s *Service) request(v *view, m *message, mac string, now time.Time) {
	self := v.gate.iface.self.Addr()
	if sid := m.addrOpt(optServerID); sid.IsValid() && sid != self {
		s.mu.Lock()
		s.t.dropOffer(mac)
		s.mu.Unlock()
		return
	}
	want := m.ciaddr // renewing, rebinding
	if want.IsUnspecified() {
		want = m.addrOpt(optRequestedIP) // selecting, init-reboot
	}
	if !want.IsValid() || want.IsUnspecified() {
		s.c.dropped.Add(1)
		return
	}
	p := v.gate.pool
	s.mu.Lock()
	verdict := s.t.decideRequest(p, mac, want, now)
	s.mu.Unlock()
	if verdict == verdictCheck && s.conflict(want, mac) {
		s.quarantine(p, want, now)
		verdict = verdictNak
	}
	s.mu.Lock()
	if verdict != verdictNak && s.t.decideRequest(p, mac, want, now) == verdictNak {
		verdict = verdictNak // changed meanwhile
	}
	if verdict == verdictNak {
		s.mu.Unlock()
		if s.send(v, m, replyFields{typ: msgNak}, nil) {
			s.c.naks.Add(1)
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
			return
		}
	}
	l := &lease{mac: mac, ip: want, hostname: m.hostName(), clientID: m.clientID(), expires: now.Add(v.leaseTime), updated: now}
	if old != nil {
		if l.hostname == "" {
			l.hostname = old.hostname
		}
		if l.clientID == "" {
			l.clientID = old.clientID
		}
	}
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
	if s.send(v, m, replyFields{typ: msgAck, yiaddr: want, ciaddr: m.ciaddr}, leaseOptions(v, host)) {
		s.c.acks.Add(1)
	}
}

// decline handles a DECLINE of the address the client holds or was
// offered: the lease ends and the address is quarantined for 10 minutes.
func (s *Service) decline(v *view, m *message, mac string, now time.Time) {
	if sid := m.addrOpt(optServerID); sid.IsValid() && sid != v.gate.iface.self.Addr() {
		return
	}
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
	s.refreshNames()
}

// release ends the client's lease on ciaddr; the address stays reserved
// for it for 24 hours.
func (s *Service) release(v *view, m *message, mac string, now time.Time) {
	if sid := m.addrOpt(optServerID); sid.IsValid() && sid != v.gate.iface.self.Addr() {
		return
	}
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
	s.refreshNames()
}

// inform answers an INFORM from an address of the subnet with the options
// (no lease).
func (s *Service) inform(v *view, m *message) {
	ci := m.ciaddr
	if !v.gate.pool.usable(ci) {
		s.c.dropped.Add(1)
		return
	}
	if s.send(v, m, replyFields{typ: msgAck, ciaddr: ci}, slicesClone(v.opts)) {
		s.c.informs.Add(1)
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

// leaseOptions are the options of an OFFER or ACK: lease time, T1 (50 %),
// T2 (87.5 %), the view's options and the host name.
func leaseOptions(v *view, host string) []option {
	secs := uint32(v.leaseTime / time.Second)
	opts := make([]option, 0, len(v.opts)+4)
	opts = append(opts, option{code: optLeaseTime, data: u32(secs)},
		option{code: optRenewal, data: u32(secs / 2)}, option{code: optRebinding, data: u32(secs / 8 * 7)})
	opts = append(opts, v.opts...)
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
