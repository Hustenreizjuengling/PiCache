package settings

import (
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"github.com/hustenreizjuengling/picache/internal/apperr"
)

// Clients configures the names PiCache gives client addresses
// (docs/ARCHITECTURE.md 7.1 step 5).
type Clients struct {
	NameSources NameSources `json:"nameSources"`
}

// NameSources switches the sources of the names of client addresses. The
// first hit wins: a configured client name, the DHCP lease name (DHCP),
// the PTR name (PTR), the name of /etc/hosts (HostsFile), then the name of
// another address with the same MAC from these sources. WHOIS is no name:
// it annotates public source addresses with their network's owner (RDAP;
// sends data out, off by default).
type NameSources struct {
	PTR       bool `json:"ptr"`
	DHCP      bool `json:"dhcp"`
	HostsFile bool `json:"hostsFile"`
	WHOIS     bool `json:"whois"`
}

// NTP configures the NTP server (PICACHE_NTP_LISTEN, docs/ARCHITECTURE.md
// 2): Enabled answers on the bound listener without a restart; Stratum is
// announced while the host clock is synchronised.
type NTP struct {
	Enabled bool `json:"enabled"`
	Stratum int  `json:"stratum"`
}

// Stratum bounds of ntp.stratum.
const (
	MinNTPStratum = 2
	MaxNTPStratum = 15
)

// Network configures the outbound proxy (docs/ARCHITECTURE.md 6.1
// "Outbound proxy"). Proxy.Password is write-only and sealed like the
// sync token (secrets.go); ProxyFor chooses the traffic that uses it.
type Network struct {
	Proxy    Proxy    `json:"proxy"`
	ProxyFor ProxyFor `json:"proxyFor"`
}

// Proxy is the outbound proxy: URL http://host:port or socks5://host:port
// ("" = none), the user name and the password (input only: absent or null
// keeps the stored one, "" removes it, a value replaces it; PasswordSet
// reports whether one is stored and is ignored in requests).
type Proxy struct {
	URL         string  `json:"url"`
	Username    string  `json:"username"`
	Password    *string `json:"password,omitzero"`
	PasswordSet bool    `json:"passwordSet"`
}

// ProxyFor switches the traffic that goes through the proxy: list and
// cache-domains downloads, the release check, notifications. Never RDAP,
// the follower sync, DNS upstreams, the download cache or SNI.
type ProxyFor struct {
	Lists         bool `json:"lists"`
	UpdateCheck   bool `json:"updateCheck"`
	Notifications bool `json:"notifications"`
}

// Any reports whether some traffic uses the proxy.
func (p ProxyFor) Any() bool { return p.Lists || p.UpdateCheck || p.Notifications }

// Limits of the proxy settings.
const (
	MaxProxyURL      = 255
	MaxProxyUsername = 255
	MaxProxyPassword = 255
)

// ProxyAddress is a parsed outbound proxy URL.
type ProxyAddress struct {
	Scheme string // http | socks5
	Host   string // a host name (lower-case) or an IP literal (without brackets)
	Port   int
}

// Origin returns scheme://host:port (the form errors, logs and the
// support bundle use; never credentials).
func (p ProxyAddress) Origin() string {
	return p.Scheme + "://" + net.JoinHostPort(p.Host, strconv.Itoa(p.Port))
}

// ParseProxyURL parses network.proxy.url: http://host:port or
// socks5://host:port, without user info, path, query or fragment, at most
// 255 characters. It returns the field message on failure.
func ParseProxyURL(s string) (ProxyAddress, string) {
	const form = "must be http://host:port or socks5://host:port"
	if len(s) > MaxProxyURL || !printableASCII(s) {
		return ProxyAddress{}, form
	}
	u, err := url.Parse(s)
	if err != nil {
		return ProxyAddress{}, form
	}
	if u.User != nil {
		return ProxyAddress{}, "put the user name and password into their own fields"
	}
	scheme := strings.ToLower(u.Scheme)
	if (scheme != "http" && scheme != "socks5") || u.Opaque != "" || (u.Path != "" && u.Path != "/") ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(s, "#") {
		return ProxyAddress{}, form
	}
	host := strings.ToLower(u.Hostname())
	port, err := strconv.Atoi(u.Port())
	if host == "" || err != nil || port < 1 || port > 65535 {
		return ProxyAddress{}, form
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		if ip.Zone() != "" {
			return ProxyAddress{}, form
		}
		if ip = ip.Unmap(); ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
			return ProxyAddress{}, "link-local, multicast and unspecified addresses are not allowed"
		}
		host = ip.String()
	} else if !validHostname(host) {
		return ProxyAddress{}, form
	}
	return ProxyAddress{Scheme: scheme, Host: host, Port: port}, ""
}

// ProxyBound is the origin a stored proxy password belongs to: scheme,
// host, port and user name ("" without a proxy).
func ProxyBound(p Proxy) string {
	a, msg := ParseProxyURL(p.URL)
	if p.URL == "" || msg != "" {
		return ""
	}
	return a.Origin() + "|" + p.Username
}

// printable reports whether s is valid UTF-8 without control characters
// (spaces allowed).
func printable(s string) bool {
	for _, r := range s {
		if r == 0xfffd || r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return false
		}
	}
	return true
}

// normalize trims the proxy members and lower-cases the URL's scheme and
// host.
func (n *Network) normalize() {
	p := &n.Proxy
	p.URL = strings.TrimSpace(p.URL)
	if a, msg := ParseProxyURL(p.URL); p.URL != "" && msg == "" {
		p.URL = a.Origin()
	}
	p.Username = strings.TrimSpace(p.Username)
}

// validate checks the form of the network section; the password (secrets)
// and the listeners of this machine are checked by their callers.
func (n *Network) validate() error {
	p := n.Proxy
	if p.URL != "" {
		if _, msg := ParseProxyURL(p.URL); msg != "" {
			return apperr.Invalid("network.proxy.url", "%s", msg)
		}
	} else if n.ProxyFor.Any() {
		return apperr.Invalid("network.proxy.url", "set a proxy first")
	}
	if len([]rune(p.Username)) > MaxProxyUsername || !printable(p.Username) {
		return apperr.Invalid("network.proxy.username", "at most %d printable characters", MaxProxyUsername)
	}
	return nil
}

// validate checks the NTP section.
func (n *NTP) validate() error {
	if n.Stratum < MinNTPStratum || n.Stratum > MaxNTPStratum {
		return apperr.Invalid("ntp.stratum", "must be between %d and %d", MinNTPStratum, MaxNTPStratum)
	}
	return nil
}
