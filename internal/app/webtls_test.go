package app

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/asn1"
	"encoding/pem"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/netutil"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type tlsEnv struct {
	m     *webTLS
	set   *settings.Store
	log   *syncBuffer
	dir   string
	mu    sync.Mutex
	clock time.Time
	addrs []netutil.HostAddr
}

func (e *tlsEnv) advance(d time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.clock = e.clock.Add(d)
}

func (e *tlsEnv) setAddrs(a ...netutil.HostAddr) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.addrs = a
}

func hostAddr(iface, cidr string) netutil.HostAddr {
	return netutil.HostAddr{Iface: iface, Prefix: netip.MustParsePrefix(cidr), Up: true}
}

// newTLSEnv returns a manager for a temporary data directory with the host
// "pihost" (192.168.1.10, fd00::10, a public IPv6 address, link-local and
// docker addresses that are left out), local domain "lan", no search
// domains, and a clock that only moves when the test says so.
func newTLSEnv(t *testing.T, certFile, keyFile string) *tlsEnv {
	t.Helper()
	dir := t.TempDir()
	d, err := db.Open(filepath.Join(dir, "picache.db"), 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	set, err := settings.Open(context.Background(), d, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	e := &tlsEnv{set: set, log: &syncBuffer{}, dir: dir, clock: time.Now()}
	e.addrs = []netutil.HostAddr{hostAddr("eth0", "192.168.1.10/24"), hostAddr("eth0", "fd00::10/64"),
		hostAddr("eth0", "2001:db8::10/64"), hostAddr("eth0", "fe80::1/64"), hostAddr("docker0", "172.17.0.1/16")}
	e.m = newWebTLS(dir, certFile, keyFile, true, []string{"proxy.example.com"}, "picache-0123456789ab", set, nil,
		slog.New(slog.NewTextHandler(e.log, nil)))
	e.m.now = func() time.Time { e.mu.Lock(); defer e.mu.Unlock(); return e.clock }
	e.m.hostname = func() (string, error) { return "PiHost", nil }
	e.m.search = func() []string { return nil }
	e.m.hostAddrs = func() []netutil.HostAddr { e.mu.Lock(); defer e.mu.Unlock(); return slices.Clone(e.addrs) }
	return e
}

func (e *tlsEnv) served(t *testing.T) *servedCert {
	t.Helper()
	c := e.m.cur.Load()
	if c == nil {
		t.Fatal("nothing served")
	}
	return c
}

func verifyLeaf(leaf, ca *x509.Certificate, host string) error {
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	_, err := leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: host, CurrentTime: leaf.NotBefore.Add(2 * time.Hour)})
	return err
}

func fileMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != want {
		t.Errorf("%s: mode %v, want %v", path, fi.Mode().Perm(), want)
	}
}

// A new installation creates the local CA at the first start: critical
// name constraints for PiCache's own names and stable addresses, path
// length 0, certificate signing only; its leaf verifies and never outlives
// the CA.
func TestLocalCACreatedAndConstrained(t *testing.T) {
	e := newTLSEnv(t, "", "")
	e.m.start()
	c := e.served(t)
	if c.source != sourceLocalCA {
		t.Fatalf("source %s", c.source)
	}
	tlsDir := filepath.Join(e.dir, "tls")
	fileMode(t, filepath.Join(tlsDir, "ca.crt"), 0o644)
	fileMode(t, filepath.Join(tlsDir, "ca.key"), 0o600)
	fileMode(t, filepath.Join(tlsDir, "cert.pem"), 0o644)
	fileMode(t, filepath.Join(tlsDir, "key.pem"), 0o600)
	ca := e.m.ca.cert
	if !ca.IsCA || ca.MaxPathLen != 0 || !ca.MaxPathLenZero || ca.KeyUsage != x509.KeyUsageCertSign|x509.KeyUsageCRLSign ||
		!ca.PermittedDNSDomainsCritical || len(ca.SubjectKeyId) == 0 || ca.NotAfter.Sub(ca.NotBefore) < 9*365*24*time.Hour {
		t.Fatalf("CA %+v", ca)
	}
	critical := false
	for _, ext := range ca.Extensions {
		if ext.Id.Equal(asn1.ObjectIdentifier{2, 5, 29, 30}) {
			critical = ext.Critical
		}
	}
	if !critical {
		t.Fatal("the name constraints must be critical")
	}
	if !strings.HasPrefix(ca.Subject.CommonName, "PiCache local CA pihost 01234567") || len(ca.Subject.CommonName) > 64 ||
		!slices.Equal(ca.Subject.Organization, []string{"PiCache"}) {
		t.Fatalf("CA subject %q", ca.Subject)
	}
	wantNames := []string{"localhost", "localhost.lan", "picache", "picache.lan", "pihost", "pihost.lan"}
	if got := slices.Sorted(slices.Values(ca.PermittedDNSDomains)); !slices.Equal(got, wantNames) {
		t.Fatalf("permitted names %v, want %v", got, wantNames)
	}
	var addrs []string
	for _, r := range ca.PermittedIPRanges {
		addrs = append(addrs, r.String())
	}
	if want := []string{"127.0.0.1/32", "::1/128", "192.168.1.10/32", "fd00::10/128"}; !slices.Equal(addrs, want) {
		t.Fatalf("permitted addresses %v, want %v", addrs, want)
	}
	leaf := c.chain[0]
	if leaf.IsCA || !slices.Equal(leaf.AuthorityKeyId, ca.SubjectKeyId) || leaf.KeyUsage != x509.KeyUsageDigitalSignature ||
		!slices.Equal(leaf.ExtKeyUsage, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}) || leaf.Subject.CommonName != "pihost" ||
		leaf.NotAfter.After(ca.NotAfter) || leaf.NotAfter.Sub(leaf.NotBefore) > 826*24*time.Hour {
		t.Fatalf("leaf %+v", leaf)
	}
	for _, h := range []string{"picache.lan", "pihost", "localhost", "192.168.1.10", "fd00::10", "::1"} {
		if err := verifyLeaf(leaf, ca, h); err != nil {
			t.Errorf("%s: %v", h, err)
		}
	}
	// cert.pem is the served leaf (a rolled-back 0.10 serves it too).
	b, err := os.ReadFile(filepath.Join(tlsDir, "cert.pem"))
	if err != nil || !bytes.Equal(b, certPEM(leaf)) {
		t.Fatalf("cert.pem is not the served leaf: %v", err)
	}
	st := e.m.Status()
	if st.Source != sourceLocalCA || !st.CAAvailable || st.LocalCA == nil || st.LocalCA.RenewalNeeded || st.Fallback ||
		!slices.Contains(st.HostsCovered, "picache.lan") || !slices.Contains(st.HostsNotCovered, "proxy.example.com") ||
		st.Certificate == nil || st.Certificate.KeyType != "ECDSA P-256" || st.Certificate.ChainLength != 1 {
		t.Fatalf("status %+v", st)
	}
	if b, ok := e.m.CACert(); !ok || !bytes.Equal(b, e.m.ca.certPEM) {
		t.Fatal("CACert")
	}
}

// Leaves the CA's key signs for names and addresses outside its
// constraints fail verification.
func TestLocalCAConstraintsHold(t *testing.T) {
	e := newTLSEnv(t, "", "")
	e.m.start()
	ca := &testCA{cert: e.m.ca.cert, key: e.m.ca.key}
	for _, tc := range []struct {
		name string
		o    leafOpts
		host string
	}{
		{"router name", leafOpts{dns: []string{"fritz.box"}}, "fritz.box"},
		{"other LAN device", leafOpts{dns: []string{"nas.lan"}}, "nas.lan"},
		{"bare local domain", leafOpts{dns: []string{"lan"}}, "lan"},
		{"public address", leafOpts{ips: []net.IP{net.ParseIP("8.8.8.8")}}, "8.8.8.8"},
		{"private address outside", leafOpts{ips: []net.IP{net.ParseIP("192.168.1.11")}}, "192.168.1.11"},
		{"public IPv6", leafOpts{ips: []net.IP{net.ParseIP("2001:db8::10")}}, "2001:db8::10"},
	} {
		_, _, leaf := ca.leaf(t, tc.o)
		if err := verifyLeaf(leaf, ca.cert, tc.host); err == nil {
			t.Errorf("%s: a leaf for %s verifies", tc.name, tc.host)
		}
	}
	// A CA without IPv6 addresses excludes the whole family.
	id := hostIdentity{hostname: "pihost", names: []string{"picache"}, addrs: []netip.Addr{netip.MustParseAddr("192.168.1.10")}, localDomain: "lan"}
	v4only, _, err := newLocalCA(id, "picache-0123456789ab", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(v4only.cert.ExcludedIPRanges) != 1 || v4only.cert.ExcludedIPRanges[0].String() != "::/0" {
		t.Fatalf("excluded %v", v4only.cert.ExcludedIPRanges)
	}
	_, _, leaf := (&testCA{cert: v4only.cert, key: v4only.key}).leaf(t, leafOpts{ips: []net.IP{net.ParseIP("fd00::10")}})
	if err := verifyLeaf(leaf, v4only.cert, "fd00::10"); err == nil {
		t.Fatal("an IPv6 leaf of a CA without IPv6 addresses verifies")
	}
	// The local domain, its parents and the search domains are never
	// permitted, not even as server names.
	id = hostIdentity{names: []string{"picache", "lan", "home.arpa", "arpa", "picache.home.arpa"}, localDomain: "lan", search: []string{"home.arpa"},
		addrs: []netip.Addr{netip.MustParseAddr("127.0.0.1")}}
	names, _, _ := id.caConstraints()
	if !slices.Equal(names, []string{"picache", "picache.home.arpa"}) {
		t.Fatalf("constraint names %v", names)
	}
	// A leaf never outlives the CA.
	lc, _, err := newLocalCA(id, "x", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	cp, _, err := lc.issueLeaf(id, lc.cert.NotAfter.Add(-10*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	blk, _ := pem.Decode(cp)
	short, _ := x509.ParseCertificate(blk.Bytes)
	if !short.NotAfter.Equal(lc.cert.NotAfter) {
		t.Fatalf("leaf %v outlives the CA %v", short.NotAfter, lc.cert.NotAfter)
	}
}

// A new server name or address is outside the CA's constraints: the leaf
// keeps verifying (it never gets such a SAN), and renewalNeeded is
// reported until a new CA covers it. A lost address leaves the leaf.
func TestLocalCARenewalAfterChanges(t *testing.T) {
	e := newTLSEnv(t, "", "")
	e.m.start()
	ca := e.m.ca.cert
	first := e.served(t).chain[0]
	e.setAddrs(hostAddr("eth0", "192.168.1.20/24"), hostAddr("eth0", "fd00::10/64"))
	if _, err := e.set.Update(context.Background(), func(a *settings.All) error {
		a.DNS.ServerNames = append(a.DNS.ServerNames, "dnsbox")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	e.advance(2 * time.Hour)
	e.m.tick()
	leaf := e.served(t).chain[0]
	if leaf.SerialNumber.Cmp(first.SerialNumber) == 0 {
		t.Fatal("the leaf must be re-issued without the lost address")
	}
	if slices.ContainsFunc(leaf.IPAddresses, func(ip net.IP) bool { return ip.String() == "192.168.1.20" }) ||
		slices.Contains(leaf.DNSNames, "dnsbox") || slices.ContainsFunc(leaf.IPAddresses, func(ip net.IP) bool { return ip.String() == "192.168.1.10" }) {
		t.Fatalf("leaf SANs %v %v", leaf.DNSNames, leaf.IPAddresses)
	}
	for _, h := range []string{"picache.lan", "fd00::10"} {
		if err := verifyLeaf(leaf, ca, h); err != nil {
			t.Fatalf("%s: %v", h, err)
		}
	}
	st := e.m.Status()
	if !st.LocalCA.RenewalNeeded || !slices.Contains(st.HostsNotCovered, "dnsbox") || !slices.Contains(st.HostsNotCovered, "192.168.1.20") {
		t.Fatalf("status %+v", st.LocalCA)
	}
	status, msg, hint, _ := e.m.health(e.m.now())
	if status != "warn" || !strings.HasPrefix(msg, "the local CA does not cover ") || !strings.Contains(msg, "dnsbox") ||
		!strings.Contains(hint, "create a new local CA") {
		t.Fatalf("health %s %q %q", status, msg, hint)
	}
	// A new CA includes them.
	info, replaced, err := e.m.NewLocalCA()
	if err != nil || !replaced || info.RenewalNeeded || !slices.Contains(info.PermittedNames, "dnsbox") ||
		!slices.Contains(info.PermittedAddresses, "192.168.1.20") {
		t.Fatalf("new CA %+v %v %v", info, replaced, err)
	}
	if err := verifyLeaf(e.served(t).chain[0], e.m.ca.cert, "192.168.1.20"); err != nil {
		t.Fatal(err)
	}
	// Expiry renewal: 30 days before the leaf expires.
	old := e.served(t).chain[0]
	e.advance(old.NotAfter.Sub(e.m.now()) - 29*24*time.Hour)
	e.m.tick()
	if renewed := e.served(t).chain[0]; !renewed.NotAfter.After(old.NotAfter) {
		t.Fatal("the leaf must be renewed 30 days before it expires")
	}
}

// A self-signed certificate of 0.10 stays until 30 days before it expires;
// then the local CA is created and becomes the source.
func TestLegacySelfSignedSwitch(t *testing.T) {
	e := newTLSEnv(t, "", "")
	tlsDir := filepath.Join(e.dir, "tls")
	if err := os.MkdirAll(tlsDir, 0o750); err != nil {
		t.Fatal(err)
	}
	c, k := legacySelfSigned(t, e.clock.Add(60*24*time.Hour))
	if err := os.WriteFile(filepath.Join(tlsDir, "cert.pem"), c, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tlsDir, "key.pem"), k, 0o600); err != nil {
		t.Fatal(err)
	}
	e.m.start()
	if s := e.served(t); s.source != sourceSelfSigned || !bytes.Equal(certPEM(s.chain[0]), c) {
		t.Fatalf("the legacy certificate must stay: %s", s.source)
	}
	if st := e.m.Status(); st.CAAvailable || !st.Certificate.SelfSigned {
		t.Fatalf("status %+v", st)
	}
	e.advance(31 * 24 * time.Hour)
	e.m.tick()
	s := e.served(t)
	if s.source != sourceLocalCA || e.m.ca == nil {
		t.Fatalf("source %s after the renewal date", s.source)
	}
	b, _ := os.ReadFile(filepath.Join(tlsDir, "cert.pem"))
	if !bytes.Equal(b, certPEM(s.chain[0])) {
		t.Fatal("cert.pem must be the new leaf")
	}
	if !strings.Contains(e.log.String(), "replaced the self-signed web certificate") {
		t.Fatalf("log: %s", e.log)
	}
}

func writeFile(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0o640); err != nil {
		t.Fatal(err)
	}
}

// Certificate files are reloaded by content: a key and a certificate
// replaced in two steps, a broken file keeps the previous certificate and
// is retried until it is good, and a replacement with the same mtime is
// noticed.
func TestFilesReloadByContent(t *testing.T) {
	ca := newTestCA(t, "LE", nil)
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "fullchain.pem"), filepath.Join(dir, "privkey.pem")
	aCert, aKey, a := ca.leaf(t, leafOpts{dns: []string{"picache.example.com"}})
	writeFile(t, certFile, aCert)
	writeFile(t, keyFile, aKey)
	e := newTLSEnv(t, certFile, keyFile)
	e.m.start()
	if s := e.served(t); s.source != sourceFiles || !s.chain[0].Equal(a) {
		t.Fatalf("source %s", s.source)
	}
	if e.m.ca != nil {
		t.Fatal("no local CA is needed while the files work")
	}
	// Step 1 of a deploy hook: the new key.
	bCert, bKey, b := ca.leaf(t, leafOpts{dns: []string{"picache.example.com"}})
	writeFile(t, keyFile, bKey)
	e.m.tick()
	e.m.tick()
	st := e.m.Status()
	if !e.served(t).chain[0].Equal(a) || st.Error == "" || st.Fallback || st.Source != sourceFiles {
		t.Fatalf("a broken pair must keep the previous certificate: %+v", st)
	}
	if n := strings.Count(e.log.String(), "could not load the changed web certificate files"); n != 1 {
		t.Fatalf("logged %d times, want once per content", n)
	}
	status, msg, _, _ := e.m.health(e.m.now())
	if status != "warn" || !strings.HasPrefix(msg, "could not load the changed certificate files: ") ||
		!strings.HasSuffix(msg, "; the previous certificate is still in use") {
		t.Fatalf("health %s %q", status, msg)
	}
	// Step 2: the certificate.
	writeFile(t, certFile, bCert)
	e.m.tick()
	if st := e.m.Status(); !e.served(t).chain[0].Equal(b) || st.Error != "" {
		t.Fatalf("the complete pair must be loaded: %+v", st)
	}
	if !strings.Contains(e.log.String(), "loaded the new web certificate") {
		t.Fatalf("log: %s", e.log)
	}
	// Same size and mtime, other content.
	fi, _ := os.Stat(certFile)
	cCert, cKey, c := ca.leaf(t, leafOpts{dns: []string{"picache.example.com"}})
	writeFile(t, certFile, cCert)
	writeFile(t, keyFile, cKey)
	_ = os.Chtimes(certFile, fi.ModTime(), fi.ModTime())
	_ = os.Chtimes(keyFile, fi.ModTime(), fi.ModTime())
	e.m.tick()
	if !e.served(t).chain[0].Equal(c) {
		t.Fatal("a replacement with the same mtime must be loaded")
	}
	// Larger than 1 MiB: an error, the previous certificate stays.
	writeFile(t, certFile, bytes.Repeat([]byte("x"), maxCertFileSize+1))
	e.m.tick()
	if st := e.m.Status(); !e.served(t).chain[0].Equal(c) || !strings.Contains(st.Error, "larger than 1 MiB") {
		t.Fatalf("oversized file: %+v", st)
	}
}

// A broken pair of certificate files at the start still serves HTTPS (the
// local CA's certificate) and fails the health check; the files are
// retried every minute.
func TestBrokenFilesAtStartFallBack(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "fullchain.pem"), filepath.Join(dir, "privkey.pem")
	writeFile(t, certFile, []byte("not a certificate"))
	writeFile(t, keyFile, []byte("not a key"))
	e := newTLSEnv(t, certFile, keyFile)
	e.m.start()
	st := e.m.Status()
	if st.Source != sourceLocalCA || !st.Fallback || st.Error == "" || !st.EnvOverride {
		t.Fatalf("status %+v", st)
	}
	status, msg, hint, _ := e.m.health(e.m.now())
	if status != "fail" || !strings.HasPrefix(msg, "the configured certificate cannot be used: ") ||
		!strings.HasSuffix(msg, "; PiCache serves its local CA certificate instead") || !strings.HasPrefix(hint, "fix PICACHE_WEB_TLS_CERT") {
		t.Fatalf("health %s %q %q", status, msg, hint)
	}
	// A real handshake works.
	ln, err := tls.Listen("tcp", "127.0.0.1:0", e.m.tlsConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.(*tls.Conn).Handshake()
			c.Close()
		}
	}()
	conn, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // test
	if err != nil {
		t.Fatalf("HTTPS must be served: %v", err)
	}
	if !conn.ConnectionState().PeerCertificates[0].Equal(e.served(t).chain[0]) {
		t.Fatal("the fallback certificate must be served")
	}
	conn.Close()
	// The files are fixed: loaded on the next tick.
	ca := newTestCA(t, "LE", nil)
	c, k, leaf := ca.leaf(t, leafOpts{dns: []string{"picache.example.com"}})
	writeFile(t, certFile, c)
	writeFile(t, keyFile, k)
	e.m.tick()
	if st := e.m.Status(); st.Fallback || st.Error != "" || st.Source != sourceFiles || !e.served(t).chain[0].Equal(leaf) {
		t.Fatalf("after the fix: %+v", st)
	}
}

// The minimum TLS version applies to the next handshake, without a restart.
func TestTLSMinVersionPerHandshake(t *testing.T) {
	e := newTLSEnv(t, "", "")
	e.m.start()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", e.m.tlsConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _ = c.(*tls.Conn).Handshake(); c.Close() }()
		}
	}()
	dial := func(max uint16) error {
		c, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{InsecureSkipVerify: true, MaxVersion: max}) //nolint:gosec // test
		if err == nil {
			c.Close()
		}
		return err
	}
	if err := dial(tls.VersionTLS12); err != nil {
		t.Fatalf("TLS 1.2 by default: %v", err)
	}
	if _, err := e.set.Update(context.Background(), func(a *settings.All) error { a.Web.TLSMinVersion = "1.3"; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := dial(tls.VersionTLS12); err == nil {
		t.Fatal("a TLS 1.2 client must be refused right after the change")
	}
	if err := dial(tls.VersionTLS13); err != nil {
		t.Fatal(err)
	}
}

// An upload is stored atomically as uploaded.pem (0600, the key first),
// served at once, kept across restarts; a delete falls back at once. A
// corrupt uploaded.pem at the start serves the fallback and fails the
// health check.
func TestUploadServeDelete(t *testing.T) {
	e := newTLSEnv(t, "", "")
	e.m.start()
	ca := newTestCA(t, "Public CA", nil)
	c, k, leaf := ca.leaf(t, leafOpts{dns: []string{"picache.lan", "*.example.com"}})
	if err := e.m.CheckUpload(string(c), string(k)); err != nil {
		t.Fatal(err)
	}
	info, err := e.m.Upload(string(c), string(k))
	if err != nil || info.Subject != "CN=leaf" {
		t.Fatalf("upload %+v %v", info, err)
	}
	up := filepath.Join(e.dir, "tls", "uploaded.pem")
	fileMode(t, up, 0o600)
	b, _ := os.ReadFile(up)
	if blocks := pemBlocks(b); len(blocks) != 2 || blocks[0].Type != "PRIVATE KEY" {
		t.Fatalf("uploaded.pem blocks %v", blocks)
	}
	entries, _ := os.ReadDir(filepath.Join(e.dir, "tls"))
	for _, en := range entries {
		if strings.Contains(en.Name(), ".tmp") {
			t.Fatalf("temporary file left: %s", en.Name())
		}
	}
	st := e.m.Status()
	if st.Source != sourceUploaded || !st.UploadStored || !e.served(t).chain[0].Equal(leaf) ||
		!slices.Contains(st.HostsCovered, "picache.lan") || !slices.Contains(st.HostsNotCovered, "pihost") {
		t.Fatalf("status %+v", st)
	}
	// A restart loads it again.
	e2 := newTLSEnv(t, "", "")
	e2.m.dataDir = e.dir
	e2.m.start()
	if s := e2.served(t); s.source != sourceUploaded || !s.chain[0].Equal(leaf) {
		t.Fatalf("after a restart: %s", s.source)
	}
	del, err := e.m.DeleteUpload()
	if err != nil || del.FingerprintSHA256 != info.FingerprintSHA256 {
		t.Fatalf("delete %+v %v", del, err)
	}
	if s := e.served(t); s.source != sourceLocalCA {
		t.Fatalf("after the delete: %s", s.source)
	}
	if _, err := e.m.DeleteUpload(); apperr.KindOf(err) != apperr.KindNotFound {
		t.Fatalf("second delete: %v", err)
	}
	// Corrupt at the start.
	writeFile(t, up, []byte("-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n"))
	e3 := newTLSEnv(t, "", "")
	e3.m.dataDir = e.dir
	e3.m.start()
	st = e3.m.Status()
	if st.Source != sourceLocalCA || !st.Fallback || st.Error == "" || !st.UploadStored {
		t.Fatalf("corrupt upload: %+v", st)
	}
	status, msg, hint, _ := e3.m.health(e3.m.now())
	if status != "fail" || !strings.Contains(msg, "the configured certificate cannot be used") ||
		hint != "upload the certificate again or delete it under System > HTTPS certificate" {
		t.Fatalf("health %s %q %q", status, msg, hint)
	}
	if strings.Contains(e.log.String()+e2.log.String()+e3.log.String(), "PRIVATE KEY") {
		t.Fatal("a log line contains key material")
	}
}

// A symbolic link in place of the TLS directory is refused (the fallback
// is an in-memory certificate, HTTPS keeps working).
func TestTLSDirSymlinkRefused(t *testing.T) {
	e := newTLSEnv(t, "", "")
	other := t.TempDir()
	if err := os.Symlink(other, filepath.Join(e.dir, "tls")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	e.m.start()
	if s := e.served(t); s.source != sourceSelfSigned || !e.m.baseTemp {
		t.Fatalf("source %s", s.source)
	}
	if entries, _ := os.ReadDir(other); len(entries) != 0 {
		t.Fatalf("files written through the link: %v", entries)
	}
}

// The in-memory emergency certificate is reported (status error, health
// warning), kept while the TLS directory stays unusable (no new
// certificate every hour) and replaced by the local CA once it works.
func TestTLSTemporaryCertificateReported(t *testing.T) {
	e := newTLSEnv(t, "", "")
	blocker := filepath.Join(e.dir, "tls")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.m.start()
	st := e.m.Status()
	if st.Source != sourceSelfSigned || st.Fallback || !strings.HasPrefix(st.Error, "the TLS directory cannot be used: ") ||
		!strings.Contains(st.Error, "not a directory") {
		t.Fatalf("status %+v", st)
	}
	status, msg, hint, _ := e.m.health(e.m.now())
	if status != "warn" || msg != st.Error || !strings.Contains(hint, "<data>/tls") {
		t.Fatalf("health %s %q %q", status, msg, hint)
	}
	first := e.served(t).chain[0].SerialNumber
	e.advance(2 * time.Hour)
	e.m.tick()
	if got := e.served(t).chain[0].SerialNumber; got.Cmp(first) != 0 {
		t.Fatal("a new temporary certificate was served while the directory is still unusable")
	}
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	e.advance(2 * time.Hour)
	e.m.tick()
	if st := e.m.Status(); st.Source != sourceLocalCA || st.Error != "" {
		t.Fatalf("after the directory was fixed: %+v", st)
	}
}

// The health check "tls": every row, and none without a listener.
func TestTLSHealthRows(t *testing.T) {
	e := newTLSEnv(t, "", "")
	e.m.start()
	if st, msg, _, show := e.m.health(e.m.now()); !show || st != "ok" || msg != "" {
		t.Fatalf("fresh: %s %q", st, msg)
	}
	// The local CA expires within 90 days (the leaf is capped at the CA).
	e.advance(e.m.ca.cert.NotAfter.Sub(e.m.now()) - 60*24*time.Hour)
	e.m.tick()
	st, msg, hint, _ := e.m.health(e.m.now())
	if st != "warn" || !strings.HasPrefix(msg, "the local CA expires in ") ||
		hint != "create a new local CA under System > HTTPS certificate and trust it on your devices" {
		t.Fatalf("CA expiry: %s %q %q", st, msg, hint)
	}

	// An uploaded certificate that expires within 21 days, then expired.
	e = newTLSEnv(t, "", "")
	e.m.start()
	ca := newTestCA(t, "Public CA", nil)
	c, k, leaf := ca.leaf(t, leafOpts{dns: []string{"picache.lan"}, notAfter: e.clock.Add(10 * 24 * time.Hour)})
	if _, err := e.m.Upload(string(c), string(k)); err != nil {
		t.Fatal(err)
	}
	st, msg, hint, _ = e.m.health(e.m.now())
	date := leaf.NotAfter.UTC().Format(time.DateOnly)
	if st != "warn" || msg != "the web certificate expires in 9 days ("+date+")" && msg != "the web certificate expires in 10 days ("+date+")" ||
		hint != "upload a renewed certificate under System > HTTPS certificate" {
		t.Fatalf("expiring: %s %q %q", st, msg, hint)
	}
	e.advance(11 * 24 * time.Hour)
	st, msg, _, _ = e.m.health(e.m.now())
	if st != "fail" || msg != "the web certificate expired on "+date {
		t.Fatalf("expired: %s %q", st, msg)
	}

	// No row without an HTTPS listener.
	e = newTLSEnv(t, "", "")
	e.m.listener = false
	e.m.start()
	if _, _, _, show := e.m.health(e.m.now()); show {
		t.Fatal("no tls check without a listener")
	}
	if st := e.m.Status(); st.Listener || st.Source != sourceNone || st.Certificate != nil || len(st.HostsCovered) != 0 {
		t.Fatalf("status without a listener %+v", st)
	}
	if _, err := os.Stat(filepath.Join(e.dir, "tls", "ca.crt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("nothing is created without a listener")
	}
	if _, _, err := e.m.NewLocalCA(); apperr.KindOf(err) != apperr.KindConflict {
		t.Fatalf("new CA without a listener: %v", err)
	}
}

// The own names and addresses: host name and server names with the local
// and search domains; stable private addresses only; the container's own
// addresses are left out in a bridge network.
func TestHostIdentity(t *testing.T) {
	s := settings.Defaults()
	s.DNS.ServerNames = []string{"picache", "dns.example.com"}
	names := ownNames("PiHost", &s, []string{"fritz.box"})
	want := []string{"localhost", "localhost.lan", "localhost.fritz.box", "picache", "picache.lan", "picache.fritz.box",
		"pihost", "pihost.lan", "pihost.fritz.box", "dns.example.com"}
	if !slices.Equal(names, want) {
		t.Fatalf("names %v", names)
	}
	temp := hostAddr("eth0", "fd00::99/64")
	temp.Temporary = true
	down := hostAddr("eth1", "10.0.0.5/24")
	down.Up = false
	addrs := ownAddrs([]netutil.HostAddr{hostAddr("eth0", "192.168.1.10/24"), hostAddr("eth0", "203.0.113.5/24"), temp, down,
		hostAddr("eth0", "2001:db8::10/64"), hostAddr("wg0", "10.8.0.1/24"), hostAddr("eth0", "fe80::1/64")}, false)
	var got []string
	for _, a := range addrs {
		got = append(got, a.String())
	}
	if !slices.Equal(got, []string{"127.0.0.1", "::1", "192.168.1.10", "203.0.113.5"}) {
		t.Fatalf("addrs %v", got)
	}
	if got := ownAddrs([]netutil.HostAddr{hostAddr("eth0", "172.18.0.2/16")}, true); len(got) != 2 {
		t.Fatalf("bridge network %v", got)
	}
	if n := caSubject(strings.Repeat("h", 80), "picache-0123456789ab"); len(n) > 64 || !strings.HasSuffix(n, " 01234567") {
		t.Fatalf("subject %q", n)
	}
	id := hostIdentity{names: []string{"picache", "b"}, extra: []string{"a.example", "10.0.0.1"}, addrs: []netip.Addr{netip.MustParseAddr("::1"), netip.MustParseAddr("127.0.0.1")}}
	if got := id.hostList(); !slices.Equal(got, []string{"a.example", "b", "picache", "10.0.0.1", "127.0.0.1", "::1"}) {
		t.Fatalf("host list %v", got)
	}
}

// In the local CA's last 30 days the leaf is renewed once (capped at the
// CA), not on every tick; in its last day a new CA is created.
func TestLocalCAEndOfLife(t *testing.T) {
	e := newTLSEnv(t, "", "")
	e.m.start()
	caEnd := e.m.ca.cert.NotAfter
	e.advance(caEnd.Sub(e.m.now()) - 20*24*time.Hour)
	e.m.tick()
	leaf := e.served(t).chain[0]
	if !leaf.NotAfter.Equal(caEnd) {
		t.Fatalf("leaf %v, want capped at %v", leaf.NotAfter, caEnd)
	}
	e.advance(time.Minute)
	e.m.tick()
	if again := e.served(t).chain[0]; again.SerialNumber.Cmp(leaf.SerialNumber) != 0 {
		t.Fatal("a capped leaf must not be re-issued on every tick")
	}
	oldCA := e.m.ca.cert
	e.advance(caEnd.Sub(e.m.now()) - 12*time.Hour)
	e.m.tick()
	if e.m.ca.cert.Equal(oldCA) || !e.served(t).chain[0].NotAfter.After(caEnd) {
		t.Fatal("a new CA must be created in the old one's last day")
	}
}
