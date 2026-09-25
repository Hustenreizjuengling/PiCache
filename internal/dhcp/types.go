package dhcp

import "time"

// Service states (Status.State).
const (
	StateUnavailable = "unavailable" // PICACHE_DHCP off, not Linux, container bridge network, sockets failed
	StateOff         = "off"         // dhcp.enabled is false
	StateBlocked     = "blocked"     // a safety gate applies (Status.Blockers)
	StateServing     = "serving"
	StateError       = "error" // a configuration or runtime error (Status.Error)
)

// States of the IPv6 announcements.
const (
	StateSending = "sending" // router advertisements
)

// Sources of a detected DHCP server.
const (
	SourceProbe   = "probe"   // answered PiCache's probe
	SourceRequest = "request" // named by a client's broadcast DHCPREQUEST (option 54)
)

// Status is GET /dhcp.
type Status struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"` // why it is unavailable
	State     string `json:"state"`
	// Blockers: dynamic-address, other-server, no-interface, range (in
	// this order).
	Blockers     []string         `json:"blockers"`
	Error        string           `json:"error,omitempty"`
	Interface    *StatusInterface `json:"interface,omitempty"`
	Pool         *StatusPool      `json:"pool,omitempty"`
	Router       string           `json:"router,omitempty"`
	DNSServer    string           `json:"dnsServer,omitempty"`
	Domain       string           `json:"domain,omitempty"`
	OtherServers []OtherServer    `json:"otherServers"`
	LastProbe    *LastProbe       `json:"lastProbe,omitempty"`
	Counters     Counters         `json:"counters"`
	IPv6         StatusIPv6       `json:"ipv6"`
}

// StatusInterface is the served interface.
type StatusInterface struct {
	Name      string `json:"name"`
	MAC       string `json:"mac"`
	IPv4      string `json:"ipv4"` // PiCache's address (server identifier)
	PrefixLen int    `json:"prefixLen"`
	Dynamic   bool   `json:"dynamic"` // the address has a finite lifetime (DHCP client): blocker dynamic-address
}

// StatusPool describes the range.
type StatusPool struct {
	Start  string `json:"start"`
	End    string `json:"end"`
	Size   int    `json:"size"`   // addresses handed out dynamically (skipped ones not counted)
	Used   int    `json:"used"`   // active leases inside the range
	Static int    `json:"static"` // static leases
}

// OtherServer is another DHCP server seen on the interface.
type OtherServer struct {
	Address  string    `json:"address"`
	ServerID string    `json:"serverId"`
	Source   string    `json:"source"` // probe | request
	LastSeen time.Time `json:"lastSeen"`
}

// LastProbe summarises the last probe.
type LastProbe struct {
	Time    time.Time `json:"time"`
	Servers int       `json:"servers"`
}

// Counters count DHCPv4 packets since the start.
type Counters struct {
	Received int64 `json:"received"`
	Offers   int64 `json:"offers"`
	Acks     int64 `json:"acks"`
	Naks     int64 `json:"naks"`
	Declines int64 `json:"declines"`
	Releases int64 `json:"releases"`
	Informs  int64 `json:"informs"`
	Dropped  int64 `json:"dropped"` // malformed, rate limited or not for this server
}

// StatusIPv6 is the state of the IPv6 announcements.
type StatusIPv6 struct {
	RouterAdvertisements RAStatus     `json:"routerAdvertisements"`
	DHCPv6               DHCPv6Status `json:"dhcpv6"`
}

// RAStatus describes the router advertisements.
type RAStatus struct {
	Enabled   bool   `json:"enabled"`
	Available bool   `json:"available"`        // the raw ICMPv6 socket is open
	Reason    string `json:"reason,omitempty"` // why it is not available
	State     string `json:"state"`            // off | blocked | sending | error
	// Blockers: no-interface, no-ula, no-raw-socket.
	Blockers      []string  `json:"blockers"`
	Address       string    `json:"address,omitempty"` // the announced ULA
	LastSent      time.Time `json:"lastSent,omitzero"`
	Sent          int64     `json:"sent"`
	Solicitations int64     `json:"solicitations"` // solicitations answered
	Error         string    `json:"error,omitempty"`
}

// DHCPv6Status describes stateless DHCPv6.
type DHCPv6Status struct {
	Enabled bool   `json:"enabled"`
	State   string `json:"state"` // off | blocked | serving | error
	// Blockers: no-interface, no-ula, no-socket (while blocked).
	Blockers []string `json:"blockers"`
	Replies  int64    `json:"replies"`
	Ignored  int64    `json:"ignored"` // other message types (no address leases) and malformed packets
	Error    string   `json:"error,omitempty"`
}

// Interface is an entry of GET /dhcp/interfaces.
type Interface struct {
	Name     string          `json:"name"`
	MAC      string          `json:"mac"`
	IPv4     []string        `json:"ipv4"` // CIDR
	IPv6     []InterfaceIPv6 `json:"ipv6"`
	Dynamic4 bool            `json:"dynamic4"` // an IPv4 address has a finite lifetime (DHCP client)
	Virtual  bool            `json:"virtual"`  // bridge, virtual Ethernet or tunnel: cannot be served
}

// InterfaceIPv6 is an IPv6 address of an interface.
type InterfaceIPv6 struct {
	Address    string `json:"address"`
	Kind       string `json:"kind"` // ula | global | link-local
	Temporary  bool   `json:"temporary"`
	Deprecated bool   `json:"deprecated"`
}

// ProbeServer is a DHCP server that answered a probe.
type ProbeServer struct {
	Address  string `json:"address"`         // source address of its offer
	ServerID string `json:"serverId"`        // option 54
	Offer    string `json:"offer,omitempty"` // the address it offered
	MAC      string `json:"mac,omitempty"`   // from the neighbour table
}

// ProbeResult is POST /dhcp/probe.
type ProbeResult struct {
	Servers    []ProbeServer `json:"servers"`
	DurationMs int64         `json:"durationMs"`
}

// Lease is an entry of GET /dhcp/leases.
type Lease struct {
	MAC      string    `json:"mac"`
	IP       string    `json:"ip"`
	Hostname string    `json:"hostname,omitempty"` // the static entry's name, else the one the client sent
	ClientID string    `json:"clientId,omitempty"` // option 61 (hex)
	Expires  time.Time `json:"expires"`
	Active   bool      `json:"active"`
	Static   bool      `json:"static"` // the address of the client's static lease
	// ClientName is the configured client of the address or MAC (filled
	// by the API).
	ClientName   string `json:"clientName,omitempty"`
	DNSName      string `json:"dnsName,omitempty"`
	NameConflict bool   `json:"nameConflict,omitzero"` // another client holds the host name: no DNS name
}

// StaticLease is an entry of GET /dhcp/static.
type StaticLease struct {
	MAC       string    `json:"mac"`
	IP        string    `json:"ip"`
	Hostname  string    `json:"hostname,omitempty"`
	Comment   string    `json:"comment,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	Active    bool      `json:"active"` // the client holds an active lease on the address
}

// StaticInput creates a static lease (POST /dhcp/static).
type StaticInput struct {
	MAC      string `json:"mac"`
	IP       string `json:"ip"`
	Hostname string `json:"hostname,omitempty"`
	Comment  string `json:"comment,omitempty"`
}

// StaticUpdate changes a static lease (PUT /dhcp/static/{mac}).
type StaticUpdate struct {
	IP       string `json:"ip"`
	Hostname string `json:"hostname,omitempty"`
	Comment  string `json:"comment,omitempty"`
}
