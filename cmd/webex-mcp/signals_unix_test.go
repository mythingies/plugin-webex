//go:build unix

package main

import (
	"context"
	"errors"
	"os"
	"syscall"
	"testing"
	"time"
)

// A SIGTERM or SIGHUP delivered to the process cancels the review context,
// and serveThenClose still runs Close afterwards.
func TestReviewSignals_CancelAndClose(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGHUP} {
		t.Run(sig.String(), func(t *testing.T) {
			ctx, stop := reviewContext()
			defer stop()
			closed := false
			err := serveThenClose(ctx,
				func(ctx context.Context) error {
					if err := syscall.Kill(os.Getpid(), sig); err != nil {
						return err
					}
					select {
					case <-ctx.Done():
						return nil
					case <-time.After(5 * time.Second):
						return errors.New("signal did not cancel the context")
					}
				},
				func(context.Context) error { closed = true; return nil },
				time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if !closed {
				t.Error("Close did not run after the signal")
			}
		})
	}
}
