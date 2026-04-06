//go:build !windows

package auth

// restrictFileAccess is a no-op on Unix — os.WriteFile with 0600 mode
// correctly restricts permissions via POSIX file mode bits.
func restrictFileAccess(_ string) error {
	return nil
}
