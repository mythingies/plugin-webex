//go:build !windows

package auth

// RestrictFileAccess is a no-op on Unix — os.WriteFile with 0600 mode
// correctly restricts permissions via POSIX file mode bits.
func RestrictFileAccess(_ string) error {
	return nil
}
