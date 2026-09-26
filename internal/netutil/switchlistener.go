package netutil

import (
	"net"
	"sync"
)

// SwitchListener wraps the listener of a service that is switched on and
// off at run time without a restart (DoT, the dedicated DoH listeners):
// while enabled reports false, connections are closed right after accept;
// open connections are tracked (at most max; beyond it a new connection is
// closed) so that CloseAll can end them when the service is switched off.
// Nothing is read or written in Accept (a TLS handshake happens later, in
// the connection's goroutine).
type SwitchListener struct {
	net.Listener
	enabled func() bool
	max     int

	mu    sync.Mutex
	conns map[*switchConn]struct{}
}

// NewSwitchListener wraps ln (see SwitchListener).
func NewSwitchListener(ln net.Listener, enabled func() bool, max int) *SwitchListener {
	return &SwitchListener{Listener: ln, enabled: enabled, max: max, conns: map[*switchConn]struct{}{}}
}

// Accept returns the next connection while the service is enabled.
func (l *SwitchListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if !l.enabled() {
			_ = c.Close()
			continue
		}
		sc := &switchConn{Conn: c, l: l}
		l.mu.Lock()
		if len(l.conns) >= l.max {
			l.mu.Unlock()
			_ = c.Close()
			continue
		}
		l.conns[sc] = struct{}{}
		l.mu.Unlock()
		return sc, nil
	}
}

// CloseAll closes every open connection (the service was switched off).
func (l *SwitchListener) CloseAll() {
	l.mu.Lock()
	open := make([]*switchConn, 0, len(l.conns))
	for c := range l.conns {
		open = append(open, c)
	}
	l.mu.Unlock()
	for _, c := range open {
		_ = c.Close()
	}
}

// Open returns the number of open connections.
func (l *SwitchListener) Open() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.conns)
}

type switchConn struct {
	net.Conn
	l    *SwitchListener
	once sync.Once
}

func (c *switchConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() {
		c.l.mu.Lock()
		delete(c.l.conns, c)
		c.l.mu.Unlock()
	})
	return err
}
