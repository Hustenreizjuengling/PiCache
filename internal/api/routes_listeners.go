package api

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/config"
	"github.com/hustenreizjuengling/picache/internal/netutil"
)

// The listeners of System → Network (docs/ARCHITECTURE.md 2 "Listeners"):
// bootstrap configuration changed in the UI and applied at the next start
// (listeners.next.json). Roles set by PICACHE_*_LISTEN or a flag are
// locked; in Docker the listeners are set in the compose file.

// ListenersConfig is GET /system/listeners.
type ListenersConfig struct {
	Editable        bool             `json:"editable"`
	Reason          string           `json:"reason,omitempty"` // docker
	RestartRequired bool             `json:"restartRequired"`  // a saved set waits for the next start
	Roles           []ListenerRole   `json:"roles"`            // in the order of config.ListenerRoles
	Failed          *ListenersFailed `json:"failed,omitempty"`
}

// ListenerRole is one role: what is bound now, what is saved for it
// (absent when the saved set does not set the role; [] = off), the
// built-in default, and whether the host locks it.
type ListenerRole struct {
	Role       string    `json:"role"`
	Bound      []string  `json:"bound"`
	Saved      *[]string `json:"saved,omitzero"`
	Default    []string  `json:"default"`
	Locked     bool      `json:"locked"`
	LockedBy   string    `json:"lockedBy,omitempty"` // the variable, or "flag"
	CanDisable bool      `json:"canDisable"`
}

// ListenersFailed is the last saved set that could not be bound
// completely (listeners.failed.json): the roles that failed with their
// errors and the saved set.
type ListenersFailed struct {
	Time  time.Time           `json:"time"`
	Roles map[string]string   `json:"roles"`
	Saved map[string][]string `json:"saved"`
}

// ListenerManager is implemented by internal/app: the listeners files.
type ListenerManager interface {
	// SavedListeners returns the saved set (listeners.next.json, else
	// listeners.json; nil when there is none) and whether a set waits for
	// the next start (listeners.next.json exists).
	SavedListeners() (roles map[string][]string, next bool)
	// FailedListeners returns listeners.failed.json (nil: none).
	FailedListeners() *ListenersFailed
	// SaveListeners writes the set for the next start.
	SaveListeners(roles map[string][]string) error
}

var errNoListeners = apperr.Unavailable("the listeners cannot be changed here")

func (s *Server) registerListenerRoutes() {
	s.route("GET /api/v1/system/listeners", permRead, s.listenersGet)
	s.route("PUT /api/v1/system/listeners", permSession, s.listenersPut)
}

// dockerListeners reports a Docker deployment (PICACHE_RUN_AS): the
// listeners are read-only.
func (s *Server) dockerListeners() bool { return s.d.Config != nil && s.d.Config.RunAs != "" }

// listenerValues returns the flag, environment or default values of every
// role (the Config's), and the locks.
func (s *Server) listenerValues() (map[string][]string, map[string]string) {
	vals := config.DefaultListeners()
	locks := map[string]string{}
	c := s.d.Config
	if c == nil {
		return vals, locks
	}
	for role, v := range map[string][]string{config.RoleDNS: c.DNSListen, config.RoleCache: c.CacheListen,
		config.RoleSNI: c.SNIListen, config.RoleWeb: c.WebListen, config.RoleWebTLS: c.WebTLSListen,
		config.RoleDoT: c.DoTListen, config.RoleDoH: c.DoHListen, config.RoleNTP: c.NTPListen} {
		if lock := c.ListenerLock[role]; lock != "" {
			vals[role], locks[role] = slices.Clone(v), lock
			if vals[role] == nil {
				vals[role] = []string{}
			}
		}
	}
	return vals, locks
}

// listenersConfig builds GET /system/listeners.
func (s *Server) listenersConfig() ListenersConfig {
	out := ListenersConfig{Editable: !s.dockerListeners(), Roles: []ListenerRole{}}
	if !out.Editable {
		out.Reason = "docker"
	}
	var saved map[string][]string
	if s.d.Listeners != nil {
		saved, out.RestartRequired = s.d.Listeners.SavedListeners()
		out.Failed = s.d.Listeners.FailedListeners()
	}
	var bound map[string][]string
	if s.d.Runtime != nil {
		bound = s.d.Runtime.Listeners().Bound
	}
	defaults := config.DefaultListeners()
	_, locks := s.listenerValues()
	for _, role := range config.ListenerRoles {
		// Whether the saved set may turn the role off: the rule of the file
		// (web only while the saved set gives webTls addresses).
		r := ListenerRole{Role: role, Default: defaults[role], Bound: boundOf(bound, role), CanDisable: config.CanDisable(role, saved)}
		if v, ok := saved[role]; ok {
			c := slices.Clone(v)
			if c == nil {
				c = []string{}
			}
			r.Saved = &c
		}
		if lock := locks[role]; lock != "" {
			r.Locked, r.LockedBy = true, lock
		}
		out.Roles = append(out.Roles, r)
	}
	return out
}

// boundOf returns the bound addresses of a role (dns: dns-udp and dns-tcp;
// webTls: web-tls).
func boundOf(bound map[string][]string, role string) []string {
	var out []string
	switch role {
	case config.RoleDNS:
		for _, a := range append(slices.Clone(bound["dns-udp"]), bound["dns-tcp"]...) {
			if !slices.Contains(out, a) {
				out = append(out, a)
			}
		}
	case config.RoleWebTLS:
		out = slices.Clone(bound["web-tls"])
	default:
		out = slices.Clone(bound[role])
	}
	if out == nil {
		out = []string{}
	}
	return out
}

func (s *Server) listenersGet(w http.ResponseWriter, r *http.Request) error {
	return ok(w, s.listenersConfig())
}

// listenersPut saves the listeners for the next start: the whole saved set
// (an absent role is removed from the file). The checks run in this order
// (all 400): syntax, a role set on the host, an address that is not this
// machine's, two roles on the same port and protocol, dns or cache empty,
// no web listener, no web listener on all addresses or loopback, the
// lock-out guard (the connection's local address stays served); then the
// current password.
func (s *Server) listenersPut(w http.ResponseWriter, r *http.Request) error {
	if s.dockerListeners() {
		return apperr.Conflict("in Docker the listeners are set in the compose file")
	}
	if s.d.Listeners == nil {
		return errNoListeners
	}
	var in struct {
		Listeners       map[string][]string `json:"listeners"`
		CurrentPassword string              `json:"currentPassword"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	if in.Listeners == nil {
		in.Listeners = map[string][]string{}
	}
	local := r.Context().Value(http.LocalAddrContextKey)
	var localIP netip.Addr
	if a, ok := local.(net.Addr); ok {
		localIP = netutil.AddrFromNet(a)
	}
	if err := s.checkListeners(in.Listeners, localIP); err != nil {
		return err
	}
	if err := s.confirmCurrentPassword(r.Context(), principal(r), in.CurrentPassword); err != nil {
		return err
	}
	old, _ := s.d.Listeners.SavedListeners()
	if err := s.d.Listeners.SaveListeners(in.Listeners); err != nil {
		return err
	}
	if old == nil {
		old = map[string][]string{}
	}
	s.audit(r, "system.listeners.update", "", map[string]any{"old": old, "new": in.Listeners})
	return ok(w, s.listenersConfig())
}

// checkListeners validates a saved set (the order of listenersPut).
func (s *Server) checkListeners(roles map[string][]string, localIP netip.Addr) error {
	// Syntax (listeners.<role>[i]), unknown roles, at most 8 each.
	for role, addrs := range roles {
		if !slices.Contains(config.ListenerRoles, role) {
			return apperr.Invalid("listeners."+role, "unknown role")
		}
		if addrs == nil {
			addrs = []string{}
			roles[role] = addrs
		}
		if len(addrs) > config.MaxListenerAddrs {
			return apperr.Invalid("listeners."+role, "at most %d addresses", config.MaxListenerAddrs)
		}
		seen := map[string]bool{}
		for i, a := range addrs {
			c, err := config.ParseListenerAddr(a)
			if err != nil {
				return apperr.Invalid(fmt.Sprintf("listeners.%s[%d]", role, i), "%v", err)
			}
			if seen[c] {
				return apperr.Invalid(fmt.Sprintf("listeners.%s[%d]", role, i), "listed twice")
			}
			seen[c] = true
			addrs[i] = c
		}
	}
	// The rules of the file itself: [] only where a role may be off (web
	// only while the saved set gives webTls addresses).
	if err := config.ValidateListeners(roles); err != nil {
		if le, ok := errors.AsType[*config.ListenerError](err); ok {
			return apperr.Invalid(le.Field, "%s", le.Msg)
		}
		return err
	}
	vals, locks := s.listenerValues()
	for _, role := range config.ListenerRoles {
		if _, ok := roles[role]; ok && locks[role] != "" {
			by := locks[role]
			if by == "flag" {
				return apperr.Invalid("listeners."+role, "set by a command-line flag on the host")
			}
			return apperr.Invalid("listeners."+role, "set by %s on the host", by)
		}
	}
	// Addresses of this machine (wildcards pass).
	own := s.ownAddrs()
	for _, role := range config.ListenerRoles {
		for i, a := range roles[role] {
			ip := listenerIP(a)
			if ip.IsValid() && !ip.IsUnspecified() && !slices.Contains(own, ip) {
				return apperr.Invalid(fmt.Sprintf("listeners.%s[%d]", role, i), "%s is not an address of this machine", ip)
			}
		}
	}
	effective := map[string][]string{}
	for _, role := range config.ListenerRoles {
		effective[role] = vals[role]
		if v, ok := roles[role]; ok {
			effective[role] = v
		}
	}
	if err := checkPortClashes(effective); err != nil {
		return err
	}
	for _, role := range []string{config.RoleDNS, config.RoleCache} {
		if len(effective[role]) == 0 {
			return apperr.Invalid("listeners."+role, "at least one address")
		}
	}
	web := append(slices.Clone(effective[config.RoleWeb]), effective[config.RoleWebTLS]...)
	if len(web) == 0 {
		return apperr.Invalid("listeners.web", "keep a web listener")
	}
	if !slices.ContainsFunc(web, func(a string) bool {
		ip := listenerIP(a)
		return !ip.IsValid() || ip.IsUnspecified() || ip.IsLoopback()
	}) {
		return apperr.Invalid("listeners.web", "keep a web listener on all addresses or on loopback: the command line tools and the health check use it")
	}
	if localIP.IsValid() {
		served := slices.ContainsFunc(web, func(a string) bool {
			ip := listenerIP(a)
			return !ip.IsValid() || ip == localIP || (ip.IsUnspecified() && ip.Is4() == localIP.Is4()) ||
				(ip.IsUnspecified() && ip.Is6()) // [::] listens on both families
		})
		if !served {
			return apperr.Invalid("listeners.web", "you are connected through %s; keep a web listener on it or on all addresses", localIP)
		}
	}
	return nil
}

// ownAddrs returns this machine's addresses (loopback included).
func (s *Server) ownAddrs() []netip.Addr {
	if s.localAddrs != nil {
		return s.localAddrs()
	}
	return netutil.LocalAddrs()
}

// listenerIP returns the IP of a listener address (invalid for ":port",
// which listens on every address of both families).
func listenerIP(a string) netip.Addr {
	host, _, err := net.SplitHostPort(a)
	if err != nil || host == "" {
		return netip.Addr{}
	}
	ip, _ := netip.ParseAddr(host)
	return ip.Unmap()
}

// listenerRolesTCP and listenerRolesUDP are the roles per protocol.
var (
	listenerRolesTCP = []string{config.RoleDNS, config.RoleCache, config.RoleSNI, config.RoleWeb, config.RoleWebTLS, config.RoleDoT, config.RoleDoH}
	listenerRolesUDP = []string{config.RoleDNS, config.RoleNTP}
)

// checkPortClashes refuses two roles on the same port and protocol whose
// addresses overlap (equal, or either a wildcard of an overlapping family).
func checkPortClashes(effective map[string][]string) error {
	for _, roles := range [][]string{listenerRolesTCP, listenerRolesUDP} {
		for i, a := range roles {
			for _, b := range roles[:i] {
				for _, x := range effective[a] {
					for _, y := range effective[b] {
						if listenersOverlap(x, y) {
							_, p, _ := net.SplitHostPort(x)
							return apperr.Invalid("listeners."+a, "%s uses port %s like %s", a, p, b)
						}
					}
				}
			}
		}
	}
	return nil
}

// listenersOverlap reports two addresses on the same port whose addresses
// overlap: equal, or either a wildcard (":p" and "[::]:p" both families,
// "0.0.0.0:p" IPv4) that covers the other's family.
func listenersOverlap(x, y string) bool {
	_, px, err1 := net.SplitHostPort(x)
	_, py, err2 := net.SplitHostPort(y)
	if err1 != nil || err2 != nil || px != py {
		return false
	}
	ix, iy := listenerIP(x), listenerIP(y)
	covers := func(w, o netip.Addr) bool {
		switch {
		case !w.IsValid(): // ":p"
			return true
		case w.IsUnspecified() && w.Is6():
			return true
		case w.IsUnspecified():
			return !o.IsValid() || o.Is4()
		}
		return false
	}
	return ix == iy || covers(ix, iy) || covers(iy, ix)
}
