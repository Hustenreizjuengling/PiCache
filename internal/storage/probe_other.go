//go:build !linux

package storage

import "os"

// mountState cannot detect mount points outside Linux (development builds).
func mountState(string, os.FileInfo) (mounted, supported bool) { return false, false }

// statDir is not available outside Linux.
func statDir(*os.Root) (fsInfo, error) { return fsInfo{}, errUnsupported }

// sameDevice is not available outside Linux.
func sameDevice(os.FileInfo, os.FileInfo) (same, ok bool) { return false, false }

// blockDevice is not available outside Linux.
func blockDevice(os.FileInfo) string { return "" }
