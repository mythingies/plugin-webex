//go:build windows

package main

import (
	"os"
	"syscall"
)

// reviewSignals are the signals that stop --review so Reply Desk can close
// its store and release the instance lock. On Windows, Ctrl-C and
// Ctrl-Break arrive as os.Interrupt, and closing the console window, logging
// off, or shutting down (CTRL_CLOSE_EVENT, CTRL_LOGOFF_EVENT,
// CTRL_SHUTDOWN_EVENT) arrive as syscall.SIGTERM.
func reviewSignals() []os.Signal {
	return []os.Signal{os.Interrupt, syscall.SIGTERM}
}
