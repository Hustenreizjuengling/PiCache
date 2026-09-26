package app

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/hustenreizjuengling/picache/internal/netutil"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// Lifetimes of the local CA and its leaf (docs/ARCHITECTURE.md 2).
const (
	caLifetime     = 10 * 365 * 24 * time.Hour
	leafLifetime   = 825 * 24 * time.Hour
	certBackdate   = time.Hour
	renewBefore    = 30 * 24 * time.Hour // a leaf (local CA or legacy self-signed) is replaced this long before it expires
	sanRecheckWait = time.Hour           // a leaf is re-issued for changed names at most this often
	maxCASubject   = 64
)

// hostIdentity is what PiCache calls itself: its own names (N), its own
// addresses (A) and the extra hosts of the web settings (E).
type hostIdentity struct {
	hostname    string       // lower-case, "" if unknown
	names       []string     // N: localhost, picache, the host name, dns.serverNames; single-label ones also with the local and search domains
	addrs       []netip.Addr // A: 127.0.0.1, ::1 and the stable private addresses of up, non-virtual interfaces
	extra       []string     // E: web.allowedHosts and PICACHE_WEB_HOSTS
	localDomain string
	search      []string
}

// ownNames returns N for a host name, the settings and the resolv.conf
// search domains (lower-case, without a trailing dot, no duplicates).
func ownNames(hostname string, s *settings.All, search []string) []string {
	domains := append([]string{s.DNS.LocalDomain}, search...)
	var out []string
	add := func(n string) {
		n = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(n), "."))
		if n == "" || !settings.ValidHostname(n) || slices.Contains(out, n) {
			return
		}
		out = append(out, n)
		if !strings.Contains(n, ".") {
			for _, d := range domains {
				d = strings.ToLower(strings.Trim(strings.TrimSpace(d), "."))
				if fqdn := n + "." + d; d != "" && settings.ValidHostname(fqdn) && !slices.Contains(out, fqdn) {
					out = append(out, fqdn)
				}
			}
		}
	}
	add("localhost")
	add("picache")
	add(hostname)
	for _, n := range s.DNS.ServerNames {
		add(n)
	}
	return out
}

// ownAddrs returns A: the loopback addresses and the addresses of up,
// non-virtual interfaces without link-local, multicast, temporary,
// deprecated and tentative addresses and without public IPv6 addresses (a
// provider's prefix changes; such hosts are reached by name). In a
// container bridge network the container's own addresses are left out
// (clients reach it through the host).
func ownAddrs(host []netutil.HostAddr, bridge bool) []netip.Addr {
	out := []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.IPv6Loopback()}
	if bridge {
		return out
	}
	for _, h := range host {
		ip := netutil.Canon(h.Prefix.Addr())
		if !h.Up || h.Loopback || netutil.VirtualInterface(h.Iface) || h.Temporary || h.Deprecated || h.Tentative ||
			!ip.IsValid() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsMulticast() || ip.IsUnspecified() ||
			(ip.Is6() && !netutil.IsULA(ip)) || slices.Contains(out, ip) {
			continue
		}
		out = append(out, ip)
	}
	return out
}

// extraHosts returns E: web.allowedHosts and PICACHE_WEB_HOSTS.
func extraHosts(s *settings.All, webHosts []string) []string {
	var out []string
	for _, h := range slices.Concat(s.Web.AllowedHosts, webHosts) {
		h = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(h), "."))
		if h != "" && !slices.Contains(out, h) {
			out = append(out, h)
		}
	}
	return out
}

// hostList returns N ∪ A ∪ E as sorted strings: names alphabetically, then
// addresses.
func (id hostIdentity) hostList() []string {
	var names []string
	var addrs []netip.Addr
	for _, n := range slices.Concat(id.names, id.extra) {
		if ip, err := netip.ParseAddr(strings.Trim(n, "[]")); err == nil {
			if ip = netutil.Canon(ip); !slices.Contains(addrs, ip) {
				addrs = append(addrs, ip)
			}
		} else if !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	for _, ip := range id.addrs {
		if !slices.Contains(addrs, ip) {
			addrs = append(addrs, ip)
		}
	}
	slices.Sort(names)
	slices.SortFunc(addrs, func(a, b netip.Addr) int { return a.Compare(b) })
	out := names
	for _, ip := range addrs {
		out = append(out, ip.String())
	}
	return out
}

// coverage splits hostList by whether leaf covers each entry (x509
// VerifyHostname rules, wildcards included). Both lists are never nil.
func (id hostIdentity) coverage(leaf *x509.Certificate) (covered, notCovered []string) {
	covered, notCovered = []string{}, []string{}
	for _, h := range id.hostList() {
		if leaf != nil && leaf.VerifyHostname(h) == nil {
			covered = append(covered, h)
		} else {
			notCovered = append(notCovered, h)
		}
	}
	return covered, notCovered
}

// caConstraints returns the name constraints of a new local CA: the own
// names without those that equal the local domain or a search domain or
// are a parent of one (at a label boundary), and the own addresses as
// single-address ranges; a family without any address is excluded as a
// whole, so IP addresses are never unconstrained.
func (id hostIdentity) caConstraints() (names []string, permitted, excluded []*net.IPNet) {
	for _, n := range id.names {
		if !broadName(n, id.localDomain, id.search) {
			names = append(names, n)
		}
	}
	var has4, has6 bool
	for _, ip := range id.addrs {
		bits := ip.BitLen()
		permitted = append(permitted, &net.IPNet{IP: net.IP(ip.AsSlice()), Mask: net.CIDRMask(bits, bits)})
		has4, has6 = has4 || ip.Is4(), has6 || ip.Is6()
	}
	if !has4 {
		excluded = append(excluded, &net.IPNet{IP: net.IPv4zero.To4(), Mask: net.CIDRMask(0, 32)})
	}
	if !has6 {
		excluded = append(excluded, &net.IPNet{IP: net.IPv6zero, Mask: net.CIDRMask(0, 128)})
	}
	return names, permitted, excluded
}

// broadName reports whether n equals the local domain or a search domain,
// or is a parent of one: a constraint for it would let the CA sign names
// of other devices.
func broadName(n, localDomain string, search []string) bool {
	for _, d := range append([]string{localDomain}, search...) {
		d = strings.ToLower(strings.Trim(strings.TrimSpace(d), "."))
		if d != "" && (d == n || strings.HasSuffix(d, "."+n)) {
			return true
		}
	}
	return false
}

// permitsName reports whether ca's DNS constraints permit n (the name or a
// subdomain of a permitted name; x509 semantics).
func permitsName(ca *x509.Certificate, n string) bool {
	for _, c := range ca.PermittedDNSDomains {
		if strings.HasPrefix(c, ".") {
			if strings.HasSuffix(n, c) {
				return true
			}
		} else if n == c || strings.HasSuffix(n, "."+c) {
			return true
		}
	}
	return false
}

// permitsAddr reports whether ca's IP constraints permit ip.
func permitsAddr(ca *x509.Certificate, ip netip.Addr) bool {
	nip := net.IP(ip.AsSlice())
	for _, ex := range ca.ExcludedIPRanges {
		if ex.Contains(nip) {
			return false
		}
	}
	for _, p := range ca.PermittedIPRanges {
		if len(p.IP) == len(nip) && p.Contains(nip) {
			return true
		}
	}
	return false
}

// leafSANs returns (N ∪ A) ∩ the CA's constraints, and the entries outside
// them (renewal needed: only a new CA covers them).
func (id hostIdentity) leafSANs(ca *x509.Certificate) (names []string, addrs []netip.Addr, outside []string) {
	for _, n := range id.names {
		if permitsName(ca, n) {
			names = append(names, n)
		} else {
			outside = append(outside, n)
		}
	}
	for _, ip := range id.addrs {
		if permitsAddr(ca, ip) {
			addrs = append(addrs, ip)
		} else {
			outside = append(outside, ip.String())
		}
	}
	return names, addrs, outside
}

// localCA is PiCache's CA with its key (<data>/tls/ca.crt, ca.key).
type localCA struct {
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	certPEM []byte
}

func randomSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
}

// caSubject returns "PiCache local CA <host name> <instance>" of at most 64
// bytes (the host name is shortened). instance is the instance id without
// its "picache-" prefix, shortened to 8 characters.
func caSubject(hostname, instanceID string) string {
	inst := strings.TrimPrefix(instanceID, "picache-")
	if len(inst) > 8 {
		inst = inst[:8]
	}
	const prefix = "PiCache local CA"
	room := maxCASubject - len(prefix) - 2 - len(inst)
	if hostname == "" || room <= 0 {
		return strings.TrimSpace(prefix + " " + inst)
	}
	if len(hostname) > room {
		hostname = hostname[:room]
	}
	return strings.TrimSpace(prefix + " " + hostname + " " + inst)
}

// newLocalCA creates a CA (ECDSA P-256, 10 years, path length 0, critical
// name constraints for PiCache's own names and addresses).
func newLocalCA(id hostIdentity, instanceID string, now time.Time) (*localCA, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, nil, err
	}
	names, permitted, excluded := id.caConstraints()
	tmpl := &x509.Certificate{
		SerialNumber:                serial,
		Subject:                     pkix.Name{CommonName: caSubject(id.hostname, instanceID), Organization: []string{"PiCache"}},
		NotBefore:                   now.Add(-certBackdate),
		NotAfter:                    now.Add(caLifetime),
		KeyUsage:                    x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid:       true,
		IsCA:                        true,
		MaxPathLen:                  0,
		MaxPathLenZero:              true,
		PermittedDNSDomainsCritical: true,
		PermittedDNSDomains:         names,
		PermittedIPRanges:           permitted,
		ExcludedIPRanges:            excluded,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return &localCA{cert: cert, key: key, certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})},
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), nil
}

// parseLocalCA loads a CA from its PEM files.
func parseLocalCA(certPEM, keyPEM []byte) (*localCA, error) {
	cb, _ := pem.Decode(certPEM)
	kb, _ := pem.Decode(keyPEM)
	if cb == nil || cb.Type != "CERTIFICATE" || kb == nil || kb.Type != "PRIVATE KEY" {
		return nil, errors.New("not a PEM certificate and PKCS#8 key")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, err
	}
	k, err := x509.ParsePKCS8PrivateKey(kb.Bytes)
	if err != nil {
		return nil, err
	}
	key, ok := k.(*ecdsa.PrivateKey)
	if !ok || !cert.IsCA || !key.PublicKey.Equal(cert.PublicKey) {
		return nil, errors.New("the CA key does not match its certificate")
	}
	return &localCA{cert: cert, key: key, certPEM: pem.EncodeToMemory(cb)}, nil
}

// issueLeaf issues the HTTPS certificate: a new P-256 key, the SANs
// (N ∪ A) within the CA's constraints, 825 days but never beyond the CA.
// It returns the certificate and key PEM.
func (ca *localCA) issueLeaf(id hostIdentity, now time.Time) (certPEM, keyPEM []byte, err error) {
	names, addrs, _ := id.leafSANs(ca.cert)
	if len(names) == 0 && len(addrs) == 0 {
		return nil, nil, errors.New("none of PiCache's names and addresses is within the local CA's constraints")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, nil, err
	}
	notAfter := now.Add(leafLifetime)
	if notAfter.After(ca.cert.NotAfter) {
		notAfter = ca.cert.NotAfter
	}
	cn := "PiCache"
	if id.hostname != "" && (!strings.Contains(id.hostname, ".") || permitsName(ca.cert, id.hostname)) {
		cn = id.hostname // a dotted host name only within the constraints (some verifiers check the CN too)
	}
	ips := make([]net.IP, 0, len(addrs))
	for _, ip := range addrs {
		ips = append(ips, net.IP(ip.AsSlice()))
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cn, Organization: []string{"PiCache"}},
		NotBefore:             now.Add(-certBackdate),
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              names,
		IPAddresses:           ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), nil
}

// emergencyCert returns an in-memory self-signed certificate for when
// nothing else could be loaded or created: the HTTPS listener is never
// left without a certificate.
func emergencyCert(id hostIdentity, now time.Time) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, nil, err
	}
	ips := make([]net.IP, 0, len(id.addrs))
	for _, ip := range id.addrs {
		ips = append(ips, net.IP(ip.AsSlice()))
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "PiCache", Organization: []string{"PiCache self-signed"}},
		NotBefore:             now.Add(-certBackdate),
		NotAfter:              now.Add(leafLifetime),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              id.names,
		IPAddresses:           ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("emergency certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), nil
}
