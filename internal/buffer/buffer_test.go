package buffer

import (
	"sync"
	"testing"
	"time"
)

func msg(id, priority string) NotificationMessage {
	return NotificationMessage{
		ID:          id,
		RoomID:      "room1",
		RoomTitle:   "Test Space",
		PersonEmail: "user@test.com",
		Text:        "message " + id,
		Created:     time.Now(),
		Priority:    priority,
	}
}

func TestNewDefault(t *testing.T) {
	b := New(0)
	if b.MaxSize() != 5000 {
		t.Errorf("expected default max 5000, got %d", b.MaxSize())
	}
}

func TestPushAndDrain(t *testing.T) {
	b := New(10)
	b.Push(msg("1", "low"))
	b.Push(msg("2", "high"))
	b.Push(msg("3", "low"))

	if b.Size() != 3 {
		t.Fatalf("expected size 3, got %d", b.Size())
	}

	drained := b.Drain()
	if len(drained) != 3 {
		t.Fatalf("expected 3 drained, got %d", len(drained))
	}

	// Newest first.
	if drained[0].ID != "3" {
		t.Errorf("expected newest first (id=3), got %s", drained[0].ID)
	}
	if drained[2].ID != "1" {
		t.Errorf("expected oldest last (id=1), got %s", drained[2].ID)
	}

	// Buffer should be empty after drain.
	if b.Size() != 0 {
		t.Errorf("expected empty buffer after drain, got %d", b.Size())
	}
}

func TestDrainEmpty(t *testing.T) {
	b := New(10)
	drained := b.Drain()
	if len(drained) != 0 {
		t.Errorf("expected empty slice from empty drain, got %d items", len(drained))
	}
}

func TestCapacityOverflow(t *testing.T) {
	b := New(3)
	b.Push(msg("1", "low"))
	b.Push(msg("2", "low"))
	b.Push(msg("3", "low"))
	b.Push(msg("4", "low")) // should drop "1"

	if b.Size() != 3 {
		t.Fatalf("expected size 3 after overflow, got %d", b.Size())
	}

	drained := b.Drain()
	if drained[2].ID != "2" {
		t.Errorf("expected oldest remaining id=2, got %s", drained[2].ID)
	}
	if drained[0].ID != "4" {
		t.Errorf("expected newest id=4, got %s", drained[0].ID)
	}
}

func TestDrainByPriority(t *testing.T) {
	b := New(10)
	b.Push(msg("1", "low"))
	b.Push(msg("2", "critical"))
	b.Push(msg("3", "high"))
	b.Push(msg("4", "critical"))
	b.Push(msg("5", "low"))

	critical := b.DrainByPriority([]string{"critical"})
	if len(critical) != 2 {
		t.Fatalf("expected 2 critical, got %d", len(critical))
	}
	if critical[0].ID != "4" {
		t.Errorf("expected newest critical id=4, got %s", critical[0].ID)
	}

	// Remaining should be 3 non-critical messages.
	if b.Size() != 3 {
		t.Errorf("expected 3 remaining, got %d", b.Size())
	}
}

func TestDrainByPriorityMultiple(t *testing.T) {
	b := New(10)
	b.Push(msg("1", "low"))
	b.Push(msg("2", "critical"))
	b.Push(msg("3", "high"))

	matched := b.DrainByPriority([]string{"critical", "high"})
	if len(matched) != 2 {
		t.Fatalf("expected 2 matched, got %d", len(matched))
	}
	if b.Size() != 1 {
		t.Errorf("expected 1 remaining, got %d", b.Size())
	}
}

func TestPeek(t *testing.T) {
	b := New(10)
	b.Push(msg("1", "low"))
	b.Push(msg("2", "low"))
	b.Push(msg("3", "low"))

	peeked := b.Peek(2)
	if len(peeked) != 2 {
		t.Fatalf("expected 2 peeked, got %d", len(peeked))
	}
	if peeked[0].ID != "3" {
		t.Errorf("expected newest first (id=3), got %s", peeked[0].ID)
	}

	// Buffer should still have all items.
	if b.Size() != 3 {
		t.Errorf("expected size 3 after peek, got %d", b.Size())
	}
}

func TestPeekMoreThanAvailable(t *testing.T) {
	b := New(10)
	b.Push(msg("1", "low"))

	peeked := b.Peek(100)
	if len(peeked) != 1 {
		t.Fatalf("expected 1 peeked, got %d", len(peeked))
	}
}

func TestPeekEmpty(t *testing.T) {
	b := New(10)
	peeked := b.Peek(5)
	if len(peeked) != 0 {
		t.Errorf("expected empty slice from empty peek, got %d items", len(peeked))
	}
}

func TestConcurrency(t *testing.T) {
	b := New(100)
	var wg sync.WaitGroup

	// 10 goroutines pushing concurrently.
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				b.Push(msg("concurrent", "low"))
			}
		}(i)
	}
	wg.Wait()

	if b.Size() != 100 {
		t.Errorf("expected 100 messages, got %d", b.Size())
	}

	// Concurrent drain.
	wg.Add(2)
	var d1, d2 []NotificationMessage
	go func() {
		defer wg.Done()
		d1 = b.Drain()
	}()
	go func() {
		defer wg.Done()
		d2 = b.Drain()
	}()
	wg.Wait()

	total := len(d1) + len(d2)
	if total != 100 {
		t.Errorf("expected 100 total drained, got %d", total)
	}
}
