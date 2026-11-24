package setup

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"
)

// accessDenied is what os.Rename returns on Windows while another process
// holds the file.
var accessDenied = &os.LinkError{Op: "rename", Old: "a", New: "b", Err: fs.ErrPermission}

func always(error) bool { return true }

func TestRenameWithRetry_RetriesUntilSuccess(t *testing.T) {
	calls := 0
	rename := func(string, string) error {
		calls++
		if calls < 3 {
			return accessDenied
		}
		return nil
	}
	if err := renameWithRetry(rename, always, 5, time.Millisecond, "a", "b"); err != nil {
		t.Fatalf("err = %v", err)
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3", calls)
	}
}

func TestRenameWithRetry_GivesUpAfterTries(t *testing.T) {
	calls := 0
	rename := func(string, string) error { calls++; return accessDenied }
	err := renameWithRetry(rename, always, 5, time.Millisecond, "a", "b")
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("err = %v, want the last rename error", err)
	}
	if calls != 5 {
		t.Errorf("calls = %d, want 5", calls)
	}
}

func TestRenameWithRetry_DoesNotRetryOtherErrors(t *testing.T) {
	calls := 0
	rename := func(string, string) error { calls++; return fs.ErrNotExist }
	err := renameWithRetry(rename, func(error) bool { return false }, 5, time.Millisecond, "a", "b")
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
}

func TestIsTransientRenameError(t *testing.T) {
	windows := runtime.GOOS == "windows"
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"access denied", accessDenied, windows},
		{"sharing violation", &os.LinkError{Op: "rename", Err: syscall.Errno(32)}, windows},
		{"lock violation", fmt.Errorf("x: %w", syscall.Errno(33)), windows},
		{"not exist", &os.LinkError{Op: "rename", Err: fs.ErrNotExist}, false},
		{"other", errors.New("boom"), false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		if got := isTransientRenameError(c.err); got != c.want {
			t.Errorf("%s: isTransientRenameError = %v, want %v", c.name, got, c.want)
		}
	}
}

// A scanner holding the new .mcp.json for a moment after icacls must not
// fail --setup: writeConfigFile retries the rename.
func TestWriteConfigFile_RetriesTransientRename(t *testing.T) {
	oldRename, oldRetryable, oldTries, oldBackoff := configRename, renameRetryable, configRenameTries, configRenameBackoff
	t.Cleanup(func() {
		configRename, renameRetryable, configRenameTries, configRenameBackoff = oldRename, oldRetryable, oldTries, oldBackoff
	})
	calls := 0
	configRename = func(oldpath, newpath string) error {
		calls++
		if calls == 1 {
			return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: fs.ErrPermission}
		}
		return os.Rename(oldpath, newpath)
	}
	// Retry permission errors on every OS so the test runs everywhere.
	renameRetryable = func(err error) bool { return errors.Is(err, fs.ErrPermission) }
	configRenameTries, configRenameBackoff = 5, time.Millisecond

	path := filepath.Join(t.TempDir(), mcpConfigFile)
	want := []byte("{\"mcpServers\": {}}\n")
	if err := writeConfigFile(path, want); err != nil {
		t.Fatalf("writeConfigFile: %v", err)
	}
	if calls != 2 {
		t.Errorf("rename calls = %d, want 2", calls)
	}
	got, err := os.ReadFile(path) // #nosec G304 -- test temp dir
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("content = %q, want %q", got, want)
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), mcpConfigFile+".*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Errorf("temp files left behind: %v", matches)
	}
}

// A rename that keeps failing still fails the write and removes the temp
// file.
func TestWriteConfigFile_RenameFailureCleansUp(t *testing.T) {
	oldRename, oldTries, oldBackoff := configRename, configRenameTries, configRenameBackoff
	t.Cleanup(func() { configRename, configRenameTries, configRenameBackoff = oldRename, oldTries, oldBackoff })
	configRename = func(oldpath, newpath string) error {
		return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: fs.ErrPermission}
	}
	configRenameTries, configRenameBackoff = 2, time.Millisecond

	dir := t.TempDir()
	if err := writeConfigFile(filepath.Join(dir, mcpConfigFile), []byte("{}\n")); err == nil {
		t.Fatal("writeConfigFile succeeded with a failing rename")
	}
	matches, err := filepath.Glob(filepath.Join(dir, mcpConfigFile+".*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Errorf("temp files left behind: %v", matches)
	}
}
