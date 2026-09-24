package app

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/hustenreizjuengling/picache/internal/netutil"
)

// webTLSConfig returns the TLS config for the HTTPS web listener: the user
// certificate if configured, otherwise a self-signed ECDSA certificate that
// is (re)generated when missing or close to expiry.
func (a *App) webTLSConfig() (*tls.Config, error) {
	certFile, keyFile := a.cfg.WebTLSCertFile, a.cfg.WebTLSKeyFile
	if certFile == "" {
		certFile = filepath.Join(a.paths.TLSDir, "cert.pem")
		keyFile = filepath.Join(a.paths.TLSDir, "key.pem")
		if needsNewCert(certFile) {
			if err := a.writeSelfSigned(certFile, keyFile); err != nil {
				return nil, fmt.Errorf("self-signed certificate: %w", err)
			}
		}
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load web TLS certificate: %w", err)
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{"h2", "http/1.1"},
	}, nil
}

func needsNewCert(certFile string) bool {
	b, err := os.ReadFile(certFile)
	if err != nil {
		return true
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return true
	}
	c, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		return true
	}
	return time.Until(c.NotAfter) < 30*24*time.Hour
}

func (a *App) writeSelfSigned(certFile, keyFile string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return err
	}
	names := []string{"localhost", "picache"}
	if hn, err := os.Hostname(); err == nil {
		names = append(names, strings.ToLower(hn))
	}
	var ips []net.IP
	for _, ip := range netutil.LocalAddrs() {
		if !ip.IsLinkLocalUnicast() {
			ips = append(ips, net.IP(ip.AsSlice()))
		}
	}
	slices.Sort(names)
	names = slices.Compact(names)
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "PiCache", Organization: []string{"PiCache self-signed"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(825 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              names,
		IPAddresses:           ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return err
	}
	a.log.Info("generated self-signed web certificate", slog.String("file", certFile), slog.Any("names", names))
	return nil
}
