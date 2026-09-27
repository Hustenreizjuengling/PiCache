//go:build !linux

package applog

import (
	"errors"
	"os"
)

const oNoFollow = 0

// openLogFile opens the log file for appending (a symbolic link in its
// place is refused by Lstat first; development systems) and returns its
// size.
func openLogFile(path string) (*os.File, int64, error) {
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return nil, 0, errLink
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o640)
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
