package auth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestStaticProvider(t *testing.T) {
	p := NewStaticProvider("  test-token  ")
	tok, err := p.Token()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok != "test-token" {
		t.Errorf("expected trimmed token, got %q", tok)
	}
}

func TestTokenStoreSaveLoad(t *testing.T) {
	dir := t.TempDir()
	store := &TokenStore{path: filepath.Join(dir, "tokens.json")}

	tokens := &StoredTokens{
		AccessToken:           "access-123",
		RefreshToken:          "refresh-456",
		AccessTokenExpiresAt:  time.Now().Add(14 * 24 * time.Hour),
		RefreshTokenExpiresAt: time.Now().Add(90 * 24 * time.Hour),
	}

	if err := store.Save(tokens); err != nil {
		t.Fatalf("save failed: %v", err)
	}

	// Verify file permissions (Unix only).
	if info, err := os.Stat(store.path); err == nil {
		perm := info.Mode().Perm()
		// On Windows this check is less meaningful, but on Unix should be 0600.
		if perm&0077 != 0 && runtime.GOOS != "windows" {
			t.Errorf("expected restrictive permissions, got %o", perm)
		}
	}

	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("load failed: %v", err)
	}

	if loaded.AccessToken != "access-123" {
		t.Errorf("expected access-123, got %s", loaded.AccessToken)
	}
	if loaded.RefreshToken != "refresh-456" {
		t.Errorf("expected refresh-456, got %s", loaded.RefreshToken)
	}
}

func TestTokenStoreLoadMissing(t *testing.T) {
	store := &TokenStore{path: filepath.Join(t.TempDir(), "nonexistent.json")}
	_, err := store.Load()
	if err == nil {
		t.Error("expected error loading missing file")
	}
}

func TestTokenStoreDelete(t *testing.T) {
	dir := t.TempDir()
	store := &TokenStore{path: filepath.Join(dir, "tokens.json")}

	if err := store.Save(&StoredTokens{AccessToken: "x"}); err != nil {
		t.Fatalf("save failed: %v", err)
	}

	if err := store.Delete(); err != nil {
		t.Fatalf("delete failed: %v", err)
	}

	if _, err := os.Stat(store.path); !os.IsNotExist(err) {
		t.Error("expected file to be deleted")
	}
}

func TestOAuthProviderNeedsAuth(t *testing.T) {
	cfg := OAuthConfig{
		ClientID:     "test-id",
		ClientSecret: "test-secret",
	}

	// Override store to use temp dir.
	p := &OAuthProvider{
		config: cfg,
		store:  &TokenStore{path: filepath.Join(t.TempDir(), "tokens.json")},
	}

	if !p.NeedsAuth() {
		t.Error("expected NeedsAuth=true with no tokens")
	}

	// Set valid tokens.
	p.tokens = &StoredTokens{
		AccessToken:           "valid",
		RefreshToken:          "refresh",
		AccessTokenExpiresAt:  time.Now().Add(1 * time.Hour),
		RefreshTokenExpiresAt: time.Now().Add(90 * 24 * time.Hour),
	}

	if p.NeedsAuth() {
		t.Error("expected NeedsAuth=false with valid tokens")
	}
}

func TestOAuthProviderTokenValid(t *testing.T) {
	p := &OAuthProvider{
		store: &TokenStore{path: filepath.Join(t.TempDir(), "tokens.json")},
		tokens: &StoredTokens{
			AccessToken:           "my-token",
			AccessTokenExpiresAt:  time.Now().Add(1 * time.Hour),
			RefreshTokenExpiresAt: time.Now().Add(90 * 24 * time.Hour),
		},
	}

	tok, err := p.Token()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok != "my-token" {
		t.Errorf("expected my-token, got %s", tok)
	}
}

func TestOAuthProviderTokenExpired(t *testing.T) {
	p := &OAuthProvider{
		store: &TokenStore{path: filepath.Join(t.TempDir(), "tokens.json")},
		tokens: &StoredTokens{
			AccessToken:           "expired",
			AccessTokenExpiresAt:  time.Now().Add(-1 * time.Hour),
			RefreshTokenExpiresAt: time.Now().Add(-1 * time.Hour),
		},
	}

	_, err := p.Token()
	if err == nil {
		t.Error("expected error for expired tokens")
	}
}

func TestPKCEChallenge(t *testing.T) {
	verifier, err := generateCodeVerifier()
	if err != nil {
		t.Fatalf("generateCodeVerifier failed: %v", err)
	}

	if len(verifier) == 0 {
		t.Fatal("empty verifier")
	}

	challenge := generateCodeChallenge(verifier)

	// Verify: challenge = base64url(sha256(verifier))
	h := sha256.Sum256([]byte(verifier))
	expected := base64.RawURLEncoding.EncodeToString(h[:])

	if challenge != expected {
		t.Errorf("challenge mismatch: got %s, want %s", challenge, expected)
	}
}

func TestGenerateState(t *testing.T) {
	s1, err := generateState()
	if err != nil {
		t.Fatalf("generateState failed: %v", err)
	}

	s2, _ := generateState()
	if s1 == s2 {
		t.Error("expected unique states")
	}
}

func TestHandleCallbackURL(t *testing.T) {
	// Override the callback file path by pre-creating the config dir.
	origCbPath, err := CallbackFilePath()
	if err != nil {
		t.Fatalf("CallbackFilePath failed: %v", err)
	}
	defer func() { _ = os.Remove(origCbPath) }()

	err = HandleCallbackURL("wmcp://oauth-callback?code=test-code-123&state=test-state-456")
	if err != nil {
		t.Fatalf("HandleCallbackURL failed: %v", err)
	}

	data, err := os.ReadFile(origCbPath) //nolint:gosec // test file with known path
	if err != nil {
		t.Fatalf("reading callback file: %v", err)
	}

	var cb callbackData
	if err := json.Unmarshal(data, &cb); err != nil {
		t.Fatalf("parsing callback file: %v", err)
	}

	if cb.Code != "test-code-123" {
		t.Errorf("expected code test-code-123, got %s", cb.Code)
	}
	if cb.State != "test-state-456" {
		t.Errorf("expected state test-state-456, got %s", cb.State)
	}

	_ = os.Remove(origCbPath)
}

func TestHandleCallbackURLError(t *testing.T) {
	origCbPath, _ := CallbackFilePath()
	defer func() { _ = os.Remove(origCbPath) }()

	err := HandleCallbackURL("wmcp://oauth-callback?error=access_denied&state=s1")
	if err != nil {
		t.Fatalf("HandleCallbackURL failed: %v", err)
	}

	data, err := os.ReadFile(origCbPath) //nolint:gosec // test file with known path
	if err != nil {
		t.Fatalf("reading callback file: %v", err)
	}

	var cb callbackData
	if err := json.Unmarshal(data, &cb); err != nil {
		t.Fatalf("parsing callback file: %v", err)
	}

	if cb.Error != "access_denied" {
		t.Errorf("expected error access_denied, got %s", cb.Error)
	}

	_ = os.Remove(origCbPath)
}

func TestHandleCallbackURLBadScheme(t *testing.T) {
	err := HandleCallbackURL("https://evil.com/callback?code=stolen")
	if err == nil {
		t.Error("expected error for non-wmcp scheme")
	}
}

func TestSanitizeCallbackURL(t *testing.T) {
	code, state, errMsg, err := sanitizeCallbackURL("wmcp://oauth-callback?code=abc&state=xyz")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if code != "abc" {
		t.Errorf("expected code abc, got %s", code)
	}
	if state != "xyz" {
		t.Errorf("expected state xyz, got %s", state)
	}
	if errMsg != "" {
		t.Errorf("expected empty error, got %s", errMsg)
	}
}

func TestSanitizeCallbackURLNoCode(t *testing.T) {
	_, _, _, err := sanitizeCallbackURL("wmcp://oauth-callback?state=xyz")
	if err == nil {
		t.Error("expected error when no code or error present")
	}
}

func TestStaticProviderEmpty(t *testing.T) {
	p := NewStaticProvider("  ")
	_, err := p.Token()
	if err == nil {
		t.Error("expected error for empty/whitespace-only token")
	}
}

func TestHandleCallbackURLBadHost(t *testing.T) {
	err := HandleCallbackURL("wmcp://evil.com/callback?code=stolen&state=s")
	if err == nil {
		t.Error("expected error for wrong host")
	}
}

func TestHandleCallbackURLLongParam(t *testing.T) {
	longCode := string(make([]byte, 5000))
	err := HandleCallbackURL("wmcp://oauth-callback?code=" + longCode + "&state=s")
	if err == nil {
		t.Error("expected error for oversized parameter")
	}
}

func TestIsLocalhostURI(t *testing.T) {
	tests := []struct {
		uri  string
		want bool
	}{
		{"http://localhost:19876/callback", true},
		{"http://127.0.0.1:8080/cb", true},
		{"https://evil.com/callback", false},
		{"wmcp://oauth-callback", false},
	}
	for _, tc := range tests {
		if got := isLocalhostURI(tc.uri); got != tc.want {
			t.Errorf("isLocalhostURI(%q) = %v, want %v", tc.uri, got, tc.want)
		}
	}
}
