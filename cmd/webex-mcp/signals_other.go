//go:build !windows

package main

import (
	"os"
	"syscall"
)

// reviewSignals are the signals that stop --review so Reply Desk can close
// its store and release the instance lock: Ctrl-C, a plain kill or service
// stop (SIGTERM), and closing the terminal (SIGHUP).
func reviewSignals() []os.Signal {
	return []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP}
}
