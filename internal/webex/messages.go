package webex

import (
	"errors"
	"fmt"
	"time"
)

// ErrNotFound is returned when the API answers 404, for example GetMessage on
// a deleted message.
var ErrNotFound = errors.New("webex: not found")

// RateLimitError is returned when the API answers 429 or 423 with a
// Retry-After longer than the client is willing to wait in-line.
type RateLimitError struct {
	Status     int
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("webex API rate limited (status %d), retry after %s", e.Status, e.RetryAfter)
}

// ListMessagesOptions are the query parameters of GET /messages. RoomID is
// required. Results are newest first.
type ListMessagesOptions struct {
	RoomID        string
	ParentID      string    // list replies to this thread root
	BeforeMessage string    // page backwards from this message ID (exclusive)
	Before        time.Time // only messages created before this time
	MentionedMe   bool      // mentionedPeople=me
	Max           int       // 1-1000; 0 means the API default (50)
}

// ListMessages returns one page of messages in a room, newest first.
func (c *Client) ListMessages(opts ListMessagesOptions) ([]Message, error) {
	return nil, errors.New("webex.ListMessages: not implemented")
}

// ListDirectRooms returns the user's 1:1 rooms, most recently active first,
// following Link-header paging until a room's lastActivity is before
// activeSince (when non-zero) or limit rooms have been collected (when > 0).
func (c *Client) ListDirectRooms(activeSince time.Time, limit int) ([]Space, error) {
	return nil, errors.New("webex.ListDirectRooms: not implemented")
}

// GetMessage returns one message by ID, or ErrNotFound if it was deleted.
func (c *Client) GetMessage(id string) (*Message, error) {
	return nil, errors.New("webex.GetMessage: not implemented")
}

// NewClientWithBaseURL is NewClient against a different API base URL, for
// tests that point the client at an httptest server.
func NewClientWithBaseURL(provider TokenProvider, base string) *Client {
	return NewClient(provider)
}
