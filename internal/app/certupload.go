package app

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/hustenreizjuengling/picache/internal/api"
	"github.com/hustenreizjuengling/picache/internal/apperr"
)

// Upload rules (docs/API.md, PUT /system/tls).
const (
	maxUploadCerts   = 5 // the leaf and up to 4 intermediates
	maxNotBeforeSkew = 5 * time.Minute
)

// certChain is a parsed certificate with its key, ready to serve.
type certChain struct {
	cert  *tls.Certificate    // leaf and intermediates, Leaf set
	chain []*x509.Certificate // leaf first
}

// pemBlocks returns the PEM blocks of s (text outside blocks is ignored,
// like the comments openssl writes).
func pemBlocks(s []byte) []*pem.Block {
	var out []*pem.Block
	for len(s) > 0 {
		b, rest := pem.Decode(s)
		if b == nil {
			break
		}
		out = append(out, b)
		s = rest
	}
	return out
}

// blockType returns a PEM type that is safe to echo in a message.
func blockType(t string) string {
	b := make([]byte, 0, 40)
	for i := 0; i < len(t) && len(b) < 40; i++ {
		if c := t[i]; c >= 0x20 && c < 0x7f && c != '"' && c != '\\' {
			b = append(b, c)
		}
	}
	return string(b)
}

// parseCertChain parses and checks the certPem of an upload: only
// CERTIFICATE blocks, 1 to 5 (the leaf first, then its intermediates), each
// signed by the next one; the leaf is no CA, is valid for server
// authentication (or has no extended key usage), has a DNS or IP SAN, is
// valid now (not before at most 5 minutes in the future; strict only) and
// has an RSA 2048–4096 or ECDSA P-256/P-384 key. Errors name the block
// (field certPem).
func parseCertChain(certPEM []byte, now time.Time, strict bool) ([]*x509.Certificate, error) {
	invalid := func(format string, args ...any) error { return apperr.Invalid("certPem", format, args...) }
	blocks := pemBlocks(certPEM)
	if len(blocks) == 0 {
		return nil, invalid("no certificate found (a PEM block starting with -----BEGIN CERTIFICATE-----)")
	}
	if len(blocks) > maxUploadCerts {
		return nil, invalid("at most %d certificates: the certificate and up to %d intermediates", maxUploadCerts, maxUploadCerts-1)
	}
	chain := make([]*x509.Certificate, 0, len(blocks))
	for i, b := range blocks {
		n := i + 1
		if b.Type != "CERTIFICATE" {
			if strings.Contains(b.Type, "KEY") {
				return nil, invalid("certificate %d: this block is a key; paste the key into the key field", n)
			}
			return nil, invalid("certificate %d: a CERTIFICATE block was expected, not %q", n, blockType(b.Type))
		}
		c, err := x509.ParseCertificate(b.Bytes)
		if err != nil {
			return nil, invalid("certificate %d: cannot be read (%s)", n, strings.TrimPrefix(err.Error(), "x509: "))
		}
		chain = append(chain, c)
	}
	for i := 0; i+1 < len(chain); i++ {
		if err := chain[i].CheckSignatureFrom(chain[i+1]); err != nil {
			return nil, invalid("certificate %d is not signed by certificate %d: put the certificate first, then its intermediates in order", i+1, i+2)
		}
	}
	leaf := chain[0]
	switch {
	case leaf.IsCA:
		return nil, invalid("certificate 1: is a CA certificate, not a server certificate")
	case len(leaf.ExtKeyUsage) > 0 && !slices.Contains(leaf.ExtKeyUsage, x509.ExtKeyUsageServerAuth) &&
		!slices.Contains(leaf.ExtKeyUsage, x509.ExtKeyUsageAny):
		return nil, invalid("certificate 1: is not valid for server authentication (extended key usage)")
	case len(leaf.DNSNames) == 0 && len(leaf.IPAddresses) == 0:
		return nil, invalid("certificate 1: has no DNS name or IP address (subject alternative name)")
	case !acceptedKey(leaf.PublicKey):
		return nil, invalid("certificate 1: browsers do not accept this key type; use RSA 2048–4096 or ECDSA P-256/P-384")
	}
	if strict {
		if leaf.NotBefore.After(now.Add(maxNotBeforeSkew)) {
			return nil, invalid("certificate 1: is not valid before %s", leaf.NotBefore.UTC().Format(time.DateTime+" MST"))
		}
		if !leaf.NotAfter.After(now) {
			return nil, invalid("certificate 1: expired on %s", leaf.NotAfter.UTC().Format(time.DateOnly))
		}
	}
	return chain, nil
}

// acceptedKey reports whether browsers accept a server key: RSA of 2048 to
// 4096 bits or ECDSA P-256/P-384.
func acceptedKey(k any) bool {
	switch k := k.(type) {
	case *rsa.PublicKey:
		n := k.N.BitLen()
		return n >= 2048 && n <= 4096
	case *ecdsa.PublicKey:
		return k.Curve == elliptic.P256() || k.Curve == elliptic.P384()
	}
	return false
}

// parseUploadKey parses the keyPem of an upload: exactly one PRIVATE KEY
// (PKCS#8), RSA PRIVATE KEY (PKCS#1) or EC PRIVATE KEY (SEC1) block (an
// EC PARAMETERS block, as openssl ecparam writes it, is ignored); encrypted
// keys are refused. It must match leaf. Errors have field keyPem.
func parseUploadKey(keyPEM []byte, leaf *x509.Certificate) (crypto.Signer, error) {
	invalid := func(format string, args ...any) error { return apperr.Invalid("keyPem", format, args...) }
	var blocks []*pem.Block
	for _, b := range pemBlocks(keyPEM) {
		if b.Type != "EC PARAMETERS" {
			blocks = append(blocks, b)
		}
	}
	if len(blocks) != 1 {
		return nil, invalid("must be exactly one PEM key block (PKCS#8, PKCS#1 or SEC1)")
	}
	b := blocks[0]
	if b.Type == "ENCRYPTED PRIVATE KEY" || strings.Contains(b.Headers["Proc-Type"], "ENCRYPTED") {
		return nil, invalid("the private key is encrypted; remove the passphrase first (openssl pkey -in key.pem -out key-plain.pem)")
	}
	var (
		k   any
		err error
	)
	switch b.Type {
	case "PRIVATE KEY":
		k, err = x509.ParsePKCS8PrivateKey(b.Bytes)
	case "RSA PRIVATE KEY":
		k, err = x509.ParsePKCS1PrivateKey(b.Bytes)
	case "EC PRIVATE KEY":
		k, err = x509.ParseECPrivateKey(b.Bytes)
	default:
		return nil, invalid("must be a PKCS#8, PKCS#1 (RSA) or SEC1 (EC) key block, not %q", blockType(b.Type))
	}
	if err != nil {
		return nil, invalid("the private key cannot be read")
	}
	signer, ok := k.(crypto.Signer)
	if !ok {
		return nil, invalid("unsupported key type")
	}
	pub, ok := signer.Public().(interface{ Equal(crypto.PublicKey) bool })
	if !ok || leaf == nil || !pub.Equal(leaf.PublicKey) {
		return nil, invalid("does not match the certificate")
	}
	return signer, nil
}

// parseUpload validates an upload (docs/API.md, PUT /system/tls, rules 4
// and 5) and returns the chain to serve.
func parseUpload(certPEM, keyPEM []byte, now time.Time) (*certChain, error) {
	chain, err := parseCertChain(certPEM, now, true)
	if err != nil {
		return nil, err
	}
	key, err := parseUploadKey(keyPEM, chain[0])
	if err != nil {
		return nil, err
	}
	return newCertChain(chain, key), nil
}

func newCertChain(chain []*x509.Certificate, key crypto.PrivateKey) *certChain {
	c := &tls.Certificate{PrivateKey: key, Leaf: chain[0]}
	for _, x := range chain {
		c.Certificate = append(c.Certificate, x.Raw)
	}
	return &certChain{cert: c, chain: chain}
}

// encodeUpload returns uploaded.pem: the key as PKCS#8 first, then the
// leaf and the intermediates, re-encoded (nothing of the upload's text is
// kept).
func encodeUpload(c *certChain) ([]byte, error) {
	keyDER, err := x509.MarshalPKCS8PrivateKey(c.cert.PrivateKey)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	_ = pem.Encode(&buf, &pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	for _, x := range c.chain {
		_ = pem.Encode(&buf, &pem.Block{Type: "CERTIFICATE", Bytes: x.Raw})
	}
	return buf.Bytes(), nil
}

// decodeUpload reads uploaded.pem (the key first, then the chain). Dates
// are not checked: an expired certificate is served and reported by the
// health check.
func decodeUpload(b []byte, now time.Time) (*certChain, error) {
	blocks := pemBlocks(b)
	if len(blocks) < 2 || blocks[0].Type != "PRIVATE KEY" {
		return nil, errors.New("uploaded.pem: expected the key and then the certificates")
	}
	var certs bytes.Buffer
	for _, blk := range blocks[1:] {
		_ = pem.Encode(&certs, blk)
	}
	chain, err := parseCertChain(certs.Bytes(), now, false)
	if err != nil {
		return nil, fmt.Errorf("uploaded.pem: %s", strings.TrimPrefix(err.Error(), "certPem: "))
	}
	key, err := parseUploadKey(pem.EncodeToMemory(blocks[0]), chain[0])
	if err != nil {
		return nil, fmt.Errorf("uploaded.pem: the key %s", strings.TrimPrefix(err.Error(), "keyPem: "))
	}
	return newCertChain(chain, key), nil
}

// certInfo describes a served chain.
func certInfo(c *certChain) api.CertInfo {
	leaf := c.chain[0]
	sans := slices.Clone(leaf.DNSNames)
	for _, ip := range leaf.IPAddresses {
		sans = append(sans, ip.String())
	}
	if sans == nil {
		sans = []string{}
	}
	return api.CertInfo{
		Subject:           leaf.Subject.String(),
		Issuer:            leaf.Issuer.String(),
		SANs:              sans,
		NotBefore:         leaf.NotBefore.UTC(),
		NotAfter:          leaf.NotAfter.UTC(),
		FingerprintSHA256: fingerprint(leaf.Raw),
		KeyType:           keyType(leaf.PublicKey),
		ChainLength:       len(c.cert.Certificate),
		SelfSigned:        bytes.Equal(leaf.RawIssuer, leaf.RawSubject) && leaf.CheckSignature(leaf.SignatureAlgorithm, leaf.RawTBSCertificate, leaf.Signature) == nil,
	}
}

// fingerprint returns the SHA-256 of der as upper-case hex pairs joined by ":".
func fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	h := strings.ToUpper(hex.EncodeToString(sum[:]))
	var b strings.Builder
	for i := 0; i < len(h); i += 2 {
		if i > 0 {
			b.WriteByte(':')
		}
		b.WriteString(h[i : i+2])
	}
	return b.String()
}

// keyType names a public key as api.CertInfo reports it.
func keyType(k any) string {
	switch k := k.(type) {
	case *ecdsa.PublicKey:
		switch k.Curve {
		case elliptic.P256():
			return "ECDSA P-256"
		case elliptic.P384():
			return "ECDSA P-384"
		}
	case *rsa.PublicKey:
		return fmt.Sprintf("RSA %d", k.N.BitLen())
	case ed25519.PublicKey:
		return "Ed25519"
	}
	return "other"
}
