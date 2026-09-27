//go:build linux

package applog

import (
	"errors"
	"os"
	"syscall"
)

const oNoFollow = syscall.O_NOFOLLOW

// openLogFile opens the log file for appending (O_NOFOLLOW: a symbolic
// link in its place is refused) and returns its size.
func openLogFile(path string) (*os.File, int64, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o640)
	if errors.Is(err, syscall.ELOOP) {
		return nil, 0, errLink
	}
	if err != nil {
		return nil, 0, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	if !fi.Mode().IsRegular() {
		f.Close()
		return nil, 0, errors.New("the log file is not a regular file")
	}
	return f, fi.Size(), nil
}
