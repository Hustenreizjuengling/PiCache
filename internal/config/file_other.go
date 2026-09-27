//go:build !linux

package config

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// oNoFollow: no O_NOFOLLOW here; readOwnedFile checks with Lstat.
const oNoFollow = 0

// readOwnedFile reads a regular file (not a link) of at most max bytes;
// ownership is not checked outside Linux (development systems).
func readOwnedFile(_, path string, max int64) ([]byte, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, errors.New("it is not a regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("it is larger than %d bytes", max)
	}
	return b, nil
}
