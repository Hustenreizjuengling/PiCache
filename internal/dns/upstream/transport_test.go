package upstream

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/settings"
)

// manyA answers with n A records so that the reply exceeds 1232 bytes over
// UDP; UDP replies are truncated (TC=1) like a real server would.
func manyA(n int, udpSeen, tcpSeen *atomic.Int32) dns.HandlerFunc {
	return func(w dns.ResponseWriter, q *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(q)
		for i := range n {
			m.Answer = append(m.Answer, &dns.A{
				Hdr: dns.RR_Header{Name: q.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60},
				A:   net.IPv4(10, 0, byte(i/256), byte(i%256)),
			})
		}
		if w.RemoteAddr().Network() == "udp" {
			udpSeen.Add(1)
			m.Truncate(int(q.IsEdns0().UDPSize()))
		} else {
			tcpSeen.Add(1)
		}
		_ = w.WriteMsg(m)
	}
}

func TestPlainUDPRetriesOverTCPOnTruncation(t *testing.T) {
	var udp, tcp atomic.Int32
	addr := startDNS(t, manyA(100, &udp, &tcp))
	st := newStore(t, func(d *settings.DNS) { d.Upstreams = []string{addr.String()}; d.CacheEnabled = false })
	r := newTestResolver(t, st, testOptions(), nil)
	defer r.Close()
	m, info, err := r.Resolve(context.Background(), query("big.example.", dns.TypeA, 3, false))
	if err != nil {
		t.Fatal(err)
	}
	if m.Truncated || len(m.Answer) != 100 {
		t.Fatalf("truncated=%v answers=%d, want the full TCP answer", m.Truncated, len(m.Answer))
	}
	if udp.Load() != 1 || tcp.Load() != 1 {
		t.Errorf("udp=%d tcp=%d, want 1/1", udp.Load(), tcp.Load())
	}
	if info.Upstream != addr.String() || info.RTT <= 0 {
		t.Errorf("info = %+v", info)
	}
}

func TestTCPUpstreamNeverUsesUDP(t *testing.T) {
	var udp, tcp atomic.Int32
	addr := startDNS(t, manyA(3, &udp, &tcp))
	st := newStore(t, func(d *settings.DNS) { d.Upstreams = []string{"tcp://" + addr.String()} })
	r := newTestResolver(t, st, testOptions(), nil)
	defer r.Close()
	if _, _, err := r.Resolve(context.Background(), query("tcp.example.", dns.TypeA, 1, false)); err != nil {
		t.Fatal(err)
	}
	if udp.Load() != 0 || tcp.Load() != 1 {
		t.Errorf("udp=%d tcp=%d, want 0/1", udp.Load(), tcp.Load())
	}
}

// wrongQuestion replies with a different name, optionally followed by the
// genuine reply (as a spoofer racing the real server would).
func wrongQuestion(thenGenuine bool) dns.HandlerFunc {
	return func(w dns.ResponseWriter, q *dns.Msg) {
		spoof := answerA(q, "203.0.113.66", 3600)
		spoof.Question[0].Name = "evil.example."
		spoof.Answer[0].Header().Name = "evil.example."
		_ = w.WriteMsg(spoof)
		if thenGenuine {
			_ = w.WriteMsg(answerA(q, "192.0.2.10", 60))
		}
	}
}

func TestQuestionMismatchIsRejected(t *testing.T) {
	tests := []struct {
		name     string
		scheme   string
		genuine  bool
		wantIP   string
		wantErr  string
		maxDelay time.Duration
	}{
		{name: "udp discards and times out", scheme: "", wantErr: "does not match", maxDelay: 2 * time.Second},
		{name: "udp keeps waiting for the genuine reply", scheme: "", genuine: true, wantIP: "192.0.2.10"},
		{name: "tcp fails immediately", scheme: "tcp://", wantErr: "does not match", maxDelay: time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			addr := startDNS(t, wrongQuestion(tc.genuine))
			st := newStore(t, func(d *settings.DNS) {
				d.Upstreams = []string{tc.scheme + addr.String()}
				d.UpstreamTimeoutMs = 500
				d.CacheEnabled = false
			})
			opts := testOptions()
			opts.attempt = 300 * time.Millisecond
			r := newTestResolver(t, st, opts, nil)
			defer r.Close()
			begin := time.Now()
			m, _, err := r.Resolve(context.Background(), query("good.example.", dns.TypeA, 1, false))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				if d := time.Since(begin); d > tc.maxDelay {
					t.Errorf("took %v", d)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if ip, _ := firstA(t, m); ip != tc.wantIP {
				t.Fatalf("accepted %s", ip)
			}
		})
	}
}

func TestCheckReply(t *testing.T) {
	q := newQuery("example.com.", dns.TypeA, dns.ClassINET, false)
	ok := func() *dns.Msg { m := new(dns.Msg); m.SetReply(q); return m }
	tests := []struct {
		name   string
		mutate func(m *dns.Msg)
		want   error
	}{
		{name: "matching reply, other case", mutate: func(m *dns.Msg) { m.Question[0].Name = "EXAMPLE.com." }},
		{name: "not a response", mutate: func(m *dns.Msg) { m.Response = false }, want: errNotResponse},
		{name: "other opcode", mutate: func(m *dns.Msg) { m.Opcode = dns.OpcodeNotify }, want: errNotResponse},
		{name: "no question", mutate: func(m *dns.Msg) { m.Question = nil }, want: errQuestionMismatch},
		{name: "other name", mutate: func(m *dns.Msg) { m.Question[0].Name = "example.org." }, want: errQuestionMismatch},
		{name: "other type", mutate: func(m *dns.Msg) { m.Question[0].Qtype = dns.TypeAAAA }, want: errQuestionMismatch},
		{name: "other class", mutate: func(m *dns.Msg) { m.Question[0].Qclass = dns.ClassCHAOS }, want: errQuestionMismatch},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := ok()
			tc.mutate(m)
			if err := checkReply(q, m); !errors.Is(err, tc.want) {
				t.Errorf("checkReply = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestLowerAndFold(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"example.com.", "example.com."}, {"ExAmPlE.CoM.", "example.com."}, {`a\066c.`, `a\066c.`},
	} {
		if got := lowerASCII(tc.in); got != tc.want {
			t.Errorf("lowerASCII(%q) = %q", tc.in, got)
		}
		if !equalFoldASCII(tc.in, tc.want) {
			t.Errorf("equalFoldASCII(%q, %q) = false", tc.in, tc.want)
		}
	}
	if equalFoldASCII("a.", "b.") || equalFoldASCII("a.", "a..") {
		t.Error("different names compare equal")
	}
}

func TestUnreachableUpstreamFailsFast(t *testing.T) {
	// A closed TCP port: the connection is refused at once.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	st := newStore(t, func(d *settings.DNS) { d.Upstreams = []string{"tcp://" + addr} })
	r := newTestResolver(t, st, testOptions(), nil)
	defer r.Close()
	_, _, err = r.Resolve(context.Background(), query("x.example.", dns.TypeA, 1, false))
	if err == nil {
		t.Fatal("expected an error")
	}
	stats := r.Stats()
	if len(stats) != 1 || stats[0].Errors != 1 || stats[0].LastError == "" || stats[0].LastErrorAt.IsZero() {
		t.Errorf("stats = %+v", stats)
	}
	if want := fmt.Sprintf("tcp://%s", addr); stats[0].Upstream != want {
		t.Errorf("upstream = %q, want %q", stats[0].Upstream, want)
	}
}
