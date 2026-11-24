//go:build unix

package replydesk

import (
	"errors"
	"os"
	"syscall"
)

// processState reports whether pid is running, by sending it signal 0.
// EPERM means it exists but belongs to someone else: alive.
func processState(pid int) procState {
	if pid <= 0 {
		return procUnknown
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return procUnknown
	}
	defer func() { _ = p.Release() }()
	err = p.Signal(syscall.Signal(0))
	switch {
	case err == nil, errors.Is(err, syscall.EPERM):
		return procAlive
	case errors.Is(err, os.ErrProcessDone), errors.Is(err, syscall.ESRCH):
		return procDead
	}
	return procUnknown
}
