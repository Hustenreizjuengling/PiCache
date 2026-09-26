//go:build linux

package main

import (
	"os"
	"os/signal"
	"syscall"
	"testing"
	"time"
)

// SIGHUP is delivered to the channel serve passes to the app and never
// ends the process.
func TestSIGHUPDoesNotEndProcess(t *testing.T) {
	hup := notifyHUP()
	defer signal.Stop(hup)
	for range 2 {
		if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
			t.Fatal(err)
		}
		select {
		case <-hup:
		case <-time.After(5 * time.Second):
			t.Fatal("SIGHUP was not delivered")
		}
	}
}
