//go:build !unix

package secrets

import (
	"fmt"
	"os"
)

// openKeyFile opens path for reading; with follow=false a symbolic link as
// the last path element is refused (checked before and after the open).
func openKeyFile(path string, follow bool) (*os.File, error) {
	if follow {
		return os.Open(path)
	}
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s is a symbolic link; the key must be a regular file", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	if after, err := f.Stat(); err != nil || !os.SameFile(before, after) {
		f.Close()
		return nil, fmt.Errorf("%s changed while it was opened", path)
	}
	return f, nil
}
