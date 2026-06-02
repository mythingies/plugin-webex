package auth

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zalando/go-keyring"
)

func TestTokenStoreKeyringSaveLoad(t *testing.T) {
	keyring.MockInit()

	s := &TokenStore{path: filepath.Join(t.TempDir(), "tokens.json"), useKeyring: true}
	tokens := &StoredTokens{
		AccessToken:           "kc-access",
		RefreshToken:          "kc-refresh",
		AccessTokenExpiresAt:  time.Now().Add(1 * time.Hour),
		RefreshTokenExpiresAt: time.Now().Add(90 * 24 * time.Hour),
	}

	if err := s.Save(tokens); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded, err := s.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.AccessToken != "kc-access" || loaded.RefreshToken != "kc-refresh" {
		t.Errorf("unexpected tokens: %+v", loaded)
	}

	// Path() should be empty for keychain-backed stores.
	if got := s.Path(); got != "" {
		t.Errorf("expected empty Path() for keychain store, got %q", got)
	}

	// Delete should remove the entry.
	if err := s.Delete(); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.Load(); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected os.ErrNotExist after delete, got %v", err)
	}
}

func TestTokenStoreKeyringLoadMissing(t *testing.T) {
	keyring.MockInit()
	s := &TokenStore{useKeyring: true}
	_, err := s.Load()
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected os.ErrNotExist, got %v", err)
	}
}

func TestMigrateTokensFromFile(t *testing.T) {
	keyring.MockInit()

	dir := t.TempDir()
	path := filepath.Join(dir, "tokens.json")
	legacy := []byte(`{"access_token":"legacy-a","refresh_token":"legacy-r","access_token_expires_at":"2030-01-01T00:00:00Z","refresh_token_expires_at":"2030-06-01T00:00:00Z"}`)
	if err := os.WriteFile(path, legacy, 0600); err != nil {
		t.Fatal(err)
	}

	s := &TokenStore{path: path, useKeyring: true}
	migrated, err := s.MigrateTokensFromFile()
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if !migrated {
		t.Fatal("expected migrated=true")
	}

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expected legacy file to be removed; got err=%v", err)
	}

	loaded, err := s.Load()
	if err != nil {
		t.Fatalf("load after migrate: %v", err)
	}
	if loaded.AccessToken != "legacy-a" {
		t.Errorf("expected migrated token, got %q", loaded.AccessToken)
	}
}

func TestMigrateTokensNoFile(t *testing.T) {
	keyring.MockInit()

	s := &TokenStore{path: filepath.Join(t.TempDir(), "tokens.json"), useKeyring: true}
	migrated, err := s.MigrateTokensFromFile()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if migrated {
		t.Error("expected migrated=false when no legacy file exists")
	}
}

func TestMigrateTokensFileFallback(t *testing.T) {
	// When keychain is unavailable (useKeyring=false) migration is a no-op.
	s := &TokenStore{path: filepath.Join(t.TempDir(), "tokens.json"), useKeyring: false}
	migrated, err := s.MigrateTokensFromFile()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if migrated {
		t.Error("expected migrated=false in file-fallback mode")
	}
}

func TestClientSecretKeyring(t *testing.T) {
	keyring.MockInit()

	const clientID = "test-client-id-123"
	if err := SaveClientSecret(clientID, "topsecret"); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err := LoadClientSecret(clientID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got != "topsecret" {
		t.Errorf("expected topsecret, got %q", got)
	}

	if err := DeleteClientSecret(clientID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	if _, err := LoadClientSecret(clientID); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected os.ErrNotExist after delete, got %v", err)
	}
}

func TestClientSecretMigrateFromEnv(t *testing.T) {
	keyring.MockInit()

	const clientID = "migrate-id"

	migrated, err := MigrateClientSecretFromEnv(clientID, "from-env")
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if !migrated {
		t.Fatal("expected migrated=true on first call")
	}

	// Second call is a no-op (keychain entry already exists).
	migrated, err = MigrateClientSecretFromEnv(clientID, "newer-env")
	if err != nil {
		t.Fatalf("migrate 2: %v", err)
	}
	if migrated {
		t.Error("expected migrated=false on second call")
	}

	// Stored value is the original — env override happens at read time
	// in main, not at migrate time.
	got, _ := LoadClientSecret(clientID)
	if got != "from-env" {
		t.Errorf("expected from-env, got %q", got)
	}

	_ = DeleteClientSecret(clientID)
}

func TestClientSecretMigrateEmpty(t *testing.T) {
	keyring.MockInit()

	migrated, err := MigrateClientSecretFromEnv("", "secret")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if migrated {
		t.Error("expected migrated=false with empty clientID")
	}

	migrated, err = MigrateClientSecretFromEnv("id", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if migrated {
		t.Error("expected migrated=false with empty secret")
	}
}

func TestSanitizeID(t *testing.T) {
	tests := map[string]string{
		"abc123":        "abc123",
		"abc-123_DEF":   "abc-123_DEF",
		"abc/../../etc": "abc_______etc",
		"abc\x00null":   "abc_null",
		"with spaces":   "with_spaces",
	}
	for in, want := range tests {
		if got := sanitizeID(in); got != want {
			t.Errorf("sanitizeID(%q) = %q, want %q", in, got, want)
		}
	}
}
