package replydesk

import (
	"context"
	"errors"
	"io"
	"time"
)

// ServeOptions configures the review UI server.
type ServeOptions struct {
	OpenBrowser bool          // open the launch URL in the default browser
	IdleTimeout time.Duration // default 30m
	MaxLifetime time.Duration // default 8h
	Log         io.Writer     // where the launch URL is printed; default os.Stderr
}

// Serve runs the loopback review UI until ctx is done, the idle or lifetime
// limit passes, or the user closes it.
func Serve(ctx context.Context, b Backend, opts ServeOptions) error {
	return errors.New("replydesk.Serve: not implemented")
}
