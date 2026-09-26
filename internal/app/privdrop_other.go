//go:build !linux

package app

import (
	"errors"
	"os"
	"strings"
)

func (a *App) dropPrivileges() error {
	if a.cfg.RunAs != "" {
		return errors.New("PICACHE_RUN_AS is only supported on Linux")
	}
	return nil
}

// dropNetRaw has nothing to drop on systems other than Linux (DHCP is
// Linux-only).
func dropNetRaw() (unverified, err error) { return nil, nil }

func isAddrInUse(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "address already in use") ||
		err != nil && strings.Contains(err.Error(), "Only one usage of each socket address")
}

func isPermission(err error) bool { return errors.Is(err, os.ErrPermission) }
