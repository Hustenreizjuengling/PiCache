package api

import (
	"net/http"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
)

// Upload limits of PUT /system/tls.
const (
	maxTLSUploadBody = 128 << 10 // the JSON body
	maxTLSUploadPEM  = 64 << 10  // certPem and keyPem together
)

// CertInfo describes a certificate (the served leaf).
type CertInfo struct {
	Subject           string    `json:"subject"` // RFC 2253
	Issuer            string    `json:"issuer"`  // RFC 2253
	SANs              []string  `json:"sans"`    // DNS names, then IP addresses
	NotBefore         time.Time `json:"notBefore"`
	NotAfter          time.Time `json:"notAfter"`
	FingerprintSHA256 string    `json:"fingerprintSha256"` // upper-case hex pairs joined by ":"
	KeyType           string    `json:"keyType"`           // "ECDSA P-256", "ECDSA P-384", "RSA <bits>", "Ed25519" or "other"
	ChainLength       int       `json:"chainLength"`       // the leaf and the intermediates served
	SelfSigned        bool      `json:"selfSigned"`
}

// LocalCAInfo describes PiCache's local CA.
type LocalCAInfo struct {
	Subject            string    `json:"subject"`
	NotBefore          time.Time `json:"notBefore"`
	NotAfter           time.Time `json:"notAfter"`
	FingerprintSHA256  string    `json:"fingerprintSha256"`
	PermittedNames     []string  `json:"permittedNames"`
	PermittedAddresses []string  `json:"permittedAddresses"`
	// RenewalNeeded: PiCache's names or addresses include some outside the
	// CA's name constraints (a new server name, renumbering); only a new CA
	// covers them.
	RenewalNeeded bool `json:"renewalNeeded"`
}

// TLSUpload says whether PUT /system/tls is possible for this request.
type TLSUpload struct {
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason,omitempty"` // no-listener | env-override | plain-http (when not allowed)
}

// TLSStatus is the state of the HTTPS listener's certificate
// (GET /system/tls). Source is "none" exactly when Listener is false.
type TLSStatus struct {
	Listener        bool         `json:"listener"`
	Source          string       `json:"source"`       // files | uploaded | local-ca | self-signed | none: the source of the served certificate
	EnvOverride     bool         `json:"envOverride"`  // PICACHE_WEB_TLS_CERT is set
	UploadStored    bool         `json:"uploadStored"` // <data>/tls/uploaded.pem exists
	Upload          TLSUpload    `json:"upload"`
	Certificate     *CertInfo    `json:"certificate,omitempty"`
	HostsCovered    []string     `json:"hostsCovered"`
	HostsNotCovered []string     `json:"hostsNotCovered"`
	CAAvailable     bool         `json:"caAvailable"`
	LocalCA         *LocalCAInfo `json:"localCa,omitempty"`
	// Fallback: the configured certificate (files or upload) cannot be used
	// and another source's certificate is served; Error says why (also set
	// while the last good certificate of the files is still served after a
	// failed reload).
	Fallback  bool      `json:"fallback"`
	Error     string    `json:"error,omitempty"`
	CheckedAt time.Time `json:"checkedAt,omitzero"`
}

// WebTLS is implemented by internal/app: the certificate of the HTTPS
// listener (docs/ARCHITECTURE.md 2). Every method is safe for concurrent
// use.
type WebTLS interface {
	// Status returns the current state; Upload is filled in by the API.
	Status() TLSStatus
	// CheckUpload validates a certificate chain and key for an upload
	// (apperr.Invalid with field certPem or keyPem).
	CheckUpload(certPEM, keyPEM string) error
	// Upload validates, stores <data>/tls/uploaded.pem and serves it at once.
	Upload(certPEM, keyPEM string) (CertInfo, error)
	// DeleteUpload removes the uploaded certificate (apperr.NotFound
	// without one) and returns it; serving falls back at once.
	DeleteUpload() (CertInfo, error)
	// NewLocalCA creates (or replaces) the local CA and its leaf; replaced
	// reports that a CA existed.
	NewLocalCA() (ca LocalCAInfo, replaced bool, err error)
	// CACert returns the PEM of the local CA's certificate (false without one).
	CACert() ([]byte, bool)
}

// registerTLSRoutes registers the HTTPS certificate endpoints (docs/API.md).
func (s *Server) registerTLSRoutes() {
	s.route("GET /api/v1/system/tls", permRead, s.tlsGet)
	// Changing the certificate: an admin's interactive session, with the
	// password for an upload and a new CA.
	s.route("PUT /api/v1/system/tls", permSession, s.tlsUpload)
	s.route("DELETE /api/v1/system/tls", permSession, s.tlsDelete, routeDestructive)
	s.route("POST /api/v1/system/tls/local-ca", permSession, s.tlsLocalCA)
	s.route("GET /api/v1/system/tls/ca.crt", permPublic, s.tlsCACert)
}

var errNoHTTPSListener = apperr.Conflict("no HTTPS listener: PICACHE_WEB_TLS_LISTEN is off")

// ErrNoUploadedCertificate answers DELETE /system/tls without an uploaded
// certificate.
var ErrNoUploadedCertificate error = &apperr.Error{Kind: apperr.KindNotFound, Message: "there is no uploaded certificate"}

// tlsStatus returns the status with the upload permission of this request.
func (s *Server) tlsStatus(r *http.Request) TLSStatus {
	st := TLSStatus{Source: "none"}
	if s.d.TLS != nil {
		st = s.d.TLS.Status()
	}
	if st.HostsCovered == nil {
		st.HostsCovered = []string{}
	}
	if st.HostsNotCovered == nil {
		st.HostsNotCovered = []string{}
	}
	ci := requestClient(r)
	switch {
	case !st.Listener:
		st.Upload = TLSUpload{Reason: "no-listener"}
	case st.EnvOverride:
		st.Upload = TLSUpload{Reason: "env-override"}
	case !ci.https && !ci.client.IsLoopback():
		st.Upload = TLSUpload{Reason: "plain-http"}
	default:
		st.Upload = TLSUpload{Allowed: true}
	}
	return st
}

func (s *Server) tlsGet(w http.ResponseWriter, r *http.Request) error {
	return ok(w, s.tlsStatus(r))
}

// tlsUpload stores an uploaded certificate. The rules are checked in this
// order: an HTTPS listener without PICACHE_WEB_TLS_CERT (409), HTTPS or
// this machine (the key never crosses the network in clear), the body
// sizes, the certificate chain, the key, and last the password.
func (s *Server) tlsUpload(w http.ResponseWriter, r *http.Request) error {
	st := TLSStatus{}
	if s.d.TLS != nil {
		st = s.d.TLS.Status()
	}
	switch {
	case !st.Listener:
		return errNoHTTPSListener
	case st.EnvOverride:
		return apperr.Conflict("PICACHE_WEB_TLS_CERT is set: the certificate files on the host are used")
	}
	if ci := requestClient(r); !ci.https && !ci.client.IsLoopback() {
		return apperr.Invalid("keyPem", "send the private key over HTTPS (port %d)", s.httpsPort())
	}
	var in struct {
		CertPEM         string `json:"certPem"`
		KeyPEM          string `json:"keyPem"`
		CurrentPassword string `json:"currentPassword"`
	}
	if err := decodeLimit(w, r, &in, maxTLSUploadBody); err != nil {
		return err
	}
	if len(in.CertPEM)+len(in.KeyPEM) > maxTLSUploadPEM {
		return apperr.Invalid("certPem", "certificate and key together must be at most 64 KiB")
	}
	if err := s.d.TLS.CheckUpload(in.CertPEM, in.KeyPEM); err != nil {
		return err
	}
	if err := s.confirmCurrentPassword(r.Context(), principal(r), in.CurrentPassword); err != nil {
		return err
	}
	c, err := s.d.TLS.Upload(in.CertPEM, in.KeyPEM)
	if err != nil {
		return err
	}
	s.audit(r, "system.tls.upload", c.Subject, map[string]any{"source": "uploaded", "subject": c.Subject, "issuer": c.Issuer,
		"sans": c.SANs, "notAfter": c.NotAfter, "fingerprintSha256": c.FingerprintSHA256})
	return ok(w, s.tlsStatus(r))
}

func (s *Server) tlsDelete(w http.ResponseWriter, r *http.Request) error {
	if s.d.TLS == nil {
		return ErrNoUploadedCertificate
	}
	c, err := s.d.TLS.DeleteUpload()
	if err != nil {
		return err
	}
	s.audit(r, "system.tls.delete", c.Subject, map[string]string{"subject": c.Subject, "fingerprintSha256": c.FingerprintSHA256})
	return ok(w, s.tlsStatus(r))
}

func (s *Server) tlsLocalCA(w http.ResponseWriter, r *http.Request) error {
	if s.d.TLS == nil || !s.d.TLS.Status().Listener {
		return errNoHTTPSListener
	}
	var in struct {
		CurrentPassword string `json:"currentPassword"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	if err := s.confirmCurrentPassword(r.Context(), principal(r), in.CurrentPassword); err != nil {
		return err
	}
	ca, replaced, err := s.d.TLS.NewLocalCA()
	if err != nil {
		return err
	}
	s.audit(r, "system.tls.local_ca", ca.Subject, map[string]any{"subject": ca.Subject, "notAfter": ca.NotAfter,
		"fingerprintSha256": ca.FingerprintSHA256, "permittedNames": ca.PermittedNames,
		"permittedAddresses": ca.PermittedAddresses, "replaced": replaced})
	return ok(w, s.tlsStatus(r))
}

// tlsCACert serves the local CA's certificate for devices to trust (public:
// a certificate is no secret, and a device has to fetch it before it can
// trust the HTTPS listener).
func (s *Server) tlsCACert(w http.ResponseWriter, r *http.Request) error {
	var pemBytes []byte
	found := false
	if s.d.TLS != nil {
		pemBytes, found = s.d.TLS.CACert()
	}
	if !found {
		return &apperr.Error{Kind: apperr.KindNotFound, Message: "there is no local CA"}
	}
	w.Header().Set("Content-Type", "application/x-x509-ca-cert")
	w.Header().Set("Content-Disposition", `attachment; filename="picache-ca.crt"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(pemBytes)
	return nil
}
