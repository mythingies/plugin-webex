package auth

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/zalando/go-keyring"
)

const (
	keyringAcctSecretPrefix = "oauth-client-secret-" //nolint:gosec // account-name prefix, not a credential
	secretFileName          = "client-secret"
)

// LoadClientSecret returns the OAuth client secret for the given clientID
// from the OS keychain, falling back to a 0600 file when the keychain is
// unavailable. Returns os.ErrNotExist if no secret is stored.
func LoadClientSecret(clientID string) (string, error) {
	if clientID == "" {
		return "", errors.New("clientID required")
	}
	acct := keyringAcctSecretPrefix + clientID

	if keyringAvailable() {
		s, err := keyring.Get(keyringService, acct)
		if err == nil {
			return s, nil
		}
		if errors.Is(err, keyring.ErrNotFound) {
			return "", os.ErrNotExist
		}
		return "", fmt.Errorf("reading keyring: %w", err)
	}

	path, err := secretFallbackPath(clientID)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path) //nolint:gosec // path built from validated clientID under UserConfigDir
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// SaveClientSecret writes the OAuth client secret to the OS keychain,
// falling back to a 0600 file with restricted ACL when the keychain is
// unavailable.
func SaveClientSecret(clientID, secret string) error {
	if clientID == "" || secret == "" {
		return errors.New("clientID and secret required")
	}
	acct := keyringAcctSecretPrefix + clientID

	if keyringAvailable() {
		if err := keyring.Set(keyringService, acct, secret); err != nil {
			return fmt.Errorf("writing keyring: %w", err)
		}
		return nil
	}

	path, err := secretFallbackPath(clientID)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(secret), 0600); err != nil {
		return err
	}
	return RestrictFileAccess(path)
}

// DeleteClientSecret removes a stored client secret.
func DeleteClientSecret(clientID string) error {
	if clientID == "" {
		return errors.New("clientID required")
	}
	acct := keyringAcctSecretPrefix + clientID

	if keyringAvailable() {
		if err := keyring.Delete(keyringService, acct); err != nil {
			if errors.Is(err, keyring.ErrNotFound) {
				return nil
			}
			return fmt.Errorf("deleting keyring entry: %w", err)
		}
		return nil
	}

	path, err := secretFallbackPath(clientID)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// secretFallbackPath builds the on-disk path for the file fallback. The
// clientID is incorporated as a hash to avoid path-injection from
// unexpected characters; we don't trust clientID is filesystem-safe.
func secretFallbackPath(clientID string) (string, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("getting config dir: %w", err)
	}
	dir := filepath.Join(configDir, configDirName)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("creating config dir: %w", err)
	}
	return filepath.Join(dir, secretFileName+"-"+sanitizeID(clientID)), nil
}

// sanitizeID maps an arbitrary clientID to a filesystem-safe slug. Webex
// client IDs are long hex-ish strings so this rarely changes them, but it
// hardens against unexpected input.
func sanitizeID(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
			out = append(out, c)
		default:
			out = append(out, '_')
		}
	}
	if len(out) > 64 {
		out = out[:64]
	}
	return string(out)
}
