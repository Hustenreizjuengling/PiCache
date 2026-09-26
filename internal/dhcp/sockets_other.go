//go:build !linux

package dhcp

import (
	"errors"
	"fmt"
	"os"
)

// OpenAtStart reports the DHCP server as unavailable on systems other
// than Linux (the ingress interface of a broadcast and the capability
// model are Linux-specific); with PICACHE_DHCP=off it is the opt-out.
func OpenAtStart(o StartOptions) *Sockets {
	if o.OptOut {
		return OptOutSockets()
	}
	return unsupportedSockets(reasonNotLinux)
}

var errNotLinux = errors.New(reasonNotLinux)

func platformListen4() (v4Conn, error) { return nil, errNotLinux }
func platformListen6() (v6Conn, error) { return nil, errNotLinux }

func bindDenied(err error) bool { return errors.Is(err, os.ErrPermission) }

func socketErr(what string, err error) string { return fmt.Sprintf("%s: %v", what, err) }
