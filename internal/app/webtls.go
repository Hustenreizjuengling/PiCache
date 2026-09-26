package app

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hustenreizjuengling/picache/internal/api"
	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/netutil"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// Sources of the web certificate (api.TLSStatus.Source), in this order of
// precedence: the files of PICACHE_WEB_TLS_CERT/KEY, an uploaded
// certificate, a leaf of the local CA, a self-signed certificate (the
// legacy one of 0.10 until it is due for renewal, or an in-memory
// emergency certificate).
const (
	sourceFiles      = "files"
	sourceUploaded   = "uploaded"
	sourceLocalCA    = "local-ca"
	sourceSelfSigned = "self-signed"
	sourceNone       = "none"
)

// Files in <data>/tls.
const (
	caCertFile   = "ca.crt"
	caKeyFile    = "ca.key"
	leafCertFile = "cert.pem"
	leafKeyFile  = "key.pem"
	uploadedFile = "uploaded.pem"
	tlsDirName   = "tls"

	maxCertFileSize = 1 << 20
)

// Health thresholds of the check "tls".
const (
	leafWarnBefore = 21 * 24 * time.Hour
	caWarnBefore   = 90 * 24 * time.Hour
)

// servedCert is what the HTTPS listener serves.
type servedCert struct {
	*certChain
	source string
}

// webTLS manages the certificate of the HTTPS listener
// (docs/ARCHITECTURE.md 2): it loads the configured source, falls back to
// the local CA or a self-signed certificate when that source is unusable
// (the listener is never left without a certificate), reloads changed
// certificate files by content, renews the local CA's leaf and serves the
// current certificate through GetCertificate. The minimum TLS version is
// applied per handshake (GetConfigForClient). It implements api.WebTLS.
type webTLS struct {
	dataDir    string
	certFile   string // PICACHE_WEB_TLS_CERT ("" = none)
	keyFile    string // PICACHE_WEB_TLS_KEY
	listener   bool   // an HTTPS listener is bound
	webHosts   []string
	instanceID string
	set        *settings.Store
	log        *slog.Logger

	// Sources of the host identity and the clock (tests replace them).
	now       func() time.Time
	hostname  func() (string, error)
	hostAddrs func() []netutil.HostAddr
	search    func() []string
	bridge    func() bool

	cur atomic.Pointer[servedCert]

	mu           sync.Mutex  // serialises loading, reloading and changes
	files        *certChain  // the last good pair of the certificate files
	filesGood    [32]byte    // their content hash
	filesFailed  [32]byte    // the content of the last failed load (logged once)
	filesErr     string      // why the certificate files (their current content) could not be loaded
	upload       *certChain  // the uploaded certificate
	uploadErr    string      // why uploaded.pem could not be loaded
	base         *servedCert // the local CA's leaf or a self-signed certificate
	baseTemp     bool        // base is the in-memory emergency certificate
	baseErr      string      // why the last creation of the local CA or its certificate failed
	retryAt      time.Time   // after a failed renewal: when it is tried again (errors are logged at most hourly)
	ca           *localCA
	caErrLogged  bool
	lastSANCheck time.Time
	checkedAt    time.Time
}

func newWebTLS(dataDir, certFile, keyFile string, listener bool, webHosts []string, instanceID string,
	set *settings.Store, bridge func() bool, log *slog.Logger) *webTLS {
	if bridge == nil {
		bridge = func() bool { return false }
	}
	return &webTLS{
		dataDir: dataDir, certFile: certFile, keyFile: keyFile, listener: listener, webHosts: webHosts,
		instanceID: instanceID, set: set, log: log.With(slog.String("component", "web-tls")),
		now: time.Now, hostname: os.Hostname, hostAddrs: netutil.HostAddrs, search: netutil.ResolvConfSearch, bridge: bridge,
	}
}

// identity returns PiCache's names, addresses and extra hosts now.
func (m *webTLS) identity() hostIdentity {
	s := m.set.Get()
	hn, _ := m.hostname()
	hn = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(hn), "."))
	if !settings.ValidHostname(hn) {
		hn = ""
	}
	search := m.search()
	return hostIdentity{
		hostname: hn, names: ownNames(hn, s, search), addrs: ownAddrs(m.hostAddrs(), m.bridge()),
		extra: extraHosts(s, m.webHosts), localDomain: s.DNS.LocalDomain, search: search,
	}
}

// tlsConfig returns the configuration of the HTTPS listener: the current
// certificate per handshake, the minimum version of the current settings
// (two fixed configurations; the shared one is never changed).
func (m *webTLS) tlsConfig() *tls.Config {
	mk := func(min uint16) *tls.Config {
		return &tls.Config{MinVersion: min, GetCertificate: m.getCertificate, NextProtos: []string{"h2", "http/1.1"}}
	}
	v12, v13 := mk(tls.VersionTLS12), mk(tls.VersionTLS13)
	return &tls.Config{
		MinVersion:     tls.VersionTLS12,
		GetCertificate: m.getCertificate,
		NextProtos:     []string{"h2", "http/1.1"},
		GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
			if m.set.Get().Web.TLSMinVersion == settings.TLSVersion13 {
				return v13, nil
			}
			return v12, nil
		},
	}
}

func (m *webTLS) getCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	if c := m.cur.Load(); c != nil {
		return c.cert, nil
	}
	return nil, errors.New("no web certificate")
}

// start loads the certificate before the HTTPS listener serves.
func (m *webTLS) start() {
	if !m.listener {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	if m.certFile != "" {
		m.reloadFilesLocked()
	} else if err := m.loadUploadLocked(now); err != nil && !errors.Is(err, fs.ErrNotExist) {
		m.uploadErr = err.Error()
		m.log.Error("the uploaded web certificate cannot be used; serving a fallback certificate", slog.Any("err", err))
	}
	m.chooseLocked(now)
}

// tick runs every 60 s and on SIGHUP: changed certificate files are
// loaded, the local CA's leaf is renewed (30 days before it expires, or
// when PiCache's names or addresses changed, at most hourly), and a legacy
// self-signed certificate is replaced by a leaf of the local CA 30 days
// before it expires.
func (m *webTLS) tick() {
	if !m.listener {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.certFile != "" {
		m.reloadFilesLocked()
	}
	m.chooseLocked(m.now())
}

// chooseLocked selects the served certificate by precedence and keeps the
// fallback (the local CA or a self-signed certificate) current while it
// is served.
func (m *webTLS) chooseLocked(now time.Time) {
	if !m.listener {
		return // nothing is created or served without an HTTPS listener
	}
	m.checkedAt = now
	var pick *servedCert
	switch {
	case m.certFile != "" && m.files != nil:
		pick = &servedCert{m.files, sourceFiles}
	case m.certFile == "" && m.upload != nil:
		pick = &servedCert{m.upload, sourceUploaded}
	default:
		pick = m.ensureBaseLocked(now)
	}
	m.cur.Store(pick)
}

// ensureBaseLocked returns the local CA's leaf or the self-signed
// certificate, loading, renewing or creating it as needed. It never fails:
// when nothing can be loaded or written an in-memory certificate is used.
func (m *webTLS) ensureBaseLocked(now time.Time) *servedCert {
	id := m.identity()
	if m.base == nil {
		m.loadBaseLocked()
	}
	b := m.base
	// A leaf of the local CA can be renewed usefully while the CA outlives
	// it; in the CA's last day a new CA is created (the health check warns
	// 90 days before).
	renewable := func() bool {
		return m.ca.cert.NotAfter.After(b.chain[0].NotAfter) || m.ca.cert.NotAfter.Sub(now) < 24*time.Hour
	}
	switch {
	case b == nil:
		m.renewBaseLocked(id, now, "created the local CA and its web certificate")
	case now.Before(m.retryAt):
		// A renewal failed within the last hour: keep what is served.
	case m.baseTemp:
		temp := m.base
		m.base, m.baseTemp = nil, false
		m.renewBaseLocked(id, now, "created the web certificate of the local CA")
		if m.base == nil {
			// Still failing: keep the same temporary certificate, so
			// browsers do not warn about a new one every hour.
			m.base, m.baseTemp = temp, true
		}
	case b.source == sourceSelfSigned && b.chain[0].NotAfter.Sub(now) < renewBefore:
		m.renewBaseLocked(id, now, "replaced the self-signed web certificate, which expires soon, with a certificate of the local CA")
	case b.source == sourceLocalCA && b.chain[0].NotAfter.Sub(now) < renewBefore && renewable():
		m.renewBaseLocked(id, now, "renewed the web certificate of the local CA")
	case b.source == sourceLocalCA && now.Sub(m.lastSANCheck) >= sanRecheckWait:
		m.lastSANCheck = now
		names, addrs, _ := id.leafSANs(m.ca.cert)
		if !sameSANs(b.chain[0], names, addrs) {
			m.renewBaseLocked(id, now, "re-issued the web certificate of the local CA for changed names or addresses")
		}
	}
	if m.base == nil {
		certPEM, keyPEM, err := emergencyCert(id, now)
		if err == nil {
			m.base, err = pairChain(certPEM, keyPEM, sourceSelfSigned)
		}
		if err != nil {
			m.log.Error("no web certificate at all", slog.Any("err", err))
			return nil
		}
		m.baseTemp = true
		m.log.Error("serving a temporary self-signed web certificate (see the earlier errors; retried hourly)")
	}
	return m.base
}

// sameSANs reports whether leaf has exactly these names and addresses.
func sameSANs(leaf *x509.Certificate, names []string, addrs []netip.Addr) bool {
	if !slices.Equal(slices.Sorted(slices.Values(leaf.DNSNames)), slices.Sorted(slices.Values(names))) {
		return false
	}
	var got []netip.Addr
	for _, ip := range leaf.IPAddresses {
		if a, ok := netip.AddrFromSlice(ip); ok {
			got = append(got, netutil.Canon(a))
		}
	}
	cmp := func(a, b netip.Addr) int { return a.Compare(b) }
	return slices.Equal(slices.SortedFunc(slices.Values(got), cmp), slices.SortedFunc(slices.Values(addrs), cmp))
}

// loadCALocked reads the local CA (ca.crt, ca.key) if it is not loaded
// yet; an unreadable CA is logged once.
func (m *webTLS) loadCALocked() {
	if m.ca != nil {
		return
	}
	dir, err := m.openDir(false)
	if err != nil {
		return
	}
	defer dir.Close()
	c, k, err := readPair(dir, caCertFile, caKeyFile)
	if err == nil {
		var ca *localCA
		if ca, err = parseLocalCA(c, k); err == nil {
			m.ca, m.caErrLogged = ca, false
			return
		}
	}
	if !errors.Is(err, fs.ErrNotExist) && !m.caErrLogged {
		m.caErrLogged = true
		m.log.Error("the local CA cannot be read", slog.Any("err", err))
	}
}

// loadBaseLocked reads the CA and cert.pem/key.pem: a leaf of the CA is the
// source local-ca, anything else there is the legacy self-signed
// certificate of 0.10.
func (m *webTLS) loadBaseLocked() {
	m.loadCALocked()
	dir, err := m.openDir(false)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			m.log.Error("cannot open the TLS directory", slog.Any("err", err))
		}
		return
	}
	defer dir.Close()
	c, k, err := readPair(dir, leafCertFile, leafKeyFile)
	if err != nil {
		return
	}
	src := sourceSelfSigned
	if b, err := pairChain(c, k, src); err == nil {
		if m.ca != nil && b.chain[0].CheckSignatureFrom(m.ca.cert) == nil {
			b.source = sourceLocalCA
		}
		m.base = b
	} else {
		m.log.Warn("the web certificate in the TLS directory cannot be read; a new one is issued", slog.Any("err", err))
	}
}

// renewBaseLocked issues a new leaf of the local CA (creating the CA when
// there is none or it can no longer sign a useful leaf) and writes it as
// cert.pem/key.pem, so a rolled-back earlier version serves the same
// certificate.
func (m *webTLS) renewBaseLocked(id hostIdentity, now time.Time, why string) {
	if m.ca == nil || m.ca.cert.NotAfter.Sub(now) < 24*time.Hour {
		if err := m.createCALocked(id, now); err != nil {
			m.retryAt, m.baseErr = now.Add(sanRecheckWait), err.Error()
			m.log.Error("cannot create the local CA; retried in an hour", slog.Any("err", err))
			return
		}
	}
	if err := m.issueLeafLocked(id, now); err != nil {
		m.retryAt, m.baseErr = now.Add(sanRecheckWait), err.Error()
		m.log.Error("cannot issue the web certificate of the local CA; retried in an hour", slog.Any("err", err))
		return
	}
	m.retryAt, m.baseErr = time.Time{}, ""
	m.log.Info(why, slog.String("subject", m.base.chain[0].Subject.String()),
		slog.Time("notAfter", m.base.chain[0].NotAfter), slog.Any("names", m.base.chain[0].DNSNames))
}

// createCALocked creates and stores a new local CA (the key first).
func (m *webTLS) createCALocked(id hostIdentity, now time.Time) error {
	ca, keyPEM, err := newLocalCA(id, m.instanceID, now)
	if err != nil {
		return err
	}
	dir, err := m.openDir(true)
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := writeFileAtomic(dir, caKeyFile, keyPEM, 0o600); err != nil {
		return err
	}
	if err := writeFileAtomic(dir, caCertFile, ca.certPEM, 0o644); err != nil {
		return err
	}
	m.ca = ca
	m.log.Info("created the local CA", slog.String("subject", ca.cert.Subject.String()),
		slog.Any("permittedNames", ca.cert.PermittedDNSDomains), slog.Time("notAfter", ca.cert.NotAfter))
	return nil
}

// issueLeafLocked issues and stores a leaf of the CA (the key first).
func (m *webTLS) issueLeafLocked(id hostIdentity, now time.Time) error {
	certPEM, keyPEM, err := m.ca.issueLeaf(id, now)
	if err != nil {
		return err
	}
	b, err := pairChain(certPEM, keyPEM, sourceLocalCA)
	if err != nil {
		return err
	}
	dir, err := m.openDir(true)
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := writeFileAtomic(dir, leafKeyFile, keyPEM, 0o600); err != nil {
		return err
	}
	if err := writeFileAtomic(dir, leafCertFile, certPEM, 0o644); err != nil {
		return err
	}
	m.base, m.baseTemp, m.lastSANCheck = b, false, now
	return nil
}

// pairChain parses a certificate and key PEM pair.
func pairChain(certPEM, keyPEM []byte, source string) (*servedCert, error) {
	c, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	var chain []*x509.Certificate
	for _, der := range c.Certificate {
		x, err := x509.ParseCertificate(der)
		if err != nil {
			return nil, err
		}
		chain = append(chain, x)
	}
	c.Leaf = chain[0]
	return &servedCert{&certChain{cert: &c, chain: chain}, source}, nil
}

// reloadFilesLocked loads the certificate files when their content
// differs from the pair last loaded successfully. A failure keeps the
// current certificate; it is logged once per distinct content and retried
// on every tick (a deploy hook that writes the key and the certificate in
// two steps is picked up on the next tick).
func (m *webTLS) reloadFilesLocked() {
	certPEM, err1 := readLimited(m.certFile)
	keyPEM, err2 := readLimited(m.keyFile)
	h := sha256.New()
	h.Write(certPEM)
	h.Write([]byte{0})
	h.Write(keyPEM)
	var sum [32]byte
	err := errors.Join(err1, err2)
	if err != nil {
		h.Write([]byte(err.Error()))
	}
	copy(sum[:], h.Sum(nil))
	if err == nil && m.files != nil && sum == m.filesGood {
		m.filesErr = ""
		return
	}
	var b *servedCert
	if err == nil {
		b, err = pairChain(certPEM, keyPEM, sourceFiles)
	}
	if err != nil {
		m.filesErr = err.Error()
		if sum != m.filesFailed {
			m.filesFailed = sum
			if m.files != nil {
				m.log.Error("could not load the changed web certificate files; the previous certificate is still in use",
					slog.String("cert", m.certFile), slog.Any("err", err))
			} else {
				m.log.Error("the web certificate files cannot be used; serving a fallback certificate (retried every minute)",
					slog.String("cert", m.certFile), slog.Any("err", err))
			}
		}
		return
	}
	m.files, m.filesGood, m.filesErr, m.filesFailed = b.certChain, sum, "", [32]byte{}
	m.log.Info("loaded the new web certificate", slog.String("subject", b.chain[0].Subject.String()),
		slog.Time("notAfter", b.chain[0].NotAfter))
}

// readLimited reads a file of at most 1 MiB.
func readLimited(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxCertFileSize+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxCertFileSize {
		return nil, fmt.Errorf("%s is larger than 1 MiB", path)
	}
	return b, nil
}

// loadUploadLocked reads uploaded.pem (fs.ErrNotExist without one). A TLS
// directory that cannot be opened counts as no upload: it is no fault of
// an upload, and the certificate served instead reports it (tempErrLocked).
func (m *webTLS) loadUploadLocked(now time.Time) error {
	m.upload, m.uploadErr = nil, ""
	dir, err := m.openDir(false)
	if err != nil {
		return fmt.Errorf("%w: %w", fs.ErrNotExist, err)
	}
	defer dir.Close()
	b, err := readRootFile(dir, uploadedFile)
	if err != nil {
		return err
	}
	c, err := decodeUpload(b, now)
	if err != nil {
		return err
	}
	m.upload = c
	return nil
}

// --- files in <data>/tls ---

// openDir opens <data>/tls (created 0750 with create). A symbolic link in
// its place is refused, and so is a directory swapped while it was opened.
func (m *webTLS) openDir(create bool) (*os.Root, error) {
	pr, err := os.OpenRoot(m.dataDir)
	if err != nil {
		return nil, err
	}
	defer pr.Close()
	if create {
		if err := pr.Mkdir(tlsDirName, 0o750); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
	}
	fi, err := pr.Lstat(tlsDirName)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("%s/%s is not a directory (symbolic links are not followed)", m.dataDir, tlsDirName)
	}
	r, err := pr.OpenRoot(tlsDirName)
	if err != nil {
		return nil, err
	}
	if rfi, err := r.Stat("."); err != nil || !os.SameFile(fi, rfi) {
		r.Close()
		return nil, fmt.Errorf("%s/%s changed while it was opened", m.dataDir, tlsDirName)
	}
	return r, nil
}

// readRootFile reads a regular file of at most 1 MiB (never through a
// symbolic link).
func readRootFile(dir *os.Root, name string) ([]byte, error) {
	f, err := dir.OpenFile(name, os.O_RDONLY|oNoFollow, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", name)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxCertFileSize+1))
	if err == nil && len(b) > maxCertFileSize {
		err = fmt.Errorf("%s is larger than 1 MiB", name)
	}
	return b, err
}

func readPair(dir *os.Root, certName, keyName string) ([]byte, []byte, error) {
	c, err := readRootFile(dir, certName)
	if err != nil {
		return nil, nil, err
	}
	k, err := readRootFile(dir, keyName)
	return c, k, err
}

// writeFileAtomic writes name through a temporary file in the same
// directory (created exclusively, never through a symbolic link, synced)
// and renames it, so a reader never sees a partly written file.
func writeFileAtomic(dir *os.Root, name string, data []byte, perm os.FileMode) error {
	tmp := fmt.Sprintf(".%s.%d.tmp", name, time.Now().UnixNano())
	f, err := dir.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|oNoFollow, perm)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Chmod(perm) // exactly perm, whatever the umask
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = dir.Rename(tmp, name)
	}
	if err != nil {
		_ = dir.Remove(tmp)
	}
	return err
}

// --- api.WebTLS ---

// Status returns the current state (Upload is set by the API).
func (m *webTLS) Status() api.TLSStatus {
	st := api.TLSStatus{Listener: m.listener, Source: sourceNone, EnvOverride: m.certFile != "",
		HostsCovered: []string{}, HostsNotCovered: []string{}}
	st.UploadStored = m.uploadStored()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.caFileExists() {
		m.loadCALocked()
	}
	if m.ca != nil {
		st.CAAvailable = true
		st.LocalCA = m.caInfoLocked()
	}
	if !m.listener {
		return st
	}
	st.CheckedAt = m.checkedAt
	if c := m.cur.Load(); c != nil {
		st.Source = c.source
		info := certInfo(c.certChain)
		st.Certificate = &info
		st.HostsCovered, st.HostsNotCovered = m.identity().coverage(c.chain[0])
	}
	switch {
	case m.certFile != "":
		st.Error, st.Fallback = m.filesErr, m.files == nil
	case m.uploadErr != "":
		st.Error, st.Fallback = m.uploadErr, m.upload == nil
	}
	if st.Error == "" {
		st.Error = m.tempErrLocked()
	}
	return st
}

// tempErrLocked describes why the in-memory emergency certificate is
// served ("" while it is not): it is not stored, so every start serves a
// new one and browsers warn again.
func (m *webTLS) tempErrLocked() string {
	c := m.cur.Load()
	if !m.baseTemp || c == nil || c != m.base {
		return ""
	}
	why := m.baseErr
	if why == "" {
		why = "see the log"
	}
	return "the TLS directory cannot be used: " + why + "; a temporary certificate is served (retried hourly)"
}

func (m *webTLS) uploadStored() bool {
	fi, err := os.Lstat(m.path(uploadedFile))
	return err == nil && fi.Mode().IsRegular()
}

func (m *webTLS) caFileExists() bool {
	fi, err := os.Lstat(m.path(caCertFile))
	return err == nil && fi.Mode().IsRegular()
}

func (m *webTLS) path(name string) string { return filepath.Join(m.dataDir, tlsDirName, name) }

// caInfoLocked describes the local CA (renewalNeeded: PiCache's names or
// addresses include some outside its constraints).
func (m *webTLS) caInfoLocked() *api.LocalCAInfo {
	c := m.ca.cert
	info := &api.LocalCAInfo{
		Subject: c.Subject.String(), NotBefore: c.NotBefore.UTC(), NotAfter: c.NotAfter.UTC(),
		FingerprintSHA256: fingerprint(c.Raw), PermittedNames: slices.Clone(c.PermittedDNSDomains), PermittedAddresses: []string{},
	}
	if info.PermittedNames == nil {
		info.PermittedNames = []string{}
	}
	for _, r := range c.PermittedIPRanges {
		if p, ok := netip.AddrFromSlice(r.IP); ok {
			ones, _ := r.Mask.Size()
			if pf := netip.PrefixFrom(netutil.Canon(p), ones); pf.IsSingleIP() {
				info.PermittedAddresses = append(info.PermittedAddresses, pf.Addr().String())
			} else {
				info.PermittedAddresses = append(info.PermittedAddresses, pf.String())
			}
		}
	}
	_, _, outside := m.identity().leafSANs(c)
	info.RenewalNeeded = len(outside) > 0
	return info
}

// CheckUpload validates an upload (rules 4 and 5 of PUT /system/tls).
func (m *webTLS) CheckUpload(certPEM, keyPEM string) error {
	_, err := parseUpload([]byte(certPEM), []byte(keyPEM), m.now())
	return err
}

// Upload stores the certificate as uploaded.pem and serves it at once.
func (m *webTLS) Upload(certPEM, keyPEM string) (api.CertInfo, error) {
	if !m.listener {
		return api.CertInfo{}, apperr.Conflict("no HTTPS listener: PICACHE_WEB_TLS_LISTEN is off")
	}
	c, err := parseUpload([]byte(certPEM), []byte(keyPEM), m.now())
	if err != nil {
		return api.CertInfo{}, err
	}
	b, err := encodeUpload(c)
	if err != nil {
		return api.CertInfo{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	dir, err := m.openDir(true)
	if err != nil {
		return api.CertInfo{}, err
	}
	defer dir.Close()
	if err := writeFileAtomic(dir, uploadedFile, b, 0o600); err != nil {
		return api.CertInfo{}, err
	}
	m.upload, m.uploadErr = c, ""
	m.chooseLocked(m.now())
	info := certInfo(c)
	m.log.Info("serving the uploaded web certificate", slog.String("subject", info.Subject), slog.Time("notAfter", info.NotAfter))
	return info, nil
}

// DeleteUpload removes uploaded.pem and falls back at once.
func (m *webTLS) DeleteUpload() (api.CertInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	info, found, err := m.deleteUploadLocked()
	if err != nil {
		return api.CertInfo{}, err
	}
	if !found {
		return api.CertInfo{}, api.ErrNoUploadedCertificate
	}
	return info, nil
}

func (m *webTLS) deleteUploadLocked() (api.CertInfo, bool, error) {
	dir, err := m.openDir(false)
	if errors.Is(err, fs.ErrNotExist) {
		return api.CertInfo{}, false, nil
	}
	if err != nil {
		return api.CertInfo{}, false, err
	}
	defer dir.Close()
	var info api.CertInfo
	if b, err := readRootFile(dir, uploadedFile); err == nil {
		if c, err := decodeUpload(b, m.now()); err == nil {
			info = certInfo(c)
		}
	} else if errors.Is(err, fs.ErrNotExist) {
		return api.CertInfo{}, false, nil
	}
	if err := dir.Remove(uploadedFile); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return api.CertInfo{}, false, nil
		}
		return api.CertInfo{}, false, err
	}
	m.upload, m.uploadErr = nil, ""
	if m.listener {
		m.chooseLocked(m.now())
	}
	m.log.Info("deleted the uploaded web certificate", slog.String("subject", info.Subject))
	return info, true, nil
}

// removeUploadForReset deletes the uploaded certificate for the web access
// reset (no error when there is none).
func (m *webTLS) removeUploadForReset() (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, found, err := m.deleteUploadLocked()
	return found, err
}

// NewLocalCA creates (or replaces) the local CA and its leaf. The local CA
// becomes the source unless the certificate files or an upload are in use
// (it is then prepared for later).
func (m *webTLS) NewLocalCA() (api.LocalCAInfo, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.listener {
		return api.LocalCAInfo{}, false, apperr.Conflict("no HTTPS listener: PICACHE_WEB_TLS_LISTEN is off")
	}
	replaced := m.ca != nil || m.caFileExists()
	now, id := m.now(), m.identity()
	if err := m.createCALocked(id, now); err != nil {
		return api.LocalCAInfo{}, false, err
	}
	if err := m.issueLeafLocked(id, now); err != nil {
		return api.LocalCAInfo{}, false, err
	}
	m.chooseLocked(now)
	return *m.caInfoLocked(), replaced, nil
}

// CACert returns the PEM of the local CA's certificate.
func (m *webTLS) CACert() ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.caFileExists() {
		m.loadCALocked()
	}
	if m.ca == nil {
		return nil, false
	}
	return bytes.Clone(m.ca.certPEM), true
}

// --- health check "tls" ---

// health evaluates the check "tls" (only while an HTTPS listener is bound;
// the first matching row wins).
func (m *webTLS) health(now time.Time) (status, msg, hint string, show bool) {
	if !m.listener {
		return "", "", "", false
	}
	st := m.Status()
	date := func(t time.Time) string { return t.UTC().Format(time.DateOnly) }
	days := func(t time.Time) int { return int(t.Sub(now).Hours() / 24) }
	const filesHint = "fix PICACHE_WEB_TLS_CERT and PICACHE_WEB_TLS_KEY (a matching pair the service can read); PiCache retries every minute"
	bySource := map[string]string{
		sourceFiles:      "renew the certificate files (see Let's Encrypt in docs/DEPLOYMENT.md); PiCache loads renewed files within a minute",
		sourceUploaded:   "upload a renewed certificate under System > HTTPS certificate",
		sourceLocalCA:    "PiCache renews this certificate itself; check the log for errors",
		sourceSelfSigned: "PiCache renews this certificate itself; check the log for errors",
	}
	served := "self-signed"
	if st.Source == sourceLocalCA {
		served = "local CA"
	}
	switch {
	case st.Fallback && st.EnvOverride:
		return "fail", fmt.Sprintf("the configured certificate cannot be used: %s; PiCache serves its %s certificate instead", st.Error, served),
			filesHint, true
	case st.Fallback:
		return "fail", fmt.Sprintf("the configured certificate cannot be used: %s; PiCache serves its %s certificate instead", st.Error, served),
			"upload the certificate again or delete it under System > HTTPS certificate", true
	case st.Certificate == nil:
		return "fail", "no web certificate is served", bySource[sourceSelfSigned], true
	case !st.Certificate.NotAfter.After(now):
		return "fail", "the web certificate expired on " + date(st.Certificate.NotAfter), bySource[st.Source], true
	case st.EnvOverride && st.Error != "":
		return "warn", "could not load the changed certificate files: " + st.Error + "; the previous certificate is still in use", filesHint, true
	case st.Source == sourceSelfSigned && st.Error != "":
		return "warn", st.Error, "make <data>/tls a directory (not a symbolic link) that the PiCache user can write; " +
			"until then browsers warn again after every restart", true
	case st.Certificate.NotAfter.Sub(now) < leafWarnBefore:
		return "warn", fmt.Sprintf("the web certificate expires in %d days (%s)", days(st.Certificate.NotAfter), date(st.Certificate.NotAfter)),
			bySource[st.Source], true
	case st.Source == sourceLocalCA && st.LocalCA != nil && st.LocalCA.NotAfter.Sub(now) < caWarnBefore:
		return "warn", fmt.Sprintf("the local CA expires in %d days (%s)", days(st.LocalCA.NotAfter), date(st.LocalCA.NotAfter)),
			"create a new local CA under System > HTTPS certificate and trust it on your devices", true
	case st.Source == sourceLocalCA && st.LocalCA != nil && st.LocalCA.RenewalNeeded:
		m.mu.Lock()
		_, _, outside := m.identity().leafSANs(m.ca.cert)
		m.mu.Unlock()
		list := outside
		if len(list) > 5 {
			list = append(slices.Clone(list[:5]), "…")
		}
		return "warn", "the local CA does not cover " + strings.Join(list, ", "),
			"create a new local CA under System > HTTPS certificate to include them (devices must trust the new CA)", true
	}
	return "ok", "", "", true
}
