package dnsserver

import (
	"net"
	"testing"
	"testing/synctest"
	"time"

	"github.com/miekg/dns"
)

// TestTCPWriteDeadline: a client that sends a query over plain TCP and
// never reads the reply holds the connection, the handler goroutine and
// its in-flight slot only until the reply could not be written within
// 10 s; then the connection is closed (fake time).
func TestTCPWriteDeadline(t *testing.T) {
	e := newEnv(t, nil)
	synctest.Test(t, func(t *testing.T) {
		pl := &pipeListener{conns: make(chan net.Conn), closed: make(chan struct{})}
		srv := newTCPServer(pl, &dnsHandler{s: e.srv, ctx: t.Context()})
		go func() { _ = srv.ActivateAndServe() }()
		defer func() { _ = srv.Shutdown() }()
		client, server := net.Pipe()
		defer client.Close()
		pl.conns <- lanConn{server}

		start := time.Now()
		writeFrame(t, client, question("stuck.example", dns.TypeA))
		// The server does not read the next query while its reply is
		// stuck: this write returns when the server closes the connection.
		b, _ := question("next.example", dns.TypeA).Pack()
		if _, err := client.Write(append([]byte{0, byte(len(b))}, b...)); err == nil {
			t.Fatal("the server read on while its reply was not written")
		}
		if d := time.Since(start); d < tcpWriteTimeout || d > tcpWriteTimeout+time.Second {
			t.Fatalf("connection closed after %v", d)
		}
		synctest.Wait()
		if n := e.srv.inFlight.Load(); n != 0 {
			t.Fatalf("in flight %d after the connection was closed", n)
		}
	})
}
