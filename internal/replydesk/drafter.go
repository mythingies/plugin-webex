package replydesk

import (
	"context"
	"errors"
	"time"
)

// DrafterConfig configures the claude subprocess drafter.
type DrafterConfig struct {
	Mode         string        // ModeAuto (default), ModeBare, ModeIsolated, ModeOff
	Binary       string        // default "claude"
	Model        string        // optional --model
	Timeout      time.Duration // per draft; default 120s
	MaxBudgetUSD float64       // optional --max-budget-usd per draft; 0 = unset
}

// ResolveMode turns ModeAuto into a concrete mode using the environment and
// PATH. getenv and lookPath are injected for tests.
func ResolveMode(cfg DrafterConfig, getenv func(string) string, lookPath func(string) (string, error)) string {
	return ModeOff
}

// NewDrafter returns the drafter for cfg. With ModeOff it returns a drafter
// whose Draft always fails with ErrDrafterOff.
func NewDrafter(cfg DrafterConfig) Drafter { return offDrafter{} }

// ErrDrafterOff is returned by the off drafter.
var ErrDrafterOff = errors.New("drafting is off: claude CLI not available")

type offDrafter struct{}

func (offDrafter) Draft(context.Context, DraftInput) (*DraftOutput, error) { return nil, ErrDrafterOff }
func (offDrafter) Mode() string                                            { return ModeOff }
