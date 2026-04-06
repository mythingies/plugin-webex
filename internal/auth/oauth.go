package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

const (
	authEndpoint  = "https://webexapis.com/v1/authorize"
	tokenEndpoint = "https://webexapis.com/v1/access_token" //nolint:gosec // URL, not a credential

	// DefaultScopes covers the APIs this plugin uses.
	// Override with WEBEX_SCOPES env var, or use "spark:all" for full access.
	DefaultScopes = "spark:messages_read spark:messages_write spark:rooms_read spark:memberships_read spark:people_read spark:kms meeting:schedules_read meeting:transcripts_read"

	// refreshBuffer is how early before expiry we refresh.
	refreshBuffer = 5 * time.Minute

	// callbackPollInterval is how often we check for the callback file.
	callbackPollInterval = 500 * time.Millisecond

	// callbackFileName is the file the callback handler writes to.
	callbackFileName = "oauth-callback.json"
)

// OAuthConfig holds the OAuth integration credentials.
type OAuthConfig struct {
	ClientID     string
	ClientSecret string
	RedirectURI  string // Optional; defaults to wmcp://oauth-callback.
	Scopes       string // Optional; defaults to DefaultScopes.
}

// OAuthProvider manages OAuth tokens with automatic refresh.
type OAuthProvider struct {
	config OAuthConfig
	store  *TokenStore

	mu     sync.Mutex
	tokens *StoredTokens

	httpClient *http.Client
}

// NewOAuthProvider creates a provider that handles the full OAuth lifecycle.
// It loads any previously stored tokens automatically.
func NewOAuthProvider(config OAuthConfig) (*OAuthProvider, error) {
	store, err := NewTokenStore()
	if err != nil {
		return nil, err
	}

	if config.Scopes == "" {
		config.Scopes = DefaultScopes
	}

	p := &OAuthProvider{
		config:     config,
		store:      store,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}

	// Try to load existing tokens.
	tokens, err := store.Load()
	if err == nil {
		p.tokens = tokens
		slog.Info("loaded stored OAuth tokens", "expires_at", tokens.AccessTokenExpiresAt.Format(time.RFC3339))
	}

	return p, nil
}

// Token returns a valid access token, refreshing if needed.
func (p *OAuthProvider) Token() (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.tokens == nil {
		return "", fmt.Errorf("not authenticated; run OAuth authorization flow first")
	}

	// Access token still valid.
	if time.Now().Before(p.tokens.AccessTokenExpiresAt.Add(-refreshBuffer)) {
		return p.tokens.AccessToken, nil
	}

	// Try refresh.
	if p.tokens.RefreshToken != "" && time.Now().Before(p.tokens.RefreshTokenExpiresAt) {
		if err := p.refresh(); err != nil {
			return "", fmt.Errorf("token refresh failed: %w", err)
		}
		return p.tokens.AccessToken, nil
	}

	return "", fmt.Errorf("tokens expired; re-authentication required")
}

// NeedsAuth returns true if the user must go through the browser OAuth flow.
func (p *OAuthProvider) NeedsAuth() bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.tokens == nil {
		return true
	}

	// Refresh token still valid — can auto-refresh.
	if p.tokens.RefreshToken != "" && time.Now().Before(p.tokens.RefreshTokenExpiresAt) {
		return false
	}

	// Access token still valid (no refresh needed).
	if time.Now().Before(p.tokens.AccessTokenExpiresAt.Add(-refreshBuffer)) {
		return false
	}

	return true
}

// callbackData is written to the callback file by the protocol handler process.
type callbackData struct {
	Code  string `json:"code"`
	State string `json:"state"`
	Error string `json:"error,omitempty"`
}

// CallbackFilePath returns the path where the callback handler writes the auth code.
func CallbackFilePath() (string, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("getting config dir: %w", err)
	}
	return filepath.Join(configDir, configDirName, callbackFileName), nil
}

// WriteCallbackFile is called by the --oauth-callback handler to deliver the
// authorization code to the waiting Authorize() call via the filesystem.
func WriteCallbackFile(code, state, errMsg string) error {
	path, err := CallbackFilePath()
	if err != nil {
		return err
	}

	// Ensure the directory exists.
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("creating callback dir: %w", err)
	}

	data := callbackData{Code: code, State: state, Error: errMsg}
	b, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("marshaling callback data: %w", err)
	}

	return os.WriteFile(path, b, 0600)
}

// Authorize runs the interactive OAuth authorization code flow with PKCE.
// It opens the browser, then polls for the callback file written by the
// wmcp:// protocol handler. No HTTP listener is used.
func (p *OAuthProvider) Authorize(ctx context.Context) error {
	verifier, err := generateCodeVerifier()
	if err != nil {
		return fmt.Errorf("generating PKCE verifier: %w", err)
	}
	challenge := generateCodeChallenge(verifier)

	state, err := generateState()
	if err != nil {
		return fmt.Errorf("generating state: %w", err)
	}

	redirectURI := p.config.RedirectURI
	if redirectURI == "" {
		redirectURI = ProtocolCallbackURL
	}
	// Validate redirect URI to prevent open redirect.
	if redirectURI != ProtocolCallbackURL && !isLocalhostURI(redirectURI) {
		return fmt.Errorf("redirect URI must be %s or a localhost URL, got: %s", ProtocolCallbackURL, redirectURI)
	}

	// Clean up any stale callback file before starting.
	cbPath, err := CallbackFilePath()
	if err != nil {
		return fmt.Errorf("resolving callback path: %w", err)
	}
	_ = os.Remove(cbPath)

	// Build authorization URL.
	params := url.Values{}
	params.Set("response_type", "code")
	params.Set("client_id", p.config.ClientID)
	params.Set("redirect_uri", redirectURI)
	params.Set("scope", p.config.Scopes)
	params.Set("state", state)
	params.Set("code_challenge", challenge)
	params.Set("code_challenge_method", "S256")

	authURL := authEndpoint + "?" + params.Encode()

	fmt.Fprintf(os.Stderr, "\nOpening browser for Webex authorization...\n")
	fmt.Fprintf(os.Stderr, "If the browser doesn't open, visit this URL:\n\n  %s\n\n", authURL)

	_ = openBrowser(authURL)

	// Poll for the callback file written by `webex-mcp --oauth-callback`.
	ticker := time.NewTicker(callbackPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			// Open the file directly — stat and read from the same fd to avoid
			// TOCTOU race (file could be swapped between stat and read).
			f, err := os.Open(cbPath) //nolint:gosec // path from UserConfigDir, not user input
			if err != nil {
				continue // file not yet written
			}

			// Check permissions on the open fd (not the path) to avoid TOCTOU.
			info, err := f.Stat()
			if err != nil {
				_ = f.Close()
				continue
			}
			if runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
				_ = f.Close()
				_ = os.Remove(cbPath)
				return fmt.Errorf("callback file has insecure permissions (%o); possible tampering", info.Mode().Perm())
			}

			data, err := io.ReadAll(f)
			_ = f.Close()
			if err != nil {
				continue
			}

			// Clean up immediately.
			_ = os.Remove(cbPath)

			var cb callbackData
			if err := json.Unmarshal(data, &cb); err != nil {
				return fmt.Errorf("parsing callback file: %w", err)
			}

			if cb.Error != "" {
				return fmt.Errorf("authorization denied: %s", cb.Error)
			}

			if cb.State != state {
				return fmt.Errorf("OAuth state mismatch (possible CSRF)")
			}

			if cb.Code == "" {
				return fmt.Errorf("no authorization code in callback")
			}

			return p.exchangeCode(cb.Code, redirectURI, verifier)
		}
	}
}

// Logout removes stored tokens.
func (p *OAuthProvider) Logout() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.tokens = nil
	return p.store.Delete()
}

func (p *OAuthProvider) exchangeCode(code, redirectURI, codeVerifier string) error {
	data := url.Values{}
	data.Set("grant_type", "authorization_code")
	data.Set("client_id", p.config.ClientID)
	data.Set("client_secret", p.config.ClientSecret)
	data.Set("code", code)
	data.Set("redirect_uri", redirectURI)
	data.Set("code_verifier", codeVerifier)

	return p.doTokenRequest(data)
}

func (p *OAuthProvider) refresh() error {
	data := url.Values{}
	data.Set("grant_type", "refresh_token")
	data.Set("client_id", p.config.ClientID)
	data.Set("client_secret", p.config.ClientSecret)
	data.Set("refresh_token", p.tokens.RefreshToken)

	if err := p.doTokenRequest(data); err != nil {
		return err
	}

	slog.Info("OAuth token refreshed successfully")
	return nil
}

func (p *OAuthProvider) doTokenRequest(data url.Values) error {
	resp, err := p.httpClient.PostForm(tokenEndpoint, data)
	if err != nil {
		return fmt.Errorf("token request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("reading token response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		slog.Debug("token endpoint error", "status", resp.StatusCode, "body_size", len(body))
		return fmt.Errorf("token endpoint returned status %d", resp.StatusCode)
	}

	var tokenResp struct {
		AccessToken           string `json:"access_token"`
		ExpiresIn             int64  `json:"expires_in"`
		RefreshToken          string `json:"refresh_token"`
		RefreshTokenExpiresIn int64  `json:"refresh_token_expires_in"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return fmt.Errorf("parsing token response: %w", err)
	}

	if tokenResp.AccessToken == "" {
		return fmt.Errorf("token response missing access_token")
	}
	if tokenResp.ExpiresIn <= 0 {
		return fmt.Errorf("token response has invalid expires_in: %d", tokenResp.ExpiresIn)
	}

	now := time.Now()
	p.tokens = &StoredTokens{
		AccessToken:           tokenResp.AccessToken,
		RefreshToken:          tokenResp.RefreshToken,
		AccessTokenExpiresAt:  now.Add(time.Duration(tokenResp.ExpiresIn) * time.Second),
		RefreshTokenExpiresAt: now.Add(time.Duration(tokenResp.RefreshTokenExpiresIn) * time.Second),
	}

	if err := p.store.Save(p.tokens); err != nil {
		slog.Warn("failed to persist tokens", "error", err)
	}

	return nil
}

func generateCodeVerifier() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func generateCodeChallenge(verifier string) string {
	h := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

func generateState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// isLocalhostURI checks if a URI points to localhost.
func isLocalhostURI(uri string) bool {
	parsed, err := url.Parse(uri)
	if err != nil {
		return false
	}
	host := parsed.Hostname()
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func openBrowser(rawURL string) error {
	switch runtime.GOOS {
	case "linux":
		return exec.Command("xdg-open", rawURL).Start() //nolint:gosec // opens validated auth URL in browser
	case "darwin":
		return exec.Command("open", rawURL).Start() //nolint:gosec // opens validated auth URL in browser
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", rawURL).Start() //nolint:gosec // opens validated auth URL in browser
	default:
		return fmt.Errorf("unsupported platform")
	}
}

// sanitizeCallbackURL extracts code and state from a wmcp:// callback URL.
func sanitizeCallbackURL(rawURL string) (code, state, errMsg string, err error) {
	parsed, parseErr := url.Parse(rawURL)
	if parseErr != nil {
		return "", "", "", fmt.Errorf("parsing callback URL: %w", parseErr)
	}

	code = parsed.Query().Get("code")
	state = parsed.Query().Get("state")
	errMsg = parsed.Query().Get("error")

	if code == "" && errMsg == "" {
		return "", "", "", fmt.Errorf("no code or error in callback URL")
	}

	return code, state, errMsg, nil
}
