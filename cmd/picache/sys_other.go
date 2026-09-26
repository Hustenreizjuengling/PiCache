//go:build !linux

package main

func becomeOwnerOf(string) error { return nil }

func memoryLimit() int64 { return 0 }

// oNoFollow does not exist outside Linux; O_EXCL still refuses existing names.
const oNoFollow = 0
