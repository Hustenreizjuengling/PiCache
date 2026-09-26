package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"net/netip"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// Apple configuration profiles for encrypted DNS (docs/ARCHITECTURE.md
// 19): an unsigned XML property list with one payload of type
// com.apple.dnsSettings.managed, written with encoding/xml escaping for
// every string (no plist dependency).

// Limits of the profile options.
const (
	maxProfileSSIDs   = 16
	maxSSIDBytes      = 32
	maxProfileAddrs   = 8 // per family
	profileMediaType  = "application/x-apple-aspen-config"
	errSSIDs          = "at most 16 Wi-Fi names of 1–32 bytes without control characters"
	errProfileProto   = "must be doh or dot"
	profileIDPrefix   = "org.picache.dns."
	profileProtoDoH   = "doh"
	profileProtoDoT   = "dot"
	profileNoInstance = "picache"
)

// ProfileOptions are the options of a configuration profile (the query of
// GET /dns/profile.mobileconfig and the body of POST /dns/profile-links).
type ProfileOptions struct {
	Protocol    string   `json:"protocol"`
	DNSClientID string   `json:"dnsClientId,omitempty"`
	SSIDs       []string `json:"ssids,omitempty"`
	Addresses   bool     `json:"addresses,omitempty"`
}

// normalize validates the options (the 400s of the profile checks, in
// order: protocol, ClientID, Wi-Fi names) and normalises them.
func (o *ProfileOptions) normalize() error {
	o.Protocol = strings.ToLower(strings.TrimSpace(o.Protocol))
	if o.Protocol != profileProtoDoH && o.Protocol != profileProtoDoT {
		return apperr.Invalid("protocol", errProfileProto)
	}
	if v := strings.TrimSpace(o.DNSClientID); v != "" {
		id, ok := settings.NormalizeClientID(v)
		if !ok {
			return apperr.Invalid("dnsClientId", settings.ErrClientID)
		}
		o.DNSClientID = id
	} else {
		o.DNSClientID = ""
	}
	if len(o.SSIDs) > maxProfileSSIDs {
		return apperr.Invalid("ssids", errSSIDs)
	}
	for _, s := range o.SSIDs {
		if !validSSID(s) {
			return apperr.Invalid("ssids", errSSIDs)
		}
	}
	return nil
}

// validSSID reports whether s can be a Wi-Fi name of a profile: 1–32 bytes
// of valid UTF-8 without control characters.
func validSSID(s string) bool {
	if len(s) == 0 || len(s) > maxSSIDBytes || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return false
		}
	}
	return true
}

// profileTarget is what a profile points devices at.
type profileTarget struct {
	serverName string
	dohPort    int          // the port of the DoH URL (443: omitted)
	instance   string       // the instance id
	addrs      []netip.Addr // ServerAddresses (option addresses)
}

// profileInstance returns the instance part of the identifiers: the first
// 8 characters of the instance id without its "picache-" prefix (letters
// and digits only).
func profileInstance(id string) string {
	id = strings.TrimPrefix(strings.ToLower(id), "picache-")
	var b strings.Builder
	for _, r := range id {
		if b.Len() == 8 {
			break
		}
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return profileNoInstance
	}
	return b.String()
}

// profileIdentifier is stable per instance, protocol and ClientID, so a
// new profile for the same device replaces the old one.
func profileIdentifier(instance, protocol, clientID string) string {
	id := profileIDPrefix + profileInstance(instance) + "." + protocol
	if clientID != "" {
		id += "." + clientID
	}
	return id
}

// profileUUID derives an upper-case UUID from the SHA-256 of s (version 8,
// RFC 9562 variant).
func profileUUID(s string) string {
	sum := sha256.Sum256([]byte(s))
	b := sum[:16]
	b[6] = b[6]&0x0f | 0x80
	b[8] = b[8]&0x3f | 0x80
	h := strings.ToUpper(hex.EncodeToString(b))
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// profileFilename is the file name of the download.
func profileFilename(o ProfileOptions) string {
	name := "picache-" + o.Protocol
	if o.DNSClientID != "" {
		name += "-" + o.DNSClientID
	}
	return name + ".mobileconfig"
}

// profileURL returns the DoH URL of a profile.
func profileURL(serverName string, port int, clientID string) string {
	u := "https://" + serverName
	if port != 443 && port != 0 {
		u += ":" + strconv.Itoa(port)
	}
	u += "/dns-query"
	if clientID != "" {
		u += "/" + clientID
	}
	return u
}

// plist writes a property list with every string escaped.
type plist struct{ b bytes.Buffer }

func (p *plist) raw(s string) { p.b.WriteString(s) }
func (p *plist) str(s string) {
	p.b.WriteString("<string>")
	_ = xml.EscapeText(&p.b, []byte(s))
	p.b.WriteString("</string>")
}
func (p *plist) key(k string) {
	p.b.WriteString("<key>")
	_ = xml.EscapeText(&p.b, []byte(k))
	p.b.WriteString("</key>")
}
func (p *plist) kv(k, v string) { p.key(k); p.str(v) }
func (p *plist) kint(k string, n int) {
	p.key(k)
	p.raw("<integer>" + strconv.Itoa(n) + "</integer>")
}
func (p *plist) kbool(k string, v bool) {
	p.key(k)
	if v {
		p.raw("<true/>")
	} else {
		p.raw("<false/>")
	}
}

// buildProfile returns the configuration profile of o for t.
func buildProfile(o ProfileOptions, t profileTarget) []byte {
	ident := profileIdentifier(t.instance, o.Protocol, o.DNSClientID)
	kind, long := "DoH", "HTTPS"
	if o.Protocol == profileProtoDoT {
		kind, long = "DoT", "TLS"
	}
	display := "PiCache DNS (" + kind + ")"
	if o.DNSClientID != "" {
		display += " – " + o.DNSClientID
	}
	var p plist
	p.raw(xml.Header)
	p.raw(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	p.raw(`<plist version="1.0"><dict>`)
	p.key("PayloadContent")
	p.raw("<array><dict>")
	p.key("DNSSettings")
	p.raw("<dict>")
	if o.Protocol == profileProtoDoH {
		p.kv("DNSProtocol", "HTTPS")
		p.kv("ServerURL", profileURL(t.serverName, t.dohPort, o.DNSClientID))
	} else {
		p.kv("DNSProtocol", "TLS")
		name := t.serverName
		if o.DNSClientID != "" {
			name = o.DNSClientID + "." + name
		}
		p.kv("ServerName", name)
	}
	if o.Addresses && len(t.addrs) > 0 {
		p.key("ServerAddresses")
		p.raw("<array>")
		for _, a := range t.addrs {
			p.str(a.String())
		}
		p.raw("</array>")
	}
	p.raw("</dict>")
	p.kint("OnDemandEnabled", 1)
	p.key("OnDemandRules")
	p.raw("<array>")
	if len(o.SSIDs) > 0 {
		p.raw("<dict>")
		p.kv("Action", "Connect")
		p.kv("InterfaceTypeMatch", "WiFi")
		p.key("SSIDMatch")
		p.raw("<array>")
		for _, s := range o.SSIDs {
			p.str(s)
		}
		p.raw("</array></dict><dict>")
		p.kv("Action", "Disconnect")
		p.raw("</dict>")
	} else {
		p.raw("<dict>")
		p.kv("Action", "Connect")
		p.raw("</dict>")
	}
	p.raw("</array>")
	p.kv("PayloadIdentifier", ident+".dns")
	p.kv("PayloadType", "com.apple.dnsSettings.managed")
	p.kv("PayloadUUID", profileUUID(ident+".dns"))
	p.kint("PayloadVersion", 1)
	p.raw("</dict></array>")
	p.kv("PayloadDescription", "Sends the DNS queries of this device to PiCache ("+t.serverName+") over DNS over "+long)
	p.kv("PayloadDisplayName", display)
	p.kv("PayloadIdentifier", ident)
	p.kv("PayloadOrganization", "PiCache")
	p.kbool("PayloadRemovalDisallowed", false)
	p.kv("PayloadScope", "System")
	p.kv("PayloadType", "Configuration")
	p.kv("PayloadUUID", profileUUID(ident))
	p.kint("PayloadVersion", 1)
	p.raw("</dict></plist>\n")
	return p.b.Bytes()
}
