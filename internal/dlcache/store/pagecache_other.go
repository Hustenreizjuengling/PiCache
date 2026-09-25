//go:build !linux

package cachestore

import "os"

// DropPageCache is not available outside Linux: reads may come from memory.
func DropPageCache(*os.File) bool { return false }
