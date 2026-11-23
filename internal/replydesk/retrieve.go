package replydesk

import "github.com/mythingies/plugin-webex/internal/webex"

// Dense is the ADR 0001 density gate: at least 6 words, or naming something
// concrete. Only dense messages are worth retrieving.
func Dense(text string) bool { return false }

// Rank returns up to k of mine (the user's own messages) most relevant to
// query, best first, using BM25 over dense messages with recency as the tie
// breaker. Messages with no term overlap are not returned.
func Rank(query string, mine []webex.Message, k int) []webex.Message { return nil }
