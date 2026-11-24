package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	//
	// spark:all is required (not just the granular spark:messages_* /
	// spark:rooms_* scopes) because the real-time WebSocket listener
	// registers a Mercury device with WDM (wdm-a.wbx2.com), and that
	// endpoint rejects granular-scoped tokens with HTTP 403. spark:all is
	// the only public OAuth scope that grants it — matching what a PAT
	// carries. The meeting:* scopes are a separate family that spark:all
	// does NOT cover, so they're listed explicitly.
	// Override with the WEBEX_SCOPES env var.
	DefaultScopes = "spark:all meeting:schedules_read meeting:transcripts_read"

	// refreshBuffer is how early before expiry we refresh.
	refreshBuffer = 5 * time.Minute

	// callbackPollInterval is how often we check for the callback file.
	callbackPollInterval = 500 * time.Millisecond

	// callbackFileName is the file the callback handler writes to.
	callbackFileName = "oauth-callback.json"
)

// ErrSignedOut means the stored OAuth session this process was using has
// gone: another process ran --logout (the store is empty) or --switch (the
// store holds a different session). The process stops using its in-memory
// tokens rather than act as, or save over, the other account. Restarting
// picks up whatever session is stored now.
var ErrSignedOut = errors.New("the signed-in Webex account changed or signed out; restart this process")

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

	// mu serialises token access within this process. Refreshes across
	// processes are serialised by the refresh lock file (refreshlock.go).
	mu     sync.Mutex
	tokens *StoredTokens

	// sessionID identifies the authorization the in-memory tokens belong
	// to (StoredTokens.SessionID). Only stored tokens with the same session
	// ID are adopted. Empty for legacy sessions saved without one.
	sessionID string

	// persisted reports that the in-memory session has been in the store
	// (loaded from it or saved to it). Only then does an empty store mean
	// another process signed out, rather than that our own save failed.
	persisted bool

	// signedOut is set once another process signed out or switched
	// accounts. Token returns ErrSignedOut until a new authorization.
	signedOut bool

	httpClient *http.Client

	// tokenURL is the OAuth token endpoint. Empty means tokenEndpoint;
	// tests point it at an httptest server.
	tokenURL string
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
		tokenURL:   tokenEndpoint,
	}

	// Migrate pre-v0.8.0 tokens.json into the keychain if present.
	if migrated, err := store.MigrateTokensFromFile(); err != nil {
		slog.Warn("token migration failed; continuing with current state", "error", err)
	} else if migrated {
		slog.Info("migrated tokens.json to OS keychain (legacy file removed)")
	}

	// Migrate WEBEX_CLIENT_SECRET from env into the keychain (only when
	// no entry exists yet for this clientID). This makes future launches
	// work even if the user later removes the env var.
	if migrated, err := MigrateClientSecretFromEnv(config.ClientID, config.ClientSecret); err != nil {
		slog.Warn("client-secret migration failed; continuing", "error", err)
	} else if migrated {
		slog.Info("stored OAuth client secret in OS keychain (env override still applies if set)")
	}

	// Try to load existing tokens.
	tokens, err := store.Load()
	if err == nil {
		p.setSession(tokens, true)
		slog.Info("loaded stored OAuth tokens", "expires_at", tokens.AccessTokenExpiresAt.Format(time.RFC3339))
	}

	return p, nil
}

// Token returns a valid access token, refreshing if needed.
func (p *OAuthProvider) Token() (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.tokens == nil {
		if p.signedOut {
			return "", fmt.Errorf("not authenticated: %w", ErrSignedOut)
		}
		return "", fmt.Errorf("not authenticated; run OAuth authorization flow first")
	}

	// Access token still valid.
	if accessTokenFresh(p.tokens) {
		return p.tokens.AccessToken, nil
	}

	// The token is stale. Other processes (other Claude Code sessions,
	// webex-mcp --review) share the stored tokens, so take the cross-process
	// lock and look at what is stored before refreshing ourselves.
	release, _ := acquireRefreshLock(p.refreshLockDir())
	defer release()

	if err := p.adoptStoredTokens(); err != nil {
		return "", err
	}
	if accessTokenFresh(p.tokens) {
		// Another process refreshed while we waited or since we loaded.
		slog.Debug("adopted OAuth token refreshed by another process")
		return p.tokens.AccessToken, nil
	}

	// Try refresh.
	if refreshTokenUsable(p.tokens) {
		if err := p.refresh(); err != nil {
			if errors.Is(err, ErrSignedOut) {
				return "", err
			}
			return "", fmt.Errorf("token refresh failed: %w", err)
		}
		return p.tokens.AccessToken, nil
	}

	return "", fmt.Errorf("tokens expired; re-authentication required")
}

// adoptStoredTokens reloads the token store and replaces the in-memory
// tokens when the stored ones are newer: either the stored access token is
// still fresh, or the stored refresh token is usable (so a refresh uses the
// newest refresh token, which matters if the endpoint rotated it). The
// in-memory tokens are kept when the store is unreadable.
//
// Only a stored session with this provider's session ID and OAuth client
// is adopted. A different one means another process ran --switch (or
// --setup --force, possibly with another client ID), and an empty store
// means it ran --logout. Either way this process is signed out: it drops
// its tokens, does not refresh or save them back, and gets an error
// wrapping ErrSignedOut. Legacy sessions with no session ID on either side
// still adopt each other.
//
// Callers must hold p.mu and should hold the refresh lock.
func (p *OAuthProvider) adoptStoredTokens() error {
	if p.store == nil {
		return nil
	}
	stored, err := p.loadStoredSession()
	if err != nil || stored == nil {
		return err
	}
	if accessTokenFresh(stored) || refreshTokenUsable(stored) {
		p.setSession(stored, true)
	}
	return nil
}

// loadStoredSession reloads the store and checks that it still holds this
// provider's session. It returns the stored tokens, or nil when the store
// is unreadable or this session never reached it (the caller carries on
// with its in-memory tokens). When the stored session was removed or
// replaced it signs out and returns an error wrapping ErrSignedOut.
//
// Callers must hold p.mu and should hold the refresh lock.
func (p *OAuthProvider) loadStoredSession() (*StoredTokens, error) {
	stored, err := p.loadStore()
	if errors.Is(err, os.ErrNotExist) {
		if !p.persisted {
			// Our session never reached the store (its save failed), so
			// an empty store says nothing about a sign-out.
			return nil, nil
		}
		slog.Warn("stored OAuth tokens were removed by another process; signing out")
		p.signOut()
		return nil, fmt.Errorf("stored OAuth session removed: %w", ErrSignedOut)
	}
	if err != nil || stored == nil {
		if err != nil {
			slog.Warn("could not reload stored OAuth tokens; using in-memory tokens", "error", err)
		}
		return nil, nil
	}
	if p.storedSessionReplaced(stored) {
		slog.Warn("stored OAuth tokens belong to a different session or client; signing out")
		p.signOut()
		return nil, fmt.Errorf("stored OAuth session replaced: %w", ErrSignedOut)
	}
	return stored, nil
}

// loadStore reads the token store. A file-backed store whose file has gone
// may have been moved rather than deleted: a process that can reach the OS
// keychain migrates tokens.json into it (MigrateTokensFromFile) and removes
// the file. So when the file is missing and a keychain is reachable now,
// tokens stored there for this same session are used, and this provider's
// store switches to the keychain. Anything else still reports
// os.ErrNotExist.
//
// Callers must hold p.mu.
func (p *OAuthProvider) loadStore() (*StoredTokens, error) {
	stored, err := p.store.Load()
	if !errors.Is(err, os.ErrNotExist) || p.store.useKeyring || !p.persisted {
		return stored, err
	}
	if !tokenKeyringProbe() {
		return nil, err
	}
	kc := &TokenStore{path: p.store.path, useKeyring: true}
	moved, kerr := kc.Load()
	if kerr != nil || moved == nil || !p.isMigratedSession(moved) {
		return nil, err
	}
	slog.Info("stored OAuth tokens were moved to the OS keychain by another process; using the keychain")
	// Replace the store rather than mutate it: the old value may be shared.
	p.store = kc
	return moved, nil
}

// isMigratedSession reports whether keychain tokens kc are this provider's
// session, copied there from the fallback file. A session ID is random, so
// a match on it is enough. Legacy sessions have none, and an unrelated
// legacy entry may be sitting in the keychain, so they must also carry the
// refresh or access token this process holds, as a byte-for-byte copy of
// the file would. Callers must hold p.mu.
func (p *OAuthProvider) isMigratedSession(kc *StoredTokens) bool {
	if p.storedSessionReplaced(kc) {
		return false
	}
	if p.sessionID != "" {
		return true
	}
	if p.tokens == nil {
		return false
	}
	return (kc.RefreshToken != "" && kc.RefreshToken == p.tokens.RefreshToken) ||
		(kc.AccessToken != "" && kc.AccessToken == p.tokens.AccessToken)
}

// storedSessionReplaced reports whether stored is not this provider's
// session: it has a different session ID, or it was issued to a different
// OAuth client. Tokens saved before client IDs were recorded (empty
// ClientID) are judged by session ID alone. Callers must hold p.mu.
func (p *OAuthProvider) storedSessionReplaced(stored *StoredTokens) bool {
	if stored.ClientID != "" && stored.ClientID != p.config.ClientID &&
		(p.config.ClientID != "" || stored.SessionID != "") {
		return true
	}
	return stored.SessionID != p.sessionID
}

// setSession makes t the in-memory session. persisted reports whether t is
// known to be in the store. Callers must hold p.mu or own p exclusively.
func (p *OAuthProvider) setSession(t *StoredTokens, persisted bool) {
	p.tokens = t
	p.sessionID = t.SessionID
	p.persisted = persisted
	p.signedOut = false
}

// signOut drops the in-memory session after another process signed out or
// switched accounts. Callers must hold p.mu.
func (p *OAuthProvider) signOut() {
	p.tokens = nil
	p.sessionID = ""
	p.persisted = false
	p.signedOut = true
}

// refreshLockDir is the directory for the cross-process refresh lock: the
// directory of the token store's fallback file, which NewTokenStore creates.
// Empty when the store has no path (then the refresh runs unlocked).
func (p *OAuthProvider) refreshLockDir() string {
	if p.store == nil {
		return ""
	}
	return p.store.lockDir()
}

// accessTokenFresh reports whether t's access token is valid for longer than
// refreshBuffer.
func accessTokenFresh(t *StoredTokens) bool {
	return t != nil && t.AccessToken != "" && time.Now().Before(t.AccessTokenExpiresAt.Add(-refreshBuffer))
}

// refreshTokenUsable reports whether t has an unexpired refresh token.
func refreshTokenUsable(t *StoredTokens) bool {
	return t != nil && t.RefreshToken != "" && time.Now().Before(t.RefreshTokenExpiresAt)
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
	return p.AuthorizeWith(ctx, openBrowser)
}

// AuthorizeWith is Authorize, but hands the authorization URL to open instead
// of launching the system browser. The setup wizard uses it to load the URL
// into a popup it can close once the callback arrives.
func (p *OAuthProvider) AuthorizeWith(ctx context.Context, open func(authURL string) error) error {
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

	_ = open(authURL)

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
	p.sessionID = ""
	p.persisted = false
	p.signedOut = false
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

	// A fresh authorization starts a new session. Its ID is what lets
	// other processes tell it apart from the session they hold.
	sessionID, err := newSessionID()
	if err != nil {
		return fmt.Errorf("generating session id: %w", err)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	tokens, err := p.doTokenRequest(data, sessionID)
	if err != nil {
		return err
	}

	// The new session replaces whatever is stored, on purpose. Save it
	// under the refresh lock so it cannot land between another process's
	// pre-save check of the old session and that process's save, which
	// would put the old session back over this one.
	release, _ := acquireRefreshLock(p.refreshLockDir())
	defer release()
	p.saveSession(tokens)
	return nil
}

// refresh exchanges the in-memory refresh token. Callers must hold p.mu and
// should hold the refresh lock, which keeps other up-to-date processes from
// changing the store between the check below and the save.
func (p *OAuthProvider) refresh() error {
	data := url.Values{}
	data.Set("grant_type", "refresh_token")
	data.Set("client_id", p.config.ClientID)
	data.Set("client_secret", p.config.ClientSecret)
	data.Set("refresh_token", p.tokens.RefreshToken)

	// A refresh continues the current session, so it keeps its ID.
	tokens, err := p.doTokenRequest(data, p.sessionID)
	if err != nil {
		return err
	}

	// The request takes time. If another process signed out or stored a
	// different session meanwhile (--logout, --switch, --setup --force),
	// the refreshed tokens belong to a session the user ended: drop them
	// rather than save them over the store.
	if p.store != nil {
		if _, err := p.loadStoredSession(); err != nil {
			return err
		}
	}
	p.saveSession(tokens)

	slog.Info("OAuth token refreshed successfully")
	return nil
}

// saveSession saves tokens to the store and makes them the in-memory
// session. A failed save is logged and the session kept in memory only.
// Callers must hold p.mu.
func (p *OAuthProvider) saveSession(tokens *StoredTokens) {
	persisted := false
	if p.store != nil {
		if err := p.store.Save(tokens); err != nil {
			slog.Warn("failed to persist tokens", "error", err)
		} else {
			persisted = true
		}
	}
	p.setSession(tokens, persisted)
}

// doTokenRequest posts data to the token endpoint and returns the issued
// tokens, labelled with sessionID. It neither saves them nor changes the
// in-memory session. Callers must hold p.mu.
func (p *OAuthProvider) doTokenRequest(data url.Values, sessionID string) (*StoredTokens, error) {
	client := p.httpClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	endpoint := p.tokenURL
	if endpoint == "" {
		endpoint = tokenEndpoint
	}

	resp, err := client.PostForm(endpoint, data)
	if err != nil {
		return nil, fmt.Errorf("token request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("reading token response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		slog.Debug("token endpoint error", "status", resp.StatusCode, "body_size", len(body))
		// Surface the Webex error message so users can diagnose scope mismatches,
		// invalid client credentials, expired auth codes, etc.
		var errResp struct {
			Error            string `json:"error"`
			ErrorDescription string `json:"error_description"`
			Message          string `json:"message"`
		}
		_ = json.Unmarshal(body, &errResp)
		switch {
		case errResp.ErrorDescription != "":
			return nil, fmt.Errorf("OAuth token exchange failed (status %d): %s", resp.StatusCode, errResp.ErrorDescription)
		case errResp.Message != "":
			return nil, fmt.Errorf("OAuth token exchange failed (status %d): %s", resp.StatusCode, errResp.Message)
		case errResp.Error != "":
			return nil, fmt.Errorf("OAuth token exchange failed (status %d): %s", resp.StatusCode, errResp.Error)
		default:
			return nil, fmt.Errorf("OAuth token exchange failed (status %d)", resp.StatusCode)
		}
	}

	var tokenResp struct {
		AccessToken           string `json:"access_token"`
		ExpiresIn             int64  `json:"expires_in"`
		RefreshToken          string `json:"refresh_token"`
		RefreshTokenExpiresIn int64  `json:"refresh_token_expires_in"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return nil, fmt.Errorf("parsing token response: %w", err)
	}

	if tokenResp.AccessToken == "" {
		return nil, fmt.Errorf("token response missing access_token")
	}
	if tokenResp.ExpiresIn <= 0 {
		return nil, fmt.Errorf("token response has invalid expires_in: %d", tokenResp.ExpiresIn)
	}

	now := time.Now()
	tokens := &StoredTokens{
		AccessToken:           tokenResp.AccessToken,
		RefreshToken:          tokenResp.RefreshToken,
		AccessTokenExpiresAt:  now.Add(time.Duration(tokenResp.ExpiresIn) * time.Second),
		RefreshTokenExpiresAt: now.Add(time.Duration(tokenResp.RefreshTokenExpiresIn) * time.Second),
		ClientID:              p.config.ClientID,
		SessionID:             sessionID,
	}
	return tokens, nil
}

// newSessionID returns a random identifier for a new OAuth authorization.
func newSessionID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
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
