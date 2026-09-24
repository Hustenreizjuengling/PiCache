//go:build !linux

package app

func diskFree(string) (uint64, bool) { return 0, false }
