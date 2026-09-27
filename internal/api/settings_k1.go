package api

import (
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/config"
	"github.com/hustenreizjuengling/picache/internal/settings"
	"github.com/hustenreizjuengling/picache/internal/update"
)

// checkSynced refuses a change of a member a follower syncs (section
// dns-settings): 409 with the member as the field. The configuration lock
// never applies to the sync itself (an internal update).
func (s *Server) checkSynced(old, next *settings.All) error {
	secs, _ := s.syncedSections()
	if !slices.Contains(secs, settings.SectionDNSSettings) {
		return nil
	}
	if field := settings.SyncedChange(old, next); field != "" {
		return s.syncedSection(settings.SectionDNSSettings, field)
	}
	return nil
}

// checkK1Settings runs the checks of the sections of 0.15.0 that need the
// running system: a sync source that is this machine, a proxy on one of
// PiCache's own listeners, the nightly channel in Docker. The candidate is
// not normalised yet (Store.Update does that after this callback), so the
// values are compared as they will be stored ("NIGHTLY", " https://…").
// Unchanged values are not checked again.
func (s *Server) checkK1Settings(r *http.Request, old, next *settings.All) error {
	own := s.ownAddrs()
	src := strings.TrimSpace(next.Sync.Source)
	if o, msg := settings.SyncOrigin(src); msg == "" {
		src = o
	}
	if src != "" && src != old.Sync.Source {
		if u, err := url.Parse(src); err == nil {
			if ip, err := netip.ParseAddr(u.Hostname()); err == nil && slices.Contains(own, ip.Unmap()) {
				return apperr.Invalid("sync.source", "this is the address of this PiCache")
			}
		}
	}
	if a, msg := settings.ParseProxyURL(strings.TrimSpace(next.Network.Proxy.URL)); msg == "" && a.Origin() != old.Network.Proxy.URL {
		if ip, err := netip.ParseAddr(a.Host); err == nil {
			if role := s.ownListener(ip, a.Port, own); role != "" {
				return apperr.Invalid("network.proxy.url", "this is PiCache's own %s listener", role)
			}
		}
	}
	channel := strings.ToLower(strings.TrimSpace(next.Updates.Channel))
	if channel == settings.ChannelNightly && old.Updates.Channel != settings.ChannelNightly && s.d.Updates != nil &&
		s.d.Updates.UpdateOverview(r.Context()).Mode == update.ModeDocker {
		return apperr.Invalid("updates.channel", "nightly builds have no container image")
	}
	return nil
}

// ownListener returns the role of a TCP listener of this PiCache that
// ip:port reaches ("" if none): ip is loopback or this machine's, and a
// bound listener has the port on ip or on every address.
func (s *Server) ownListener(ip netip.Addr, port int, own []netip.Addr) string {
	ip = ip.Unmap()
	if !ip.IsLoopback() && !ip.IsUnspecified() && !slices.Contains(own, ip) {
		return ""
	}
	if s.d.Runtime == nil {
		return ""
	}
	for name, addrs := range s.d.Runtime.Listeners().Bound {
		role, tcp := config.BoundRole(name)
		if !tcp {
			continue // a proxy is dialed over TCP
		}
		for _, a := range addrs {
			host, p, err := net.SplitHostPort(a)
			if err != nil || p != strconv.Itoa(port) {
				continue
			}
			h, err := netip.ParseAddr(host)
			if err != nil || h.IsUnspecified() || h.Unmap() == ip || ip.IsUnspecified() {
				return role
			}
		}
	}
	return ""
}
