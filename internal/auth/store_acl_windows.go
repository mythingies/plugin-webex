//go:build windows

package auth

import (
	"fmt"
	"os/exec"
	"os/user"
)

// RestrictFileAccess sets restrictive NTFS ACLs on a sensitive file.
// Go's os.WriteFile 0600 mode has no effect on Windows NTFS, so we
// call icacls to remove inherited permissions and grant access only
// to the current user. Use for any file containing secrets.
func RestrictFileAccess(path string) error {
	u, err := user.Current()
	if err != nil {
		return fmt.Errorf("getting current user: %w", err)
	}

	// /inheritance:r  — remove all inherited ACEs
	// /grant:r        — replace (not add) with explicit grant
	// :F              — full control for current user only
	cmd := exec.Command("icacls", path, "/inheritance:r", "/grant:r", u.Username+":F") //nolint:gosec // path from UserConfigDir, username from os/user
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("setting file ACL via icacls: %s: %w", string(output), err)
	}
	return nil
}
