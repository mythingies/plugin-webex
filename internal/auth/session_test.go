package auth

import (
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"testing"
	"time"
)

// sessionTokens is staleTokens bound to a session ID.
func sessionTokens(refresh, sessionID string) *StoredTokens {
	t := staleTokens(refresh)
	t.SessionID = sessionID
	return t
}

// freshSessionTokens has an access token that needs no refresh.
func freshSessionTokens(access, sessionID string) *StoredTokens {
	return &StoredTokens{
		AccessToken:           access,
		RefreshToken:          "refresh-" + access,
		AccessTokenExpiresAt:  time.Now().Add(time.Hour),
		RefreshTokenExpiresAt: time.Now().Add(24 * time.Hour),
		ClientID:              testClientID,
		SessionID:             sessionID,
	}
}

func assertSignedOut(t *testing.T, p *OAuthProvider, tok string, err error) {
	t.Helper()
	if !errors.Is(err, ErrSignedOut) {
		t.Fatalf("Token() err = %v, want ErrSignedOut", err)
	}
	if tok != "" {
		t.Errorf("Token() returned token %q alongside ErrSignedOut", tok)
	}
	if p.tokens != nil {
		t.Errorf("in-memory tokens kept after sign-out: access=%q", p.tokens.AccessToken)
	}
	if !p.NeedsAuth() {
		t.Error("NeedsAuth() = false after sign-out")
	}
	// It stays signed out: the next call fails the same way.
	if _, err := p.Token(); !errors.Is(err, ErrSignedOut) {
		t.Errorf("second Token() err = %v, want ErrSignedOut", err)
	}
}

// TestTokenSignedOutAfterSwitch: process A holds account A's session; another
// process ran --switch and stored account B's session. A must not adopt B's
// tokens, refresh, or save its own over B's.
func TestTokenSignedOutAfterSwitch(t *testing.T) {
	tests := []struct {
		name   string
		mine   string // session ID process A loaded
		stored *StoredTokens
	}{
		{name: "new session with stale access token", mine: "sess-a", stored: sessionTokens("refresh-b", "sess-b")},
		{name: "new session with fresh access token", mine: "sess-a", stored: freshSessionTokens("access-b", "sess-b")},
		{name: "legacy process sees a new session", mine: "", stored: freshSessionTokens("access-b", "sess-b")},
		{name: "new process sees a legacy session", mine: "sess-a", stored: freshSessionTokens("access-b", "")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ep := newFakeTokenEndpoint(t)
			store := newFileStore(t)
			if err := store.Save(sessionTokens("refresh-a", tt.mine)); err != nil {
				t.Fatal(err)
			}
			a := newTestProvider(t, store, ep.srv.URL)

			// --switch in another process.
			if err := store.Save(tt.stored); err != nil {
				t.Fatal(err)
			}

			tok, err := a.Token()
			assertSignedOut(t, a, tok, err)
			if got := ep.hits.Load(); got != 0 {
				t.Errorf("endpoint hits = %d, want 0", got)
			}
			saved, err := store.Load()
			if err != nil {
				t.Fatalf("loading store: %v", err)
			}
			if saved.SessionID != tt.stored.SessionID || saved.AccessToken != tt.stored.AccessToken ||
				saved.RefreshToken != tt.stored.RefreshToken {
				t.Errorf("store changed: got session %q access %q, want B's session %q access %q",
					saved.SessionID, saved.AccessToken, tt.stored.SessionID, tt.stored.AccessToken)
			}
			assertNoLockFile(t, store)
		})
	}
}

// TestTokenSignedOutAfterLogout: another process ran --logout. The running
// process must not refresh its in-memory tokens and save them back.
func TestTokenSignedOutAfterLogout(t *testing.T) {
	for _, sessionID := range []string{"sess-a", ""} {
		t.Run("session "+sessionID, func(t *testing.T) {
			ep := newFakeTokenEndpoint(t)
			store := newFileStore(t)
			if err := store.Save(sessionTokens("refresh-a", sessionID)); err != nil {
				t.Fatal(err)
			}
			a := newTestProvider(t, store, ep.srv.URL)

			// --logout in another process.
			if err := store.Delete(); err != nil {
				t.Fatal(err)
			}

			tok, err := a.Token()
			assertSignedOut(t, a, tok, err)
			if got := ep.hits.Load(); got != 0 {
				t.Errorf("endpoint hits = %d, want 0", got)
			}
			if _, err := store.Load(); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("store after sign-out: err = %v, want ErrNotExist (nothing saved)", err)
			}
			assertNoLockFile(t, store)
		})
	}
}

// TestSessionIDPreservedAcrossRefresh: a refresh continues the session, so
// the saved tokens keep its ID and other processes on it still adopt them.
func TestSessionIDPreservedAcrossRefresh(t *testing.T) {
	ep := newFakeTokenEndpoint(t)
	store := newFileStore(t)
	if err := store.Save(sessionTokens("refresh-0", "sess-a")); err != nil {
		t.Fatal(err)
	}
	a := newTestProvider(t, store, ep.srv.URL)
	b := newTestProvider(t, store, ep.srv.URL)

	tokA, err := a.Token()
	if err != nil {
		t.Fatalf("A.Token(): %v", err)
	}
	saved, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if saved.SessionID != "sess-a" {
		t.Errorf("saved session ID after refresh = %q, want sess-a", saved.SessionID)
	}
	if saved.AccessToken != tokA {
		t.Errorf("saved access token = %q, want %q", saved.AccessToken, tokA)
	}
	if a.sessionID != "sess-a" || a.tokens.SessionID != "sess-a" {
		t.Errorf("A session after refresh = %q / %q, want sess-a", a.sessionID, a.tokens.SessionID)
	}

	tokB, err := b.Token()
	if err != nil {
		t.Fatalf("B.Token(): %v", err)
	}
	if tokB != tokA {
		t.Errorf("B.Token() = %q, want adopted %q", tokB, tokA)
	}
	if got := ep.hits.Load(); got != 1 {
		t.Errorf("endpoint hits = %d, want 1", got)
	}
}

// TestLegacySessionsStillAdopt: tokens saved before session IDs existed have
// none on either side and still adopt each other, and a refresh of them
// keeps them legacy rather than inventing an ID other processes lack.
func TestLegacySessionsStillAdopt(t *testing.T) {
	ep := newFakeTokenEndpoint(t)
	store := newFileStore(t)
	if err := store.Save(sessionTokens("refresh-0", "")); err != nil {
		t.Fatal(err)
	}
	a := newTestProvider(t, store, ep.srv.URL)
	b := newTestProvider(t, store, ep.srv.URL)
	if _, err := a.Token(); err != nil {
		t.Fatalf("A.Token(): %v", err)
	}
	saved, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if saved.SessionID != "" {
		t.Errorf("legacy session gained ID %q on refresh", saved.SessionID)
	}
	tok, err := b.Token()
	if err != nil {
		t.Fatalf("B.Token(): %v", err)
	}
	if tok != "access-1" {
		t.Errorf("B.Token() = %q, want adopted access-1", tok)
	}
	if got := ep.hits.Load(); got != 1 {
		t.Errorf("endpoint hits = %d, want 1", got)
	}
}

// TestExchangeCodeStartsSession: a provider created before any tokens exist
// gets its session ID from the authorization, which saves it for other
// processes. A later authorization (re-login after a sign-out) starts a new
// session and clears the signed-out state.
func TestExchangeCodeStartsSession(t *testing.T) {
	ep := newFakeTokenEndpoint(t)
	store := newFileStore(t)
	p := &OAuthProvider{
		config:     OAuthConfig{ClientID: testClientID, ClientSecret: "s"},
		store:      store,
		httpClient: &http.Client{Timeout: 5 * time.Second},
		tokenURL:   ep.srv.URL,
	}
	if _, err := p.Token(); err == nil || errors.Is(err, ErrSignedOut) {
		t.Fatalf("Token() before authorization err = %v, want not-authenticated (not ErrSignedOut)", err)
	}

	if err := p.exchangeCode("code-1", ProtocolCallbackURL, "verifier"); err != nil {
		t.Fatalf("exchangeCode: %v", err)
	}
	first := p.sessionID
	if b, err := hex.DecodeString(first); err != nil || len(b) != 16 {
		t.Fatalf("session ID %q is not 16 random bytes in hex", first)
	}
	if p.tokens.SessionID != first {
		t.Errorf("in-memory tokens session = %q, want %q", p.tokens.SessionID, first)
	}
	saved, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if saved.SessionID != first {
		t.Errorf("saved session ID = %q, want %q", saved.SessionID, first)
	}
	if tok, err := p.Token(); err != nil || tok != "access-1" {
		t.Fatalf("Token() after authorization = %q, %v", tok, err)
	}

	// Another process signs out; this one notices at its next refresh.
	p.tokens.AccessTokenExpiresAt = time.Now()
	if err := store.Delete(); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Token(); !errors.Is(err, ErrSignedOut) {
		t.Fatalf("Token() after logout err = %v, want ErrSignedOut", err)
	}

	// Signing in again works and is a different session.
	if err := p.exchangeCode("code-2", ProtocolCallbackURL, "verifier"); err != nil {
		t.Fatalf("second exchangeCode: %v", err)
	}
	if p.sessionID == "" || p.sessionID == first {
		t.Errorf("second session ID = %q, want a new one (first %q)", p.sessionID, first)
	}
	if tok, err := p.Token(); err != nil || tok != "access-2" {
		t.Errorf("Token() after re-authorization = %q, %v", tok, err)
	}
}
