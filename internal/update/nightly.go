package update

import (
	"errors"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// NightlyAllowed reports whether the marker NightlyMarker allows nightly
// builds on this host (GET /system/update nightlyAllowed).
func NightlyAllowed() bool { return nightlyAllowed("") }

// nightlyAllowed checks the marker at path ("" = NightlyMarker): a regular
// file (never a link) owned by root.
func nightlyAllowed(path string) bool {
	if path == "" {
		path = NightlyMarker
	}
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	return ownedByRoot(fi)
}

// UpdateProxy is PICACHE_UPDATE_PROXY: the proxy of the root helper and
// `sudo picache update` (read from /etc/picache/picache.env only; never
// the service's settings, whose secrets root never unseals).
type UpdateProxy struct {
	Scheme string // http | socks5
	Host   string
	Port   int
}

// Origin returns scheme://host:port.
func (p UpdateProxy) Origin() string {
	return p.Scheme + "://" + net.JoinHostPort(p.Host, strconv.Itoa(p.Port))
}

// ErrUpdateProxy names the variable of an invalid value.
var ErrUpdateProxy = errors.New("PICACHE_UPDATE_PROXY must be http://host:port or socks5://host:port without a user name or password")

// ParseUpdateProxy parses PICACHE_UPDATE_PROXY ("" = none: nil).
func ParseUpdateProxy(s string) (*UpdateProxy, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	u, err := url.Parse(s)
	if err != nil || u.User != nil || u.Opaque != "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery ||
		u.Fragment != "" || strings.Contains(s, "#") || len(s) > 255 {
		return nil, ErrUpdateProxy
	}
	scheme := strings.ToLower(u.Scheme)
	port, err := strconv.Atoi(u.Port())
	host := strings.ToLower(u.Hostname())
	if (scheme != "http" && scheme != "socks5") || err != nil || port < 1 || port > 65535 || host == "" || strings.Contains(host, "%") {
		return nil, ErrUpdateProxy
	}
	return &UpdateProxy{Scheme: scheme, Host: host, Port: port}, nil
}

// ownedByRoot reports a file owned by root (never true where owners are
// unknown). Tests replace it.
var ownedByRoot = func(fi os.FileInfo) bool {
	uid, _, ok := fileOwner(fi)
	return ok && uid == 0
}
