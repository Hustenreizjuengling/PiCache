package config

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// Listener roles of the listeners files and System → Network (docs/
// ARCHITECTURE.md 2 "Listeners"), in this order.
const (
	RoleDNS    = "dns"
	RoleCache  = "cache"
	RoleSNI    = "sni"
	RoleWeb    = "web"
	RoleWebTLS = "webTls"
	RoleDoT    = "dot"
	RoleDoH    = "doh"
	RoleNTP    = "ntp"
)

// ListenerRoles are the roles in their order.
var ListenerRoles = []string{RoleDNS, RoleCache, RoleSNI, RoleWeb, RoleWebTLS, RoleDoT, RoleDoH, RoleNTP}

// BoundRole maps a name of the bound listeners (api.ListenerInfo.Bound:
// dns-udp, dns-tcp, web-tls and the roles) to its role and reports
// whether the listener is TCP (dns-udp and ntp are UDP).
func BoundRole(name string) (role string, tcp bool) {
	switch name {
	case "dns-udp":
		return RoleDNS, false
	case "dns-tcp":
		return RoleDNS, true
	case "web-tls":
		return RoleWebTLS, true
	case RoleNTP:
		return RoleNTP, false
	}
	return name, true
}

// ListenerEnv names the variable of each role.
var ListenerEnv = map[string]string{
	RoleDNS: "PICACHE_DNS_LISTEN", RoleCache: "PICACHE_CACHE_LISTEN", RoleSNI: "PICACHE_SNI_LISTEN",
	RoleWeb: "PICACHE_WEB_LISTEN", RoleWebTLS: "PICACHE_WEB_TLS_LISTEN", RoleDoT: "PICACHE_DOT_LISTEN",
	RoleDoH: "PICACHE_DOH_LISTEN", RoleNTP: "PICACHE_NTP_LISTEN",
}

// listenerFlag names the serve flag of each role.
var listenerFlag = map[string]string{
	RoleDNS: "dns-listen", RoleCache: "cache-listen", RoleSNI: "sni-listen", RoleWeb: "web-listen",
	RoleWebTLS: "web-tls-listen", RoleDoT: "dot-listen", RoleDoH: "doh-listen", RoleNTP: "ntp-listen",
}

// Files of the listeners in the data directory: the set saved in the UI
// for the next start, what the running process bound from a file, and the
// last saved set that could not be bound completely.
const (
	ListenersNextFile   = "listeners.next.json"
	ListenersFile       = "listeners.json"
	ListenersFailedFile = "listeners.failed.json"
	// MaxListenersFile bounds a listeners file.
	MaxListenersFile = 16 << 10
	// MaxListenerAddrs bounds the addresses of a role.
	MaxListenerAddrs = 8
)

// DefaultListeners returns the built-in listener addresses ([] = off).
func DefaultListeners() map[string][]string {
	return map[string][]string{
		RoleDNS: {":53"}, RoleCache: {":80"}, RoleSNI: {":443"}, RoleWeb: {":8080"}, RoleWebTLS: {":8443"},
		RoleDoT: {":853"}, RoleDoH: {}, RoleNTP: {},
	}
}

// ListenerSource says where the addresses of a role come from.
type ListenerSource string

// Sources, highest precedence first.
const (
	SourceFlag    ListenerSource = "flag"
	SourceEnv     ListenerSource = "env"
	SourceFile    ListenerSource = "file"
	SourceDefault ListenerSource = "default"
)

// canDisable reports whether a role may be empty ([] = off) in a
// listeners file given the other roles (web only while webTls has an
// address; dns and cache never).
func canDisable(role string, roles map[string][]string) bool {
	switch role {
	case RoleDNS, RoleCache:
		return false
	case RoleWeb:
		return len(roles[RoleWebTLS]) > 0
	}
	return true
}

// CanDisable is canDisable for the API.
func CanDisable(role string, roles map[string][]string) bool { return canDisable(role, roles) }

// ParseListenerAddr checks one listener address: ip:port or :port (IPv6
// in brackets, no zone, port 1–65535). It returns the canonical form.
func ParseListenerAddr(s string) (string, error) {
	host, port, err := net.SplitHostPort(strings.TrimSpace(s))
	if err != nil {
		return "", errors.New("must be ip:port or :port")
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 1 || p > 65535 || strconv.Itoa(p) != port {
		return "", errors.New("the port must be between 1 and 65535")
	}
	if host == "" {
		return ":" + port, nil
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || ip.Zone() != "" {
		return "", errors.New("must be ip:port or :port with an IP address (no host name, no zone)")
	}
	return netip.AddrPortFrom(ip.Unmap(), uint16(p)).String(), nil
}

// ParseListeners decodes a listeners file strictly: {"version":1,
// "<role>":[addr, …], …} (unknown members, duplicate names and other
// versions refused); an absent role is not set, [] is off where allowed;
// at most 8 addresses per role, each valid and listed once.
func ParseListeners(b []byte) (map[string][]string, error) {
	var raw map[string]jsontext.Value
	if err := json.Unmarshal(b, &raw, json.RejectUnknownMembers(true)); err != nil {
		return nil, fmt.Errorf("not a listeners file: %w", err)
	}
	out := map[string][]string{}
	version := 0
	for k, v := range raw {
		if k == "version" {
			if err := json.Unmarshal(v, &version); err != nil {
				return nil, errors.New("version must be 1")
			}
			continue
		}
		if !slices.Contains(ListenerRoles, k) {
			return nil, fmt.Errorf("unknown member %q", k)
		}
		var addrs []string
		if err := json.Unmarshal(v, &addrs); err != nil || addrs == nil {
			return nil, fmt.Errorf("%s must be a list of addresses", k)
		}
		out[k] = addrs
	}
	if version != 1 {
		return nil, errors.New("version must be 1")
	}
	if err := ValidateListeners(out); err != nil {
		return nil, err
	}
	return out, nil
}

// ValidateListeners checks a set of roles (the syntax rules of the
// files): known roles, at most 8 addresses each, valid and listed once,
// [] only where allowed. Addresses are made canonical in place.
func ValidateListeners(roles map[string][]string) error {
	for _, role := range ListenerRoles {
		addrs, ok := roles[role]
		if !ok {
			continue
		}
		if len(addrs) > MaxListenerAddrs {
			return &ListenerError{Field: "listeners." + role, Msg: fmt.Sprintf("at most %d addresses", MaxListenerAddrs)}
		}
		if len(addrs) == 0 && !canDisable(role, roles) {
			if role == RoleWeb {
				return &ListenerError{Field: "listeners.web", Msg: "keep a web listener"}
			}
			return &ListenerError{Field: "listeners." + role, Msg: "at least one address"}
		}
		seen := map[string]bool{}
		for i, a := range addrs {
			c, err := ParseListenerAddr(a)
			if err != nil {
				return &ListenerError{Field: fmt.Sprintf("listeners.%s[%d]", role, i), Msg: err.Error()}
			}
			if seen[c] {
				return &ListenerError{Field: fmt.Sprintf("listeners.%s[%d]", role, i), Msg: "listed twice"}
			}
			seen[c] = true
			addrs[i] = c
		}
	}
	for role := range roles {
		if !slices.Contains(ListenerRoles, role) {
			return &ListenerError{Field: "listeners." + role, Msg: "unknown role"}
		}
	}
	return nil
}

// ListenerError is a validation error of a listener set (the API maps it
// to 400 with the field).
type ListenerError struct {
	Field, Msg string
}

func (e *ListenerError) Error() string { return e.Field + ": " + e.Msg }

// EncodeListeners encodes a set of roles as a listeners file (version 1,
// roles in their order).
func EncodeListeners(roles map[string][]string) []byte {
	var b bytes.Buffer
	b.WriteString(`{"version":1`)
	for _, role := range ListenerRoles {
		addrs, ok := roles[role]
		if !ok {
			continue
		}
		if addrs == nil {
			addrs = []string{}
		}
		v, _ := json.Marshal(addrs)
		fmt.Fprintf(&b, ",%q:%s", role, v)
	}
	b.WriteString("}\n")
	return b.Bytes()
}

// WriteListenersFile writes a listeners file into dir through a temporary
// file (O_CREAT|O_EXCL|O_NOFOLLOW, 0640) and a rename.
func WriteListenersFile(dir, name string, data []byte) error {
	tmp := filepath.Join(dir, "."+name+".tmp")
	_ = os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|oNoFollow, 0o640)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// ReadListenersFile reads a listeners file of the data directory with the
// checks of files that root may read: opened without following a link and
// without blocking, a regular file with one link, owned by the owner of
// the data directory, at most 16 KiB; then ParseListeners. A missing file
// gives (nil, nil). Errors never quote the content.
func ReadListenersFile(dataDir, name string) (map[string][]string, error) {
	b, err := readOwnedFile(dataDir, filepath.Join(dataDir, name), MaxListenersFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	roles, err := ParseListeners(b)
	if err != nil {
		var le *ListenerError
		if errors.As(err, &le) {
			return nil, fmt.Errorf("%s is invalid (%s)", name, le.Field)
		}
		return nil, fmt.Errorf("%s is invalid", name)
	}
	return roles, nil
}

// Listeners is the effective listener configuration of the CLI tools.
type Listeners struct {
	Roles  map[string][]string
	Source map[string]ListenerSource
}

// EffectiveListeners computes the addresses of every role for the CLI
// tools (healthcheck, the update helper's health wait, the API client, the
// support bundle): PICACHE_*_LISTEN > listeners.json (what the running
// process bound from the file) > default. The file is ignored when
// PICACHE_RUN_AS is set (Docker: root reads nothing the service can
// write) or when it fails the checks of ReadListenersFile (warnings, which
// never quote its content). serve's flags come on top of this (Load).
func EffectiveListeners(getenv func(string) string, dataDir string) (Listeners, []string) {
	out := Listeners{Roles: DefaultListeners(), Source: map[string]ListenerSource{}}
	for _, r := range ListenerRoles {
		out.Source[r] = SourceDefault
	}
	var warnings []string
	if strings.TrimSpace(getenv("PICACHE_RUN_AS")) == "" && dataDir != "" {
		file, err := ReadListenersFile(dataDir, ListenersFile)
		if err != nil {
			warnings = append(warnings, "the saved listeners are ignored: "+err.Error())
		}
		for role, addrs := range file {
			out.Roles[role], out.Source[role] = addrs, SourceFile
		}
	}
	for role, env := range ListenerEnv {
		if v := getenv(env); v != "" {
			out.Roles[role], out.Source[role] = splitList(v), SourceEnv
		}
	}
	return out, warnings
}
