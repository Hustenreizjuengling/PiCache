//go:build !linux

package ntp

import "errors"

// ReadClock: the clock state is known on Linux only (NTP answers
// unsynchronised elsewhere).
func ReadClock() ClockState {
	return ClockState{Err: errors.New("the clock state can only be read on Linux")}
}
