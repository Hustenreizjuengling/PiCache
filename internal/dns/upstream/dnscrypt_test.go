package upstream

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/miekg/dns"
	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/nacl/box"

	"github.com/hustenreizjuengling/picache/internal/settings"
)

const testProvider = "2.dnscrypt-cert.example.test"

// escapeTXT writes raw bytes as a TXT string the way the DNS library
// presents them (quotes and backslashes escaped, unprintable bytes \DDD).
func escapeTXT(b []byte) string {
	var s strings.Builder
	for _, c := range b {
		switch {
		case c == '"' || c == '\\':
			s.WriteByte('\\')
			s.WriteByte(c)
		case c < ' ' || c > '~':
			s.WriteString("\\" + strconv.Itoa(int(c)/100) + strconv.Itoa(int(c)/10%10) + strconv.Itoa(int(c)%10))
		default:
			s.WriteByte(c)
		}
	}
	return s.String()
}

// testDNSCryptCert builds a signed certificate.
func testDNSCryptCert(provider ed25519.PrivateKey, es uint16, resolverPK [32]byte, magic [8]byte, serial uint32, from, to time.Time) []byte {
	b := make([]byte, dnscryptCertSize)
	copy(b, "DNSC")
	binary.BigEndian.PutUint16(b[4:], es)
	copy(b[72:], resolverPK[:])
	copy(b[104:], magic[:])
	binary.BigEndian.PutUint32(b[112:], serial)
	binary.BigEndian.PutUint32(b[116:], uint32(from.Unix()))
	binary.BigEndian.PutUint32(b[120:], uint32(to.Unix()))
	copy(b[8:72], ed25519.Sign(provider, b[72:]))
	return b
}

// dnscryptServer is a DNSCrypt resolver for tests on one UDP and TCP port:
// plain TXT queries for the provider name get the certificates, encrypted
// queries are answered with answer.
type dnscryptServer struct {
	addr     netip.AddrPort
	pk       ed25519.PublicKey
	sk       ed25519.PrivateKey
	es       uint16
	magic    [8]byte
	resSK    [32]byte
	resPK    [32]byte
	certs    [][]byte
	udp, tcp atomic.Int32
	// mangle, if set, returns bad UDP replies sent before the genuine one.
	mangle func(reply []byte) [][]byte
	// truncateUDP answers every UDP query with TC set.
	truncateUDP bool
	// dropGenuine sends only the replies of mangle.
	dropGenuine bool
}

func startDNSCrypt(t *testing.T, es uint16) *dnscryptServer {
	t.Helper()
	pk, sk, _ := ed25519.GenerateKey(rand.Reader)
	s := &dnscryptServer{pk: pk, sk: sk, es: es, magic: [8]byte{'q', '6', 'f', 'n', 'v', 'W', 'j', '8'}}
	_, _ = rand.Read(s.resSK[:])
	rp, _ := curve25519.X25519(s.resSK[:], curve25519.Basepoint)
	copy(s.resPK[:], rp)
	now := time.Now()
	s.certs = [][]byte{testDNSCryptCert(sk, es, s.resPK, s.magic, 7, now.Add(-time.Hour), now.Add(time.Hour))}
	var ln net.Listener
	var pc net.PacketConn
	for range 50 {
		var err error
		if ln, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
			t.Fatal(err)
		}
		if pc, err = net.ListenPacket("udp", ln.Addr().String()); err == nil {
			break
		}
		_ = ln.Close()
	}
	s.addr = netip.MustParseAddrPort(pc.LocalAddr().String())
	var wg sync.WaitGroup
	t.Cleanup(func() { _ = pc.Close(); _ = ln.Close(); wg.Wait() })
	wg.Go(func() {
		buf := make([]byte, 65536)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			for _, out := range s.handle(buf[:n], "udp") {
				_, _ = pc.WriteTo(out, from)
			}
		}
	})
	wg.Go(func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Go(func() {
				defer c.Close()
				var hdr [2]byte
				if _, err := io.ReadFull(c, hdr[:]); err != nil {
					return
				}
				in := make([]byte, binary.BigEndian.Uint16(hdr[:]))
				if _, err := io.ReadFull(c, in); err != nil {
					return
				}
				for _, out := range s.handle(in, "tcp") {
					buf := make([]byte, 2+len(out))
					binary.BigEndian.PutUint16(buf, uint16(len(out)))
					copy(buf[2:], out)
					_, _ = c.Write(buf)
				}
			})
		}
	})
	return s
}

func (s *dnscryptServer) handle(in []byte, network string) [][]byte {
	if len(in) > 8 && bytes.Equal(in[:8], s.magic[:]) {
		if network == "udp" {
			s.udp.Add(1)
		} else {
			s.tcp.Add(1)
		}
		return s.encrypted(in, network)
	}
	q := new(dns.Msg)
	if q.Unpack(in) != nil || len(q.Question) != 1 {
		return nil
	}
	m := new(dns.Msg).SetReply(q)
	if q.Question[0].Qtype == dns.TypeTXT && strings.EqualFold(q.Question[0].Name, testProvider+".") {
		for _, c := range s.certs {
			m.Answer = append(m.Answer, &dns.TXT{Hdr: dns.RR_Header{Name: q.Question[0].Name, Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: 60},
				Txt: []string{escapeTXT(c)}})
		}
	}
	out, _ := m.Pack()
	return [][]byte{out}
}

func (s *dnscryptServer) encrypted(in []byte, network string) [][]byte {
	if len(in) < 8+32+12+16 {
		return nil
	}
	var clientPK [32]byte
	copy(clientPK[:], in[8:40])
	shared, err := dnscryptSharedKey(s.es, &s.resSK, &clientPK)
	if err != nil {
		return nil
	}
	var nonce [24]byte
	copy(nonce[:12], in[40:52])
	plain, ok := dnscryptOpen(s.es, in[52:], &nonce, &shared)
	if !ok {
		return nil
	}
	if network == "udp" && len(plain) < dnscryptMinUDP || len(plain)%dnscryptBlock != 0 {
		return nil // the client must pad
	}
	plain, ok = dnscryptUnpad(plain)
	if !ok {
		return nil
	}
	q := new(dns.Msg)
	if q.Unpack(plain) != nil {
		return nil
	}
	m := answerA(q, "192.0.2.222", 60)
	if network == "udp" && s.truncateUDP {
		m.Answer, m.Truncated = nil, true
	}
	wire, _ := m.Pack()
	_, _ = rand.Read(nonce[12:])
	box := dnscryptSeal(s.es, dnscryptPad(wire, 0), &nonce, &shared)
	reply := append(append(append([]byte{}, dnscryptResolverMagic...), nonce[:]...), box...)
	if s.mangle != nil && network == "udp" {
		if s.dropGenuine {
			return s.mangle(reply)
		}
		return append(s.mangle(reply), reply)
	}
	return [][]byte{reply}
}

// stamp returns the DNSCrypt stamp of the server.
func (s *dnscryptServer) stamp() string {
	b := []byte{0x01, 0, 0, 0, 0, 0, 0, 0, 0}
	addr := s.addr.String()
	b = append(b, byte(len(addr)))
	b = append(b, addr...)
	b = append(b, 32)
	b = append(b, s.pk...)
	b = append(b, byte(len(testProvider)))
	b = append(b, testProvider...)
	return "sdns://" + base64.RawURLEncoding.EncodeToString(b)
}

func TestDNSCryptRoundTrips(t *testing.T) {
	for _, es := range []uint16{1, 2} {
		t.Run("es-version "+strconv.Itoa(int(es)), func(t *testing.T) {
			s := startDNSCrypt(t, es)
			up := s.stamp()
			st := newStore(t, func(d *settings.DNS) { d.Upstreams = []string{up}; d.CacheEnabled = false; d.Bootstrap = nil })
			r := newTestResolver(t, st, testOptions(), nil)
			defer r.Close()
			for i := range 2 {
				m, info, err := r.Resolve(context.Background(), query("crypt"+strconv.Itoa(i)+".example.", dns.TypeA, 31, false), noECS)
				if err != nil {
					t.Fatal(err)
				}
				if ip, _ := firstA(t, m); ip != "192.0.2.222" || m.Id != 31 || info.Upstream != "sdns:dnscrypt:"+testProvider {
					t.Fatalf("answer %s id %d info %+v", ip, m.Id, info)
				}
			}
			if s.udp.Load() != 2 || s.tcp.Load() != 0 {
				t.Fatalf("udp %d tcp %d", s.udp.Load(), s.tcp.Load())
			}
		})
	}
}

// TestDNSCryptTruncatedUsesTCP: a truncated UDP reply is retried over TCP.
func TestDNSCryptTruncatedUsesTCP(t *testing.T) {
	s := startDNSCrypt(t, 2)
	s.truncateUDP = true
	spec, _ := settings.ParseUpstream(s.stamp())
	tr := newDNSCrypt(spec)
	q := newQuery("tc.example.", dns.TypeA, dns.ClassINET, false)
	wire, _ := q.Pack()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	m, err := tr.exchange(ctx, q, wire)
	if err != nil {
		t.Fatal(err)
	}
	if ip, _ := firstA(t, m); ip != "192.0.2.222" || s.udp.Load() != 1 || s.tcp.Load() != 1 {
		t.Fatalf("answer %s, udp %d tcp %d", ip, s.udp.Load(), s.tcp.Load())
	}
}

// TestDNSCryptDiscardsForeignReplies: replies with another nonce prefix
// or without the resolver magic are discarded; an undecryptable reply
// requests an early refresh.
func TestDNSCryptDiscardsForeignReplies(t *testing.T) {
	s := startDNSCrypt(t, 1)
	s.mangle = func(reply []byte) [][]byte {
		foreign := bytes.Clone(reply)
		foreign[8] ^= 0xff // another query's nonce
		magic := bytes.Clone(reply)
		magic[0] ^= 0xff
		broken := bytes.Clone(reply)
		broken[len(broken)-1] ^= 0xff // fails authentication
		return [][]byte{foreign, magic, broken}
	}
	spec, _ := settings.ParseUpstream(s.stamp())
	tr := newDNSCrypt(spec)
	q := newQuery("foreign.example.", dns.TypeA, dns.ClassINET, false)
	wire, _ := q.Pack()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	m, err := tr.exchange(ctx, q, wire)
	if err != nil {
		t.Fatal(err)
	}
	if ip, _ := firstA(t, m); ip != "192.0.2.222" {
		t.Fatalf("answer %s", ip)
	}
	tr.mu.Lock()
	early := tr.lastEarly
	tr.mu.Unlock()
	if early.IsZero() {
		t.Fatal("no early refresh requested")
	}

	// Only a foreign reply arrives: the attempt times out.
	s.dropGenuine = true
	s.mangle = func(reply []byte) [][]byte { f := bytes.Clone(reply); f[9] ^= 1; return [][]byte{f} }
	ctx2, cancel2 := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel2()
	if _, err := tr.exchange(ctx2, q, wire); err == nil || !errors.Is(err, errTimeout) {
		t.Fatalf("foreign-only reply accepted: %v", err)
	}
}

func TestDNSCryptCertificateChoice(t *testing.T) {
	pk, sk, _ := ed25519.GenerateKey(rand.Reader)
	_, otherSK, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Unix(1_800_000_000, 0)
	valid := func(key ed25519.PrivateKey, es uint16, serial uint32) []byte {
		return testDNSCryptCert(key, es, [32]byte{byte(es), byte(serial)}, [8]byte{byte(serial)}, serial, now.Add(-time.Hour), now.Add(time.Hour))
	}
	txt := func(certs ...[]byte) []*dns.TXT {
		var out []*dns.TXT
		for _, c := range certs {
			out = append(out, &dns.TXT{Txt: []string{escapeTXT(c)}})
		}
		return out
	}
	expired := testDNSCryptCert(sk, 2, [32]byte{9}, [8]byte{9}, 99, now.Add(-2*time.Hour), now.Add(-time.Hour))
	future := testDNSCryptCert(sk, 2, [32]byte{9}, [8]byte{9}, 98, now.Add(time.Hour), now.Add(2*time.Hour))
	es3 := valid(sk, 3, 97)
	long := append(valid(sk, 2, 96), 0)
	badMagic := valid(sk, 2, 95)
	badMagic[0] = 'X'
	for _, tc := range []struct {
		name   string
		txts   []*dns.TXT
		es     uint16
		serial uint32
		err    bool
	}{
		{"es-version 2 preferred", txt(valid(sk, 1, 50), valid(sk, 2, 3)), 2, 3, false},
		{"highest serial", txt(valid(sk, 1, 4), valid(sk, 1, 9), valid(sk, 1, 6)), 1, 9, false},
		{"foreign signature", txt(valid(otherSK, 2, 80), valid(sk, 1, 2)), 1, 2, false},
		{"validity", txt(expired, future, valid(sk, 1, 1)), 1, 1, false},
		{"unsupported es-version", txt(es3, valid(sk, 1, 1)), 1, 1, false},
		{"124-byte rule and magic", txt(long, badMagic, valid(sk, 1, 1)), 1, 1, false},
		{"nothing valid", txt(expired, es3), 0, 0, true},
		{"only the first 8 records", append(txt(expired, expired, expired, expired, expired, expired, expired, expired), txt(valid(sk, 2, 1))...), 0, 0, true},
		{"split strings", []*dns.TXT{{Txt: []string{escapeTXT(valid(sk, 2, 5)[:60]), escapeTXT(valid(sk, 2, 5)[60:])}}}, 2, 5, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := dnscryptChoose(tc.txts, pk, now)
			if tc.err {
				if err == nil {
					t.Fatalf("chose %+v", c)
				}
				return
			}
			if err != nil || c.esVersion != tc.es || c.serial != tc.serial {
				t.Fatalf("chose %+v, %v", c, err)
			}
		})
	}
}

// TestDNSCryptRefresh: certificates are refreshed hourly; a failed refresh
// keeps the current certificate until it expires; an early refresh
// (undecryptable reply) happens at most once a minute.
func TestDNSCryptRefresh(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pk, sk, _ := ed25519.GenerateKey(rand.Reader)
		var fetches atomic.Int32
		var fail atomic.Bool
		spec := settings.UpstreamSpec{Proto: "dnscrypt", ProviderName: testProvider, DialAddr: netip.MustParseAddrPort("192.0.2.1:443")}
		copy(spec.ProviderKey[:], pk)
		tr := newDNSCrypt(spec)
		var resSK, resPK [32]byte
		_, _ = rand.Read(resSK[:])
		rp, _ := curve25519.X25519(resSK[:], curve25519.Basepoint)
		copy(resPK[:], rp)
		tr.fetch = func(context.Context) ([]*dns.TXT, error) {
			fetches.Add(1)
			if fail.Load() {
				return nil, errors.New("unreachable")
			}
			c := testDNSCryptCert(sk, 2, resPK, [8]byte{1}, uint32(fetches.Load()), time.Now().Add(-time.Minute), time.Now().Add(90*time.Minute))
			return []*dns.TXT{{Txt: []string{escapeTXT(c)}}}, nil
		}
		ctx := context.Background()
		c1, err := tr.certificate(ctx)
		if err != nil || fetches.Load() != 1 {
			t.Fatalf("first: %v, fetches %d", err, fetches.Load())
		}
		time.Sleep(59 * time.Minute)
		if c, _ := tr.certificate(ctx); c != c1 || fetches.Load() != 1 {
			t.Fatal("refreshed before the hour")
		}
		time.Sleep(2 * time.Minute)
		fail.Store(true)
		if c, err := tr.certificate(ctx); err != nil || c != c1 || fetches.Load() != 2 {
			t.Fatalf("failed refresh must keep the certificate: %v, fetches %d", err, fetches.Load())
		}
		if _, _ = tr.certificate(ctx); fetches.Load() != 2 {
			t.Fatal("a failed refresh is retried at once")
		}
		time.Sleep(time.Minute)
		if _, _ = tr.certificate(ctx); fetches.Load() != 3 {
			t.Fatalf("retry after a minute: fetches %d", fetches.Load())
		}
		time.Sleep(30 * time.Minute) // the certificate expired (90 min)
		if _, err := tr.certificate(ctx); err == nil {
			t.Fatal("an expired certificate is still used")
		}
		fail.Store(false)
		time.Sleep(time.Minute)
		c2, err := tr.certificate(ctx)
		if err != nil || c2 == c1 {
			t.Fatalf("no new certificate: %v", err)
		}
		n := fetches.Load()
		tr.requestEarlyRefresh()
		if _, _ = tr.certificate(ctx); fetches.Load() != n+1 {
			t.Fatal("early refresh not done")
		}
		tr.requestEarlyRefresh() // within a minute: ignored
		if _, _ = tr.certificate(ctx); fetches.Load() != n+1 {
			t.Fatal("second early refresh within a minute")
		}
		time.Sleep(time.Minute)
		tr.requestEarlyRefresh()
		if _, _ = tr.certificate(ctx); fetches.Load() != n+2 {
			t.Fatal("early refresh after a minute not done")
		}
	})
}

// TestDNSCryptPrimitives: the es-version 1 shared key is NaCl's box key,
// sealed boxes open only with the right key and nonce, and the padding
// round-trips.
func TestDNSCryptPrimitives(t *testing.T) {
	var sk, peerSK, peerPK [32]byte
	_, _ = rand.Read(sk[:])
	_, _ = rand.Read(peerSK[:])
	p, _ := curve25519.X25519(peerSK[:], curve25519.Basepoint)
	copy(peerPK[:], p)
	var want [32]byte
	box.Precompute(&want, &peerPK, &sk)
	got, err := dnscryptSharedKey(1, &sk, &peerPK)
	if err != nil || got != want {
		t.Fatalf("es-version 1 shared key differs from NaCl box: %v", err)
	}
	for _, es := range []uint16{1, 2} {
		key, _ := dnscryptSharedKey(es, &sk, &peerPK)
		var nonce [24]byte
		_, _ = rand.Read(nonce[:])
		for _, n := range []int{0, 1, 31, 32, 33, 64, 300} {
			msg := make([]byte, n)
			_, _ = rand.Read(msg)
			sealed := dnscryptSeal(es, msg, &nonce, &key)
			if len(sealed) != n+16 {
				t.Fatalf("es %d: sealed length %d", es, len(sealed))
			}
			if out, ok := dnscryptOpen(es, sealed, &nonce, &key); !ok || !bytes.Equal(out, msg) {
				t.Fatalf("es %d len %d: round trip failed", es, n)
			}
			other := nonce
			other[23] ^= 1
			if _, ok := dnscryptOpen(es, sealed, &other, &key); ok {
				t.Fatalf("es %d: opened with another nonce", es)
			}
		}
	}
	for _, n := range []int{0, 12, 63, 64, 255, 300} {
		padded := dnscryptPad(make([]byte, n), dnscryptMinUDP)
		if len(padded)%dnscryptBlock != 0 || len(padded) < dnscryptMinUDP || len(padded) <= n {
			t.Fatalf("pad %d → %d", n, len(padded))
		}
		if out, ok := dnscryptUnpad(padded); !ok || len(out) != n {
			t.Fatalf("unpad %d", n)
		}
	}
	if _, ok := dnscryptUnpad([]byte{1, 2, 0}); ok {
		t.Fatal("missing 0x80 accepted")
	}
	if b, ok := txtBytes([]string{`a\"b\\c\009\255`}); !ok || !bytes.Equal(b, []byte{'a', '"', 'b', '\\', 'c', 9, 255}) {
		t.Fatalf("txtBytes %v %v", b, ok)
	}
	if _, ok := txtBytes([]string{`\256`}); ok {
		t.Fatal("an escape above 255 accepted")
	}
}
