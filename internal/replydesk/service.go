package replydesk

import (
	"context"
	"errors"
	"time"
)

// Scan lists the user's DM rooms active within lookback and returns every
// pending burst, newest head first. It issues only GETs.
func Scan(ctx context.Context, api API, me string, lookback time.Duration, now time.Time) ([]Pending, error) {
	return nil, errors.New("replydesk.Scan: not implemented")
}

// Config configures a Service.
type Config struct {
	API      API
	Store    *Store
	Drafter  Drafter
	Lookback time.Duration    // default DefaultLookback
	Now      func() time.Time // default time.Now
	PreDraft int              // drafts started in the background after a scan; default 5
}

// Service holds the scan results, drafts, and send logic behind the UI.
type Service struct{}

// NewService resolves the user (GetMe) and returns a Service.
func NewService(cfg Config) (*Service, error) {
	return nil, errors.New("replydesk.NewService: not implemented")
}
