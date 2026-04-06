package buffer

import (
	"log/slog"
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

	size := len(b.items)

	// Alert on buffer pressure thresholds.
	if size >= b.maxSize {
		slog.Warn("buffer overflow: dropping oldest message", "size", size, "max", b.maxSize, "dropped_id", b.items[0].ID)
		b.items = b.items[1:]
	} else if pct := size * 100 / b.maxSize; pct >= 95 {
		slog.Warn("buffer near capacity", "utilization_pct", pct, "size", size, "max", b.maxSize)
	}

	b.items = append(b.items, msg)
}

// DrainByAgent removes and returns only messages routed to the given agent, newest first.
// If agent is empty, all messages are returned (same as Drain).
func (b *RingBuffer) DrainByAgent(agent string) []NotificationMessage {
	if agent == "" {
		return b.Drain()
	}

	b.mu.Lock()
	var matched, remaining []NotificationMessage
	for _, msg := range b.items {
		if msg.RoutedAgent == agent {
			matched = append(matched, msg)
		} else {
			remaining = append(remaining, msg)
		}
	}
	b.items = remaining
	b.mu.Unlock()

	for i, j := 0, len(matched)-1; i < j; i, j = i+1, j-1 {
		matched[i], matched[j] = matched[j], matched[i]
	}
	if matched == nil {
		return []NotificationMessage{}
	}
	return matched
}

// PeekByAgent returns the most recent n messages for a specific agent without removing them.
func (b *RingBuffer) PeekByAgent(n int, agent string) []NotificationMessage {
	if agent == "" {
		return b.Peek(n)
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	var filtered []NotificationMessage
	for i := len(b.items) - 1; i >= 0 && len(filtered) < n; i-- {
		if b.items[i].RoutedAgent == agent {
			filtered = append(filtered, b.items[i])
		}
	}
	if filtered == nil {
		return []NotificationMessage{}
	}
	return filtered
}

// Drain removes and returns all messages from the buffer, newest first.
func (b *RingBuffer) Drain() []NotificationMessage {
	b.mu.Lock()
	if len(b.items) == 0 {
		b.mu.Unlock()
		return []NotificationMessage{}
	}

	// Swap out items under lock, then reverse outside lock.
	items := b.items
	b.items = make([]NotificationMessage, 0, b.maxSize)
	b.mu.Unlock()

	// Reverse so newest is first.
	for i, j := 0, len(items)-1; i < j; i, j = i+1, j-1 {
		items[i], items[j] = items[j], items[i]
	}
	return items
}

// DrainByPriority removes and returns only messages matching the given priorities, newest first.
func (b *RingBuffer) DrainByPriority(priorities []string) []NotificationMessage {
	if len(priorities) > 100 {
		priorities = priorities[:100]
	}
	pset := make(map[string]bool, len(priorities))
	for _, p := range priorities {
		pset[p] = true
	}

	b.mu.Lock()
	var matched, remaining []NotificationMessage
	for _, msg := range b.items {
		if pset[msg.Priority] {
			matched = append(matched, msg)
		} else {
			remaining = append(remaining, msg)
		}
	}
	b.items = remaining
	b.mu.Unlock()

	// Reverse so newest is first.
	for i, j := 0, len(matched)-1; i < j; i, j = i+1, j-1 {
		matched[i], matched[j] = matched[j], matched[i]
	}
	if matched == nil {
		return []NotificationMessage{}
	}
	return matched
}

// Peek returns the most recent n messages without removing them, newest first.
func (b *RingBuffer) Peek(n int) []NotificationMessage {
	b.mu.Lock()
	defer b.mu.Unlock()

	if n <= 0 || len(b.items) == 0 {
		return []NotificationMessage{}
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
