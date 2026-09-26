package dhcp

import "time"

// Service states (Status.State).
const (
	StateUnavailable = "unavailable" // PICACHE_DHCP=off, not Linux, container bridge network, UDP 67 failed (Status.ReasonCode)
	StateOff         = "off"         // dhcp.enabled is false
	StateBlocked     = "blocked"     // a safety gate applies (Status.Blockers)
	StateServing     = "serving"
	StateError       = "error" // a configuration or runtime error (Status.Error)
)

// Reason codes of the unavailable state (Status.ReasonCode).
const (
	ReasonOptOut          = "opt-out"          // PICACHE_DHCP=off
	ReasonNotLinux        = "not-linux"        // the DHCP server needs Linux
	ReasonBridge          = "bridge"           // container bridge network
	ReasonSocket          = "socket"           // UDP 67 could not be opened (another DHCP server on this host, no permission)
	ReasonRestartRequired = "restart-required" // the ports open only at start here (Docker): restart once
)

// Reason codes of the router advertisements (RAStatus.ReasonCode, set
// while they are not available).
const (
	RAReasonDHCPUnavailable = "dhcp-unavailable" // DHCP itself is unavailable
	RAReasonRestart         = "restart-required" // enabled; the raw socket opens at the next start
	RAReasonNoCapNetRaw     = "no-cap-net-raw"   // the process did not hold CAP_NET_RAW at this start
	RAReasonDropUnverified  = "drop-unverified"  // dropping CAP_NET_RAW could not be verified: the raw socket was closed
	RAReasonSocket          = "socket"           // opening the raw socket at start failed for another reason
)

// Deployments (Status.Deployment).
const (
	DeploymentDocker  = "docker"  // Docker or Podman container
	DeploymentSystemd = "systemd" // a systemd service
	DeploymentOther   = "other"
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
	Available bool `json:"available"` // state != unavailable
	// Reason says why it is unavailable (English text for logs and the
	// CLI), or why a marker could not be written.
	Reason     string `json:"reason,omitempty"`
	ReasonCode string `json:"reasonCode,omitempty"` // with state unavailable: opt-out | not-linux | bridge | socket | restart-required
	// MarkerError says why a marker could not be written, in every state
	// (with restart-required it means the restart will not open the
	// ports).
	MarkerError string `json:"markerError,omitempty"`
	Deployment  string `json:"deployment"` // docker | systemd | other
	State       string `json:"state"`
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
	// OtherAnnouncers are other routers and DHCPv6 servers that announce
	// DNS or DHCPv6 on the interface, newest first.
	OtherAnnouncers []Announcer `json:"otherAnnouncers"`
	LastSearch      *LastSearch `json:"lastSearch,omitempty"`
}

// Kinds of another IPv6 announcer.
const (
	AnnouncerRA     = "ra"     // router advertisements (passive, while PiCache's own are sent)
	AnnouncerDHCPv6 = "dhcpv6" // answered PiCache's search
)

// Announcer is another router or DHCPv6 server on the interface.
type Announcer struct {
	Kind      string   `json:"kind"`    // ra | dhcpv6
	Address   string   `json:"address"` // its (link-local) source address
	Interface string   `json:"interface"`
	ServerID  string   `json:"serverId,omitempty"` // dhcpv6: SERVERID as colon-separated hex
	DNS       []string `json:"dns"`                // announced DNS servers
	// OwnDNS are the entries of DNS that are addresses of this machine
	// (PiCache itself; they never warn). Never null.
	OwnDNS []string `json:"ownDns"`
	// ra only: the M and O flags and the router lifetime in seconds.
	Managed        *bool     `json:"managed,omitempty"`
	Other          *bool     `json:"other,omitempty"`
	RouterLifetime *int      `json:"routerLifetime,omitempty"`
	FirstSeen      time.Time `json:"firstSeen"`
	LastSeen       time.Time `json:"lastSeen"`
	// Conflict: while PiCache announces itself, the record carries a DNS
	// server that is not an address of this machine and was seen twice.
	Conflict bool `json:"conflict"`
}

// LastSearch is the last search for other IPv6 announcers.
type LastSearch struct {
	Time   time.Time `json:"time"`
	RA     bool      `json:"ra"`     // a router solicitation was sent (the raw socket is open)
	DHCPv6 bool      `json:"dhcpv6"` // a relayed information request was sent (UDP 547 is open)
}

// RAStatus describes the router advertisements.
type RAStatus struct {
	Enabled bool `json:"enabled"`
	// Available: the raw ICMPv6 socket is open, or the option is off and
	// PiCache held CAP_NET_RAW at this start (switching the
	// advertisements on then needs one restart).
	Available  bool   `json:"available"`
	Reason     string `json:"reason,omitempty"`     // why it is not available
	ReasonCode string `json:"reasonCode,omitempty"` // with available false: dhcp-unavailable | restart-required | no-cap-net-raw | drop-unverified | socket
	State      string `json:"state"`                // off | blocked | sending | error
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
	Static   bool      `json:"static"` // the address of the client's reservation
	// ClientName is the configured client of the address or MAC (filled
	// by the API).
	ClientName string `json:"clientName,omitempty"`
	DNSName    string `json:"dnsName,omitempty"`
	// NameGenerated: DNSName is the generated <a>-<b>-<c>-<d>.<domain>.
	NameGenerated bool `json:"nameGenerated,omitzero"`
	NameConflict  bool `json:"nameConflict,omitzero"` // another client holds the host name
}

// StaticLease is an entry of GET /dhcp/static (a reservation).
type StaticLease struct {
	MAC          string    `json:"mac"`
	IP           string    `json:"ip"`
	Hostname     string    `json:"hostname,omitempty"`
	Comment      string    `json:"comment,omitempty"`
	ClientID     string    `json:"clientId,omitempty"`    // option 61 as colon-separated hex: matches too
	LeaseSeconds int       `json:"leaseSeconds,omitzero"` // 0 = dhcp.leaseSeconds
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
	Active       bool      `json:"active"` // the client holds an active lease on the address
}

// StaticInput creates a static lease (POST /dhcp/static).
type StaticInput struct {
	MAC          string `json:"mac"`
	IP           string `json:"ip"`
	Hostname     string `json:"hostname,omitempty"`
	Comment      string `json:"comment,omitempty"`
	ClientID     string `json:"clientId,omitempty"`
	LeaseSeconds int    `json:"leaseSeconds,omitzero"`
}

// StaticUpdate replaces a static lease (PUT /dhcp/static/{mac}): members
// left out are cleared.
type StaticUpdate struct {
	IP           string `json:"ip"`
	Hostname     string `json:"hostname,omitempty"`
	Comment      string `json:"comment,omitempty"`
	ClientID     string `json:"clientId,omitempty"`
	LeaseSeconds int    `json:"leaseSeconds,omitzero"`
}

// Import formats (POST /dhcp/static/import) and export formats (GET
// /dhcp/static/export).
const (
	FormatCSV   = "csv"
	FormatHosts = "hosts"
	FormatLines = "lines" // import only: "mac ip [hostname]"
)

// ImportInput is POST /dhcp/static/import.
type ImportInput struct {
	Format  string `json:"format"`
	Text    string `json:"text"`
	Replace bool   `json:"replace"` // remove reservations whose MAC is not in the text
	DryRun  bool   `json:"dryRun"`  // validate and count only
}

// ImportResult is the answer to an import: the counts say what the valid
// rows do (or would do); with any error nothing is written.
type ImportResult struct {
	Applied   bool          `json:"applied"`
	Added     int           `json:"added"`
	Updated   int           `json:"updated"`
	Unchanged int           `json:"unchanged"`
	Removed   int           `json:"removed"`
	Errors    []ImportError `json:"errors"`
}

// ImportError is the first error of one line (0: the whole batch).
type ImportError struct {
	Line    int    `json:"line"`
	Field   string `json:"field"` // mac | ip | hostname | comment | clientId | leaseSeconds | row | text
	Message string `json:"message"`
}

// Kinds, results and reasons of an exchange log entry.
const (
	LogDHCPv4 = "dhcpv4"
	LogDHCPv6 = "dhcpv6"
	LogRA     = "ra"

	ResultAnswered  = "answered"
	ResultNak       = "nak"
	ResultProcessed = "processed"
	ResultIgnored   = "ignored"

	LogReasonRapidCommit        = "rapid-commit"
	LogReasonNotReserved        = "not-reserved"
	LogReasonOtherServer        = "other-server"
	LogReasonPoolExhausted      = "pool-exhausted"
	LogReasonAddressUnavailable = "address-unavailable"
	LogReasonMoveToReservation  = "move-to-reservation"
	LogReasonClientIDConflict   = "client-id-conflict"
)

// LogEntry is one handled exchange (GET /dhcp/log).
type LogEntry struct {
	Time     time.Time `json:"time"`
	Kind     string    `json:"kind"`               // dhcpv4 | dhcpv6 | ra
	MAC      string    `json:"mac,omitempty"`      // DHCPv4; RS with a source link-layer option
	DUID     string    `json:"duid,omitempty"`     // DHCPv6 CLIENTID as colon-separated hex
	Address  string    `json:"address,omitempty"`  // offered, acknowledged or requested (DHCPv4); the link-local source (DHCPv6, RS)
	Hostname string    `json:"hostname,omitempty"` // sanitised
	In       string    `json:"in"`                 // DISCOVER | REQUEST | DECLINE | RELEASE | INFORM | INFORMATION-REQUEST | RS
	Out      string    `json:"out,omitempty"`      // OFFER | ACK | NAK | REPLY | RA
	Result   string    `json:"result"`             // answered | nak | processed | ignored
	Reason   string    `json:"reason,omitempty"`   // rapid-commit | not-reserved | other-server | pool-exhausted | address-unavailable | move-to-reservation | client-id-conflict
}
