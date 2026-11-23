package replydesk

import (
	"errors"
	"time"
)

// Store persists per-room Reply Desk state in a 0600 JSON file.
type Store struct{}

// DefaultStorePath is replydesk.json in the webex-mcp user config directory.
func DefaultStorePath() (string, error) { return "", errors.New("not implemented") }

// OpenStore loads the store at path for the given owner person ID. A file
// written for a different owner is discarded. path "" means in-memory.
func OpenStore(path, owner string) (*Store, error) { return &Store{}, nil }

// Get returns the record for a room.
func (s *Store) Get(roomID string) (Record, bool) { return Record{}, false }

// Put saves a record (UpdatedAt is set) and persists atomically.
func (s *Store) Put(r Record) error { return nil }

// All returns every record.
func (s *Store) All() []Record { return nil }

// Prune drops records last updated before the cutoff.
func (s *Store) Prune(before time.Time) (int, error) { return 0, nil }

// Purge deletes the default store file. --logout and --switch call it.
func Purge() error { return nil }
