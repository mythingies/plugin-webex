package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// MigrateTokensFromFile imports a pre-v0.8.0 tokens.json file into the
// keychain and removes the on-disk copy. Returns true if a migration
// happened. Safe to call when no legacy file exists; it returns
// (false, nil) in that case.
//
// On systems without a keychain backend (Linux without Secret Service),
// migration is a no-op: the file already lives at the same path the
// fallback uses, so it's already correctly placed.
func (s *TokenStore) MigrateTokensFromFile() (bool, error) {
	if !s.useKeyring {
		return false, nil
	}
	if s.path == "" {
		return false, nil
	}

	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("reading legacy tokens file: %w", err)
	}

	var tokens StoredTokens
	if err := json.Unmarshal(data, &tokens); err != nil {
		return false, fmt.Errorf("parsing legacy tokens file: %w", err)
	}

	if err := s.Save(&tokens); err != nil {
		return false, fmt.Errorf("writing tokens to keychain: %w", err)
	}

	if err := os.Remove(s.path); err != nil {
		return true, fmt.Errorf("removing legacy tokens file (already migrated): %w", err)
	}
	return true, nil
}

// MigrateClientSecretFromEnv writes the client secret from the environment
// into the keychain when one isn't already stored for this clientID.
// Returns true when a new entry was written. The env var is left in place;
// removing it from the user's shell or .mcp.json is the user's job.
func MigrateClientSecretFromEnv(clientID, envSecret string) (bool, error) {
	if clientID == "" || envSecret == "" {
		return false, nil
	}
	if _, err := LoadClientSecret(clientID); err == nil {
		return false, nil
	}
	if err := SaveClientSecret(clientID, envSecret); err != nil {
		return false, err
	}
	return true, nil
}
