package auth

// Cross-process refresh lock.
//
// Several processes can hold an OAuthProvider for the same stored tokens at
// once: every Claude Code session runs its own webex-mcp, and
// `webex-mcp --review` runs another. Without coordination two of them can
// see a stale access token at the same moment and both hit the token
// endpoint. The lock below serialises that window. The holder reloads the
// stored tokens and refreshes only if they are still stale, so a waiter
// usually takes over the token the holder just saved and never calls the
// endpoint itself.
//
// The lock is an advisory file created with O_CREATE|O_EXCL, which is
// atomic on every platform we ship for (including NTFS). It is best-effort
// by design: if it cannot be taken in time the caller refreshes anyway,
// which is no worse than the behaviour before the lock existed.

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
	"time"
)

// refreshLockName is the lock file, created beside the token store's
// fallback file (the webex-mcp user config directory).
const refreshLockName = "refresh.lock"

// These are variables rather than constants so tests can shorten them.
var (
	// refreshLockTimeout is how long to wait for another process to
	// release the lock before going ahead without it.
	refreshLockTimeout = 15 * time.Second

	// refreshLockPoll is how often a waiter retries.
	refreshLockPoll = 100 * time.Millisecond

	// refreshLockStaleAfter is the age past which a lock file is treated
	// as abandoned (its holder crashed) and removed. A refresh is bounded
	// by the 30s HTTP client timeout, so a live holder never gets this old.
	refreshLockStaleAfter = 60 * time.Second
)

// acquireRefreshLock takes the cross-process refresh lock in dir. It always
// returns a non-nil release func, which is safe to defer. ok reports whether
// the lock was actually taken. When it was not (timeout, or the directory is
// unusable) a warning is logged and release is a no-op, so the caller can
// carry on unlocked.
func acquireRefreshLock(dir string) (release func(), ok bool) {
	noop := func() {}
	if dir == "" {
		slog.Debug("no directory for refresh lock; refreshing without it")
		return noop, false
	}

	path := filepath.Join(dir, refreshLockName)
	owner, err := lockOwnerID()
	if err != nil {
		slog.Warn("could not create refresh lock id; refreshing without lock", "error", err)
		return noop, false
	}

	deadline := time.Now().Add(refreshLockTimeout)
	for {
		created, err := tryCreateLock(path, owner)
		if created {
			return func() { releaseRefreshLock(path, owner) }, true
		}
		if err != nil {
			// Not "already exists": the directory is missing or not
			// writable. Waiting will not fix that.
			slog.Warn("could not create refresh lock; refreshing without it", "error", err)
			return noop, false
		}

		// Someone else holds it. Clear it if it was abandoned, then retry
		// straight away rather than sleeping.
		if removeIfStale(path) {
			continue
		}

		if !time.Now().Before(deadline) {
			slog.Warn("timed out waiting for refresh lock; refreshing without it",
				"timeout", refreshLockTimeout.String())
			return noop, false
		}
		time.Sleep(refreshLockPoll)
	}
}

// tryCreateLock attempts the atomic create. It returns (true, nil) when the
// lock was taken, (false, nil) when another holder has it, and (false, err)
// for any other failure.
func tryCreateLock(path string, owner []byte) (bool, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600) // #nosec G304 -- path is the fixed lock name inside the webex-mcp config dir
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return false, nil
		}
		return false, err
	}
	_, werr := f.Write(owner)
	cerr := f.Close()
	if werr != nil || cerr != nil {
		// We created the file but could not record ownership, so release
		// could not recognise it as ours. Remove it now so nobody waits on
		// it until it goes stale.
		_ = os.Remove(path)
		return false, fmt.Errorf("writing refresh lock: %w", errors.Join(werr, cerr))
	}
	return true, nil
}

// removeIfStale deletes the lock file if it is older than
// refreshLockStaleAfter and reports whether it did.
//
// Between the Stat and the Remove another waiter may already have cleared
// the stale file and taken a fresh lock, which this would then delete. The
// cost of that race is one duplicate refresh, the same as having no lock.
func removeIfStale(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		// Gone already (released between our create and this stat): retry.
		return errors.Is(err, fs.ErrNotExist)
	}
	if time.Since(info.ModTime()) < refreshLockStaleAfter {
		return false
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		slog.Warn("could not remove stale refresh lock", "error", err)
		return false
	}
	slog.Info("removed stale refresh lock", "age", time.Since(info.ModTime()).Round(time.Second).String())
	return true
}

// releaseRefreshLock removes the lock file, but only while it still holds
// our owner id. If our lock was judged stale and replaced by another
// process, that process's lock is left alone.
func releaseRefreshLock(path string, owner []byte) {
	data, err := os.ReadFile(path) // #nosec G304 -- path is the fixed lock name inside the webex-mcp config dir
	if err != nil {
		return
	}
	if !bytes.Equal(data, owner) {
		slog.Debug("refresh lock taken over by another process; leaving it")
		return
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		slog.Warn("could not remove refresh lock", "error", err)
	}
}

// lockOwnerID returns the lock file contents: pid, acquisition time, and a
// random nonce so two acquisitions by the same pid are distinguishable.
func lockOwnerID() ([]byte, error) {
	nonce := make([]byte, 8)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return fmt.Appendf(nil, "pid=%d\ntime=%s\nnonce=%s\n",
		os.Getpid(), time.Now().UTC().Format(time.RFC3339), hex.EncodeToString(nonce)), nil
}
