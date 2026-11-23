package replydesk

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/mythingies/plugin-webex/internal/auth"
)

const (
	// storeVersion is the on-disk format version.
	storeVersion = 1
	// maxStoreSize caps the store file read at startup.
	maxStoreSize = 10 << 20
	// storeFileName is the store's name in the webex-mcp config directory.
	storeFileName = "replydesk.json"
)

// storeFile is the on-disk layout of replydesk.json.
type storeFile struct {
	Version int               `json:"version"`
	Owner   string            `json:"owner"`
	Records map[string]Record `json:"records"`
}

// Store persists per-room Reply Desk state in a 0600 JSON file. It is safe
// for concurrent use. Records handed in and out are deep copies, so callers
// can't mutate stored state behind the lock.
type Store struct {
	mu      sync.Mutex
	path    string // "" means in-memory
	owner   string
	records map[string]Record
	now     func() time.Time
}

// DefaultStorePath is replydesk.json in the webex-mcp user config directory.
func DefaultStorePath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locating user config dir: %w", err)
	}
	return filepath.Join(dir, "webex-mcp", storeFileName), nil
}

// OpenStore loads the store at path for the given owner person ID. A file
// written for a different owner is discarded. path "" means in-memory.
//
// A file that isn't valid JSON (or has an unknown version) is renamed to
// "<path>.corrupt" and the store starts empty. A file over 10 MB is an error.
func OpenStore(path, owner string) (*Store, error) {
	s := &Store{
		path:    path,
		owner:   owner,
		records: make(map[string]Record),
		now:     time.Now,
	}
	if path == "" {
		return s, nil
	}
	if owner == "" {
		return nil, errors.New("replydesk store: owner person ID is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("creating store dir: %w", err)
	}

	data, err := readCapped(path, maxStoreSize)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}

	var f storeFile
	if err := json.Unmarshal(data, &f); err != nil || f.Version != storeVersion {
		reason := "unsupported version"
		if err != nil {
			reason = "invalid JSON"
		}
		if rerr := os.Rename(path, path+".corrupt"); rerr != nil {
			return nil, fmt.Errorf("moving unreadable store aside: %w", rerr)
		}
		slog.Warn("replydesk: store unreadable, moved aside and starting empty",
			"reason", reason, "moved_to", path+".corrupt")
		return s, nil
	}

	if f.Owner != owner {
		if rerr := os.Remove(path); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
			return nil, fmt.Errorf("removing store for another account: %w", rerr)
		}
		slog.Info("replydesk: discarded state saved for a different account")
		return s, nil
	}

	for roomID, r := range f.Records {
		if roomID == "" {
			continue
		}
		if r.RoomID == "" {
			r.RoomID = roomID
		}
		if r.RoomID != roomID {
			continue
		}
		s.records[roomID] = cloneRecord(r)
	}
	return s, nil
}

// readCapped reads path, failing if it is larger than limit bytes.
func readCapped(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path) // #nosec G304 -- path is the store location chosen by the caller (user config dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	if fi, err := f.Stat(); err == nil {
		if !fi.Mode().IsRegular() {
			return nil, fmt.Errorf("replydesk store %s is not a regular file", path)
		}
		if fi.Size() > limit {
			return nil, fmt.Errorf("replydesk store is %d bytes, over the %d byte limit", fi.Size(), limit)
		}
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, fmt.Errorf("reading store: %w", err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("replydesk store is over the %d byte limit", limit)
	}
	return data, nil
}

// Get returns the record for a room.
func (s *Store) Get(roomID string) (Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.records[roomID]
	if !ok {
		return Record{}, false
	}
	return cloneRecord(r), true
}

// Put saves a record (UpdatedAt is set) and persists atomically. If the
// write fails the record is still kept in memory and the error is returned.
func (s *Store) Put(r Record) error {
	if r.RoomID == "" {
		return errors.New("replydesk store: record has no room ID")
	}
	r = cloneRecord(r)
	if r.Draft != nil {
		for i := range r.Draft.Cited {
			r.Draft.Cited[i].Text = capBytes(r.Draft.Cited[i].Text, MaxCitationLen)
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	r.UpdatedAt = s.now().UTC().Round(0)
	s.records[r.RoomID] = r
	return s.persistLocked()
}

// All returns every record, sorted by room ID.
func (s *Store) All() []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Record, 0, len(s.records))
	for _, r := range s.records {
		out = append(out, cloneRecord(r))
	}
	slices.SortFunc(out, func(a, b Record) int { return strings.Compare(a.RoomID, b.RoomID) })
	return out
}

// Prune drops records last updated before the cutoff and returns how many
// were dropped. The file is rewritten only if something changed.
func (s *Store) Prune(before time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for id, r := range s.records {
		if r.UpdatedAt.Before(before) {
			delete(s.records, id)
			n++
		}
	}
	if n == 0 {
		return 0, nil
	}
	return n, s.persistLocked()
}

// Purge deletes the default store file. --logout and --switch call it.
func Purge() error {
	path, err := DefaultStorePath()
	if err != nil {
		return err
	}
	return purgePath(path)
}

// purgePath removes path and its .tmp and .corrupt siblings. Missing files
// are not errors.
func purgePath(path string) error {
	var errs []error
	for _, p := range []string{path, path + ".tmp", path + ".corrupt"} {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, fmt.Errorf("removing %s: %w", filepath.Base(p), err))
		}
	}
	return errors.Join(errs...)
}

// persistLocked writes the store atomically: "<path>.tmp" (0600, owner-only
// ACL on Windows), then rename. s.mu must be held.
func (s *Store) persistLocked() error {
	if s.path == "" {
		return nil
	}
	data, err := json.MarshalIndent(storeFile{
		Version: storeVersion,
		Owner:   s.owner,
		Records: s.records,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding store: %w", err)
	}

	tmp := s.path + ".tmp"
	// Remove any stale tmp so O_EXCL creates a fresh file with our mode
	// rather than inheriting a looser one.
	_ = os.Remove(tmp)
	if err := writeTmp(tmp, data); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replacing store: %w", err)
	}
	return nil
}

// writeTmp creates tmp with mode 0600, writes data, syncs, and restricts the
// ACL.
func writeTmp(tmp string, data []byte) error {
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- tmp is derived from the store path in the user config dir
	if err != nil {
		return fmt.Errorf("creating store tmp: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("writing store tmp: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("syncing store tmp: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing store tmp: %w", err)
	}
	if err := auth.RestrictFileAccess(tmp); err != nil {
		return fmt.Errorf("restricting store tmp access: %w", err)
	}
	return nil
}

// capBytes truncates s to at most n bytes without splitting a UTF-8 rune.
func capBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// cloneRecord deep-copies the parts of a Record that alias memory.
func cloneRecord(r Record) Record {
	if r.Draft == nil {
		return r
	}
	d := *r.Draft
	d.Asks = slices.Clone(d.Asks)
	d.MissingInfo = slices.Clone(d.MissingInfo)
	d.Cited = slices.Clone(d.Cited)
	r.Draft = &d
	return r
}

// ErrAlreadyRunning is returned by OpenStore when another webex-mcp --review
// process holds the store.
var ErrAlreadyRunning = errors.New("another webex-mcp --review is already running")

// PutSync is Put, but the record is on disk before it returns. Use it for
// state that must survive a crash (StateSending before the POST).
func (s *Store) PutSync(r Record) error { return s.Put(r) }

// Close flushes pending writes and releases the store's instance lock.
func (s *Store) Close() error { return nil }

// Disabled reports whether persistence stopped because the state was purged
// (--logout/--switch) by another process.
func (s *Store) Disabled() bool { return false }
