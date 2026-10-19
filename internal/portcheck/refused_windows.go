package portcheck

import (
	"errors"
	"syscall"
)

// wsaECONNREFUSED is Winsock's "connection refused" (WSAECONNREFUSED),
// which syscall.ECONNREFUSED does not match on Windows.
const wsaECONNREFUSED syscall.Errno = 10061

// refused reports whether a connection attempt was refused (RST).
func refused(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, wsaECONNREFUSED)
}
