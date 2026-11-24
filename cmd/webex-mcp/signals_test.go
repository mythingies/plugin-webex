package main

import (
	"os"
	"runtime"
	"slices"
	"syscall"
	"testing"
)

// Closing the console window (Windows) or terminal (Unix) and a plain kill
// must stop --review the same way Ctrl-C does, so the store is closed and
// the instance lock released.
func TestReviewSignals(t *testing.T) {
	sigs := reviewSignals()
	want := []os.Signal{os.Interrupt, syscall.SIGTERM}
	if runtime.GOOS != "windows" {
		want = append(want, syscall.SIGHUP)
	}
	for _, s := range want {
		if !slices.Contains(sigs, s) {
			t.Errorf("reviewSignals() = %v, missing %v", sigs, s)
		}
	}
}
