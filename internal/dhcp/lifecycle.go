package dhcp

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/hustenreizjuengling/picache/internal/settings"
)

// Socket life cycle (docs/ARCHITECTURE.md 18.1): nothing is held while DHCP
// is switched off. UDP 67 and 547 are opened when it is switched on (by the
// app at start when the marker says so, else by the service), the raw
// ICMPv6 socket only at start; every socket the settings no longer need is
// closed after the announcements were withdrawn. Each open socket has one
// reader goroutine, which ends when the socket is closed.

func (s *Service) isLive() bool {
	s.sockMu.Lock()
	defer s.sockMu.Unlock()
	return s.live
}

// ctxLocked is the context of the readers (s.sockMu held).
func (s *Service) ctxLocked() context.Context {
	if s.runCtx != nil {
		return s.runCtx
	}
	return context.Background()
}

// openRuntime opens UDP 67 and then 547 while DHCP is switched on and they
// are not open. A failure is recorded for the status: permission denied
// although the process could bind them at start means the ports open only
// at start here (Docker: the switch to PICACHE_RUN_AS cleared every
// capability), which a restart fixes (the marker is written); anything
// else (another DHCP server on this host) is retried with every
// evaluation.
func (s *Service) openRuntime() {
	s.sockMu.Lock()
	defer s.sockMu.Unlock()
	so := s.d.Sockets
	so.mu.Lock()
	need4, need6, bindCapable := so.v4 == nil, so.v6 == nil, so.bindCapable
	so.mu.Unlock()
	if need4 {
		c, err := s.listen4()
		so.mu.Lock()
		switch {
		case err == nil:
			so.v4, so.v4Err, so.v4Code = c, "", ""
		case bindDenied(err) && bindCapable:
			so.v4Err, so.v4Code = reasonRestart, ReasonRestartRequired
		default:
			so.v4Err, so.v4Code = socketErr("UDP port 67", err), ReasonSocket
		}
		so.mu.Unlock()
		if err != nil {
			s.logRepeated("open4", "the DHCP server could not open UDP port 67", err)
			return
		}
		s.startReader4(c)
	}
	if need6 {
		c, err := s.listen6()
		so.mu.Lock()
		if err != nil {
			so.v6Err = socketErr("UDP port 547", err)
		} else {
			so.v6, so.v6Err = c, ""
		}
		so.mu.Unlock()
		if err != nil {
			s.logRepeated("open6", "the DHCP server could not open UDP port 547 (DHCPv6)", err)
			return
		}
		s.startReader6(c)
	}
}

// closeUnneeded closes the sockets the settings no longer need: all of
// them while DHCP is off or cannot serve here (a UDP 67 socket a running
// search opened is closed by the search), the raw socket while router
// advertisements are off.
func (s *Service) closeUnneeded(h settings.DHCP, supported bool) {
	want := supported && h.Enabled
	s.sockMu.Lock()
	defer s.sockMu.Unlock()
	so := s.d.Sockets
	so.mu.Lock()
	closed6, closedRaw := false, false
	if !want {
		if !so.probeHold {
			so.closeV4Locked()
		}
		closed6 = so.v6 != nil
		so.closeV6Locked()
	}
	if !want || !h.IPv6.RouterAdvertisements {
		closedRaw = so.icmp != nil
		so.closeICMPLocked()
	}
	so.mu.Unlock()
	if closed6 {
		s.mu.Lock()
		s.v6Joined = 0 // closing the socket left the group
		s.mu.Unlock()
	}
	if closedRaw {
		s.raMu.Lock()
		s.ra.active, s.ra.joined = false, 0
		s.ra.sched.Stop()
		s.raMu.Unlock()
	}
}

// reconcileMarkers keeps the markers in step with the settings: MarkerSockets
// exists exactly while DHCP is switched on, MarkerRA while router
// advertisements are on too (both only where DHCP can serve). A failure is
// logged at most hourly and shown in the status.
func (s *Service) reconcileMarkers(h settings.DHCP, supported bool) {
	if s.d.DataDir == "" {
		return
	}
	sockets := supported && h.Enabled
	err := setMarkers(s.d.DataDir, map[string]bool{MarkerSockets: sockets, MarkerRA: sockets && h.IPv6.RouterAdvertisements})
	if err == nil {
		s.markerErr.Store(nil)
		return
	}
	msg := "the DHCP marker files in the data directory could not be updated (" + err.Error() +
		"), so the DHCP sockets may not match the settings after a restart"
	s.markerErr.Store(&msg)
	s.logRepeated("marker", "updating the DHCP marker files failed", err)
}

// setMarkers creates (empty regular files, 0640) or removes the markers in
// dir, through an os.Root: a symbolic link or anything else in a marker's
// place is removed, never followed.
func setMarkers(dir string, want map[string]bool) error {
	r, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer r.Close()
	var errs []error
	for name, on := range want {
		fi, err := r.Lstat(name)
		exists := err == nil
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
			continue
		}
		switch {
		case on && exists && fi.Mode().IsRegular():
			continue
		case exists:
			if err := r.Remove(name); err != nil {
				errs = append(errs, err)
				continue
			}
		}
		if !on {
			continue
		}
		f, err := r.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
		if err == nil {
			err = f.Chmod(0o640) // independent of the umask
			if cerr := f.Close(); err == nil {
				err = cerr
			}
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("create %s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

// startReader4 and its siblings start the reader of a socket that was just
// opened (s.sockMu held); it ends when the socket is closed.
func (s *Service) startReader4(c v4Conn) {
	ctx := s.ctxLocked()
	s.readers.Go(func() { s.readLoop4(ctx, c) })
}

func (s *Service) startReader6(c v6Conn) {
	ctx := s.ctxLocked()
	s.readers.Go(func() { s.readLoop6(ctx, c) })
}

func (s *Service) startReaderICMP(c icmpConn) {
	ctx := s.ctxLocked()
	s.readers.Go(func() { s.readLoopICMP(ctx, c) })
}
