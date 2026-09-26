package upstream

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"time"

	"github.com/miekg/dns"
)

const (
	ednsSize      = 1232 // advertised UDP payload (DNS Flag Day 2020)
	udpBufferSize = 4096 // receive buffer; replies above ednsSize are a protocol violation
	minMsgSize    = 12   // DNS header
)

var (
	errTimeout          = errors.New("timeout")
	errQuestionMismatch = errors.New("reply does not match the question")
	errNotResponse      = errors.New("reply is not a DNS response")
	errIDMismatch       = errors.New("reply ID does not match")
	errShortReply       = errors.New("reply too short")
	errRefused          = errors.New("upstream answered REFUSED")
)

// aLongTimeAgo is a deadline in the past; setting it unblocks pending I/O.
var aLongTimeAgo = time.Unix(1, 0)

// transport exchanges one query with one upstream. q is shared read-only
// between concurrent attempts; wire is its packed form (never modified).
// The reply's ID must equal q.Id; the question is verified by the caller.
type transport interface {
	exchange(ctx context.Context, q *dns.Msg, wire []byte) (*dns.Msg, error)
	close()
}

// newQuery builds a fresh upstream query: random ID, RD=1, AD=1 (RFC 6840
// 5.7, so the upstream reports its validation result), our OPT (1232, DO as
// given) and no client EDNS options.
func newQuery(name string, qtype, qclass uint16, do bool) *dns.Msg {
	q := &dns.Msg{MsgHdr: dns.MsgHdr{
		Id:                dns.Id(),
		Opcode:            dns.OpcodeQuery,
		RecursionDesired:  true,
		AuthenticatedData: true,
	}}
	q.Question = []dns.Question{{Name: name, Qtype: qtype, Qclass: qclass}}
	q.SetEdns0(ednsSize, do)
	return q
}

// checkReply verifies that m answers q: a response to a standard query that
// echoes the question (name compared case-insensitively) and, when q sent a
// client subnet, does not carry another one (checkECS).
func checkReply(q, m *dns.Msg) error {
	if !m.Response || m.Opcode != dns.OpcodeQuery {
		return errNotResponse
	}
	if len(m.Question) != 1 {
		return errQuestionMismatch
	}
	a, b := q.Question[0], m.Question[0]
	if a.Qtype != b.Qtype || a.Qclass != b.Qclass || !equalFoldASCII(a.Name, b.Name) {
		return errQuestionMismatch
	}
	return checkECS(q, m)
}

// errNoPublicAddr fails an attempt to a plain upstream given by name whose
// resolved addresses are all private, loopback or this machine's.
var errNoPublicAddr = errors.New("resolves to no public address")

// plainTransport is classic DNS over UDP (retried over TCP when the reply
// is truncated) or TCP only. An upstream given by name (host) is resolved
// through the bootstrap servers like DoT and DoH, and only its public
// unicast addresses that are not this machine's are dialled (filter), in
// the bootstrap order.
type plainTransport struct {
	addr    string // IP literal upstreams: "ip:port"
	tcpOnly bool

	host   string // named upstreams: the name, its port and resolver
	port   uint16
	boot   *bootstrap
	filter func(ctx context.Context, addrs []netip.Addr) ([]netip.Addr, error)
}

func (t *plainTransport) exchange(ctx context.Context, q *dns.Msg, wire []byte) (*dns.Msg, error) {
	if t.host == "" {
		return exchangePlain(ctx, t.addr, q, wire, t.tcpOnly)
	}
	addrs, err := t.boot.lookup(ctx, t.host)
	if err != nil {
		return nil, err
	}
	if addrs, err = t.filter(ctx, addrs); err != nil || len(addrs) == 0 {
		return nil, errNoPublicAddr
	}
	var lastErr error
	for _, a := range addrs {
		m, err := exchangePlain(ctx, netip.AddrPortFrom(a, t.port).String(), q, wire, t.tcpOnly)
		if err == nil {
			return m, nil
		}
		lastErr = err
		if ctx.Err() != nil || errors.Is(err, errTimeout) {
			break // an address that did not answer in time used up the attempt
		}
	}
	return nil, lastErr
}

func (t *plainTransport) close() {}

func exchangePlain(ctx context.Context, addr string, q *dns.Msg, wire []byte, tcpOnly bool) (*dns.Msg, error) {
	if !tcpOnly {
		m, err := exchangeUDP(ctx, addr, q, wire)
		if err != nil || !m.Truncated {
			return m, err
		}
	}
	return exchangeTCP(ctx, addr, q, wire)
}

// exchangeUDP sends wire over a fresh connected UDP socket (random source
// port) and waits for a reply with the right ID that matches the question.
// Anything else is discarded (possible spoofing attempt) until ctx ends.
func exchangeUDP(ctx context.Context, addr string, q *dns.Msg, wire []byte) (*dns.Msg, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "udp", addr)
	if err != nil {
		return nil, ctxErr(ctx, err)
	}
	defer c.Close()
	stop := watchConn(ctx, c)
	defer stop()
	if _, err := c.Write(wire); err != nil {
		return nil, ctxErr(ctx, err)
	}
	buf := make([]byte, udpBufferSize)
	var discarded error
	for {
		n, err := c.Read(buf)
		if err != nil {
			if discarded != nil && isTimeout(ctx, err) {
				return nil, fmt.Errorf("%w (discarded: %w)", errTimeout, discarded)
			}
			return nil, ctxErr(ctx, err)
		}
		m := new(dns.Msg)
		if err := m.Unpack(buf[:n]); err != nil {
			discarded = fmt.Errorf("malformed reply: %w", err)
			continue
		}
		if m.Id != q.Id {
			continue // late reply to an earlier query or a blind spoof
		}
		if err := checkReply(q, m); err != nil {
			discarded = err
			continue
		}
		return m, nil
	}
}

// exchangeTCP sends wire over a fresh TCP connection.
func exchangeTCP(ctx context.Context, addr string, q *dns.Msg, wire []byte) (*dns.Msg, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, ctxErr(ctx, err)
	}
	defer c.Close()
	m, _, err := streamRoundTrip(ctx, c, q.Id, wire)
	return m, err
}

// streamRoundTrip writes one length-prefixed message and reads one reply
// (TCP, DoT). reusable reports whether c may carry further queries.
func streamRoundTrip(ctx context.Context, c net.Conn, id uint16, wire []byte) (m *dns.Msg, reusable bool, err error) {
	stop := watchConn(ctx, c)
	m, err = func() (*dns.Msg, error) {
		buf := make([]byte, 2+len(wire))
		binary.BigEndian.PutUint16(buf, uint16(len(wire)))
		copy(buf[2:], wire)
		if _, err := c.Write(buf); err != nil {
			return nil, err
		}
		var hdr [2]byte
		if _, err := io.ReadFull(c, hdr[:]); err != nil {
			return nil, err
		}
		n := binary.BigEndian.Uint16(hdr[:])
		if n < minMsgSize {
			return nil, errShortReply
		}
		body := make([]byte, n)
		if _, err := io.ReadFull(c, body); err != nil {
			return nil, err
		}
		m := new(dns.Msg)
		if err := m.Unpack(body); err != nil {
			return nil, fmt.Errorf("malformed reply: %w", err)
		}
		if m.Id != id {
			return nil, errIDMismatch
		}
		return m, nil
	}()
	fired := !stop() // ctx ended: the deadline was moved into the past
	if err != nil {
		return nil, false, ctxErr(ctx, err)
	}
	if fired || c.SetDeadline(time.Time{}) != nil {
		return m, false, nil
	}
	return m, true, nil
}

// watchConn applies ctx's deadline to c and interrupts pending I/O when ctx
// is cancelled. stop reports false if the interrupt already happened.
func watchConn(ctx context.Context, c net.Conn) (stop func() bool) {
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	}
	return context.AfterFunc(ctx, func() { _ = c.SetDeadline(aLongTimeAgo) })
}

// isTimeout reports whether err is (or was caused by) a deadline.
func isTimeout(ctx context.Context, err error) bool {
	return errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) ||
		errors.Is(err, context.DeadlineExceeded)
}

// ctxErr maps errors caused by ctx or I/O deadlines to errTimeout or
// context.Canceled so that stats and the UI show a readable reason.
func ctxErr(ctx context.Context, err error) error {
	switch {
	case errors.Is(ctx.Err(), context.Canceled):
		return context.Canceled
	case isTimeout(ctx, err):
		return errTimeout
	}
	return err
}

// equalFoldASCII compares DNS names case-insensitively (ASCII only; other
// bytes must match exactly).
func equalFoldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		if lowerByte(a[i]) != lowerByte(b[i]) {
			return false
		}
	}
	return true
}

func lowerByte(c byte) byte {
	if 'A' <= c && c <= 'Z' {
		return c + ('a' - 'A')
	}
	return c
}

// lowerASCII lower-cases ASCII letters (DNS names are case-insensitive only
// for ASCII).
func lowerASCII(s string) string {
	for i := 0; i < len(s); i++ {
		if 'A' <= s[i] && s[i] <= 'Z' {
			b := []byte(s)
			for j := i; j < len(b); j++ {
				b[j] = lowerByte(b[j])
			}
			return string(b)
		}
	}
	return s
}
