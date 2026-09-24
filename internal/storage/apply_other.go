//go:build !linux

package storage

import "errors"

// oNoFollow does not exist outside Linux; O_EXCL still refuses existing names.
const oNoFollow = 0

var errApplyUnsupported = errors.New("storage apply is only supported on Linux")

// systemHostEnv refuses to run: mount units need Linux and systemd.
func systemHostEnv() hostEnv {
	return hostEnv{checkPrivileges: func() error { return errApplyUnsupported }}
}

func checkRootOwnedChain(string) error { return errApplyUnsupported }

func chownPath(string, int, int) error { return errApplyUnsupported }
