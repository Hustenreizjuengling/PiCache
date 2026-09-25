package sni

import (
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hustenreizjuengling/picache/internal/netutil"
)

// Relay tuning.
const (
	relayChunk  = 4 << 20          // bytes per io.CopyN call
	idleTimeout = 5 * time.Minute  // no bytes in either direction
	relayTick   = 30 * time.Second // read deadline granularity of the idle check
)

var (
	errIdle     = errors.New("idle timeout")
	errLifetime = errors.New("maximum connection lifetime reached")
)

// relayState is shared by both directions of one connection.
type relayState struct {
	end        time.Time    // lifetime limit
	lastActive atomic.Int64 // unix nanos of the last observed transfer
}

func (st *relayState) touch() { st.lastActive.Store(time.Now().UnixNano()) }

func (st *relayState) idleFor() time.Duration {
	return time.Duration(time.Now().UnixNano() - st.lastActive.Load())
}

// relay copies client→upstream and upstream→client until both directions
// ended (EOF is propagated as a TCP half-close), an error occurred, the
// connection was idle for idleTimeout, a write stalled for idleTimeout, or
// end was reached. It returns the bytes copied in each direction.
func (s *Server) relay(client, upstream net.Conn, end time.Time) (up, down int64) {
	st := &relayState{end: end}
	st.touch()
	c, u := ioConn(client), ioConn(upstream)
	abort := func() {
		_ = client.Close()
		_ = upstream.Close()
	}
	finish := func(dst net.Conn, err error) {
		if err != nil || !closeWrite(dst) {
			abort()
		}
	}
	var wg sync.WaitGroup
	wg.Go(func() {
		var err error
		up, err = st.pipe(u, c, &s.bytesUp)
		finish(u, err)
	})
	var err error
	down, err = st.pipe(c, u, &s.bytesDown)
	finish(c, err)
	wg.Wait()
	return up, down
}

// pipe copies src to dst in io.CopyN chunks (splice(2) for a TCP pair).
// Each call is bounded by a read deadline of relayTick; a timeout only ends
// the relay if the whole connection has been idle for idleTimeout. A write
// that makes no progress for idleTimeout ends it too.
func (st *relayState) pipe(dst, src net.Conn, counter *atomic.Int64) (int64, error) {
	var total int64
	for {
		now := time.Now()
		if !now.Before(st.end) {
			return total, errLifetime
		}
		readBy := now.Add(relayTick)
		if readBy.After(st.end) {
			readBy = st.end
		}
		writeBy := now.Add(idleTimeout)
		if err := src.SetReadDeadline(readBy); err != nil {
			return total, err
		}
		if err := dst.SetWriteDeadline(writeBy); err != nil {
			return total, err
		}
		n, err := io.CopyN(dst, src, relayChunk)
		total += n
		if n > 0 {
			counter.Add(n)
			st.touch()
		}
		switch {
		case err == nil:
		case errors.Is(err, io.EOF):
			return total, nil
		case errors.Is(err, os.ErrDeadlineExceeded) && time.Now().Before(writeBy):
			// The read deadline fired (the write deadline cannot have yet):
			// nothing is lost, continue unless the connection is idle.
			if st.idleFor() >= idleTimeout {
				return total, errIdle
			}
		default:
			return total, err
		}
	}
}

// ioConn returns the raw *net.TCPConn behind c (splice-capable), or c.
func ioConn(c net.Conn) net.Conn {
	if tc := netutil.UnwrapTCP(c); tc != nil {
		return tc
	}
	return c
}

// closeWrite half-closes a TCP connection; false if c cannot half-close.
func closeWrite(c net.Conn) bool {
	tc, ok := c.(*net.TCPConn)
	return ok && tc.CloseWrite() == nil
}
