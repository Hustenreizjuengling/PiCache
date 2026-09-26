package upstream

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/miekg/dns"
	"golang.org/x/crypto/chacha20"
	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/nacl/secretbox"
	"golang.org/x/crypto/poly1305"
	"golang.org/x/crypto/salsa20/salsa"

	"github.com/hustenreizjuengling/picache/internal/settings"
)

// DNSCrypt client (protocol version 2; es-versions 1,
// X25519-XSalsa20Poly1305, and 2, X25519-XChaCha20Poly1305 with the shared
// key derived by HChaCha20). The resolver's certificate is fetched with a
// TXT query for the provider name sent directly to the stamp's address
// (never through the upstreams or the bootstrap servers), checked against
// the provider key (Ed25519) and refreshed hourly; a new X25519 key pair is
// made for every certificate, so the shared key is computed once per
// certificate.

const (
	dnscryptCertSize   = 124
	dnscryptMaxCerts   = 8   // TXT records examined per certificate fetch
	dnscryptMinUDP     = 256 // padded query length over UDP
	dnscryptBlock      = 64  // queries are padded to a multiple of this
	dnscryptHalfNonce  = 12
	dnscryptMaxReply   = 64 << 10
	dnscryptRefresh    = time.Hour
	dnscryptEarlyGap   = time.Minute // at most one early refresh per minute
	dnscryptRetryAfter = time.Minute // a failed refresh is tried again after this
	dnscryptUDPBuffer  = 4096        // a resolver answers UDP with at most the query's length
)

var (
	dnscryptCertMagic     = []byte("DNSC")
	dnscryptResolverMagic = []byte{0x72, 0x36, 0x66, 0x6e, 0x76, 0x57, 0x6a, 0x38}

	errNoDNSCryptCert = errors.New("no valid DNSCrypt certificate")
	errDNSCryptReply  = errors.New("DNSCrypt reply cannot be decrypted")
)

// dnscryptCert is a verified resolver certificate with the client key pair
// made for it.
type dnscryptCert struct {
	esVersion   uint16
	resolverPK  [32]byte
	clientMagic [8]byte
	serial      uint32
	notBefore   time.Time
	notAfter    time.Time
	clientPK    [32]byte
	shared      [32]byte
}

// dnscryptTransport exchanges queries with one DNSCrypt resolver.
type dnscryptTransport struct {
	addr        netip.AddrPort
	provider    string
	providerKey ed25519.PublicKey
	now         func() time.Time
	// fetch returns the TXT records of the provider name (tests replace it).
	fetch func(ctx context.Context) ([]*dns.TXT, error)

	fetchSem chan struct{} // one certificate fetch at a time

	mu        sync.Mutex
	cert      *dnscryptCert
	refreshAt time.Time // the next refresh (hourly; after a failure a minute later)
	lastEarly time.Time // the last early refresh (undecryptable reply)
}

func newDNSCrypt(spec settings.UpstreamSpec) *dnscryptTransport {
	t := &dnscryptTransport{addr: spec.DialAddr, provider: spec.ProviderName,
		providerKey: ed25519.PublicKey(spec.ProviderKey[:]), now: time.Now, fetchSem: make(chan struct{}, 1)}
	t.fetch = t.fetchTXT
	return t
}

func (t *dnscryptTransport) close() {}

func (t *dnscryptTransport) exchange(ctx context.Context, q *dns.Msg, wire []byte) (*dns.Msg, error) {
	cert, err := t.certificate(ctx)
	if err != nil {
		return nil, err
	}
	m, err := t.exchangeProto(ctx, cert, "udp", q.Id, wire)
	if err != nil || !m.Truncated {
		return m, err
	}
	return t.exchangeProto(ctx, cert, "tcp", q.Id, wire)
}

// certificate returns a certificate that is valid now, fetching one when
// none is, when the hourly refresh is due or an early refresh was
// requested. A failed refresh keeps the current certificate until it
// expires.
func (t *dnscryptTransport) certificate(ctx context.Context) (*dnscryptCert, error) {
	now := t.now()
	t.mu.Lock()
	cur := t.cert
	usable := cur != nil && !now.Before(cur.notBefore) && now.Before(cur.notAfter)
	due := !now.Before(t.refreshAt)
	t.mu.Unlock()
	if usable && !due {
		return cur, nil
	}
	if usable {
		// Refresh while the current certificate still serves; a query
		// that finds another refresh running uses the current one.
		select {
		case t.fetchSem <- struct{}{}:
		default:
			return cur, nil
		}
	} else {
		select {
		case t.fetchSem <- struct{}{}:
		case <-ctx.Done():
			return nil, ctxErr(ctx, ctx.Err())
		}
	}
	defer func() { <-t.fetchSem }()
	t.mu.Lock()
	if t.cert != cur && t.cert != nil && t.now().Before(t.cert.notAfter) {
		c := t.cert // refreshed while this query waited
		t.mu.Unlock()
		return c, nil
	}
	t.mu.Unlock()

	next, err := t.refresh(ctx)
	now = t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	if err != nil {
		t.refreshAt = now.Add(dnscryptRetryAfter)
		if usable && now.Before(cur.notAfter) {
			return cur, nil
		}
		return nil, err
	}
	t.cert = next
	t.refreshAt = now.Add(dnscryptRefresh)
	if next.notAfter.Before(t.refreshAt) {
		t.refreshAt = next.notAfter
	}
	return next, nil
}

// requestEarlyRefresh makes the next query fetch the certificate again (at
// most once a minute): a reply that cannot be decrypted means the
// resolver changed its keys.
func (t *dnscryptTransport) requestEarlyRefresh() {
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.lastEarly.IsZero() && now.Sub(t.lastEarly) < dnscryptEarlyGap {
		return
	}
	t.lastEarly = now
	t.refreshAt = now
}

// refresh fetches the certificates and chooses one (dnscryptChoose); the
// client key pair and the shared key are made for it.
func (t *dnscryptTransport) refresh(ctx context.Context) (*dnscryptCert, error) {
	ctx, cancel := context.WithTimeout(ctx, defaultAttemptTimeout)
	defer cancel()
	txts, err := t.fetch(ctx)
	if err != nil {
		return nil, fmt.Errorf("DNSCrypt certificate: %w", ctxErr(ctx, err))
	}
	c, err := dnscryptChoose(txts, t.providerKey, t.now())
	if err != nil {
		return nil, err
	}
	var sk [32]byte
	if _, err := rand.Read(sk[:]); err != nil {
		return nil, err
	}
	pk, err := curve25519.X25519(sk[:], curve25519.Basepoint)
	if err != nil {
		return nil, err
	}
	copy(c.clientPK[:], pk)
	if c.shared, err = dnscryptSharedKey(c.esVersion, &sk, &c.resolverPK); err != nil {
		return nil, err
	}
	return c, nil
}

// fetchTXT asks the resolver itself for the TXT records of the provider
// name (UDP, TCP when the reply is truncated).
func (t *dnscryptTransport) fetchTXT(ctx context.Context) ([]*dns.TXT, error) {
	q := newQuery(dns.Fqdn(t.provider), dns.TypeTXT, dns.ClassINET, false)
	wire, err := q.Pack()
	if err != nil {
		return nil, err
	}
	m, err := exchangePlain(ctx, t.addr.String(), q, wire, false)
	if err != nil {
		return nil, err
	}
	if m.Rcode != dns.RcodeSuccess {
		return nil, fmt.Errorf("the resolver answered %s", dns.RcodeToString[m.Rcode])
	}
	var out []*dns.TXT
	for _, rr := range m.Answer {
		if txt, ok := rr.(*dns.TXT); ok {
			out = append(out, txt)
		}
	}
	return out, nil
}

// dnscryptChoose checks at most 8 TXT records and chooses a certificate:
// exactly 124 bytes starting with "DNSC", signed with the provider key,
// valid now, es-version 1 or 2; es-version 2 when a valid one exists, and
// within the version the highest serial.
func dnscryptChoose(txts []*dns.TXT, providerKey ed25519.PublicKey, now time.Time) (*dnscryptCert, error) {
	var best *dnscryptCert
	for i, txt := range txts {
		if i == dnscryptMaxCerts {
			break
		}
		b, ok := txtBytes(txt.Txt)
		if !ok {
			continue
		}
		c, ok := parseDNSCryptCert(b, providerKey, now)
		if !ok {
			continue
		}
		if best == nil || c.esVersion > best.esVersion || (c.esVersion == best.esVersion && c.serial > best.serial) {
			best = c
		}
	}
	if best == nil {
		return nil, errNoDNSCryptCert
	}
	return best, nil
}

// parseDNSCryptCert parses and verifies one certificate:
// "DNSC" es-version(2) minor(2) signature(64) resolver-pk(32)
// client-magic(8) serial(4) ts-start(4) ts-end(4); the signature covers
// everything after it.
func parseDNSCryptCert(b []byte, providerKey ed25519.PublicKey, now time.Time) (*dnscryptCert, bool) {
	if len(b) != dnscryptCertSize || !bytes.Equal(b[:4], dnscryptCertMagic) || len(providerKey) != ed25519.PublicKeySize {
		return nil, false
	}
	c := &dnscryptCert{esVersion: binary.BigEndian.Uint16(b[4:6])}
	if c.esVersion != 1 && c.esVersion != 2 {
		return nil, false
	}
	if !ed25519.Verify(providerKey, b[72:], b[8:72]) {
		return nil, false
	}
	copy(c.resolverPK[:], b[72:104])
	copy(c.clientMagic[:], b[104:112])
	c.serial = binary.BigEndian.Uint32(b[112:116])
	c.notBefore = time.Unix(int64(binary.BigEndian.Uint32(b[116:120])), 0)
	c.notAfter = time.Unix(int64(binary.BigEndian.Uint32(b[120:124])), 0)
	if now.Before(c.notBefore) || !now.Before(c.notAfter) {
		return nil, false
	}
	return c, true
}

// txtBytes joins the character-strings of a TXT record into its bytes
// (the library escapes quotes, backslashes and unprintable bytes as \X
// and \DDD).
func txtBytes(parts []string) ([]byte, bool) {
	var out []byte
	for _, s := range parts {
		for i := 0; i < len(s); i++ {
			c := s[i]
			if c != '\\' {
				out = append(out, c)
				continue
			}
			if i+3 < len(s) && isDigit(s[i+1]) && isDigit(s[i+2]) && isDigit(s[i+3]) {
				v := int(s[i+1]-'0')*100 + int(s[i+2]-'0')*10 + int(s[i+3]-'0')
				if v > 255 {
					return nil, false
				}
				out = append(out, byte(v))
				i += 3
				continue
			}
			if i+1 >= len(s) {
				return nil, false
			}
			out = append(out, s[i+1])
			i++
		}
		if len(out) > dnscryptCertSize {
			return nil, false
		}
	}
	return out, true
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// dnscryptSharedKey derives the shared key of an es-version from the
// client's secret key and the resolver's public key: X25519, then HSalsa20
// (1, as NaCl's box) or HChaCha20 (2), each with a zero nonce.
func dnscryptSharedKey(es uint16, sk, pk *[32]byte) ([32]byte, error) {
	var out [32]byte
	s, err := curve25519.X25519(sk[:], pk[:])
	if err != nil {
		return out, err // a low-order resolver key
	}
	switch es {
	case 1:
		var zero [16]byte
		salsa.HSalsa20(&out, &zero, (*[32]byte)(s), &salsa.Sigma)
	case 2:
		k, err := chacha20.HChaCha20(s, make([]byte, 16))
		if err != nil {
			return out, err
		}
		copy(out[:], k)
	default:
		return out, fmt.Errorf("unsupported es-version %d", es)
	}
	return out, nil
}

// dnscryptSeal encrypts msg (tag first, as NaCl's secretbox): XSalsa20 or
// XChaCha20 with Poly1305.
func dnscryptSeal(es uint16, msg []byte, nonce *[24]byte, key *[32]byte) []byte {
	if es == 1 {
		return secretbox.Seal(nil, msg, nonce, key)
	}
	c, _ := chacha20.NewUnauthenticatedCipher(key[:], nonce[:])
	var block0 [64]byte
	c.XORKeyStream(block0[:], block0[:])
	var polyKey [32]byte
	copy(polyKey[:], block0[:32])
	out := make([]byte, poly1305.TagSize+len(msg))
	ct := out[poly1305.TagSize:]
	first := min(len(msg), 32)
	for i := range first {
		ct[i] = block0[32+i] ^ msg[i]
	}
	c.SetCounter(1)
	c.XORKeyStream(ct[first:], msg[first:])
	var tag [poly1305.TagSize]byte
	poly1305.Sum(&tag, ct, &polyKey)
	copy(out, tag[:])
	return out
}

// dnscryptOpen decrypts and authenticates a box of dnscryptSeal.
func dnscryptOpen(es uint16, box []byte, nonce *[24]byte, key *[32]byte) ([]byte, bool) {
	if es == 1 {
		return secretbox.Open(nil, box, nonce, key)
	}
	if len(box) < poly1305.TagSize {
		return nil, false
	}
	c, _ := chacha20.NewUnauthenticatedCipher(key[:], nonce[:])
	var block0 [64]byte
	c.XORKeyStream(block0[:], block0[:])
	var polyKey [32]byte
	copy(polyKey[:], block0[:32])
	var tag [poly1305.TagSize]byte
	copy(tag[:], box[:poly1305.TagSize])
	ct := box[poly1305.TagSize:]
	if !poly1305.Verify(&tag, ct, &polyKey) {
		return nil, false
	}
	out := make([]byte, len(ct))
	first := min(len(ct), 32)
	for i := range first {
		out[i] = block0[32+i] ^ ct[i]
	}
	c.SetCounter(1)
	c.XORKeyStream(out[first:], ct[first:])
	return out, true
}

// dnscryptPad pads a query with 0x80 and zeros to a multiple of 64 bytes,
// at least min bytes.
func dnscryptPad(msg []byte, minLen int) []byte {
	n := max(minLen, (len(msg)+1+dnscryptBlock-1)/dnscryptBlock*dnscryptBlock)
	out := make([]byte, n)
	copy(out, msg)
	out[len(msg)] = 0x80
	return out
}

// dnscryptUnpad removes the padding (0x80 followed by zeros).
func dnscryptUnpad(b []byte) ([]byte, bool) {
	i := len(b) - 1
	for i >= 0 && b[i] == 0 {
		i--
	}
	if i < 0 || b[i] != 0x80 {
		return nil, false
	}
	return b[:i], true
}

// dnscryptQuery encrypts a query: client magic, client public key, the
// 12-byte client nonce (the other half of the 24-byte nonce is zero, as
// the specification says) and the padded, encrypted query. It returns the
// packet and the nonce.
func dnscryptQuery(c *dnscryptCert, wire []byte, minLen int) ([]byte, [24]byte, error) {
	var nonce [24]byte
	if _, err := rand.Read(nonce[:dnscryptHalfNonce]); err != nil {
		return nil, nonce, err
	}
	box := dnscryptSeal(c.esVersion, dnscryptPad(wire, minLen), &nonce, &c.shared)
	pkt := make([]byte, 0, 8+32+dnscryptHalfNonce+len(box))
	pkt = append(pkt, c.clientMagic[:]...)
	pkt = append(pkt, c.clientPK[:]...)
	pkt = append(pkt, nonce[:dnscryptHalfNonce]...)
	pkt = append(pkt, box...)
	return pkt, nonce, nil
}

// dnscryptReply decrypts a resolver reply: the resolver magic, the first
// 12 bytes of its nonce equal to the query's (else it is not the reply to
// this query: foreign), then the decrypted, unpadded message.
func dnscryptReply(c *dnscryptCert, pkt []byte, nonce *[24]byte) (msg []byte, foreign bool, err error) {
	if len(pkt) < 8+24+poly1305.TagSize || !bytes.Equal(pkt[:8], dnscryptResolverMagic) ||
		!bytes.Equal(pkt[8:8+dnscryptHalfNonce], nonce[:dnscryptHalfNonce]) {
		return nil, true, nil
	}
	var full [24]byte
	copy(full[:], pkt[8:32])
	plain, ok := dnscryptOpen(c.esVersion, pkt[32:], &full, &c.shared)
	if !ok {
		return nil, false, errDNSCryptReply
	}
	if plain, ok = dnscryptUnpad(plain); !ok {
		return nil, false, errDNSCryptReply
	}
	return plain, false, nil
}

// exchangeProto sends one encrypted query over UDP (padded to at least
// 256 bytes) or TCP (2-byte length prefix) and returns the verified
// reply's message. UDP replies that are not for this query are discarded
// until the context ends; a reply that cannot be decrypted requests an
// early certificate refresh.
func (t *dnscryptTransport) exchangeProto(ctx context.Context, c *dnscryptCert, network string, id uint16, wire []byte) (*dns.Msg, error) {
	minLen := 0
	if network == "udp" {
		minLen = dnscryptMinUDP
	}
	pkt, nonce, err := dnscryptQuery(c, wire, minLen)
	if err != nil {
		return nil, err
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, network, t.addr.String())
	if err != nil {
		return nil, ctxErr(ctx, err)
	}
	defer conn.Close()
	stop := watchConn(ctx, conn)
	defer stop()
	if network == "tcp" {
		buf := make([]byte, 2+len(pkt))
		binary.BigEndian.PutUint16(buf, uint16(len(pkt)))
		copy(buf[2:], pkt)
		pkt = buf
	}
	if _, err := conn.Write(pkt); err != nil {
		return nil, ctxErr(ctx, err)
	}
	var discarded error
	for {
		var reply []byte
		if network == "tcp" {
			var hdr [2]byte
			if _, err := io.ReadFull(conn, hdr[:]); err != nil {
				return nil, ctxErr(ctx, err)
			}
			reply = make([]byte, binary.BigEndian.Uint16(hdr[:]))
			if _, err := io.ReadFull(conn, reply); err != nil {
				return nil, ctxErr(ctx, err)
			}
		} else {
			buf := make([]byte, dnscryptUDPBuffer)
			n, err := conn.Read(buf)
			if err != nil {
				if discarded != nil && isTimeout(ctx, err) {
					return nil, fmt.Errorf("%w (discarded: %w)", errTimeout, discarded)
				}
				return nil, ctxErr(ctx, err)
			}
			reply = buf[:n]
		}
		plain, foreign, err := dnscryptReply(c, reply, &nonce)
		switch {
		case err != nil:
			t.requestEarlyRefresh()
			if network == "tcp" {
				return nil, err
			}
			discarded = err
			continue
		case foreign:
			if network == "tcp" {
				return nil, errors.New("DNSCrypt reply to another query")
			}
			discarded = errors.New("DNSCrypt reply to another query")
			continue
		}
		m := new(dns.Msg)
		if err := m.Unpack(plain); err != nil {
			if network == "tcp" {
				return nil, fmt.Errorf("malformed reply: %w", err)
			}
			discarded = fmt.Errorf("malformed reply: %w", err)
			continue
		}
		if m.Id != id {
			if network == "tcp" {
				return nil, errIDMismatch
			}
			continue
		}
		return m, nil
	}
}
