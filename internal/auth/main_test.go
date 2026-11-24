package auth

import (
	"os"
	"testing"

	"github.com/zalando/go-keyring"
)

// TestMain keeps every test off the real OS keychain. go-keyring is pointed
// at its in-memory mock, and file-backed stores see no keychain unless a
// test opts in with useTokenKeychain. Without this, a file-mode test whose
// store goes missing would probe the developer's keychain and could adopt
// a real session from it.
func TestMain(m *testing.M) {
	keyring.MockInit()
	tokenKeyringProbe = func() bool { return false }
	os.Exit(m.Run())
}

// useTokenKeychain gives the test a fresh mock keychain and makes the token
// keychain probe report it available.
func useTokenKeychain(t *testing.T) {
	t.Helper()
	keyring.MockInit()
	old := tokenKeyringProbe
	tokenKeyringProbe = func() bool { return true }
	t.Cleanup(func() { tokenKeyringProbe = old })
}
