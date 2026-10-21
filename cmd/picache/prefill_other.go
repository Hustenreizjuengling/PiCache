//go:build !linux

package main

import (
	"fmt"
	"os"
)

func prefillEnable([]string) int {
	fmt.Fprintln(os.Stderr, "picache prefill enable: the prefill service runs on Linux with systemd only")
	return 2
}

func prefillDisable([]string) int {
	fmt.Fprintln(os.Stderr, "picache prefill disable: the prefill service runs on Linux with systemd only")
	return 2
}
