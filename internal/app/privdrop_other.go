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

func isAddrInUse(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "address already in use") ||
		err != nil && strings.Contains(err.Error(), "Only one usage of each socket address")
}

func isPermission(err error) bool { return errors.Is(err, os.ErrPermission) }
