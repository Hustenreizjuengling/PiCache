//go:build !linux

package db

func checkLocalFS(string) error { return nil }
