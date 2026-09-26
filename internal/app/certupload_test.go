package app

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
)

// Every rule of an upload (certPem, then keyPem), with the field and the
// message that names the block.
func TestParseUploadRules(t *testing.T) {
	now := time.Now()
	root := newTestCA(t, "Root", nil)
	inter := newTestCA(t, "Intermediate", root)
	key := genKey(t, "p256")
	leafPEM, keyPEM, leaf := inter.leaf(t, leafOpts{key: key, dns: []string{"picache.example.com"}})
	chainPEM := slices.Concat(leafPEM, certPEM(inter.cert))
	p384, ed, rsa1024, p521 := genKey(t, "p384"), genKey(t, "ed25519"), genKey(t, "rsa1024"), genKey(t, "p521")
	rsaKey := genKey(t, "rsa2048")
	rsaLeaf, _, _ := inter.leaf(t, leafOpts{key: rsaKey, dns: []string{"a.example"}})
	ecKey := key.(*ecdsa.PrivateKey)
	sec1, err := x509.MarshalECPrivateKey(ecKey)
	if err != nil {
		t.Fatal(err)
	}
	pkcs1 := x509.MarshalPKCS1PrivateKey(rsaKey.(*rsa.PrivateKey))
	mk := func(o leafOpts) []byte {
		if o.dns == nil && o.ips == nil {
			o.dns = []string{"x.example"}
		}
		p, _, _ := inter.leaf(t, o)
		return slices.Concat(p, certPEM(inter.cert))
	}
	pemOf := func(typ string, b []byte, headers map[string]string) []byte {
		return pem.EncodeToMemory(&pem.Block{Type: typ, Headers: headers, Bytes: b})
	}
	for _, tc := range []struct {
		name       string
		cert, key  []byte
		field, msg string // "" = accepted
	}{
		{name: "chain PKCS#8", cert: chainPEM, key: keyPEM},
		{name: "leaf only", cert: leafPEM, key: keyPEM},
		{name: "comments around blocks", cert: slices.Concat([]byte("Bag Attributes\n"), chainPEM, []byte("junk\n")), key: keyPEM},
		{name: "SEC1 key", cert: chainPEM, key: pemOf("EC PRIVATE KEY", sec1, nil)},
		{name: "SEC1 key with EC PARAMETERS", cert: chainPEM, key: slices.Concat(pemOf("EC PARAMETERS", []byte{6, 8, 42, 134, 72, 206, 61, 3, 1, 7}, nil), pemOf("EC PRIVATE KEY", sec1, nil))},
		{name: "PKCS#1 key", cert: rsaLeaf, key: pemOf("RSA PRIVATE KEY", pkcs1, nil)},
		{name: "IP SAN only", cert: mk(leafOpts{key: key, ips: []net.IP{net.ParseIP("192.168.1.10")}}), key: keyPEM},
		{name: "no EKU", cert: mk(leafOpts{key: key, noEKU: true}), key: keyPEM},
		{name: "P-384", cert: mk(leafOpts{key: p384}), key: pkcs8PEM(t, p384)},
		{name: "no block", cert: []byte("hello"), key: keyPEM, field: "certPem", msg: "no certificate found"},
		{name: "six blocks", cert: bytes.Repeat(certPEM(inter.cert), 6), key: keyPEM, field: "certPem", msg: "at most 5 certificates"},
		{name: "key in cert field", cert: slices.Concat(leafPEM, keyPEM), key: keyPEM, field: "certPem", msg: "certificate 2: this block is a key"},
		{name: "other block type", cert: slices.Concat(leafPEM, pemOf("X509 CRL", []byte{1}, nil)), key: keyPEM, field: "certPem", msg: `certificate 2: a CERTIFICATE block was expected, not "X509 CRL"`},
		{name: "garbage certificate", cert: pemOf("CERTIFICATE", []byte{1, 2, 3}, nil), key: keyPEM, field: "certPem", msg: "certificate 1: cannot be read"},
		{name: "wrong order", cert: slices.Concat(certPEM(inter.cert), leafPEM), key: keyPEM, field: "certPem", msg: "certificate 1 is not signed by certificate 2"},
		{name: "unrelated intermediate", cert: slices.Concat(leafPEM, certPEM(root.cert)), key: keyPEM, field: "certPem", msg: "certificate 1 is not signed by certificate 2"},
		{name: "CA leaf", cert: mk(leafOpts{key: key, isCA: true}), key: keyPEM, field: "certPem", msg: "certificate 1: is a CA certificate"},
		{name: "client EKU", cert: mk(leafOpts{key: key, eku: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}), key: keyPEM, field: "certPem", msg: "server authentication"},
		{name: "no SAN", cert: mk(leafOpts{key: key, dns: []string{}, ips: []net.IP{}}), key: keyPEM, field: "certPem", msg: "no DNS name or IP address"},
		{name: "not yet valid", cert: mk(leafOpts{key: key, notBefore: now.Add(time.Hour), notAfter: now.Add(48 * time.Hour)}), key: keyPEM, field: "certPem", msg: "certificate 1: is not valid before"},
		{name: "4 minutes early is fine", cert: mk(leafOpts{key: key, notBefore: now.Add(4 * time.Minute)}), key: keyPEM},
		{name: "expired", cert: mk(leafOpts{key: key, notBefore: now.Add(-48 * time.Hour), notAfter: now.Add(-time.Hour)}), key: keyPEM, field: "certPem", msg: "certificate 1: expired on"},
		{name: "Ed25519", cert: mk(leafOpts{key: ed}), key: pkcs8PEM(t, ed), field: "certPem", msg: "browsers do not accept this key type"},
		{name: "RSA 1024", cert: mk(leafOpts{key: rsa1024}), key: pkcs8PEM(t, rsa1024), field: "certPem", msg: "browsers do not accept this key type"},
		{name: "P-521", cert: mk(leafOpts{key: p521}), key: pkcs8PEM(t, p521), field: "certPem", msg: "use RSA 2048–4096 or ECDSA P-256/P-384"},
		{name: "encrypted PKCS#8", cert: chainPEM, key: pemOf("ENCRYPTED PRIVATE KEY", []byte{1, 2}, nil), field: "keyPem", msg: "the private key is encrypted; remove the passphrase first"},
		{name: "legacy encrypted", cert: chainPEM, key: pemOf("EC PRIVATE KEY", []byte{1, 2}, map[string]string{"Proc-Type": "4,ENCRYPTED", "DEK-Info": "AES-128-CBC,00"}), field: "keyPem", msg: "openssl pkey -in key.pem -out key-plain.pem"},
		{name: "two keys", cert: chainPEM, key: slices.Concat(keyPEM, keyPEM), field: "keyPem", msg: "exactly one PEM key block"},
		{name: "no key", cert: chainPEM, key: []byte("nothing"), field: "keyPem", msg: "exactly one PEM key block"},
		{name: "certificate as key", cert: chainPEM, key: leafPEM, field: "keyPem", msg: `not "CERTIFICATE"`},
		{name: "garbage key", cert: chainPEM, key: pemOf("PRIVATE KEY", []byte{1, 2, 3}, nil), field: "keyPem", msg: "cannot be read"},
		{name: "mismatch", cert: chainPEM, key: pkcs8PEM(t, genKey(t, "p256")), field: "keyPem", msg: "does not match the certificate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := parseUpload(tc.cert, tc.key, now)
			if tc.field == "" {
				if err != nil || c == nil || c.cert.Leaf == nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			e, ok := apperr.As(err)
			if !ok || e.Kind != apperr.KindInvalid || e.Field != tc.field || !strings.Contains(e.Message, tc.msg) {
				t.Fatalf("err %v, want %s: %s", err, tc.field, tc.msg)
			}
		})
	}
	// RSA 8192 (slow to generate; only the key check).
	if acceptedKey(&rsa.PublicKey{N: new(big.Int).Lsh(big.NewInt(1), 8191), E: 65537}) {
		t.Fatal("RSA 8192 must be refused")
	}
	if !acceptedKey(leaf.PublicKey) {
		t.Fatal("P-256 must be accepted")
	}
}

// The stored form: the key as PKCS#8 first, then the chain, re-encoded;
// it decodes to the same chain (also when it has expired since).
func TestUploadEncodeDecode(t *testing.T) {
	inter := newTestCA(t, "Intermediate", newTestCA(t, "Root", nil))
	k := genKey(t, "p256")
	lp, _, _ := inter.leaf(t, leafOpts{key: k, dns: []string{"a.example"}})
	c, err := parseUpload(slices.Concat([]byte("comment\n"), lp, certPEM(inter.cert)), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY",
		Bytes: sec1Of(t, k)}), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	b, err := encodeUpload(c)
	if err != nil {
		t.Fatal(err)
	}
	blocks := pemBlocks(b)
	if len(blocks) != 3 || blocks[0].Type != "PRIVATE KEY" || blocks[1].Type != "CERTIFICATE" || bytes.Contains(b, []byte("comment")) {
		t.Fatalf("stored form: %s", b)
	}
	d, err := decodeUpload(b, time.Now().Add(365*24*time.Hour))
	if err != nil || len(d.cert.Certificate) != 2 || !bytes.Equal(d.chain[0].Raw, c.chain[0].Raw) {
		t.Fatalf("decode: %v", err)
	}
	info := certInfo(d)
	if info.ChainLength != 2 || info.KeyType != "ECDSA P-256" || info.SelfSigned || len(info.FingerprintSHA256) != 95 ||
		info.SANs[0] != "a.example" || !strings.Contains(info.Issuer, "CN=Intermediate") {
		t.Fatalf("info %+v", info)
	}
	if _, err := decodeUpload(slices.Concat(certPEM(inter.cert), pkcs8PEM(t, k)), time.Now()); err == nil {
		t.Fatal("the key must come first")
	}
}

// FuzzParseUpload: arbitrary PEM input never panics, and whatever is
// accepted is a usable chain whose key matches the leaf.
func FuzzParseUpload(f *testing.F) {
	ca := newTestCA(&testing.T{}, "Fuzz CA", nil)
	k := genKey(&testing.T{}, "p256")
	lp, kp, _ := ca.leaf(&testing.T{}, leafOpts{key: k, dns: []string{"a.example"}})
	f.Add(lp, kp)
	f.Add(slices.Concat(lp, certPEM(ca.cert)), kp)
	f.Add([]byte("-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n"), []byte("-----BEGIN PRIVATE KEY-----\n-----END PRIVATE KEY-----\n"))
	f.Add([]byte{}, []byte{})
	f.Fuzz(func(t *testing.T, cert, key []byte) {
		c, err := parseUpload(cert, key, time.Now())
		if err != nil {
			if _, ok := apperr.As(err); !ok {
				t.Fatalf("not a user-facing error: %v", err)
			}
			return
		}
		if c.cert.Leaf == nil || len(c.cert.Certificate) == 0 || len(c.cert.Certificate) > maxUploadCerts {
			t.Fatalf("accepted an unusable chain")
		}
		b, err := encodeUpload(c)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := decodeUpload(b, time.Now()); err != nil {
			t.Fatalf("stored form does not load: %v", err)
		}
	})
}

// FuzzDecodeUpload: a corrupt uploaded.pem never panics.
func FuzzDecodeUpload(f *testing.F) {
	ca := newTestCA(&testing.T{}, "Fuzz CA", nil)
	k := genKey(&testing.T{}, "p256")
	lp, kp, _ := ca.leaf(&testing.T{}, leafOpts{key: k, dns: []string{"a.example"}})
	f.Add(slices.Concat(kp, lp))
	f.Add([]byte("-----BEGIN PRIVATE KEY-----\n-----END PRIVATE KEY-----\n"))
	f.Fuzz(func(t *testing.T, b []byte) {
		if c, err := decodeUpload(b, time.Now()); err == nil && c.cert.Leaf == nil {
			t.Fatal("decoded without a leaf")
		}
	})
}

func sec1Of(t *testing.T, k any) []byte {
	t.Helper()
	b, err := x509.MarshalECPrivateKey(k.(*ecdsa.PrivateKey))
	if err != nil {
		t.Fatal(err)
	}
	return b
}
