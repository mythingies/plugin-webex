//go:build windows

package replydesk

import (
	"errors"
	"math"

	"golang.org/x/sys/windows"
)

// stillActive is STILL_ACTIVE, the exit code GetExitCodeProcess reports for
// a process that is running.
const stillActive = 259

// processState reports whether pid is running: OpenProcess with
// PROCESS_QUERY_LIMITED_INFORMATION (the least access GetExitCodeProcess
// needs), then GetExitCodeProcess == STILL_ACTIVE. Access denied means the
// process exists (another user's, or protected): alive.
func processState(pid int) procState {
	if pid <= 0 || pid > math.MaxUint32 {
		return procUnknown
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid)) // #nosec G115 -- pid is range-checked above
	if err != nil {
		switch {
		case errors.Is(err, windows.ERROR_INVALID_PARAMETER):
			return procDead // no process has this pid
		case errors.Is(err, windows.ERROR_ACCESS_DENIED):
			return procAlive
		}
		return procUnknown
	}
	defer func() { _ = windows.CloseHandle(h) }()
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return procUnknown
	}
	if code == stillActive {
		return procAlive
	}
	return procDead
}
