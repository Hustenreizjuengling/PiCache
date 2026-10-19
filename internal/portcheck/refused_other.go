//go:build !windows

package portcheck

import (
	"errors"
	"syscall"
)

// refused reports whether a connection attempt was refused (RST).
func refused(err error) bool { return errors.Is(err, syscall.ECONNREFUSED) }
