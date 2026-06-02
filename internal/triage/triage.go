// Package triage provides a durable, local "still to process" list for
// inbound Webex messages.
//
// The list is a personal reminder, not an outward signal: nothing here is
// ever sent to Webex or visible to message senders. Reading an item never
// changes its status — that is the whole point. An item stays PENDING until
// the user (or an agent acting on their behalf) explicitly marks it
// PROCESSED via MarkProcessed. This solves the problem where reading a
// message in a native client clears the unread badge the user relies on as
// their todo reminder.
//
// State persists to a 0600 file under the user's config dir so reminders
// survive restarts (the in-memory ring buffer does not).
package triage

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/mythingies/plugin-webex/internal/auth"
)

const (
	configDirName = "webex-mcp"
	stateFileName = "pending.json"
)

// Status is the processing state of a triage item.
type Status string

const (
	// StatusPending means the item still needs the user's attention.
	StatusPending Status = "pending"
	// StatusProcessed means the user has explicitly dealt with the item.
	StatusProcessed Status = "processed"
)

// Item is a single inbound message tracked for processing. Fields mirror
// buffer.NotificationMessage plus triage bookkeeping.
type Item struct {
	ID          string    `json:"id"`
	RoomID      string    `json:"roomId"`
	RoomTitle   string    `json:"roomTitle"`
	PersonEmail string    `json:"personEmail"`
	Text        string    `json:"text"`
	Created     time.Time `json:"created"`
	Priority    string    `json:"priority"`
	RoutedAgent string    `json:"routedAgent,omitempty"`

	Status      Status    `json:"status"`
	AddedAt     time.Time `json:"addedAt"`
	ProcessedAt time.Time `json:"processedAt,omitempty"`
}

// Store is a thread-safe, disk-backed triage list.
type Store struct {
	mu    sync.Mutex
	items map[string]*Item // keyed by message ID
	path  string           // persistence path; empty disables persistence
}

// New creates a Store persisting to the user's config dir. Existing state is
// loaded if present. If the config dir can't be resolved, the store still
// works in-memory (persistence disabled) so the listener never fails to run.
func New() (*Store, error) {
	s := &Store{items: make(map[string]*Item)}

	configDir, err := os.UserConfigDir()
	if err != nil {
		// Degrade to in-memory rather than block message intake.
		return s, nil
	}

	dir := filepath.Join(configDir, configDirName)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("creating config dir: %w", err)
	}
	s.path = filepath.Join(dir, stateFileName)

	if err := s.load(); err != nil {
		return nil, fmt.Errorf("loading triage state: %w", err)
	}
	return s, nil
}

// NewWithPath creates a Store at an explicit path (used in tests).
func NewWithPath(path string) (*Store, error) {
	s := &Store{items: make(map[string]*Item), path: path}
	if path != "" {
		if err := s.load(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// Add records an item as PENDING. It is idempotent: re-adding an existing ID
// does NOT reset its status (so a message that's already been PROCESSED stays
// processed, and a re-delivered PENDING item isn't duplicated). Reading or
// re-receiving a message must never resurrect or clear a reminder.
func (s *Store) Add(item Item) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if item.ID == "" {
		return errors.New("triage: item ID required")
	}
	if _, exists := s.items[item.ID]; exists {
		return nil // already tracked; leave status untouched
	}

	it := item // copy
	it.Status = StatusPending
	if it.AddedAt.IsZero() {
		it.AddedAt = item.Created
	}
	s.items[item.ID] = &it
	return s.persist()
}

// ListPending returns all PENDING items, newest-first by Created. Reading
// does not change any status.
func (s *Store) ListPending() []Item {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]Item, 0, len(s.items))
	for _, it := range s.items {
		if it.Status == StatusPending {
			out = append(out, *it)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Created.After(out[j].Created)
	})
	return out
}

// Get returns a copy of the item with the given ID, or (zero, false).
func (s *Store) Get(id string) (Item, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if it, ok := s.items[id]; ok {
		return *it, true
	}
	return Item{}, false
}

// MarkProcessed transitions the named items from PENDING to PROCESSED. This
// is the ONLY path that clears a reminder — it never happens automatically,
// on read, or on reply. Unknown IDs are reported in the returned slice.
// Returns the IDs that were not found.
func (s *Store) MarkProcessed(ids ...string) (notFound []string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	changed := false
	for _, id := range ids {
		it, ok := s.items[id]
		if !ok {
			notFound = append(notFound, id)
			continue
		}
		if it.Status != StatusProcessed {
			it.Status = StatusProcessed
			it.ProcessedAt = now
			changed = true
		}
	}
	if changed {
		if err := s.persist(); err != nil {
			return notFound, err
		}
	}
	return notFound, nil
}

// PendingCount returns the number of items still needing attention.
func (s *Store) PendingCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, it := range s.items {
		if it.Status == StatusPending {
			n++
		}
	}
	return n
}

// Prune removes PROCESSED items older than the cutoff to bound file growth.
// PENDING items are never pruned — a reminder must not vanish on its own.
// Returns the number of items removed.
func (s *Store) Prune(processedBefore time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	removed := 0
	for id, it := range s.items {
		if it.Status == StatusProcessed && !it.ProcessedAt.IsZero() && it.ProcessedAt.Before(processedBefore) {
			delete(s.items, id)
			removed++
		}
	}
	if removed > 0 {
		if err := s.persist(); err != nil {
			return removed, err
		}
	}
	return removed, nil
}

// load reads state from disk. A missing file is not an error.
func (s *Store) load() error {
	data, err := os.ReadFile(s.path) //nolint:gosec // path is under the user's own config dir
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if len(data) == 0 {
		return nil
	}

	var items []*Item
	if err := json.Unmarshal(data, &items); err != nil {
		return fmt.Errorf("parsing triage state: %w", err)
	}
	for _, it := range items {
		if it != nil && it.ID != "" {
			s.items[it.ID] = it
		}
	}
	return nil
}

// persist writes state to disk with restrictive permissions. Caller holds mu.
func (s *Store) persist() error {
	if s.path == "" {
		return nil // in-memory mode
	}

	items := make([]*Item, 0, len(s.items))
	for _, it := range s.items {
		items = append(items, it)
	}
	data, err := json.Marshal(items)
	if err != nil {
		return fmt.Errorf("marshaling triage state: %w", err)
	}

	// Write atomically: temp file + rename, so a crash mid-write can't
	// corrupt an existing reminder list.
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	if err := auth.RestrictFileAccess(tmp); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("restricting triage file access: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
