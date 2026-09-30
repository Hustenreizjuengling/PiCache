//go:build !windows

package upstream

import "syscall"

// errConnReset is the error of a connection the peer reset (cutHandshake).
const errConnReset = syscall.ECONNRESET
