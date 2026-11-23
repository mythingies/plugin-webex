package replydesk

import (
	"time"

	"github.com/mythingies/plugin-webex/internal/webex"
)

// Detect returns the pending burst in one DM room, or nil when the room is
// answered, belongs to a bot, or its newest burst message is older than
// lookback. newestFirst is the room's fetched history, newest first; me is
// the user's person ID. Detect is pure: no I/O.
func Detect(me string, room webex.Space, newestFirst []webex.Message, now time.Time, lookback time.Duration) *Pending {
	return nil
}

// IsBot reports whether a sender email belongs to a Webex bot.
func IsBot(email string) bool { return false }
