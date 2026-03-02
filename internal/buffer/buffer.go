package buffer

import (
	"sync"
	"time"
)

// NotificationMessage represents a buffered inbound Webex message.
type NotificationMessage struct {
	ID          string    `json:"id"`
	RoomID      string    `json:"roomId"`
	RoomTitle   string    `json:"roomTitle"`
	PersonID    string    `json:"personId"`
	PersonEmail string    `json:"personEmail"`
	PersonName  string    `json:"personName"`
	Text        string    `json:"text"`
	HTML        string    `json:"html"`
	Created     time.Time `json:"created"`
	Priority    string    `json:"priority"`
	RoutedAgent string    `json:"routedAgent"`
	Mentions    []string  `json:"mentions,omitempty"`
}

// RingBuffer is a thread-safe bounded buffer for notification messages.
// When full, the oldest message is dropped on Push.
type RingBuffer struct {
	mu      sync.Mutex
	items   []NotificationMessage
	maxSize int
}

// New creates a RingBuffer with the given capacity.
func New(maxSize int) *RingBuffer {
	if maxSize <= 0 {
		maxSize = 5000
	}
	return &RingBuffer{
		items:   make([]NotificationMessage, 0, maxSize),
		maxSize: maxSize,
	}
}

// Push adds a message to the buffer. If at capacity, the oldest message is dropped.
func (b *RingBuffer) Push(msg NotificationMessage) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if len(b.items) >= b.maxSize {
		// Drop oldest (index 0).
		b.items = b.items[1:]
	}
	b.items = append(b.items, msg)
}

// Drain removes and returns all messages from the buffer, newest first.
func (b *RingBuffer) Drain() []NotificationMessage {
	b.mu.Lock()
	defer b.mu.Unlock()

	if len(b.items) == 0 {
		return nil
	}

	out := make([]NotificationMessage, len(b.items))
	copy(out, b.items)
	b.items = b.items[:0]

	// Reverse so newest is first.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// DrainByPriority removes and returns only messages matching the given priorities, newest first.
func (b *RingBuffer) DrainByPriority(priorities []string) []NotificationMessage {
	pset := make(map[string]bool, len(priorities))
	for _, p := range priorities {
		pset[p] = true
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	var matched, remaining []NotificationMessage
	for _, msg := range b.items {
		if pset[msg.Priority] {
			matched = append(matched, msg)
		} else {
			remaining = append(remaining, msg)
		}
	}
	b.items = remaining

	// Reverse so newest is first.
	for i, j := 0, len(matched)-1; i < j; i, j = i+1, j-1 {
		matched[i], matched[j] = matched[j], matched[i]
	}
	return matched
}

// Peek returns the most recent n messages without removing them, newest first.
func (b *RingBuffer) Peek(n int) []NotificationMessage {
	b.mu.Lock()
	defer b.mu.Unlock()

	if n <= 0 || len(b.items) == 0 {
		return nil
	}
	if n > len(b.items) {
		n = len(b.items)
	}

	// Take the last n items (most recent).
	start := len(b.items) - n
	out := make([]NotificationMessage, n)
	copy(out, b.items[start:])

	// Reverse so newest is first.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// Size returns the current number of buffered messages.
func (b *RingBuffer) Size() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.items)
}

// MaxSize returns the buffer capacity.
func (b *RingBuffer) MaxSize() int {
	return b.maxSize
}
