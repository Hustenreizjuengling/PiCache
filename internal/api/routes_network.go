package api

import (
	"context"
	"net/http"
	"net/netip"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
)

// Network is implemented by internal/app: the network check and the
// discovery scan (docs/ARCHITECTURE.md 17).
type Network interface {
	// Check returns the network check; it is computed at most every 30 s
	// (every 2 s while a scan runs), and starting or finishing a scan
	// invalidates it. With a valid client it adds Requester, computed for
	// every call (never from the cached check).
	Check(ctx context.Context, client netip.Addr) NetworkCheck
	// Scan starts a discovery scan in the background and returns the number
	// of addresses it probes: apperr.Conflict while a scan runs,
	// apperr.TooMany within 60 s after the previous start,
	// apperr.Unavailable in a container bridge network, on systems other
	// than Linux or without a private IPv4 network.
	Scan() (int, error)
	// Interfaces returns the interfaces of this machine (GET
	// /network/interfaces).
	Interfaces(ctx context.Context) NetworkInterfaces
}

// NetworkInterfaces is GET /network/interfaces: this machine's interfaces
// without loopback (at most 64, sorted by name; none outside Linux), in a
// container bridge network the container's own (Mode "bridge").
type NetworkInterfaces struct {
	Mode       string             `json:"mode"` // host | bridge
	Interfaces []NetworkInterface `json:"interfaces"`
}

// NetworkInterface is one interface (sysfs, the route snapshot and the
// default routes). Lists are never null.
type NetworkInterface struct {
	Name      string `json:"name"`
	Index     int    `json:"index"`
	MAC       string `json:"mac,omitempty"`
	Up        bool   `json:"up"`
	OperState string `json:"operState"` // up | down | dormant | lowerlayerdown | notpresent | testing | unknown
	MTU       int    `json:"mtu"`
	SpeedMbps int    `json:"speedMbps,omitzero"` // omitted when unknown
	Duplex    string `json:"duplex,omitempty"`   // full | half; omitted when unknown
	// Addresses are the interface's addresses with their prefix lengths;
	// Networks the non-default routes through it (at most 64).
	Addresses       []string               `json:"addresses"`
	Networks        []string               `json:"networks"`
	Virtual         bool                   `json:"virtual"` // the sysfs node is below /sys/devices/virtual
	RxBytes         int64                  `json:"rxBytes"`
	TxBytes         int64                  `json:"txBytes"`
	RxErrors        int64                  `json:"rxErrors"`
	TxErrors        int64                  `json:"txErrors"`
	DefaultGateways []NetworkInterfaceGate `json:"defaultGateways"`
}

// NetworkInterfaceGate is a default gateway that uses an interface.
type NetworkInterfaceGate struct {
	Family  string `json:"family"` // ipv4 | ipv6
	Gateway string `json:"gateway"`
}

// NetworkCheck is GET /network/check.
type NetworkCheck struct {
	CheckedAt time.Time `json:"checkedAt"`
	Mode      string    `json:"mode"` // host | bridge (container bridge network)
	// StatsAvailable: the query counts come from the statistics of the
	// last 24 h; false while logs.db is unavailable, client addresses are
	// anonymised or the statistics are off (then from the in-memory
	// activity, less exact).
	StatsAvailable bool            `json:"statsAvailable"`
	Router         *NetworkRouter  `json:"router,omitempty"`
	Self           NetworkSelf     `json:"self"`
	Queries24h     NetworkQueries  `json:"queries24h"`
	Checks         []NetworkItem   `json:"checks"`
	Devices        []NetworkDevice `json:"devices"`
	Scan           NetworkScan     `json:"scan"`
	// DHCP: PiCache's own DHCP server (the router's DNS steps for IPv4 do
	// not apply while it serves; its router advertisements announce it as
	// IPv6 DNS server).
	DHCP NetworkDHCP `json:"dhcp"`
	// Requester is the effective client of this request (v0.16.0; the
	// trusted-proxy rules of the web ACL), present for every authenticated
	// request.
	Requester *NetworkRequester `json:"requester,omitempty"`
}

// NetworkRequester describes the device that asked for the network check
// (the getting-started checklist: "Test from a device").
type NetworkRequester struct {
	Address string `json:"address"`
	// Local: loopback or one of this machine's addresses (also a reverse
	// proxy on this machine that is not trusted).
	Local bool `json:"local"`
	// MAC, Name and ClientID of the entry of devices whose ips contain
	// the address.
	MAC      string `json:"mac,omitempty"`
	Name     string `json:"name,omitempty"`
	ClientID int64  `json:"clientId,omitzero"`
	// Queries24h and LastQuery are those the device list shows for that
	// entry (every address of its MAC), without one the activity of the
	// address (statistics, else the in-memory activity); both absent while
	// client addresses are anonymised (Privacy). LastQuery also looks back
	// 30 days.
	Queries24h *int64    `json:"queries24h,omitempty"`
	LastQuery  time.Time `json:"lastQuery,omitzero"`
	Privacy    bool      `json:"privacy,omitzero"`
}

// NetworkDHCP describes PiCache's own DHCP server for the network check.
type NetworkDHCP struct {
	Serving              bool   `json:"serving"`              // PiCache hands out IPv4 addresses
	Interface            string `json:"interface,omitempty"`  // the interface it serves
	RouterAdvertisements bool   `json:"routerAdvertisements"` // it sends router advertisements (RDNSS/DNSSL)
}

// NetworkRouter is the default gateway.
type NetworkRouter struct {
	IPv4 string   `json:"ipv4,omitempty"`
	IPv6 []string `json:"ipv6"` // IPv6 default gateway and the neighbour addresses with the router's MAC
	MAC  string   `json:"mac,omitempty"`
	Name string   `json:"name,omitempty"` // PTR name of the gateway
	Kind string   `json:"kind"`           // fritzbox | generic | unknown
}

// NetworkSelf are this machine's addresses (no loopback, no virtual bridges,
// no Tailscale addresses except on a default route's interface); those of
// the interfaces of the default routes first (the addresses LAN devices
// use), then by value.
type NetworkSelf struct {
	IPv4    []string `json:"ipv4"`
	ULA     []string `json:"ula"`
	Global  []string `json:"global"`
	DNSIPv6 bool     `json:"dnsIpv6"` // a DNS listener serves IPv6
	// Dynamic4 (v0.16.0): an IPv4 address of the interface of the IPv4
	// default route (lowest metric) has a finite valid lifetime, i.e. a
	// DHCP client configured it (independent of PiCache's own DHCP
	// server). Absent in a container bridge network, on systems other than
	// Linux and without an IPv4 default route (or an IPv4 address on its
	// interface).
	Dynamic4 *bool `json:"dynamic4,omitempty"`
}

// NetworkQueries counts the DNS queries of the last 24 h from other devices
// (loopback and this machine's addresses left out).
type NetworkQueries struct {
	Total      int64 `json:"total"`
	IPv4       int64 `json:"ipv4"`
	IPv6       int64 `json:"ipv6"`
	FromRouter int64 `json:"fromRouter"`
}

// NetworkItem is one check; Data depends on the ID (NetworkForwarding,
// NetworkIPv6DNS, NetworkIPv6Address, NetworkRefused, NetworkDeviceCounts).
type NetworkItem struct {
	ID     string `json:"id"`
	Status string `json:"status"` // ok | info | warn
	Data   any    `json:"data"`
}

// NetworkForwarding is the data of router-forwarding and container-nat.
type NetworkForwarding struct {
	RouterQueries   int64    `json:"routerQueries"`
	TotalQueries    int64    `json:"totalQueries"`
	Share           float64  `json:"share"`           // 0–1
	RouterAddresses []string `json:"routerAddresses"` // most queries first
}

// NetworkIPv6DNS is the data of ipv6-dns.
type NetworkIPv6DNS struct {
	LANHasIPv6  bool     `json:"lanHasIPv6"`
	IPv6Queries int64    `json:"ipv6Queries"`
	IPv6Clients int      `json:"ipv6Clients"`
	ULA         []string `json:"ula"`
	Global      []string `json:"global"`
	// HostIgnoresRA: this machine has no ULA or global address and its
	// default-route interface (or every interface) ignores IPv6 router
	// advertisements (Linux accept_ra), so it cannot tell whether the
	// network uses IPv6.
	HostIgnoresRA bool `json:"hostIgnoresRA"`
	// RouterRDNSS are the DNS servers the IPv6 default router announces in
	// its router advertisements (router lifetime > 0); present only while
	// PiCache records advertisements (its own router advertisements are
	// on), empty when the router announces none.
	RouterRDNSS *[]string `json:"routerRdnss,omitzero"`
}

// NetworkIPv6Address is the data of ipv6-address.
type NetworkIPv6Address struct {
	ULA    []string `json:"ula"`
	Global []string `json:"global"`
}

// NetworkRefused is the data of refused.
type NetworkRefused struct {
	Sources []NetworkRefusedSource `json:"sources"` // newest first, at most 20
	Since   time.Time              `json:"since"`
	// TrustConnectedNetworks is the setting dns.trustConnectedNetworks.
	TrustConnectedNetworks bool `json:"trustConnectedNetworks"`
}

// NetworkRefusedSource is a source address the DNS ACL dropped queries of.
type NetworkRefusedSource struct {
	Address string    `json:"address"`
	Count   int64     `json:"count"`
	Last    time.Time `json:"last"`
	// OnLink: the address is inside a network this machine is connected to
	// (the networks dns.trustConnectedNetworks would allow).
	OnLink bool `json:"onLink"`
}

// NetworkDeviceCounts is the data of devices.
type NetworkDeviceCounts struct {
	Total    int `json:"total"`
	Active   int `json:"active"`
	Inactive int `json:"inactive"`
	Never    int `json:"never"`
}

// NetworkDevice is a device of the neighbour table (grouped by MAC).
type NetworkDevice struct {
	MAC        string    `json:"mac"`
	IPs        []string  `json:"ips"` // IPv4, then ULA, global, link-local
	Name       string    `json:"name,omitempty"`
	ClientID   int64     `json:"clientId,omitzero"`
	LastQuery  time.Time `json:"lastQuery,omitzero"`
	Queries24h int64     `json:"queries24h"`
	Status     string    `json:"status"` // active | inactive | never
	// Vendor of the MAC (IEEE registries) and whether the MAC is locally
	// administered ("private", randomised); both omitted when empty.
	Vendor        string `json:"vendor,omitempty"`
	MACRandomized bool   `json:"macRandomized,omitzero"`
}

// NetworkScan is the state of the discovery scan.
type NetworkScan struct {
	Running    bool      `json:"running"`
	StartedAt  time.Time `json:"startedAt,omitzero"`
	FinishedAt time.Time `json:"finishedAt,omitzero"`
	Addresses  int       `json:"addresses,omitzero"`
}

// registerNetworkRoutes registers the network check endpoints (docs/API.md).
func (s *Server) registerNetworkRoutes() {
	s.route("GET /api/v1/network/check", permRead, s.networkCheck)
	s.route("POST /api/v1/network/scan", permAdmin, s.networkScan, routeExempt)
	s.route("GET /api/v1/network/interfaces", permRead, s.networkInterfaces)
}

func (s *Server) networkInterfaces(w http.ResponseWriter, r *http.Request) error {
	if s.d.Network == nil {
		return errNoNetwork
	}
	return ok(w, s.d.Network.Interfaces(r.Context()))
}

var errNoNetwork = apperr.Unavailable("the network check is not available")

func (s *Server) networkCheck(w http.ResponseWriter, r *http.Request) error {
	if s.d.Network == nil {
		return errNoNetwork
	}
	return ok(w, s.d.Network.Check(r.Context(), requestClient(r).client))
}

func (s *Server) networkScan(w http.ResponseWriter, r *http.Request) error {
	if s.d.Network == nil {
		return errNoNetwork
	}
	n, err := s.d.Network.Scan()
	if err != nil {
		return err
	}
	s.audit(r, "network.scan", "", map[string]int{"addresses": n})
	return writeJSON(w, http.StatusAccepted, struct {
		Started   bool `json:"started"`
		Addresses int  `json:"addresses"`
	}{true, n})
}
