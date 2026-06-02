package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/zalando/go-keyring"
)

const (
	configDirName = "webex-mcp"
	tokenFileName = "tokens.json"

	// Keychain service used for all webex-mcp secrets. Account names
	// distinguish entries within the service.
	keyringService = "webex-mcp"
	keyringAcctTok = "oauth-tokens"
)

// StoredTokens holds persisted OAuth tokens.
type StoredTokens struct {
	AccessToken           string    `json:"access_token"`
	RefreshToken          string    `json:"refresh_token"`
	AccessTokenExpiresAt  time.Time `json:"access_token_expires_at"`
	RefreshTokenExpiresAt time.Time `json:"refresh_token_expires_at"`
}

// TokenStore handles persistent OAuth token storage. It prefers the OS
// keychain (Windows Credential Manager, macOS Keychain, Linux Secret
// Service) and falls back to a 0600 file when no keychain backend is
// available — common on headless Linux, WSL, and minimal containers.
type TokenStore struct {
	path       string // file fallback path
	useKeyring bool   // chosen at construction by probing the backend
}

// NewTokenStore creates a store backed by the OS keychain when available,
// otherwise a file at the user's config directory.
func NewTokenStore() (*TokenStore, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return nil, fmt.Errorf("getting config dir: %w", err)
	}

	dir := filepath.Join(configDir, configDirName)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("creating config dir: %w", err)
	}

	s := &TokenStore{
		path:       filepath.Join(dir, tokenFileName),
		useKeyring: keyringAvailable(),
	}
	return s, nil
}

// Load reads stored tokens from the keychain or fallback file.
func (s *TokenStore) Load() (*StoredTokens, error) {
	if s.useKeyring {
		raw, err := keyring.Get(keyringService, keyringAcctTok)
		if err == nil {
			var tokens StoredTokens
			if err := json.Unmarshal([]byte(raw), &tokens); err != nil {
				return nil, fmt.Errorf("parsing keyring tokens: %w", err)
			}
			return &tokens, nil
		}
		if errors.Is(err, keyring.ErrNotFound) {
			return nil, os.ErrNotExist
		}
		return nil, fmt.Errorf("reading keyring: %w", err)
	}

	data, err := os.ReadFile(s.path)
	if err != nil {
		return nil, err
	}
	var tokens StoredTokens
	if err := json.Unmarshal(data, &tokens); err != nil {
		return nil, fmt.Errorf("parsing token file: %w", err)
	}
	return &tokens, nil
}

// Save writes tokens to the keychain or fallback file with restricted
// permissions. On Windows the file fallback path is rarely used (Credential
// Manager is always available), but if it is, NTFS ACLs are explicitly set
// via icacls because Go's 0600 mode has no effect on NTFS.
func (s *TokenStore) Save(tokens *StoredTokens) error {
	data, err := json.Marshal(tokens) //nolint:gosec // token storage is the purpose of this function
	if err != nil {
		return fmt.Errorf("marshaling tokens: %w", err)
	}

	if s.useKeyring {
		if err := keyring.Set(keyringService, keyringAcctTok, string(data)); err != nil {
			return fmt.Errorf("writing keyring: %w", err)
		}
		return nil
	}

	if err := os.WriteFile(s.path, data, 0600); err != nil {
		return err
	}
	return RestrictFileAccess(s.path)
}

// Delete removes stored tokens.
func (s *TokenStore) Delete() error {
	if s.useKeyring {
		if err := keyring.Delete(keyringService, keyringAcctTok); err != nil {
			if errors.Is(err, keyring.ErrNotFound) {
				return os.ErrNotExist
			}
			return fmt.Errorf("deleting keyring entry: %w", err)
		}
		return nil
	}
	return os.Remove(s.path)
}

// Path returns the file fallback path. Empty result indicates the store is
// keychain-backed and has no on-disk path.
func (s *TokenStore) Path() string {
	if s.useKeyring {
		return ""
	}
	return s.path
}

// UsingKeyring reports whether the store reads/writes from the OS keychain.
func (s *TokenStore) UsingKeyring() bool {
	return s.useKeyring
}

// keyringAvailable probes the OS keychain by writing and deleting a sentinel
// value. Returns false on Linux when no Secret Service backend is running.
// This is the only reliable way to detect availability across platforms,
// since go-keyring's errors aren't sentinel values for "no backend".
func keyringAvailable() bool {
	const probeAcct = "_probe"
	if err := keyring.Set(keyringService, probeAcct, "ok"); err != nil {
		return false
	}
	_ = keyring.Delete(keyringService, probeAcct)
	return true
}
