package dhcp

import (
	"context"
	"net/netip"
	"slices"
	"sync"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/netutil"
)

// Probe timing and bounds (docs/ARCHITECTURE.md 18).
const (
	probeFresh       = 10 * time.Minute // a probe answer blocks serving this long
	requestFresh     = 24 * time.Hour   // a client's REQUEST naming another server blocks this long
	probeMinGap      = 10 * time.Second // between two probes started through the API
	maxProbeServers  = 16
	maxOtherServers  = 32
	maxRecentProbeIf = 16
)

// probeWait is how long a probe collects offers (a variable for tests):
// 5 s, because servers that ping an address before offering it (dnsmasq
// waits 3 s for an echo reply) answer a little after 3 s.
var probeWait = 5 * time.Second

// broadcast4 is the limited broadcast address.
var broadcast4 = netip.AddrFrom4([4]byte{255, 255, 255, 255})

// probeRun collects the offers answering one probe.
type probeRun struct {
	xid     uint32
	ifIndex int
	self    netip.Addr

	mu    sync.Mutex
	found []ProbeServer
	addrs []netip.Addr
}

func (r *probeRun) add(address, serverID, offer netip.Addr) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.found) >= maxProbeServers || slices.Contains(r.addrs, address) {
		return
	}
	ps := ProbeServer{Address: address.String(), ServerID: serverID.String()}
	if offer.Is4() && !offer.IsUnspecified() {
		ps.Offer = offer.String()
	}
	r.found = append(r.found, ps)
	r.addrs = append(r.addrs, address)
}

// Probe looks for other DHCP servers on the configured interface (else the
// interface of the default route) and returns what answered within 5 s
// (POST /dhcp/probe): apperr.Unavailable when DHCP is unavailable here or
// UDP 67 cannot be opened, apperr.TooMany within 10 s of the previous
// probe, apperr.Invalid (field dhcp.interface) when the interface cannot
// be probed. While DHCP is off UDP 67 is opened for this probe only.
func (s *Service) Probe(ctx context.Context) (ProbeResult, error) {
	v := s.view.Load()
	if ok, _, reason := s.support(); !ok {
		return ProbeResult{}, apperr.Unavailable("the DHCP server is not available: %s", reason)
	}
	name := s.d.Settings.Get().DHCP.Interface
	if name == "" {
		name = netutil.DefaultRouteInterface()
	}
	iface := v.gate.iface
	if iface.name != name || iface.problem != "" {
		iface = s.env.lookupIface(name)
	}
	if name == "" {
		return ProbeResult{}, apperr.Invalid("dhcp.interface", "choose the interface to search first")
	}
	if iface.problem != "" {
		return ProbeResult{}, apperr.Invalid("dhcp.interface", "%s", iface.problem)
	}
	now := s.now()
	s.mu.Lock()
	if !s.apiProbe.IsZero() && now.Sub(s.apiProbe) < probeMinGap && !now.Before(s.apiProbe) {
		s.mu.Unlock()
		return ProbeResult{}, apperr.TooMany("the last search for DHCP servers was less than 10 seconds ago")
	}
	s.apiProbe = now
	s.mu.Unlock()
	release, err := s.probeSocket()
	if err != nil {
		return ProbeResult{}, err
	}
	res, err := s.runProbe(ctx, iface)
	release()
	if err != nil {
		return ProbeResult{}, err
	}
	if len(res.Servers) == 0 {
		// The admin searched and nothing answered, typically after switching
		// off the router's DHCP server: REQUESTs that named another server
		// before this search no longer block. A server that is still active
		// is seen again in the next client's REQUEST.
		s.mu.Lock()
		s.others = slices.DeleteFunc(s.others, func(o otherServer) bool {
			return o.iface == iface.name && o.source == SourceRequest && o.lastSeen.Before(now)
		})
		s.mu.Unlock()
	}
	s.Kick()
	return res, nil
}

// probeSocket makes sure UDP 67 is open for a probe: while DHCP is off it
// is opened for this probe only (release closes it again unless DHCP was
// switched on meanwhile). In an installation that opens the DHCP ports
// only at start (Docker) the search needs DHCP switched on.
func (s *Service) probeSocket() (release func(), err error) {
	s.sockMu.Lock()
	defer s.sockMu.Unlock()
	so := s.d.Sockets
	so.mu.Lock()
	open, held, bindCapable := so.v4 != nil, so.probeHold, so.bindCapable
	so.mu.Unlock()
	if open {
		return func() {}, nil
	}
	if held {
		return nil, apperr.TooMany("a search for DHCP servers is running")
	}
	c, err := s.listen4()
	switch {
	case err != nil && bindDenied(err) && bindCapable:
		return nil, apperr.Unavailable("the search for DHCP servers needs the DHCP server switched on here: PiCache opens the DHCP ports " +
			"only at start in this installation (switch it on, restart PiCache, then search)")
	case err != nil:
		return nil, apperr.Unavailable("the DHCP server is not available: %s", socketErr("UDP port 67", err))
	}
	so.mu.Lock()
	so.v4, so.v4Err, so.v4Code, so.probeHold = c, "", "", true
	so.mu.Unlock()
	s.startReader4(c)
	return func() {
		s.sockMu.Lock()
		defer s.sockMu.Unlock()
		supported, _, _ := s.support()
		keep := supported && s.d.Settings.Get().DHCP.Enabled
		so.mu.Lock()
		defer so.mu.Unlock()
		so.probeHold = false
		if !keep && so.v4 == c {
			so.closeV4Locked()
		}
	}, nil
}

// runProbe sends a DHCPDISCOVER as a relay agent (giaddr = PiCache's
// address, hops 1, chaddr = the interface's MAC, a random xid) to
// 255.255.255.255:67 out of the interface. Servers answer relayed requests
// unicast to giaddr:67, PiCache's server socket, which hands offers with
// the xid to the run (PiCache itself never answers relayed requests). The
// result replaces the earlier probe answers of the interface.
func (s *Service) runProbe(ctx context.Context, iface ifaceState) (ProbeResult, error) {
	v4, _, _, v4Err, _, _ := s.d.Sockets.get()
	if v4 == nil {
		return ProbeResult{}, apperr.Unavailable("the DHCP socket is not open: %s", v4Err)
	}
	if iface.problem != "" || !iface.self.IsValid() {
		return ProbeResult{}, apperr.Invalid("dhcp.interface", "%s", iface.problem)
	}
	s.probeMu.Lock()
	defer s.probeMu.Unlock()
	self := iface.self.Addr()
	run := &probeRun{xid: randomXID(), ifIndex: iface.index, self: self}
	s.probe.Store(run)
	defer s.probe.Store(nil)
	start := s.now()
	pkt := newRequest(run.xid, self, iface.mac, msgDiscover)
	if err := v4.WriteTo(pkt, iface.index, self, netip.AddrPortFrom(broadcast4, serverPort)); err != nil {
		return ProbeResult{}, err
	}
	t := time.NewTimer(probeWait)
	select {
	case <-ctx.Done():
		t.Stop()
		return ProbeResult{}, ctx.Err()
	case <-t.C:
	}
	s.probe.Store(nil)
	run.mu.Lock()
	found := slices.Clone(run.found)
	run.mu.Unlock()
	if found == nil {
		found = []ProbeServer{}
	}
	for i := range found {
		if s.neighbour != nil {
			if ip, err := netip.ParseAddr(found[i].Address); err == nil {
				if mac, ok := s.neighbour(ip); ok {
					found[i].MAC = mac
				}
			}
		}
	}
	end := s.now()
	s.mu.Lock()
	s.others = slices.DeleteFunc(s.others, func(o otherServer) bool { return o.iface == iface.name && o.source == SourceProbe })
	for _, f := range found {
		addr, _ := netip.ParseAddr(f.Address)
		sid, _ := netip.ParseAddr(f.ServerID)
		s.recordOtherLocked(otherServer{iface: iface.name, address: addr, serverID: sid, source: SourceProbe, lastSeen: end})
	}
	s.lastProbe = &LastProbe{Time: start.UTC(), Servers: len(found)}
	if len(s.probeFor) >= maxRecentProbeIf {
		clear(s.probeFor)
	}
	s.probeFor[iface.name] = end
	s.mu.Unlock()
	return ProbeResult{Servers: found, DurationMs: end.Sub(start).Milliseconds()}, nil
}

// probeReply hands an offer answering the running probe to it.
func (s *Service) probeReply(m *message, ifIndex int, src netip.AddrPort) {
	run := s.probe.Load()
	if run == nil || m.xid != run.xid || ifIndex != run.ifIndex || m.msgType() != msgOffer {
		return
	}
	from := src.Addr()
	sid := m.addrOpt(optServerID)
	if !sid.IsValid() {
		sid = from
	}
	if !from.Is4() || from == run.self || sid == run.self {
		return
	}
	run.add(from, sid, m.yiaddr)
}

// recordOther records a server a client named in a REQUEST (passive
// detection).
func (s *Service) recordOther(o otherServer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recordOtherLocked(o)
}

// recordOtherLocked updates or adds o (s.mu held); at most 32 entries, the
// least recently seen goes first.
func (s *Service) recordOtherLocked(o otherServer) {
	if i := slices.IndexFunc(s.others, func(x otherServer) bool {
		return x.iface == o.iface && x.source == o.source && x.serverID == o.serverID && x.address == o.address
	}); i >= 0 {
		s.others[i].lastSeen = o.lastSeen
		return
	}
	if len(s.others) >= maxOtherServers {
		oldest := 0
		for i, x := range s.others {
			if x.lastSeen.Before(s.others[oldest].lastSeen) {
				oldest = i
			}
		}
		s.others = slices.Delete(s.others, oldest, oldest+1)
	}
	s.others = append(s.others, o)
}

// otherServers returns the servers seen on iface that still count: probe
// answers of the last 10 minutes, REQUESTs of the last 24 hours, newest
// first (s.mu held).
func (s *Service) otherServers(iface string, now time.Time) []otherServer {
	var out []otherServer
	for _, o := range s.others {
		fresh := probeFresh
		if o.source == SourceRequest {
			fresh = requestFresh
		}
		if o.iface == iface && now.Sub(o.lastSeen) < fresh {
			out = append(out, o)
		}
	}
	slices.SortFunc(out, func(a, b otherServer) int { return b.lastSeen.Compare(a.lastSeen) })
	return out
}

// gateOthers reports whether another server counts on iface and whether
// iface was probed within the last 10 minutes and not before since.
func (s *Service) gateOthers(iface string, now, since time.Time) (others, probed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	at, ok := s.probeFor[iface]
	return len(s.otherServers(iface, now)) > 0, ok && now.Sub(at) < probeFresh && !now.Before(at) && !at.Before(since)
}

// startAttempt marks the beginning of an attempt to start serving (trying
// true; kept while the attempt lasts) or its end (trying false) and
// returns its start.
func (s *Service) startAttempt(trying bool, now time.Time) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case !trying:
		s.startAt = time.Time{}
	case s.startAt.IsZero(), now.Before(s.startAt):
		// A clock that went back (NTP after a boot without a real-time
		// clock) must not leave every later probe "before" the attempt.
		s.startAt = now
	}
	return s.startAt
}
