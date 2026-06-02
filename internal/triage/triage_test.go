package triage

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pending.json")
	s, err := NewWithPath(path)
	if err != nil {
		t.Fatalf("NewWithPath: %v", err)
	}
	return s, path
}

func sampleItem(id string) Item {
	return Item{
		ID:          id,
		RoomID:      "room-" + id,
		RoomTitle:   "Room " + id,
		PersonEmail: "alice@example.com",
		Text:        "please review " + id,
		Created:     time.Now(),
		Priority:    "high",
	}
}

func TestAddAndListPending(t *testing.T) {
	s, _ := newTestStore(t)

	if err := s.Add(sampleItem("a")); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := s.Add(sampleItem("b")); err != nil {
		t.Fatalf("Add: %v", err)
	}

	pending := s.ListPending()
	if len(pending) != 2 {
		t.Fatalf("expected 2 pending, got %d", len(pending))
	}
	if s.PendingCount() != 2 {
		t.Errorf("PendingCount = %d, want 2", s.PendingCount())
	}
}

func TestAddRequiresID(t *testing.T) {
	s, _ := newTestStore(t)
	if err := s.Add(Item{Text: "no id"}); err == nil {
		t.Error("expected error adding item without ID")
	}
}

func TestAddIdempotentDoesNotResetStatus(t *testing.T) {
	s, _ := newTestStore(t)
	_ = s.Add(sampleItem("a"))

	if _, err := s.MarkProcessed("a"); err != nil {
		t.Fatalf("MarkProcessed: %v", err)
	}

	// Re-adding the same ID (e.g. message re-delivered) must NOT resurrect it.
	if err := s.Add(sampleItem("a")); err != nil {
		t.Fatalf("re-Add: %v", err)
	}
	if got, _ := s.Get("a"); got.Status != StatusProcessed {
		t.Errorf("status after re-Add = %q, want processed (must not resurrect)", got.Status)
	}
	if s.PendingCount() != 0 {
		t.Errorf("PendingCount = %d, want 0", s.PendingCount())
	}
}

func TestReadingNeverMutates(t *testing.T) {
	s, _ := newTestStore(t)
	_ = s.Add(sampleItem("a"))

	// Many reads of various kinds.
	for i := 0; i < 5; i++ {
		_ = s.ListPending()
		_, _ = s.Get("a")
		_ = s.PendingCount()
	}

	if got, _ := s.Get("a"); got.Status != StatusPending {
		t.Errorf("reading changed status to %q; must stay pending", got.Status)
	}
}

func TestMarkProcessedOnlyNamed(t *testing.T) {
	s, _ := newTestStore(t)
	_ = s.Add(sampleItem("a"))
	_ = s.Add(sampleItem("b"))
	_ = s.Add(sampleItem("c"))

	notFound, err := s.MarkProcessed("a", "c", "zzz")
	if err != nil {
		t.Fatalf("MarkProcessed: %v", err)
	}
	if len(notFound) != 1 || notFound[0] != "zzz" {
		t.Errorf("notFound = %v, want [zzz]", notFound)
	}

	pending := s.ListPending()
	if len(pending) != 1 || pending[0].ID != "b" {
		t.Errorf("expected only b pending, got %+v", pending)
	}
	if got, _ := s.Get("a"); got.ProcessedAt.IsZero() {
		t.Error("processed item should have ProcessedAt set")
	}
}

func TestListPendingNewestFirst(t *testing.T) {
	s, _ := newTestStore(t)
	now := time.Now()
	older := sampleItem("old")
	older.Created = now.Add(-1 * time.Hour)
	newer := sampleItem("new")
	newer.Created = now

	_ = s.Add(older)
	_ = s.Add(newer)

	pending := s.ListPending()
	if pending[0].ID != "new" {
		t.Errorf("expected newest first, got %s", pending[0].ID)
	}
}

func TestPersistenceRoundTrip(t *testing.T) {
	s, path := newTestStore(t)
	_ = s.Add(sampleItem("a"))
	_ = s.Add(sampleItem("b"))
	if _, err := s.MarkProcessed("a"); err != nil {
		t.Fatalf("MarkProcessed: %v", err)
	}

	// Reload from disk into a fresh store.
	s2, err := NewWithPath(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if s2.PendingCount() != 1 {
		t.Errorf("after reload PendingCount = %d, want 1", s2.PendingCount())
	}
	if got, ok := s2.Get("a"); !ok || got.Status != StatusProcessed {
		t.Errorf("processed status did not survive reload: %+v", got)
	}
	if got, ok := s2.Get("b"); !ok || got.Status != StatusPending {
		t.Errorf("pending status did not survive reload: %+v", got)
	}
}

func TestPersistenceFilePerms(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix perms not meaningful on windows")
	}
	s, path := newTestStore(t)
	_ = s.Add(sampleItem("a"))

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm&0077 != 0 {
		t.Errorf("file perms = %o, want no group/other access", perm)
	}
}

func TestPrunePreservesPending(t *testing.T) {
	s, _ := newTestStore(t)
	_ = s.Add(sampleItem("keep-pending"))
	_ = s.Add(sampleItem("old-processed"))
	_ = s.Add(sampleItem("new-processed"))

	if _, err := s.MarkProcessed("old-processed", "new-processed"); err != nil {
		t.Fatalf("MarkProcessed: %v", err)
	}

	// Backdate one processed item by mutating via reload trick: set ProcessedAt
	// directly through a fresh store is awkward; instead prune with a cutoff in
	// the future so both processed items qualify, and confirm pending survives.
	removed, err := s.Prune(time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if removed != 2 {
		t.Errorf("removed = %d, want 2 processed items", removed)
	}
	if s.PendingCount() != 1 {
		t.Errorf("PendingCount = %d, want 1 (pending must never be pruned)", s.PendingCount())
	}
	if _, ok := s.Get("keep-pending"); !ok {
		t.Error("pending item was pruned; it must never be removed automatically")
	}
}

func TestInMemoryModeNoPath(t *testing.T) {
	s, err := NewWithPath("")
	if err != nil {
		t.Fatalf("NewWithPath(empty): %v", err)
	}
	if err := s.Add(sampleItem("a")); err != nil {
		t.Fatalf("Add in memory: %v", err)
	}
	if s.PendingCount() != 1 {
		t.Errorf("in-memory PendingCount = %d, want 1", s.PendingCount())
	}
}
