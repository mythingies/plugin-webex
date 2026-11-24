//go:build !unix && !windows

package replydesk

// processState cannot tell on this platform; the lock falls back to its
// mtime rule.
func processState(int) procState { return procUnknown }
