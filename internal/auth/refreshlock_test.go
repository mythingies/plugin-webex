package auth

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testClientID = "client-under-test"

// fakeTokenEndpoint is an httptest OAuth token endpoint that counts
// refresh requests and records the refresh tokens it was sent.
type fakeTokenEndpoint struct {
	srv    *httptest.Server
	hits   atomic.Int64
	delay  time.Duration
	status int // 0 means 200

	mu            sync.Mutex
	refreshTokens []string
}

func newFakeTokenEndpoint(t *testing.T) *fakeTokenEndpoint {
	t.Helper()
	f := &fakeTokenEndpoint{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := f.hits.Add(1)
		r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.refreshTokens = append(f.refreshTokens, r.PostForm.Get("refresh_token"))
		f.mu.Unlock()

		if f.delay > 0 {
			time.Sleep(f.delay)
		}
		w.Header().Set("Content-Type", "application/json")
		if f.status != 0 && f.status != http.StatusOK {
			w.WriteHeader(f.status)
			_, _ = w.Write([]byte(`{"error_description":"refresh token revoked"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":             fmt.Sprintf("access-%d", n),
			"expires_in":               3600,
			"refresh_token":            fmt.Sprintf("refresh-%d", n),
			"refresh_token_expires_in": 86400,
		})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeTokenEndpoint) sentRefreshTokens() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.refreshTokens...)
}

// staleTokens returns tokens whose access token needs a refresh but whose
// refresh token is still good.
func staleTokens(refresh string) *StoredTokens {
	return &StoredTokens{
		AccessToken:           "stale-access",
		RefreshToken:          refresh,
		AccessTokenExpiresAt:  time.Now().Add(time.Minute), // inside refreshBuffer
		RefreshTokenExpiresAt: time.Now().Add(24 * time.Hour),
		ClientID:              testClientID,
	}
}

// newFileStore returns a file-backed token store in a fresh temp dir, so the
// refresh lock lands in the same dir.
func newFileStore(t *testing.T) *TokenStore {
	t.Helper()
	return &TokenStore{path: filepath.Join(t.TempDir(), tokenFileName), useKeyring: false}
}

// newTestProvider mimics NewOAuthProvider: it loads whatever is in the
// store into memory, like a process that started earlier.
func newTestProvider(t *testing.T, store *TokenStore, endpoint string) *OAuthProvider {
	t.Helper()
	p := &OAuthProvider{
		config:     OAuthConfig{ClientID: testClientID, ClientSecret: "s"},
		store:      store,
		httpClient: &http.Client{Timeout: 5 * time.Second},
		tokenURL:   endpoint,
	}
	tokens, err := store.Load()
	if err != nil {
		t.Fatalf("loading store: %v", err)
	}
	p.setSession(tokens, true)
	return p
}

// shortLockTimings shrinks the lock timings for the duration of a test.
func shortLockTimings(t *testing.T, timeout time.Duration) {
	t.Helper()
	oldTimeout, oldPoll := refreshLockTimeout, refreshLockPoll
	refreshLockTimeout = timeout
	refreshLockPoll = 5 * time.Millisecond
	t.Cleanup(func() {
		refreshLockTimeout, refreshLockPoll = oldTimeout, oldPoll
	})
}

// captureLogs routes slog to a buffer for the duration of a test.
func captureLogs(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })
	return buf
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func lockPath(store *TokenStore) string {
	return filepath.Join(filepath.Dir(store.path), refreshLockName)
}

func assertNoLockFile(t *testing.T, store *TokenStore) {
	t.Helper()
	if _, err := os.Stat(lockPath(store)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("refresh lock still present after Token(): stat err = %v", err)
	}
}

// --- OAuthProvider.Token() across "processes" ---

func TestTokenAdoptsTokenRefreshedByOtherProcess(t *testing.T) {
	ep := newFakeTokenEndpoint(t)
	store := newFileStore(t)
	if err := store.Save(staleTokens("refresh-0")); err != nil {
		t.Fatal(err)
	}

	a := newTestProvider(t, store, ep.srv.URL)
	b := newTestProvider(t, store, ep.srv.URL)

	tokA, err := a.Token()
	if err != nil {
		t.Fatalf("A.Token(): %v", err)
	}
	if tokA != "access-1" {
		t.Fatalf("A.Token() = %q, want access-1", tokA)
	}
	if got := ep.hits.Load(); got != 1 {
		t.Fatalf("endpoint hits after A = %d, want 1", got)
	}

	// B still holds the stale token in memory.
	if b.tokens.AccessToken != "stale-access" {
		t.Fatalf("precondition: B in-memory token = %q", b.tokens.AccessToken)
	}
	tokB, err := b.Token()
	if err != nil {
		t.Fatalf("B.Token(): %v", err)
	}
	if tokB != tokA {
		t.Errorf("B.Token() = %q, want adopted %q", tokB, tokA)
	}
	if got := ep.hits.Load(); got != 1 {
		t.Errorf("endpoint hits after B = %d, want 1 (B must adopt, not refresh)", got)
	}
	if b.tokens.RefreshToken != "refresh-1" {
		t.Errorf("B did not adopt the stored refresh token: %q", b.tokens.RefreshToken)
	}

	// B's next call is served from memory.
	if _, err := b.Token(); err != nil {
		t.Fatal(err)
	}
	if got := ep.hits.Load(); got != 1 {
		t.Errorf("endpoint hits after second B call = %d, want 1", got)
	}
	assertNoLockFile(t, store)
}

func TestTokenConcurrentProvidersRefreshOnce(t *testing.T) {
	ep := newFakeTokenEndpoint(t)
	ep.delay = 50 * time.Millisecond // widen the race window
	store := newFileStore(t)
	if err := store.Save(staleTokens("refresh-0")); err != nil {
		t.Fatal(err)
	}

	providers := []*OAuthProvider{
		newTestProvider(t, store, ep.srv.URL),
		newTestProvider(t, store, ep.srv.URL),
		newTestProvider(t, store, ep.srv.URL),
	}

	const callsPerProvider = 4
	var (
		wg      sync.WaitGroup
		start   = make(chan struct{})
		results = make(chan string, len(providers)*callsPerProvider)
		errs    = make(chan error, len(providers)*callsPerProvider)
	)
	for _, p := range providers {
		for range callsPerProvider {
			wg.Add(1)
			go func(p *OAuthProvider) {
				defer wg.Done()
				<-start
				tok, err := p.Token()
				if err != nil {
					errs <- err
					return
				}
				results <- tok
			}(p)
		}
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		t.Errorf("Token(): %v", err)
	}
	for tok := range results {
		if tok != "access-1" {
			t.Errorf("Token() = %q, want access-1", tok)
		}
	}
	if got := ep.hits.Load(); got != 1 {
		t.Errorf("endpoint hits = %d, want exactly 1", got)
	}
	assertNoLockFile(t, store)
}

func TestTokenRemovesStaleLock(t *testing.T) {
	ep := newFakeTokenEndpoint(t)
	store := newFileStore(t)
	if err := store.Save(staleTokens("refresh-0")); err != nil {
		t.Fatal(err)
	}
	// A crashed holder left this behind two minutes ago.
	lp := lockPath(store)
	if err := os.WriteFile(lp, []byte("pid=1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * refreshLockStaleAfter)
	if err := os.Chtimes(lp, old, old); err != nil {
		t.Fatal(err)
	}
	// If the stale sweep failed we'd wait the full timeout; keep it short
	// so a failure shows up as a log line rather than a slow test.
	shortLockTimings(t, 2*time.Second)
	logs := captureLogs(t)

	p := newTestProvider(t, store, ep.srv.URL)
	start := time.Now()
	tok, err := p.Token()
	if err != nil {
		t.Fatalf("Token(): %v", err)
	}
	if tok != "access-1" {
		t.Errorf("Token() = %q, want access-1", tok)
	}
	if got := ep.hits.Load(); got != 1 {
		t.Errorf("endpoint hits = %d, want 1", got)
	}
	if elapsed := time.Since(start); elapsed >= refreshLockTimeout {
		t.Errorf("Token() took %v; stale lock was waited on instead of removed", elapsed)
	}
	if !strings.Contains(logs.String(), "removed stale refresh lock") {
		t.Errorf("expected stale-lock log, got:\n%s", logs.String())
	}
	if strings.Contains(logs.String(), "timed out") {
		t.Errorf("stale lock caused a timeout:\n%s", logs.String())
	}
	assertNoLockFile(t, store)
}

func TestTokenHeldLockTimesOutAndRefreshes(t *testing.T) {
	ep := newFakeTokenEndpoint(t)
	store := newFileStore(t)
	if err := store.Save(staleTokens("refresh-0")); err != nil {
		t.Fatal(err)
	}
	// A live holder that never lets go (fresh mtime, so not stale).
	lp := lockPath(store)
	foreign := []byte("pid=99999\nnonce=someone-else\n")
	if err := os.WriteFile(lp, foreign, 0600); err != nil {
		t.Fatal(err)
	}
	shortLockTimings(t, 150*time.Millisecond)
	logs := captureLogs(t)

	p := newTestProvider(t, store, ep.srv.URL)
	start := time.Now()
	tok, err := p.Token()
	if err != nil {
		t.Fatalf("Token(): %v", err)
	}
	if tok != "access-1" {
		t.Errorf("Token() = %q, want access-1", tok)
	}
	if got := ep.hits.Load(); got != 1 {
		t.Errorf("endpoint hits = %d, want 1", got)
	}
	if elapsed := time.Since(start); elapsed < refreshLockTimeout {
		t.Errorf("Token() returned after %v; expected to wait at least %v", elapsed, refreshLockTimeout)
	}
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "timed out waiting for refresh lock") {
		t.Errorf("expected timeout warning, got:\n%s", logs.String())
	}
	// The other holder's lock must be left alone.
	got, err := os.ReadFile(lp) // #nosec G304 -- test reads its own temp-dir file
	if err != nil {
		t.Fatalf("foreign lock was removed: %v", err)
	}
	if !bytes.Equal(got, foreign) {
		t.Errorf("foreign lock contents changed: %q", got)
	}
}

func TestTokenRefreshTokenSelection(t *testing.T) {
	tests := []struct {
		name   string
		memory *StoredTokens
		stored *StoredTokens // nil: store file absent
		want   string        // refresh token the endpoint should receive
	}{
		{
			name:   "stored refresh token is newer",
			memory: staleTokens("refresh-old"),
			stored: staleTokens("refresh-new"),
			want:   "refresh-new",
		},
		{
			name:   "stored refresh token expired",
			memory: staleTokens("refresh-mem"),
			stored: func() *StoredTokens {
				s := staleTokens("refresh-dead")
				s.RefreshTokenExpiresAt = time.Now().Add(-time.Hour)
				return s
			}(),
			want: "refresh-mem",
		},
		{
			name:   "stored refresh token empty",
			memory: staleTokens("refresh-mem"),
			stored: staleTokens(""),
			want:   "refresh-mem",
		},
		{
			// The in-memory session was never saved (the save failed),
			// so an empty store is not a sign-out.
			name:   "store missing and session never saved",
			memory: staleTokens("refresh-mem"),
			stored: nil,
			want:   "refresh-mem",
		},
		{
			name:   "stored tokens belong to another client",
			memory: staleTokens("refresh-mem"),
			stored: func() *StoredTokens {
				s := staleTokens("refresh-other-client")
				s.ClientID = "someone-else"
				return s
			}(),
			want: "refresh-mem",
		},
		{
			name:   "stored tokens predate client id field",
			memory: staleTokens("refresh-mem"),
			stored: func() *StoredTokens {
				s := staleTokens("refresh-legacy")
				s.ClientID = ""
				return s
			}(),
			want: "refresh-legacy",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ep := newFakeTokenEndpoint(t)
			store := newFileStore(t)
			if tt.stored != nil {
				if err := store.Save(tt.stored); err != nil {
					t.Fatal(err)
				}
			}
			p := &OAuthProvider{
				config:     OAuthConfig{ClientID: testClientID},
				store:      store,
				httpClient: &http.Client{Timeout: 5 * time.Second},
				tokenURL:   ep.srv.URL,
				tokens:     tt.memory,
			}
			if _, err := p.Token(); err != nil {
				t.Fatalf("Token(): %v", err)
			}
			sent := ep.sentRefreshTokens()
			if len(sent) != 1 || sent[0] != tt.want {
				t.Errorf("refresh tokens sent = %q, want [%q]", sent, tt.want)
			}
			// The refreshed tokens are persisted for the other processes.
			saved, err := store.Load()
			if err != nil {
				t.Fatalf("loading saved tokens: %v", err)
			}
			if saved.AccessToken != "access-1" {
				t.Errorf("saved access token = %q, want access-1", saved.AccessToken)
			}
			assertNoLockFile(t, store)
		})
	}
}

func TestTokenStaleOutcomes(t *testing.T) {
	expiredAll := &StoredTokens{
		AccessToken:           "a",
		RefreshToken:          "r",
		AccessTokenExpiresAt:  time.Now().Add(-time.Hour),
		RefreshTokenExpiresAt: time.Now().Add(-time.Minute),
		ClientID:              testClientID,
	}
	freshStored := &StoredTokens{
		AccessToken:           "fresh-from-store",
		RefreshToken:          "r2",
		AccessTokenExpiresAt:  time.Now().Add(time.Hour),
		RefreshTokenExpiresAt: time.Now().Add(24 * time.Hour),
		ClientID:              testClientID,
	}
	tests := []struct {
		name     string
		memory   *StoredTokens
		stored   *StoredTokens
		status   int
		wantTok  string
		wantErr  string
		wantHits int64
	}{
		{
			name:     "everything expired and nothing stored",
			memory:   expiredAll,
			wantErr:  "tokens expired; re-authentication required",
			wantHits: 0,
		},
		{
			name:     "everything expired in memory but store has a fresh token",
			memory:   expiredAll,
			stored:   freshStored,
			wantTok:  "fresh-from-store",
			wantHits: 0,
		},
		{
			name:     "refresh rejected by endpoint",
			memory:   staleTokens("refresh-0"),
			status:   http.StatusBadRequest,
			wantErr:  "token refresh failed: OAuth token exchange failed (status 400): refresh token revoked",
			wantHits: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ep := newFakeTokenEndpoint(t)
			ep.status = tt.status
			store := newFileStore(t)
			if tt.stored != nil {
				if err := store.Save(tt.stored); err != nil {
					t.Fatal(err)
				}
			}
			p := &OAuthProvider{
				config:     OAuthConfig{ClientID: testClientID},
				store:      store,
				httpClient: &http.Client{Timeout: 5 * time.Second},
				tokenURL:   ep.srv.URL,
				tokens:     tt.memory,
			}
			tok, err := p.Token()
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("Token() err = %v, want %q", err, tt.wantErr)
				}
			} else if err != nil {
				t.Fatalf("Token(): %v", err)
			}
			if tok != tt.wantTok {
				t.Errorf("Token() = %q, want %q", tok, tt.wantTok)
			}
			if got := ep.hits.Load(); got != tt.wantHits {
				t.Errorf("endpoint hits = %d, want %d", got, tt.wantHits)
			}
			assertNoLockFile(t, store)
		})
	}
}

func TestTokenFreshDoesNotTouchLockOrStore(t *testing.T) {
	store := newFileStore(t)
	// A held lock would make a locked path wait; a fresh token must not.
	if err := os.WriteFile(lockPath(store), []byte("held"), 0600); err != nil {
		t.Fatal(err)
	}
	shortLockTimings(t, 5*time.Second)
	p := &OAuthProvider{ // #nosec G101 -- test fixture tokens, not credentials
		store: store,
		tokens: &StoredTokens{
			AccessToken:          "fresh",
			AccessTokenExpiresAt: time.Now().Add(time.Hour),
		},
		tokenURL: "http://127.0.0.1:0/unused",
	}
	start := time.Now()
	tok, err := p.Token()
	if err != nil || tok != "fresh" {
		t.Fatalf("Token() = %q, %v", tok, err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("fresh Token() took %v; it should not wait on the lock", elapsed)
	}
}

// --- lock primitives ---

func TestAcquireRefreshLock(t *testing.T) {
	tests := []struct {
		name     string
		setup    func(t *testing.T, dir string) // prepare dir before acquiring
		dir      func(dir string) string
		wantOK   bool
		wantLock bool // lock file exists while held
		maxWait  time.Duration
	}{
		{
			name:     "free lock is taken",
			wantOK:   true,
			wantLock: true,
			maxWait:  time.Second,
		},
		{
			name: "stale lock is replaced",
			setup: func(t *testing.T, dir string) {
				p := filepath.Join(dir, refreshLockName)
				if err := os.WriteFile(p, []byte("old"), 0600); err != nil {
					t.Fatal(err)
				}
				old := time.Now().Add(-time.Hour)
				if err := os.Chtimes(p, old, old); err != nil {
					t.Fatal(err)
				}
			},
			wantOK:   true,
			wantLock: true,
			maxWait:  time.Second,
		},
		{
			name: "held lock times out",
			setup: func(t *testing.T, dir string) {
				if err := os.WriteFile(filepath.Join(dir, refreshLockName), []byte("held"), 0600); err != nil {
					t.Fatal(err)
				}
			},
			wantOK:   false,
			wantLock: true, // the other holder's file
			maxWait:  2 * time.Second,
		},
		{
			name:    "missing directory fails fast",
			dir:     func(dir string) string { return filepath.Join(dir, "does", "not", "exist") },
			wantOK:  false,
			maxWait: 100 * time.Millisecond, // well under the timeout
		},
		{
			name:    "empty directory skips the lock",
			dir:     func(string) string { return "" },
			wantOK:  false,
			maxWait: 100 * time.Millisecond,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			shortLockTimings(t, 300*time.Millisecond)
			_ = captureLogs(t)
			base := t.TempDir()
			if tt.setup != nil {
				tt.setup(t, base)
			}
			dir := base
			if tt.dir != nil {
				dir = tt.dir(base)
			}

			start := time.Now()
			release, ok := acquireRefreshLock(dir)
			elapsed := time.Since(start)
			if release == nil {
				t.Fatal("release func is nil")
			}
			defer release()

			if ok != tt.wantOK {
				t.Errorf("ok = %v, want %v", ok, tt.wantOK)
			}
			if elapsed > tt.maxWait {
				t.Errorf("acquire took %v, want <= %v", elapsed, tt.maxWait)
			}
			if dir != "" {
				_, err := os.Stat(filepath.Join(dir, refreshLockName))
				if exists := err == nil; exists != tt.wantLock {
					t.Errorf("lock file exists = %v, want %v", exists, tt.wantLock)
				}
			}
		})
	}
}

func TestRefreshLockContentsAndRelease(t *testing.T) {
	dir := t.TempDir()
	release, ok := acquireRefreshLock(dir)
	if !ok {
		t.Fatal("lock not acquired")
	}
	p := filepath.Join(dir, refreshLockName)
	data, err := os.ReadFile(p) // #nosec G304 -- test reads its own temp-dir file
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), fmt.Sprintf("pid=%d\n", os.Getpid())) {
		t.Errorf("lock file missing pid: %q", data)
	}
	var stamp string
	for line := range strings.SplitSeq(string(data), "\n") {
		if v, found := strings.CutPrefix(line, "time="); found {
			stamp = v
		}
	}
	if _, err := time.Parse(time.RFC3339, stamp); err != nil {
		t.Errorf("lock file time %q is not RFC3339: %v", stamp, err)
	}

	release()
	if _, err := os.Stat(p); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("lock file still present after release: %v", err)
	}
	release() // idempotent
}

func TestRefreshLockReleaseLeavesTakenOverLock(t *testing.T) {
	dir := t.TempDir()
	release, ok := acquireRefreshLock(dir)
	if !ok {
		t.Fatal("lock not acquired")
	}
	// Simulate another process deciding our lock was stale and taking it.
	p := filepath.Join(dir, refreshLockName)
	if err := os.WriteFile(p, []byte("pid=1\nnonce=other\n"), 0600); err != nil {
		t.Fatal(err)
	}
	release()
	if _, err := os.Stat(p); err != nil {
		t.Errorf("release removed another process's lock: %v", err)
	}
}

func TestRefreshLockSerialisesHolders(t *testing.T) {
	shortLockTimings(t, 5*time.Second)
	dir := t.TempDir()

	var (
		inside  atomic.Int32
		maxSeen atomic.Int32
		wg      sync.WaitGroup
	)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, ok := acquireRefreshLock(dir)
			if !ok {
				t.Error("lock not acquired")
				return
			}
			n := inside.Add(1)
			for {
				m := maxSeen.Load()
				if n <= m || maxSeen.CompareAndSwap(m, n) {
					break
				}
			}
			time.Sleep(10 * time.Millisecond)
			inside.Add(-1)
			release()
		}()
	}
	wg.Wait()
	if got := maxSeen.Load(); got != 1 {
		t.Errorf("max concurrent holders = %d, want 1", got)
	}
}
