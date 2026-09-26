package app

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"testing"
	"time"
)

// Test certificates: a CA (optionally below another), leaves with chosen
// properties.

type testCA struct {
	cert *x509.Certificate
	key  crypto.Signer
}

func genKey(t *testing.T, kind string) crypto.Signer {
	t.Helper()
	var (
		k   crypto.Signer
		err error
	)
	switch kind {
	case "p256", "":
		k, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	case "p384":
		k, err = ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	case "p521":
		k, err = ecdsa.GenerateKey(elliptic.P521(), rand.Reader)
	case "rsa1024":
		k, err = rsa.GenerateKey(rand.Reader, 1024)
	case "rsa2048":
		k, err = rsa.GenerateKey(rand.Reader, 2048)
	case "ed25519":
		_, k, err = ed25519.GenerateKey(rand.Reader)
	default:
		t.Fatalf("unknown key kind %q", kind)
	}
	if err != nil {
		t.Fatal(err)
	}
	return k
}

var testSerial = big.NewInt(100)

func nextSerial() *big.Int {
	testSerial = new(big.Int).Add(testSerial, big.NewInt(1))
	return testSerial
}

func newTestCA(t *testing.T, name string, parent *testCA) *testCA {
	t.Helper()
	key := genKey(t, "p256")
	tmpl := &x509.Certificate{
		SerialNumber: nextSerial(), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign, BasicConstraintsValid: true, IsCA: true,
	}
	signerCert, signerKey := tmpl, key
	if parent != nil {
		signerCert, signerKey = parent.cert, parent.key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, signerCert, key.Public(), signerKey)
	if err != nil {
		t.Fatal(err)
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &testCA{cert: c, key: key}
}

type leafOpts struct {
	key                 crypto.Signer // nil: a new P-256 key
	dns                 []string
	ips                 []net.IP
	notBefore, notAfter time.Time
	isCA                bool
	eku                 []x509.ExtKeyUsage
	noEKU               bool
}

// leaf issues a certificate and returns its PEM, the key PEM (PKCS#8) and
// the parsed certificate.
func (ca *testCA) leaf(t *testing.T, o leafOpts) ([]byte, []byte, *x509.Certificate) {
	t.Helper()
	if o.key == nil {
		o.key = genKey(t, "p256")
	}
	if o.notBefore.IsZero() {
		o.notBefore = time.Now().Add(-time.Hour)
	}
	if o.notAfter.IsZero() {
		o.notAfter = time.Now().Add(90 * 24 * time.Hour)
	}
	if o.eku == nil && !o.noEKU {
		o.eku = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	}
	tmpl := &x509.Certificate{
		SerialNumber: nextSerial(), Subject: pkix.Name{CommonName: "leaf"},
		NotBefore: o.notBefore, NotAfter: o.notAfter, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: o.eku, BasicConstraintsValid: true, IsCA: o.isCA, DNSNames: o.dns, IPAddresses: o.ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, o.key.Public(), ca.key)
	if err != nil {
		t.Fatal(err)
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pkcs8PEM(t, o.key), c
}

func pkcs8PEM(t *testing.T, k crypto.Signer) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

func certPEM(c *x509.Certificate) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})
}

// legacySelfSigned writes a certificate like 0.10's self-signed one.
func legacySelfSigned(t *testing.T, notAfter time.Time) ([]byte, []byte) {
	t.Helper()
	key := genKey(t, "p256")
	tmpl := &x509.Certificate{
		SerialNumber: nextSerial(), Subject: pkix.Name{CommonName: "PiCache", Organization: []string{"PiCache self-signed"}},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: notAfter, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true,
		DNSNames: []string{"localhost", "picache"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pkcs8PEM(t, key)
}

// newTestLogger logs text to w.
func newTestLogger(w io.Writer) *slog.Logger { return slog.New(slog.NewTextHandler(w, nil)) }
