package upstream

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/settings"
)

// testCA is a certificate authority for chains with chosen validity.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pool *x509.CertPool
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "picache test CA"},
		NotBefore: time.Now().Add(-365 * 24 * time.Hour), NotAfter: time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true, IsCA: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &testCA{cert: cert, key: key, pool: pool}
}

// leaf issues a server certificate for 127.0.0.1 (ip) or only for
// "other.test" (!ip), valid from notBefore to notAfter.
func (ca *testCA) leaf(t *testing.T, notBefore, notAfter time.Time, ip bool) *tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "doh test"},
		NotBefore: notBefore, NotAfter: notAfter,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames: []string{"other.test"},
	}
	if ip {
		tmpl.IPAddresses = []net.IP{net.IPv4(127, 0, 0, 1)}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Certificate{Certificate: [][]byte{der, ca.cert.Raw}, PrivateKey: key}
}

// TestStaleClockReachesEncryptedUpstream: a host whose clock is behind the
// validity of its DoH upstream's renewed certificate (restored from the
// last shutdown) and not synchronised learns the start of that validity
// as the clock of the certificate checks and gets its answers; a
// synchronised or unreadable clock, an expired certificate, an untrusted
// chain or one for another name change nothing.
func TestStaleClockReachesEncryptedUpstream(t *testing.T) {
	ca, other := newTestCA(t), newTestCA(t)
	day := 24 * time.Hour
	future := time.Now().Add(30 * day).Truncate(time.Second)
	for _, tc := range []struct {
		name             string
		cert             *tls.Certificate
		synced, readable bool
		ok               bool
	}{
		{"stale and not synchronised", ca.leaf(t, future, future.Add(90*day), true), false, true, true},
		{"synchronised", ca.leaf(t, future, future.Add(90*day), true), true, true, false},
		{"unreadable", ca.leaf(t, future, future.Add(90*day), true), false, false, false},
		{"expired", ca.leaf(t, time.Now().Add(-120*day), time.Now().Add(-30*day), true), false, true, false},
		{"untrusted", other.leaf(t, future, future.Add(90*day), true), false, true, false},
		{"another name", ca.leaf(t, future, future.Add(90*day), false), false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := startDoHCert(t, tc.cert, func(w http.ResponseWriter, q *dns.Msg) { writeMsg(w, answerA(q, "192.0.2.93", 60)) })
			st := newStore(t, func(dd *settings.DNS) {
				dd.Upstreams = []string{d.srv.URL + "/dns-query"}
				dd.CacheEnabled = false
			})
			opts := testOptions()
			opts.rootCAs = ca.pool
			r := newTestResolver(t, st, opts, nil)
			defer r.Close()
			r.SetClockReader(func() (bool, bool) { return tc.synced, tc.readable })
			for i := range 2 {
				_, _, err := r.Resolve(context.Background(), query("q"+strconv.Itoa(i)+".example.", dns.TypeA, 1, false), noECS)
				if (err == nil) != tc.ok {
					t.Fatalf("query %d: err %v, want ok %v", i, err, tc.ok)
				}
			}
			floor := r.certFloor.Load()
			switch {
			case tc.ok && floor != future.UnixNano():
				t.Errorf("certificate clock %v, want %v", time.Unix(0, floor), future)
			case !tc.ok && floor != 0:
				t.Errorf("certificate clock set to %v", time.Unix(0, floor))
			}
			if r.ClockGuard() {
				t.Error("the clock guard must not switch to plain DNS")
			}
		})
	}
}

// TestStaleClockReachesDoQ: the same over QUIC (quic-go wraps the
// certificate error of the TLS handshake).
func TestStaleClockReachesDoQ(t *testing.T) {
	ca := newTestCA(t)
	future := time.Now().Add(30 * 24 * time.Hour).Truncate(time.Second)
	s := startDoQ(t, &doqServer{cert: ca.leaf(t, future, future.Add(90*24*time.Hour), true),
		answer: func(q *dns.Msg) (*dns.Msg, []byte) { return answerA(q, "192.0.2.94", 60), nil }}, time.Minute)
	st := newStore(t, func(dd *settings.DNS) { dd.Upstreams = []string{"quic://" + s.addr.String()} })
	opts := testOptions()
	opts.rootCAs = ca.pool
	r := newTestResolver(t, st, opts, nil)
	defer r.Close()
	r.SetClockReader(func() (bool, bool) { return false, true })
	if _, _, err := r.Resolve(context.Background(), query("doq.example.", dns.TypeA, 1, false), noECS); err != nil {
		t.Fatal(err)
	}
	if floor := r.certFloor.Load(); floor != future.UnixNano() {
		t.Errorf("certificate clock %v, want %v", time.Unix(0, floor), future)
	}
}

// TestCertTime: the learned bound applies only while the host clock is
// behind it.
func TestCertTime(t *testing.T) {
	st := newStore(t, nil)
	r := newTestResolver(t, st, testOptions(), nil)
	defer r.Close()
	if d := time.Since(r.certTime()); d < 0 || d > time.Second {
		t.Fatalf("without a bound: %v off", d)
	}
	ahead := time.Now().Add(time.Hour)
	r.certFloor.Store(ahead.UnixNano())
	if got := r.certTime(); !got.Equal(time.Unix(0, ahead.UnixNano())) {
		t.Fatalf("behind the bound: %v, want %v", got, ahead)
	}
	r.certFloor.Store(time.Now().Add(-time.Hour).UnixNano())
	if d := time.Since(r.certTime()); d < 0 || d > time.Second {
		t.Fatalf("past the bound: %v off", d)
	}
}
