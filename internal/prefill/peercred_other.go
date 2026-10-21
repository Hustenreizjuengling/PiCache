//go:build !linux

package prefill

import (
	"errors"
	"net"
)

// peerUID needs SO_PEERCRED: the prefill service runs on Linux only, and
// elsewhere refuses every connection.
func peerUID(net.Conn) (int, error) {
	return -1, errors.New("peer credentials need Linux")
}
