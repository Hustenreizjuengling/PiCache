//go:build unix

package secrets

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// openKeyFile opens path for reading. O_NONBLOCK keeps a FIFO from blocking
// the open (readKeyFile then refuses it as not regular); with follow=false,
// O_NOFOLLOW refuses a symbolic link as the last path element without a
// race between a check and the open.
func openKeyFile(path string, follow bool) (*os.File, error) {
	flag := os.O_RDONLY | unix.O_NONBLOCK | unix.O_CLOEXEC
	if !follow {
		flag |= unix.O_NOFOLLOW
	}
	f, err := os.OpenFile(path, flag, 0)
	if err != nil && !follow && (errors.Is(err, unix.ELOOP) || errors.Is(err, unix.EMLINK)) {
		if fi, lerr := os.Lstat(path); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("%s is a symbolic link; the key must be a regular file", path)
		}
	}
	return f, err
}
