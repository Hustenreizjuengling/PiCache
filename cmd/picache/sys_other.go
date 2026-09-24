//go:build !linux

package main

func becomeOwnerOf(string) error { return nil }

func memoryLimit() int64 { return 0 }
