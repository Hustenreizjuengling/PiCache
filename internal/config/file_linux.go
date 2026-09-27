//go:build linux

package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
)

// oNoFollow refuses a symbolic link as the last path element.
const oNoFollow = syscall.O_NOFOLLOW

// readOwnedFile reads a file root may read before it drops privileges:
// opened with O_RDONLY|O_NOFOLLOW|O_NONBLOCK, then fstat: a regular file
// with one link, owned by the owner of ownerOf, at most max bytes.
func readOwnedFile(ownerOf, path string, max int64) ([]byte, error) {
	var dir syscall.Stat_t
	if err := syscall.Stat(ownerOf, &dir); err != nil {
		return nil, fmt.Errorf("the data directory: %w", err)
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	switch {
	case errors.Is(err, syscall.ENOENT):
		return nil, os.ErrNotExist
	case errors.Is(err, syscall.ELOOP):
		return nil, errors.New("it is a symbolic link")
	case err != nil:
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		return nil, err
	}
	switch {
	case st.Mode&syscall.S_IFMT != syscall.S_IFREG:
		return nil, errors.New("it is not a regular file")
	case st.Nlink != 1:
		return nil, errors.New("it has more than one link")
	case st.Uid != dir.Uid:
		return nil, errors.New("it is not owned by the owner of the data directory")
	case st.Size > max:
		return nil, fmt.Errorf("it is larger than %d bytes", max)
	}
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("it is larger than %d bytes", max)
	}
	return b, nil
}
