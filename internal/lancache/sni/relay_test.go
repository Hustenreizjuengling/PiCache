package sni

import (
	"io"
	"net"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// pipeRelay runs s.relay between two in-memory pipes; the test talks to
// the client and upstream ends.
type pipeRelay struct {
	client, upstream net.Conn // test ends
	done             chan struct{}
	up, down         int64
}

func startPipeRelay(s *Server, lifetime time.Duration) *pipeRelay {
	cTest, cRelay := net.Pipe()
	uRelay, uTest := net.Pipe()
	p := &pipeRelay{client: cTest, upstream: uTest, done: make(chan struct{})}
	go func() {
		defer close(p.done)
		p.up, p.down = s.relay(cRelay, uRelay, time.Now().Add(lifetime))
	}()
	return p
}

func (p *pipeRelay) finished() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

func TestRelayIdleTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := New(Deps{})
		p := startPipeRelay(s, maxLifetime)
		defer p.client.Close()
		defer p.upstream.Close()
		time.Sleep(idleTimeout - time.Second)
		synctest.Wait()
		if p.finished() {
			t.Fatal("relay ended before the idle timeout")
		}
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if !p.finished() {
			t.Fatal("idle relay not closed")
		}
	})
}

func TestRelayOneWayTransferIsNotIdle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := New(Deps{})
		p := startPipeRelay(s, maxLifetime)
		defer p.client.Close()
		defer p.upstream.Close()
		var received atomic.Int64
		go func() {
			n, _ := io.Copy(io.Discard, p.client)
			received.Add(n)
		}()
		// A slow download: 1 KiB per minute for 20 minutes while the client
		// never sends anything.
		chunk := make([]byte, 1024)
		for range 20 {
			if _, err := p.upstream.Write(chunk); err != nil {
				t.Fatalf("relay closed during an active download: %v", err)
			}
			time.Sleep(time.Minute)
		}
		synctest.Wait()
		if p.finished() {
			t.Fatal("relay ended during an active download")
		}
		time.Sleep(idleTimeout + relayTick)
		synctest.Wait()
		if !p.finished() {
			t.Fatal("relay not closed after the transfer went idle")
		}
		if p.down != 20*1024 || p.up != 0 || s.Stats().BytesDown != 20*1024 {
			t.Fatalf("bytes up %d down %d stats %+v", p.up, p.down, s.Stats())
		}
	})
}

func TestRelayLifetime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := New(Deps{})
		p := startPipeRelay(s, 10*time.Minute)
		defer p.client.Close()
		defer p.upstream.Close()
		go func() { _, _ = io.Copy(io.Discard, p.upstream) }()
		for i := 0; ; i++ {
			if _, err := p.client.Write([]byte("x")); err != nil {
				break
			}
			if i > 1000 {
				t.Fatal("lifetime not enforced")
			}
			time.Sleep(10 * time.Second)
		}
		synctest.Wait()
		if !p.finished() {
			t.Fatal("relay still running")
		}
		if elapsed := time.Duration(p.up) * 10 * time.Second; elapsed < 9*time.Minute || elapsed > 11*time.Minute {
			t.Fatalf("relay lasted about %v", elapsed)
		}
	})
}

func TestRelayWriteStall(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := New(Deps{})
		p := startPipeRelay(s, maxLifetime)
		defer p.client.Close()
		defer p.upstream.Close()
		// The client never reads: the relay's write to it blocks.
		go func() { _, _ = p.upstream.Write(make([]byte, 64)) }()
		time.Sleep(idleTimeout + relayTick + time.Second)
		synctest.Wait()
		if !p.finished() {
			t.Fatal("stalled write not aborted")
		}
	})
}
