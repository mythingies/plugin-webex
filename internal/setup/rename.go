package setup

import (
	"errors"
	"io/fs"
	"os"
	"runtime"
	"syscall"
	"time"
)

// The .mcp.json replace is a rename of a freshly written temp file. On
// Windows an antivirus scanner or the search indexer often opens that file
// right after icacls changes its ACL, and a rename while it is held fails
// with "Access is denied" or a sharing violation. Those are retried a few
// times; nothing else is. Variables so tests can inject a rename and
// shorten the backoff. (internal/replydesk has the same retry for its store;
// it is duplicated here rather than shared.)
var (
	configRename        = os.Rename
	configRenameTries   = 5
	configRenameBackoff = 20 * time.Millisecond
	// renameRetryable reports whether a failed rename is worth retrying.
	renameRetryable = isTransientRenameError
)

// Windows error codes for a file another process has open.
const (
	errorSharingViolation syscall.Errno = 32 // ERROR_SHARING_VIOLATION
	errorLockViolation    syscall.Errno = 33 // ERROR_LOCK_VIOLATION
)

// isTransientRenameError reports whether err is a Windows rename failure
// caused by another process briefly holding the file: access denied
// (fs.ErrPermission), or a sharing or lock violation. Always false
// elsewhere, where a rename error is not transient.
func isTransientRenameError(err error) bool {
	if runtime.GOOS != "windows" || err == nil {
		return false
	}
	if errors.Is(err, fs.ErrPermission) {
		return true
	}
	var errno syscall.Errno
	return errors.As(err, &errno) && (errno == errorSharingViolation || errno == errorLockViolation)
}

// renameWithRetry calls rename up to tries times, sleeping backoff
// (doubling) between attempts, while retryable reports the error as
// transient. It returns the last error.
func renameWithRetry(rename func(oldpath, newpath string) error, retryable func(error) bool, tries int, backoff time.Duration, oldpath, newpath string) error {
	var err error
	n := max(tries, 1)
	for i := range n {
		if err = rename(oldpath, newpath); err == nil {
			return nil
		}
		if i == n-1 || !retryable(err) {
			break
		}
		time.Sleep(backoff)
		backoff *= 2
	}
	return err
}
