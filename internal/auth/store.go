package auth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	configDirName = "webex-mcp"
	tokenFileName = "tokens.json"
)

// StoredTokens holds persisted OAuth tokens.
type StoredTokens struct {
	AccessToken           string    `json:"access_token"`
	RefreshToken          string    `json:"refresh_token"`
	AccessTokenExpiresAt  time.Time `json:"access_token_expires_at"`
	RefreshTokenExpiresAt time.Time `json:"refresh_token_expires_at"`
}

// TokenStore handles persistent token storage.
type TokenStore struct {
	path string
}

// NewTokenStore creates a store at the user's config directory.
func NewTokenStore() (*TokenStore, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return nil, fmt.Errorf("getting config dir: %w", err)
	}

	dir := filepath.Join(configDir, configDirName)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("creating config dir: %w", err)
	}

	return &TokenStore{path: filepath.Join(dir, tokenFileName)}, nil
}

// Load reads stored tokens from disk.
func (s *TokenStore) Load() (*StoredTokens, error) {
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

// Save writes tokens to disk with restricted permissions.
func (s *TokenStore) Save(tokens *StoredTokens) error {
	data, err := json.MarshalIndent(tokens, "", "  ") //nolint:gosec // token storage is the purpose of this function
	if err != nil {
		return fmt.Errorf("marshaling tokens: %w", err)
	}
	return os.WriteFile(s.path, data, 0600)
}

// Delete removes the stored token file.
func (s *TokenStore) Delete() error {
	return os.Remove(s.path)
}

// Path returns the token file path.
func (s *TokenStore) Path() string {
	return s.path
}
