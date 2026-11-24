package replydesk

// Instance lock: one webex-mcp --review per store.
//
// Two --review processes on the same replydesk.json would each keep their
// own in-memory map and overwrite the other's file on every write. OpenStore
// therefore takes "replydesk.lock" in the store directory, created with
// O_CREATE|O_EXCL (atomic on every platform we ship for, including NTFS).
// The holder touches its mtime every lockHeartbeat.
//
// An existing lock is taken over when its holder is gone: at once when the
// pid it records is no longer running, or is our own pid but not a lock this
// process holds (left by an earlier process that had our pid); otherwise
// when its mtime is older than lockStaleAfter (pid unreadable, liveness
// unknown, or a crashed holder's pid reused by an unrelated process).
//
// A holder whose lock was taken over (the file is gone or names another
// owner) marks it lost for good, and the Store stops writing (ErrLockLost),
// so two processes never both write replydesk.json.

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// lockFileName is the instance lock beside the store file.
const lockFileName = "replydesk.lock"

// Variables rather than constants so tests can shorten them. Read once by
// acquireInstanceLock, so changing them does not race a running heartbeat.
var (
	// lockHeartbeat is how often the holder refreshes the lock's mtime.
	lockHeartbeat = 60 * time.Second
	// lockStaleAfter is the mtime age past which a lock is abandoned.
	lockStaleAfter = 3 * time.Minute
)

// heldLocks holds the owner contents of the locks this process holds, so a
// lock file naming our own pid can be told apart: held by us (a second
// OpenStore in this process) or left by an earlier process with our pid.
var (
	heldLocksMu sync.Mutex
	heldLocks   = make(map[string]bool)
)

func setHeld(owner []byte, held bool) {
	heldLocksMu.Lock()
	defer heldLocksMu.Unlock()
	if held {
		heldLocks[string(owner)] = true
	} else {
		delete(heldLocks, string(owner))
	}
}

func isHeldHere(owner []byte) bool {
	heldLocksMu.Lock()
	defer heldLocksMu.Unlock()
	return heldLocks[string(owner)]
}

// procState is what processState can tell about a pid.
type procState int

const (
	procUnknown procState = iota // could not tell
	procAlive
	procDead
)

// instanceLock is a held replydesk.lock.
type instanceLock struct {
	path  string
	owner []byte
	// lost is set once the lock was seen taken over. It never clears.
	lost atomic.Bool

	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

// acquireInstanceLock takes the lock in dir. A lock held by a live process
// is ErrAlreadyRunning (wrapped with the holder's pid when readable).
func acquireInstanceLock(dir string) (*instanceLock, error) {
	path := filepath.Join(dir, lockFileName)
	owner, err := instanceLockOwner()
	if err != nil {
		return nil, fmt.Errorf("creating store lock id: %w", err)
	}
	staleAfter := lockStaleAfter

	// Two rounds: the second follows a stale-lock removal. A third
	// would mean another process is racing us for the takeover, and
	// whoever won it is running.
	for range 2 {
		created, err := createLockFile(path, owner)
		if err != nil {
			return nil, err
		}
		if created {
			l := &instanceLock{
				path:  path,
				owner: owner,
				stop:  make(chan struct{}),
				done:  make(chan struct{}),
			}
			setHeld(owner, true)
			go l.heartbeat(lockHeartbeat)
			return l, nil
		}

		data, rerr := os.ReadFile(path) // #nosec G304 -- path is the fixed lock name inside the store dir
		info, err := os.Stat(path)
		if errors.Is(err, fs.ErrNotExist) || errors.Is(rerr, fs.ErrNotExist) {
			continue // released between our create and the checks
		}
		if err != nil {
			return nil, fmt.Errorf("checking store lock: %w", err)
		}
		if rerr != nil {
			data = nil // unreadable: decide by mtime alone
		}
		age := time.Since(info.ModTime())
		why := lockStaleReason(data, age, staleAfter)
		if why == "" {
			return nil, lockHeldError(path)
		}
		// Between the checks and the Remove another process may have taken
		// over the stale lock first; its O_EXCL create and ours cannot both
		// succeed, but the Remove could delete its fresh lock. That window
		// needs two --review processes starting within microseconds of each
		// other after a crash; the loser of it sees its lock gone and stops
		// saving (ErrLockLost), so the cost is a window that no longer
		// saves, not two writers.
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("removing stale store lock: %w", err)
		}
		slog.Info("replydesk: took over a stale store lock",
			"reason", why, "age", age.Round(time.Second).String())
	}
	return nil, lockHeldError(path)
}

// lockStaleReason decides whether an existing lock with content data and
// mtime age is abandoned. It returns why, or "" when the lock is held.
func lockStaleReason(data []byte, age, staleAfter time.Duration) string {
	if pid, ok := lockPID(data); ok {
		if pid == os.Getpid() {
			if isHeldHere(data) {
				return "" // a second OpenStore in this process
			}
			return "left by an earlier process with this pid"
		}
		if processState(pid) == procDead {
			return "holder exited"
		}
	}
	// The pid is unreadable, alive, or of unknown state. An alive pid may
	// belong to an unrelated process that reused a crashed holder's pid, so
	// a lock whose heartbeat stopped is still taken over. A holder that is
	// in fact running notices and stops saving (ErrLockLost).
	if age >= staleAfter {
		return "heartbeat stopped"
	}
	return ""
}

// lockPID parses the pid= line of a lock file.
func lockPID(data []byte) (int, bool) {
	for line := range strings.SplitSeq(string(data), "\n") {
		v, ok := strings.CutPrefix(strings.TrimSpace(line), "pid=")
		if !ok {
			continue
		}
		pid, err := strconv.Atoi(v)
		if err != nil || pid <= 0 {
			return 0, false
		}
		return pid, true
	}
	return 0, false
}

// lockHeldError is ErrAlreadyRunning, naming the holder's pid if the lock
// file records one.
func lockHeldError(path string) error {
	data, err := os.ReadFile(path) // #nosec G304 -- path is the fixed lock name inside the store dir
	if err == nil {
		for line := range strings.SplitSeq(string(data), "\n") {
			if pid, ok := strings.CutPrefix(line, "pid="); ok && pid != "" {
				return fmt.Errorf("%w (pid %s)", ErrAlreadyRunning, pid)
			}
		}
	}
	return ErrAlreadyRunning
}

// createLockFile does the atomic create. It returns (true, nil) when the
// lock was taken, (false, nil) when the file already exists, and an error
// for anything else.
func createLockFile(path string, owner []byte) (bool, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) // #nosec G304 -- path is the fixed lock name inside the store dir
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return false, nil
		}
		return false, fmt.Errorf("creating store lock: %w", err)
	}
	_, werr := f.Write(owner)
	cerr := f.Close()
	if werr != nil || cerr != nil {
		// Unrecorded ownership: release could not recognise it, so do not
		// leave it for others to wait out.
		_ = os.Remove(path)
		return false, fmt.Errorf("writing store lock: %w", errors.Join(werr, cerr))
	}
	return true, nil
}

// heartbeat touches the lock's mtime every interval until release, or until
// the lock is seen taken over: another process's lock is never refreshed.
func (l *instanceLock) heartbeat(interval time.Duration) {
	defer close(l.done)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-t.C:
		}
		if l.isLost() {
			return
		}
		if !l.ours() {
			continue // unreadable just now; look again next tick
		}
		now := time.Now()
		if err := os.Chtimes(l.path, now, now); err != nil {
			slog.Warn("replydesk: could not refresh store lock", "error", err)
		}
	}
}

// isLost reports whether the lock was taken over: the file is gone or names
// another owner. Once true it stays true, and a warning is logged once. A
// read error other than a missing file proves nothing and does not count.
func (l *instanceLock) isLost() bool {
	if l.lost.Load() {
		return true
	}
	data, err := os.ReadFile(l.path) // #nosec G304 -- path is the fixed lock name inside the store dir
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false
	}
	if err == nil && bytes.Equal(data, l.owner) {
		return false
	}
	if l.lost.CompareAndSwap(false, true) {
		slog.Warn("replydesk: store lock was taken over by another webex-mcp --review; this process no longer saves Reply Desk state")
	}
	return true
}

// ours reports whether the lock file still holds our owner id.
func (l *instanceLock) ours() bool {
	data, err := os.ReadFile(l.path) // #nosec G304 -- path is the fixed lock name inside the store dir
	return err == nil && bytes.Equal(data, l.owner)
}

// release stops the heartbeat and removes the lock if it is still ours.
// It is safe to call more than once.
func (l *instanceLock) release() error {
	var err error
	l.stopOnce.Do(func() {
		close(l.stop)
		<-l.done
		defer setHeld(l.owner, false)
		if !l.ours() {
			return
		}
		if rerr := os.Remove(l.path); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
			err = fmt.Errorf("removing store lock: %w", rerr)
		}
	})
	return err
}

// instanceLockOwner is the lock file content: pid, time, and a nonce so two
// locks taken by one pid are distinguishable.
func instanceLockOwner() ([]byte, error) {
	nonce := make([]byte, 8)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return fmt.Appendf(nil, "pid=%d\ntime=%s\nnonce=%s\n",
		os.Getpid(), time.Now().UTC().Format(time.RFC3339), hex.EncodeToString(nonce)), nil
}
