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
	"runtime"
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
	// tombstoneFileName marks that Purge ran. It sits beside the store and
	// holds the purge time; a running Store that sees one newer than its
	// open time stops persisting.
	tombstoneFileName = "replydesk.purged"
	// tmpPattern is the os.CreateTemp pattern suffix for the store's temp
	// files, appended to the store's base name.
	tmpPattern = ".tmp-*"
	// maxTombstoneSize caps the tombstone read.
	maxTombstoneSize = 256
)

// Variables rather than constants so tests can change them before
// OpenStore. They are read once at open.
var (
	// storeFlushInterval is the minimum gap between background writes.
	storeFlushInterval = 250 * time.Millisecond
	// renameTries and renameBackoff retry the atomic rename. On Windows an
	// antivirus or indexer briefly holding the target makes it fail with
	// "Access is denied".
	renameTries   = defaultRenameTries()
	renameBackoff = 20 * time.Millisecond
)

func defaultRenameTries() int {
	if runtime.GOOS == "windows" {
		return 5
	}
	return 1
}

// ErrPurged is returned by Put, PutSync and Prune after the state was purged
// (--logout/--switch) while this store was open. Disabled then reports true.
var ErrPurged = errors.New("replydesk store: state was purged by --logout or --switch; not saving")

// ErrLockLost is returned by Put, PutSync and Prune once another webex-mcp
// --review took over the instance lock (this one's heartbeat stopped, e.g.
// across a long sleep). Unlike a purge the in-memory state is kept, but
// nothing is written again: the other process owns replydesk.json now.
// LockLost then reports true.
var ErrLockLost = errors.New("replydesk store: another webex-mcp --review took over the store; not saving")

// errStoreClosed is returned by writes after Close.
var errStoreClosed = errors.New("replydesk store: closed")

// storeFile is the on-disk layout of replydesk.json.
type storeFile struct {
	Version int               `json:"version"`
	Owner   string            `json:"owner"`
	Records map[string]Record `json:"records"`
}

// Store persists per-room Reply Desk state in a 0600 JSON file. It is safe
// for concurrent use. Records handed in and out are deep copies, so callers
// can't mutate stored state behind the lock.
//
// Put only updates memory and wakes a single writer goroutine, which writes
// at most every storeFlushInterval with the latest state; the disk write
// (and icacls on Windows) never runs on the caller's goroutine. PutSync
// writes before it returns. Close flushes.
//
// A disk store holds replydesk.lock for its lifetime, so a second
// --review on the same store gets ErrAlreadyRunning. If another process
// takes the lock over anyway (this one's heartbeat stalled), the store
// stops writing for good and keeps its state in memory (ErrLockLost).
type Store struct {
	mu      sync.Mutex
	path    string // "" means in-memory
	owner   string
	records map[string]Record
	now     func() time.Time

	// The rest is used only in disk mode.

	// rename replaces the store file with a written temp file.
	rename func(oldpath, newpath string) error
	// openedAt is the wall time OpenStore finished clearing old
	// tombstones; a tombstone at or after it means a purge since.
	openedAt time.Time
	// gen counts in-memory changes; written is the gen last on disk.
	gen, written uint64
	lastErr      error
	disabled     bool
	closed       bool

	writeMu    sync.Mutex    // serialises disk writes (writer, PutSync, Close)
	kick       chan struct{} // capacity 1: a pending write
	stop       chan struct{} // closed by Close
	writerDone chan struct{}
	lock       *instanceLock
	closeOnce  sync.Once
	closeErr   error
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
// A disk store takes the instance lock (ErrAlreadyRunning if another
// --review holds it) and removes a tombstone left by an earlier Purge, so a
// --review started after --logout begins clean. Call Close when done.
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
	dir := filepath.Dir(path)
	// The directory is the shared webex-mcp config dir, so its ACL is left
	// alone: restricting it on Windows would not give new files a private
	// ACL (icacls /grant:r without (OI) sets nothing inheritable) and would
	// change the ACL of every other file there. Each temp file is
	// restricted instead, in the writer.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating store dir: %w", err)
	}

	lock, err := acquireInstanceLock(dir)
	if err != nil {
		return nil, err
	}
	s.lock = lock
	tries, backoff := renameTries, renameBackoff
	s.rename = func(oldpath, newpath string) error {
		return renameRetry(os.Rename, tries, backoff, oldpath, newpath)
	}
	if err := s.load(); err != nil {
		_ = lock.release()
		return nil, err
	}

	s.kick = make(chan struct{}, 1)
	s.stop = make(chan struct{})
	s.writerDone = make(chan struct{})
	go s.writer(storeFlushInterval)
	return s, nil
}

// load clears a stale tombstone and reads the store file. The instance lock
// must be held.
func (s *Store) load() error {
	tomb := s.tombstonePath()
	if err := os.Remove(tomb); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing old purge marker: %w", err)
	}
	s.openedAt = time.Now()

	data, err := readCapped(s.path, maxStoreSize)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}

	var f storeFile
	if err := json.Unmarshal(data, &f); err != nil || f.Version != storeVersion {
		reason := "unsupported version"
		if err != nil {
			reason = "invalid JSON"
		}
		if rerr := s.rename(s.path, s.path+".corrupt"); rerr != nil {
			return fmt.Errorf("moving unreadable store aside: %w", rerr)
		}
		slog.Warn("replydesk: store unreadable, moved aside and starting empty",
			"reason", reason, "moved_to", s.path+".corrupt")
		return nil
	}

	if f.Owner != s.owner {
		if rerr := os.Remove(s.path); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
			return fmt.Errorf("removing store for another account: %w", rerr)
		}
		slog.Info("replydesk: discarded state saved for a different account")
		return nil
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
	return nil
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

// Put saves a record (UpdatedAt is set) in memory and schedules a background
// write. It returns the error of the last background write, if that failed
// and nothing has been written since; the record is kept in memory either
// way. After a purge (see Disabled) it returns ErrPurged and keeps nothing.
// After the instance lock was lost (see LockLost) it keeps the record in
// memory, writes nothing, and returns ErrLockLost.
func (s *Store) Put(r Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.putLocked(r); err != nil {
		return err
	}
	if s.lockLostFlagLocked() {
		return ErrLockLost
	}
	s.scheduleLocked()
	return s.lastErr
}

// PutSync is Put, but the record is on disk before it returns. Use it for
// state that must survive a crash (StateSending before the POST). The
// record is kept in memory even if the write fails, or is refused with
// ErrLockLost.
func (s *Store) PutSync(r Record) error {
	s.mu.Lock()
	err := s.putLocked(r)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if s.path == "" {
		return nil
	}
	return s.flush()
}

// putLocked validates r and stores a copy in memory. s.mu must be held.
func (s *Store) putLocked(r Record) error {
	if r.RoomID == "" {
		return errors.New("replydesk store: record has no room ID")
	}
	if s.disabled {
		return ErrPurged
	}
	if s.closed {
		return errStoreClosed
	}
	r = cloneRecord(r)
	if r.Draft != nil {
		for i := range r.Draft.Cited {
			r.Draft.Cited[i].Text = capBytes(r.Draft.Cited[i].Text, MaxCitationLen)
		}
	}
	r.UpdatedAt = s.now().UTC().Round(0)
	s.records[r.RoomID] = r
	s.gen++
	return nil
}

// scheduleLocked wakes the writer. s.mu must be held.
func (s *Store) scheduleLocked() {
	if s.path == "" {
		return
	}
	select {
	case s.kick <- struct{}{}:
	default: // a write is already pending; it will pick up this change
	}
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
// were dropped. A write is scheduled only if something changed; the error is
// the last background write's, as with Put.
func (s *Store) Prune(before time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.disabled {
		return 0, ErrPurged
	}
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
	if s.closed {
		return n, errStoreClosed
	}
	s.gen++
	if s.lockLostFlagLocked() {
		return n, ErrLockLost
	}
	s.scheduleLocked()
	return n, s.lastErr
}

// LastError returns the error from the most recent failed disk write, or nil
// once a later write succeeds. Background write failures are also logged.
func (s *Store) LastError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastErr
}

// Disabled reports whether persistence stopped because the state was purged
// (--logout/--switch) by another process. It checks for the purge marker,
// so it notices a purge before the next write does.
func (s *Store) Disabled() bool {
	if s.path == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return s.disabled
	}
	return s.checkPurgedLocked()
}

// LockLost reports whether persistence stopped because another webex-mcp
// --review took over the instance lock. It reads the lock file, so it
// notices a takeover before the heartbeat or the next write does. Once true
// it stays true.
func (s *Store) LockLost() bool {
	if s.lock == nil {
		return false
	}
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		// Close released the lock; only a loss seen before then counts.
		return s.lock.lost.Load()
	}
	return s.lock.isLost()
}

// lockLostFlagLocked reports a lock loss already seen, without touching the
// disk. s.mu must be held (it orders the read with the caller's state).
func (s *Store) lockLostFlagLocked() bool {
	return s.lock != nil && s.lock.lost.Load()
}

// Close stops the writer, flushes pending changes, and releases the
// instance lock. It is safe to call more than once; later calls return the
// first call's result. Get and All keep working on the in-memory state.
func (s *Store) Close() error {
	if s.path == "" {
		return nil
	}
	s.closeOnce.Do(func() {
		close(s.stop)
		<-s.writerDone
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		ferr := s.flush()
		if errors.Is(ferr, ErrPurged) || errors.Is(ferr, ErrLockLost) {
			// Disabled or LockLost reports it. After a purge nothing was
			// lost that should be kept; after a takeover the other process
			// owns the file.
			ferr = nil
		}
		s.closeErr = errors.Join(ferr, s.lock.release())
	})
	return s.closeErr
}

// writer is the single background writer. It sleeps until kicked, waits so
// writes are at least interval apart, then writes the latest state.
func (s *Store) writer(interval time.Duration) {
	defer close(s.writerDone)
	var last time.Time
	for {
		select {
		case <-s.stop:
			return
		case <-s.kick:
		}
		if wait := interval - time.Since(last); !last.IsZero() && wait > 0 {
			t := time.NewTimer(wait)
			select {
			case <-s.stop:
				t.Stop()
				return // Close flushes
			case <-t.C:
			}
		}
		err := s.flush()
		last = time.Now()
		if errors.Is(err, ErrLockLost) {
			return // permanent; isLost logged it once
		}
		if err != nil && !errors.Is(err, ErrPurged) {
			slog.Warn("replydesk: saving state failed; it is kept in memory and retried on the next change",
				"error", err)
		}
	}
}

// flush writes the current state if it changed since the last successful
// write. It checks for a purge before and after writing, and that the
// instance lock is still ours right before replacing the file.
func (s *Store) flush() error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	s.mu.Lock()
	if s.checkPurgedLocked() {
		s.mu.Unlock()
		return ErrPurged
	}
	if s.lockLostFlagLocked() {
		s.mu.Unlock()
		return ErrLockLost
	}
	if s.gen == s.written {
		s.mu.Unlock()
		return nil
	}
	gen := s.gen
	data, err := json.MarshalIndent(storeFile{
		Version: storeVersion,
		Owner:   s.owner,
		Records: s.records,
	}, "", "  ")
	s.mu.Unlock()
	if err != nil {
		err = fmt.Errorf("encoding store: %w", err)
	} else {
		err = s.writeFile(data)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if errors.Is(err, ErrLockLost) {
		return err // not a write failure: LockLost reports it
	}
	if err != nil {
		s.lastErr = err
		return err
	}
	// A purge that ran while we wrote: undo our write, so the state the
	// user asked to delete is not left on disk.
	if s.checkPurgedLocked() {
		if rerr := os.Remove(s.path); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
			slog.Warn("replydesk: could not remove state written during a purge", "error", rerr)
		}
		return ErrPurged
	}
	if gen > s.written {
		s.written = gen
	}
	s.lastErr = nil
	return nil
}

// writeFile writes data atomically: a fresh temp file in the store dir
// (0600, owner-only ACL on Windows), then rename over the store. The
// rename is skipped (ErrLockLost) if another process took over the
// instance lock while the temp file was written.
func (s *Store) writeFile(data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(s.path), filepath.Base(s.path)+tmpPattern)
	if err != nil {
		return fmt.Errorf("creating store tmp: %w", err)
	}
	tmp := f.Name()
	if err := writeTmp(f, data); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	// As late as possible: a takeover between this check and the rename
	// is still possible, but the window is the rename itself.
	if s.lock != nil && s.lock.isLost() {
		_ = os.Remove(tmp)
		return ErrLockLost
	}
	if err := s.rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replacing store: %w", err)
	}
	return nil
}

// writeTmp writes data to f, syncs and closes it, and restricts its ACL.
func writeTmp(f *os.File, data []byte) error {
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
	if err := auth.RestrictFileAccess(f.Name()); err != nil {
		return fmt.Errorf("restricting store tmp access: %w", err)
	}
	return nil
}

// renameRetry calls rename up to tries times, sleeping backoff (doubling)
// between attempts. A missing source is not retried.
func renameRetry(rename func(oldpath, newpath string) error, tries int, backoff time.Duration, oldpath, newpath string) error {
	var err error
	n := max(tries, 1)
	for i := range n {
		if err = rename(oldpath, newpath); err == nil {
			return nil
		}
		if errors.Is(err, fs.ErrNotExist) || i == n-1 {
			break
		}
		time.Sleep(backoff)
		backoff *= 2
	}
	return err
}

// ---- purge ----

func (s *Store) tombstonePath() string {
	return filepath.Join(filepath.Dir(s.path), tombstoneFileName)
}

// checkPurgedLocked reports whether persistence is off, turning it off if a
// purge happened since open: records are dropped, nothing is deleted, and a
// warning is logged once. s.mu must be held.
func (s *Store) checkPurgedLocked() bool {
	if s.disabled {
		return true
	}
	if !purgedSince(s.tombstonePath(), s.openedAt) {
		return false
	}
	s.disabled = true
	s.records = make(map[string]Record)
	slog.Warn("replydesk: Reply Desk state was deleted by --logout or --switch; no longer saving it")
	return true
}

// purgedSince reports whether the tombstone at path records a purge at or
// after t. An unreadable tombstone counts as a purge: OpenStore removed any
// older one, so one that exists now was written after open.
func purgedSince(path string, t time.Time) bool {
	data, err := readCapped(path, maxTombstoneSize)
	if errors.Is(err, fs.ErrNotExist) {
		return false
	}
	if err != nil {
		return true
	}
	at, perr := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(data)))
	if perr != nil {
		return true
	}
	return !at.Before(t)
}

// Purge deletes the default store file. --logout and --switch call it. A
// --review still running notices the purge marker it leaves and stops
// saving.
func Purge() error {
	path, err := DefaultStorePath()
	if err != nil {
		return err
	}
	return purgePath(path)
}

// purgePath writes the purge marker, then removes path and its temp and
// .corrupt siblings. Missing files are not errors. The instance lock is left
// for its holder.
//
// The marker is written first so a running Store's post-write check sees
// it even if its write lands between our removes; written after, a write
// racing the removes could survive.
func purgePath(path string) error {
	var errs []error
	dir := filepath.Dir(path)
	if _, err := os.Stat(dir); err == nil {
		stamp := time.Now().UTC().Format(time.RFC3339Nano) + "\n"
		if err := os.WriteFile(filepath.Join(dir, tombstoneFileName), []byte(stamp), 0o600); err != nil {
			errs = append(errs, fmt.Errorf("writing purge marker: %w", err))
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		errs = append(errs, fmt.Errorf("checking store dir: %w", err))
	}

	targets := []string{path, path + ".tmp", path + ".corrupt"}
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		errs = append(errs, fmt.Errorf("listing store tmp files: %w", err))
	}
	tmpPrefix := filepath.Base(path) + strings.TrimSuffix(tmpPattern, "*")
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), tmpPrefix) {
			targets = append(targets, filepath.Join(dir, e.Name()))
		}
	}
	for _, p := range targets {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, fmt.Errorf("removing %s: %w", filepath.Base(p), err))
		}
	}
	return errors.Join(errs...)
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
