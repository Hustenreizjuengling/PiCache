package api

import (
	"context"
	"net/http"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
)

// Network is implemented by internal/app: the network check and the
// discovery scan (docs/ARCHITECTURE.md 17).
type Network interface {
	// Check returns the network check; it is computed at most every 30 s
	// (every 2 s while a scan runs), and starting or finishing a scan
	// invalidates it.
	Check(ctx context.Context) NetworkCheck
	// Scan starts a discovery scan in the background and returns the number
	// of addresses it probes: apperr.Conflict while a scan runs,
	// apperr.TooMany within 60 s after the previous start,
	// apperr.Unavailable in a container bridge network, on systems other
	// than Linux or without a private IPv4 network.
	Scan() (int, error)
}

// NetworkCheck is GET /network/check.
type NetworkCheck struct {
	CheckedAt time.Time `json:"checkedAt"`
	Mode      string    `json:"mode"` // host | bridge (container bridge network)
	// StatsAvailable: the query counts come from the statistics of the
	// last 24 h; false while logs.db is unavailable or client addresses
	// are anonymised (then from the in-memory activity, less exact).
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

// NetworkSelf are this machine's addresses (no loopback, no virtual bridges).
type NetworkSelf struct {
	IPv4    []string `json:"ipv4"`
	ULA     []string `json:"ula"`
	Global  []string `json:"global"`
	DNSIPv6 bool     `json:"dnsIpv6"` // a DNS listener serves IPv6
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
	s.route("POST /api/v1/network/scan", permAdmin, s.networkScan)
}

var errNoNetwork = apperr.Unavailable("the network check is not available")

func (s *Server) networkCheck(w http.ResponseWriter, r *http.Request) error {
	if s.d.Network == nil {
		return errNoNetwork
	}
	return ok(w, s.d.Network.Check(r.Context()))
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
