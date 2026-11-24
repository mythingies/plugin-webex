package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// otherClient rebinds t to a different OAuth client.
func otherClient(t *StoredTokens) *StoredTokens {
	t.ClientID = "other-client"
	return t
}

// sameStoredTokens compares tokens field by field; time.Time values that
// went through JSON lose their monotonic reading, so == would not do.
func sameStoredTokens(a, b *StoredTokens) bool {
	return a.AccessToken == b.AccessToken && a.RefreshToken == b.RefreshToken &&
		a.ClientID == b.ClientID && a.SessionID == b.SessionID &&
		a.AccessTokenExpiresAt.Equal(b.AccessTokenExpiresAt) &&
		a.RefreshTokenExpiresAt.Equal(b.RefreshTokenExpiresAt)
}

// blockingTokenEndpoint is a token endpoint whose refresh_token grants block
// until release is called, so a test can act while a refresh is in flight.
// authorization_code grants answer at once.
type blockingTokenEndpoint struct {
	srv       *httptest.Server
	hits      atomic.Int64
	refreshIn chan struct{} // receives once per refresh request that arrived
	codeIn    chan struct{} // receives once per code exchange that arrived
	unblock   chan struct{}
	once      sync.Once
}

func newBlockingTokenEndpoint(t *testing.T) *blockingTokenEndpoint {
	t.Helper()
	b := &blockingTokenEndpoint{
		refreshIn: make(chan struct{}, 8),
		codeIn:    make(chan struct{}, 8),
		unblock:   make(chan struct{}),
	}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := b.hits.Add(1)
		r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		prefix := "code"
		if r.PostForm.Get("grant_type") == "refresh_token" {
			prefix = "refreshed"
			b.refreshIn <- struct{}{}
			<-b.unblock
		} else {
			b.codeIn <- struct{}{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":             fmt.Sprintf("%s-access-%d", prefix, n),
			"expires_in":               3600,
			"refresh_token":            fmt.Sprintf("%s-refresh-%d", prefix, n),
			"refresh_token_expires_in": 86400,
		})
	}))
	t.Cleanup(func() {
		b.release()
		b.srv.Close()
	})
	return b
}

func (b *blockingTokenEndpoint) release() { b.once.Do(func() { close(b.unblock) }) }

func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

type tokenResult struct {
	tok string
	err error
}

// startToken runs p.Token() in the background.
func startToken(p *OAuthProvider) <-chan tokenResult {
	ch := make(chan tokenResult, 1)
	go func() {
		tok, err := p.Token()
		ch <- tokenResult{tok, err}
	}()
	return ch
}

func expireInMemory(p *OAuthProvider) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.tokens != nil {
		p.tokens.AccessTokenExpiresAt = time.Now()
	}
}

// TestRefreshInFlightDropsResultWhenSessionEnded (A1): another process
// changes the store while this one waits on the token endpoint, without
// waiting for the refresh lock (its wait timed out, or it predates the
// lock). The refresh must not save the old session back over the result.
func TestRefreshInFlightDropsResultWhenSessionEnded(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(t *testing.T, store *TokenStore)
		want   *StoredTokens // nil: store stays empty
	}{
		{
			name: "logout whose lock wait timed out",
			mutate: func(t *testing.T, store *TokenStore) {
				if err := store.Delete(); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "switch to another session",
			want: freshSessionTokens("access-b", "sess-b"),
		},
		{
			name: "setup with another client",
			want: otherClient(freshSessionTokens("access-c", "sess-c")),
		},
		{
			name: "another client reusing the session id",
			want: otherClient(freshSessionTokens("access-c", "sess-a")),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Short enough that Delete gives up waiting for the lock the
			// in-flight refresh holds.
			shortLockTimings(t, 50*time.Millisecond)
			_ = captureLogs(t)
			ep := newBlockingTokenEndpoint(t)
			store := newFileStore(t)
			if err := store.Save(sessionTokens("refresh-a", "sess-a")); err != nil {
				t.Fatal(err)
			}
			a := newTestProvider(t, store, ep.srv.URL)

			res := startToken(a)
			waitFor(t, ep.refreshIn, "refresh request")
			if tt.mutate != nil {
				tt.mutate(t, store)
			} else if err := store.Save(tt.want); err != nil {
				t.Fatal(err)
			}
			ep.release()

			r := <-res
			assertSignedOut(t, a, r.tok, r.err)
			saved, err := store.Load()
			if tt.want == nil {
				if !errors.Is(err, os.ErrNotExist) {
					t.Errorf("store after logout: err = %v, want ErrNotExist (refresh must not resurrect it)", err)
				}
			} else {
				if err != nil {
					t.Fatalf("loading store: %v", err)
				}
				if !sameStoredTokens(saved, tt.want) {
					t.Errorf("store overwritten: got session %q client %q access %q, want session %q client %q access %q",
						saved.SessionID, saved.ClientID, saved.AccessToken, tt.want.SessionID, tt.want.ClientID, tt.want.AccessToken)
				}
			}
			assertNoLockFile(t, store)
		})
	}
}

// TestLogoutWaitsForInFlightRefresh (A1): Delete takes the refresh lock, so
// a logout during another process's refresh waits for it to finish and then
// removes what it saved. The store ends empty and the refreshing process
// signs out at its next refresh.
func TestLogoutWaitsForInFlightRefresh(t *testing.T) {
	shortLockTimings(t, 5*time.Second)
	_ = captureLogs(t)
	ep := newBlockingTokenEndpoint(t)
	store := newFileStore(t)
	if err := store.Save(sessionTokens("refresh-a", "sess-a")); err != nil {
		t.Fatal(err)
	}
	a := newTestProvider(t, store, ep.srv.URL)

	res := startToken(a)
	waitFor(t, ep.refreshIn, "refresh request")

	// --logout in another process (a separate TokenStore value, as there).
	deleted := make(chan error, 1)
	go func() { deleted <- (&TokenStore{path: store.path}).Delete() }()
	select {
	case err := <-deleted:
		t.Fatalf("Delete returned (%v) while a refresh held the lock", err)
	case <-time.After(200 * time.Millisecond):
	}

	ep.release()
	r := <-res
	if r.err != nil {
		t.Fatalf("in-flight Token(): %v (the refresh finished before the logout)", r.err)
	}
	if err := <-deleted; err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.Load(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("store after logout: err = %v, want ErrNotExist", err)
	}

	expireInMemory(a)
	tok, err := a.Token()
	assertSignedOut(t, a, tok, err)
	if got := ep.hits.Load(); got != 1 {
		t.Errorf("endpoint hits = %d, want 1", got)
	}
	if _, err := store.Load(); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("store resurrected after sign-out: err = %v", err)
	}
	assertNoLockFile(t, store)
}

// TestNewSessionDuringInFlightRefreshIsKept (A1): --setup --force signs in
// while another process is refreshing the old session. The new session must
// be what ends up stored, and the refreshing process must sign out.
func TestNewSessionDuringInFlightRefreshIsKept(t *testing.T) {
	shortLockTimings(t, 5*time.Second)
	_ = captureLogs(t)
	ep := newBlockingTokenEndpoint(t)
	store := newFileStore(t)
	if err := store.Save(sessionTokens("refresh-a", "sess-a")); err != nil {
		t.Fatal(err)
	}
	a := newTestProvider(t, store, ep.srv.URL)
	// The setup process loaded the old session at start, as
	// NewOAuthProvider does, and has its own store value.
	s := newTestProvider(t, &TokenStore{path: store.path}, ep.srv.URL)

	res := startToken(a)
	waitFor(t, ep.refreshIn, "refresh request")

	exchanged := make(chan error, 1)
	go func() { exchanged <- s.exchangeCode("code", ProtocolCallbackURL, "verifier") }()
	waitFor(t, ep.codeIn, "code exchange")
	// Let the exchange reach its save before the refresh returns.
	time.Sleep(100 * time.Millisecond)
	ep.release()

	if err := <-exchanged; err != nil {
		t.Fatalf("exchangeCode: %v", err)
	}
	r := <-res
	if r.err != nil && !errors.Is(r.err, ErrSignedOut) {
		t.Fatalf("in-flight Token(): %v", r.err)
	}

	newSession := s.sessionID
	if newSession == "" || newSession == "sess-a" {
		t.Fatalf("setup session ID = %q, want a new one", newSession)
	}
	saved, err := store.Load()
	if err != nil {
		t.Fatalf("loading store: %v", err)
	}
	if saved.SessionID != newSession || !strings.HasPrefix(saved.AccessToken, "code-access-") {
		t.Fatalf("store holds session %q access %q, want the new session %q from the code exchange",
			saved.SessionID, saved.AccessToken, newSession)
	}

	// Whichever order the saves took, A is off the old session by its next
	// refresh and leaves the new one alone.
	expireInMemory(a)
	tok, err := a.Token()
	assertSignedOut(t, a, tok, err)
	after, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !sameStoredTokens(after, saved) {
		t.Errorf("new session changed after A signed out: session %q access %q", after.SessionID, after.AccessToken)
	}
	if tok, err := s.Token(); err != nil || tok != saved.AccessToken {
		t.Errorf("setup provider Token() = %q, %v; want its new session", tok, err)
	}
	assertNoLockFile(t, store)
}

// TestExchangeCodeSavesUnderRefreshLock (A1): the save of a new session
// waits for the refresh lock, so it cannot land between another process's
// pre-save check and its save.
func TestExchangeCodeSavesUnderRefreshLock(t *testing.T) {
	shortLockTimings(t, 5*time.Second)
	ep := newFakeTokenEndpoint(t)
	store := newFileStore(t)
	p := &OAuthProvider{
		config:     OAuthConfig{ClientID: testClientID, ClientSecret: "s"},
		store:      store,
		httpClient: &http.Client{Timeout: 5 * time.Second},
		tokenURL:   ep.srv.URL,
	}

	release, ok := acquireRefreshLock(p.refreshLockDir())
	if !ok {
		t.Fatal("could not take the refresh lock")
	}
	done := make(chan error, 1)
	go func() { done <- p.exchangeCode("code", ProtocolCallbackURL, "verifier") }()

	deadline := time.Now().Add(5 * time.Second)
	for ep.hits.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(150 * time.Millisecond)
	if _, err := store.Load(); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("new session saved while another process held the refresh lock: err = %v", err)
	}
	release()

	if err := <-done; err != nil {
		t.Fatalf("exchangeCode: %v", err)
	}
	saved, err := store.Load()
	if err != nil {
		t.Fatalf("first-time authorization not saved: %v", err)
	}
	if saved.SessionID == "" || saved.SessionID != p.sessionID || saved.AccessToken != "access-1" {
		t.Errorf("saved session %q access %q, want %q access-1", saved.SessionID, saved.AccessToken, p.sessionID)
	}
	assertNoLockFile(t, store)
}

// TestFileStoreFollowsMigrationToKeychain (A4): this process started in
// file mode; another process that can reach the keychain moved tokens.json
// into it and deleted the file. That is not a logout: this process follows
// its session into the keychain and keeps working.
func TestFileStoreFollowsMigrationToKeychain(t *testing.T) {
	for _, sessionID := range []string{"sess-a", ""} {
		t.Run("session "+sessionID, func(t *testing.T) {
			useTokenKeychain(t)
			logs := captureLogs(t)
			ep := newFakeTokenEndpoint(t)
			store := newFileStore(t)
			if err := store.Save(sessionTokens("refresh-a", sessionID)); err != nil {
				t.Fatal(err)
			}
			a := newTestProvider(t, store, ep.srv.URL)

			migrator := &TokenStore{path: store.path, useKeyring: true}
			if migrated, err := migrator.MigrateTokensFromFile(); err != nil || !migrated {
				t.Fatalf("MigrateTokensFromFile = %v, %v", migrated, err)
			}

			tok, err := a.Token()
			if err != nil {
				t.Fatalf("Token() after migration: %v", err)
			}
			if tok != "access-1" {
				t.Errorf("Token() = %q, want access-1", tok)
			}
			if sent := ep.sentRefreshTokens(); len(sent) != 1 || sent[0] != "refresh-a" {
				t.Errorf("refresh tokens sent = %q, want [refresh-a]", sent)
			}
			if !a.store.UsingKeyring() {
				t.Error("provider store did not switch to the keychain")
			}
			if store.UsingKeyring() {
				t.Error("the original store value was mutated")
			}
			saved, err := migrator.Load()
			if err != nil {
				t.Fatalf("loading keychain: %v", err)
			}
			if saved.AccessToken != "access-1" || saved.SessionID != sessionID {
				t.Errorf("keychain holds access %q session %q, want access-1 / %q", saved.AccessToken, saved.SessionID, sessionID)
			}
			if _, err := os.Stat(store.path); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("token file recreated after migration: stat err = %v", err)
			}
			if strings.Contains(logs.String(), "signing out") {
				t.Errorf("migration treated as sign-out:\n%s", logs.String())
			}
			assertNoLockFile(t, store)
		})
	}
}

// TestFileStoreMissingKeychainOtherSession (A4): the file is gone and the
// keychain holds some other session. That is still a sign-out, and the
// keychain entry is left alone.
func TestFileStoreMissingKeychainOtherSession(t *testing.T) {
	tests := []struct {
		name string
		mine string
		kc   *StoredTokens
	}{
		{name: "different session id", mine: "sess-a", kc: freshSessionTokens("access-b", "sess-b")},
		{name: "another client", mine: "sess-a", kc: otherClient(freshSessionTokens("access-c", "sess-a"))},
		{name: "unrelated legacy entry", mine: "", kc: freshSessionTokens("access-old", "")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			useTokenKeychain(t)
			_ = captureLogs(t)
			ep := newFakeTokenEndpoint(t)
			store := newFileStore(t)
			if err := store.Save(sessionTokens("refresh-a", tt.mine)); err != nil {
				t.Fatal(err)
			}
			a := newTestProvider(t, store, ep.srv.URL)

			kc := &TokenStore{path: store.path, useKeyring: true}
			if err := kc.Save(tt.kc); err != nil {
				t.Fatal(err)
			}
			if err := store.Delete(); err != nil {
				t.Fatal(err)
			}

			tok, err := a.Token()
			assertSignedOut(t, a, tok, err)
			if got := ep.hits.Load(); got != 0 {
				t.Errorf("endpoint hits = %d, want 0", got)
			}
			saved, err := kc.Load()
			if err != nil {
				t.Fatal(err)
			}
			if !sameStoredTokens(saved, tt.kc) {
				t.Errorf("keychain entry changed: session %q access %q", saved.SessionID, saved.AccessToken)
			}
		})
	}
}
