package netutil

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"math/big"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"
)

func TestSwitchListener(t *testing.T) {
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var on atomic.Bool
	ln := NewSwitchListener(raw, on.Load, 2)
	defer ln.Close()
	accepted := make(chan net.Conn, 8)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted <- c
		}
	}()
	dial := func() net.Conn {
		c, err := net.Dial("tcp", raw.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	closedByServer := func(c net.Conn) bool {
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, err := c.Read(make([]byte, 1))
		return errors.Is(err, io.EOF) || isReset(err)
	}

	// Off: closed right after accept.
	c := dial()
	if !closedByServer(c) {
		t.Fatal("connection not closed while off")
	}
	c.Close()

	// On: accepted and tracked, at most 2.
	on.Store(true)
	c1, c2 := dial(), dial()
	defer c1.Close()
	defer c2.Close()
	s1, s2 := <-accepted, <-accepted
	if ln.Open() != 2 {
		t.Fatalf("open %d", ln.Open())
	}
	c3 := dial()
	if !closedByServer(c3) {
		t.Fatal("third connection not closed at the cap")
	}
	c3.Close()

	// Switched off: every open connection is closed.
	on.Store(false)
	ln.CloseAll()
	if !closedByServer(c1) || !closedByServer(c2) {
		t.Fatal("open connections not closed")
	}
	if ln.Open() != 0 {
		t.Fatalf("open %d after CloseAll", ln.Open())
	}
	_ = s1.Close() // closing again is harmless
	_ = s2.Close()
}

func isReset(err error) bool {
	var ne *net.OpError
	return errors.As(err, &ne) && !ne.Timeout()
}

func TestClientListClientIDs(t *testing.T) {
	l := NewClientList([]string{"192.168.1.5", "clientid:Kid", "aa:bb:cc:dd:ee:ff", "clientid:kid", "bogus"})
	if l.Len() != 4 || !l.HasClientIDs() {
		t.Fatalf("len %d, ids %v", l.Len(), l.HasClientIDs())
	}
	if e, ok := l.MatchClientID("kid"); !ok || e != "clientid:kid" {
		t.Fatal(e, ok)
	}
	if _, ok := l.MatchClientID(""); ok {
		t.Fatal("empty ClientID matched")
	}
	if _, ok := l.MatchMAC("clientid:kid"); ok {
		t.Fatal("a ClientID entry matched as a MAC")
	}
	if e, ok := l.Match(netip.MustParseAddr("192.168.1.5"), ""); !ok || e != "192.168.1.5" {
		t.Fatal(e, ok)
	}
	if NewClientList([]string{"10.0.0.1"}).HasClientIDs() {
		t.Fatal("HasClientIDs without entries")
	}
}

// The chain of the encrypted DNS listeners: a source outside the ACL is
// closed at accept, before TLS, so it never sees the certificate.
func TestLimitListenerBeforeTLS(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		DNSNames: []string{"dns.lan"}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	conf := &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var allow atomic.Bool
	acl := func() *ACL {
		if allow.Load() {
			return &ACL{allowAll: true}
		}
		return &ACL{} // nothing allowed, loopback included
	}
	ln := tls.NewListener(NewSwitchListener(LimitListener(raw, acl, 32, 1024), func() bool { return true }, 1024), conf)
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
	var seen bool
	cfg := &tls.Config{InsecureSkipVerify: true, VerifyConnection: func(tls.ConnectionState) error { seen = true; return nil }}
	if _, err := tls.Dial("tcp", raw.Addr().String(), cfg); err == nil || seen {
		t.Fatalf("handshake outside the ACL: %v, certificate seen %v", err, seen)
	}
	allow.Store(true)
	c, err := tls.Dial("tcp", raw.Addr().String(), cfg)
	if err != nil || !seen {
		t.Fatalf("inside the ACL: %v", err)
	}
	c.Close()
}
