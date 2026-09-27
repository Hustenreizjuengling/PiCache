//go:build linux

package ntp

import (
	"time"

	"golang.org/x/sys/unix"
)

// Clock status and state of adjtimex (linux/timex.h).
const (
	staUnsync = 0x0040
	timeError = 5
)

// ReadClock reads the state of the host clock with adjtimex and modes 0
// (read only; the unit re-allows the call, 6.2). EPERM (a unit without
// SystemCallFilter=adjtimex, a container) or ENOSYS give Err.
func ReadClock() ClockState {
	var tx unix.Timex
	state, err := unix.Adjtimex(&tx)
	if err != nil {
		return ClockState{Err: err}
	}
	return ClockState{Synced: tx.Status&staUnsync == 0 && state != timeError,
		MaxError: time.Duration(int64(tx.Maxerror)) * time.Microsecond}
}
