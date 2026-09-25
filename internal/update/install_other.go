//go:build !linux

package update

import (
	"context"
	"errors"
	"io/fs"
	"os"
)

// oNoFollow does not exist outside Linux; O_EXCL still refuses existing names.
const oNoFollow = 0

var errUnsupported = errors.New("installing updates is only supported on Linux")

// SystemHost refuses to run: updates replace a systemd service's binary.
func SystemHost(health func(ctx context.Context) error) Host {
	return Host{
		CheckPrivileges: func() error { return errUnsupported },
		Systemctl:       func(context.Context, ...string) error { return errUnsupported },
		Health:          health,
	}
}

func checkRootOwnedChain(string) error { return errUnsupported }

func fileOwner(fs.FileInfo) (uid, gid int, ok bool) { return -1, -1, false }

func fchown(*os.File, int, int) error { return nil }

func linkCount(fs.FileInfo) (uint64, bool) { return 0, false }

func userFreeBytes(string) (uint64, bool) { return 0, false }

func syncDir(string) {}
