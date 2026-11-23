package replydesk

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- fake backend ----

type fakeBackend struct {
	mu sync.Mutex

	status StatusView
	items  []ItemView

	itemsCalls []bool
	rescans    int
	drafts     [][2]string // room, instruction
	sends      []SendRequest
	dismisses  [][2]string
	snoozes    []snoozeCall
	ctxErrs    []error // ctx.Err() seen by Rescan/Draft/Send at call time

	draftErr   *APIError
	sendID     string
	sendStatus int
	sendErr    *APIError
	sendDelay  time.Duration // how long Send takes, e.g. re-check GETs waiting on Retry-After
	dismissErr *APIError
	snoozeErr  *APIError
}

type snoozeCall struct {
	room  string
	hours int
}

func (f *fakeBackend) Status() StatusView {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status
}

func (f *fakeBackend) Items(all bool) []ItemView {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.itemsCalls = append(f.itemsCalls, all)
	return f.items
}

func (f *fakeBackend) Rescan(ctx context.Context) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rescans++
	f.ctxErrs = append(f.ctxErrs, ctx.Err())
}

func (f *fakeBackend) Draft(ctx context.Context, roomID, instruction string) *APIError {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.drafts = append(f.drafts, [2]string{roomID, instruction})
	f.ctxErrs = append(f.ctxErrs, ctx.Err())
	return f.draftErr
}

func (f *fakeBackend) Send(ctx context.Context, req SendRequest) (string, int, *APIError) {
	f.mu.Lock()
	delay := f.sendDelay
	f.mu.Unlock()
	time.Sleep(delay)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sends = append(f.sends, req)
	f.ctxErrs = append(f.ctxErrs, ctx.Err())
	return f.sendID, f.sendStatus, f.sendErr
}

func (f *fakeBackend) Dismiss(roomID, reason string) *APIError {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dismisses = append(f.dismisses, [2]string{roomID, reason})
	return f.dismissErr
}

func (f *fakeBackend) Snooze(roomID string, hours int) *APIError {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snoozes = append(f.snoozes, snoozeCall{roomID, hours})
	return f.snoozeErr
}

// mutatingCalls counts every call that is a response to a POST.
func (f *fakeBackend) mutatingCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rescans + len(f.drafts) + len(f.sends) + len(f.dismisses) + len(f.snoozes)
}

var _ Backend = (*fakeBackend)(nil)

// ---- harness ----

// lineWriter hands each Write to the test over a channel, so reading the
// launch URL is race-free.
type lineWriter chan string

func (w lineWriter) Write(p []byte) (int, error) {
	w <- string(p)
	return len(p), nil
}

var launchRE = regexp.MustCompile(`http://127\.0\.0\.1:(\d+)/login\?t=([A-Za-z0-9_-]+)`)

type harness struct {
	t      *testing.T
	s      *reviewServer
	b      *fakeBackend
	base   string // http://127.0.0.1:PORT
	host   string // 127.0.0.1:PORT
	launch string
	secret string
	client *http.Client
	done   chan error
	cancel context.CancelFunc
	cookie *http.Cookie
	openMu sync.Mutex
	opened []string
	logged string
}

type harnessOpts struct {
	opts ServeOptions
	deps serveDeps
}

func startHarness(t *testing.T, b *fakeBackend, ho harnessOpts) *harness {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	h := &harness{t: t, b: b, done: make(chan error, 1)}
	lines := make(lineWriter, 8)
	ho.opts.Log = lines
	if ho.deps.open == nil {
		ho.deps.open = func(u string) error {
			h.openMu.Lock()
			defer h.openMu.Unlock()
			h.opened = append(h.opened, u)
			return nil
		}
	}
	s, err := newReviewServer(ln, b, ho.opts, ho.deps)
	if err != nil {
		t.Fatalf("newReviewServer: %v", err)
	}
	h.s = s
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() { h.done <- s.run(ctx) }()

	select {
	case line := <-lines:
		h.logged = line
	case <-time.After(5 * time.Second):
		t.Fatal("launch URL never printed")
	}
	m := launchRE.FindStringSubmatch(h.logged)
	if m == nil {
		t.Fatalf("no launch URL in %q", h.logged)
	}
	// run prints the URL, opens the browser, then calls Rescan once; wait
	// for that so tests see a settled backend.
	waitFor(t, func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return b.rescans >= 1
	})
	h.launch = m[0]
	h.secret = m[2]
	h.host = "127.0.0.1:" + m[1]
	h.base = "http://" + h.host
	h.client = &http.Client{
		Timeout:       5 * time.Second,
		Transport:     &http.Transport{DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-h.done:
			if err != nil {
				t.Errorf("run = %v, want nil", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("server did not stop")
		}
	})
	return h
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in 5s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (h *harness) openedURLs() []string {
	h.openMu.Lock()
	defer h.openMu.Unlock()
	return append([]string(nil), h.opened...)
}

// wait returns run's result, failing if it does not return in time.
func (h *harness) wait(d time.Duration) error {
	h.t.Helper()
	select {
	case err := <-h.done:
		h.done <- err // let Cleanup see it too
		return err
	case <-time.After(d):
		h.t.Fatalf("server still running after %s", d)
		return nil
	}
}

func (h *harness) running() bool {
	select {
	case err := <-h.done:
		h.done <- err
		return false
	default:
		return true
	}
}

func (h *harness) do(req *http.Request) (*http.Response, []byte) {
	h.t.Helper()
	resp, err := h.client.Do(req) // #nosec G704 -- test client talks to the loopback server under test
	if err != nil {
		h.t.Fatalf("%s %s: %v", req.Method, req.URL.Path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatalf("read body: %v", err)
	}
	return resp, body
}

func (h *harness) newReq(method, path string, body io.Reader) *http.Request {
	h.t.Helper()
	req, err := http.NewRequest(method, h.base+path, body)
	if err != nil {
		h.t.Fatalf("new request: %v", err)
	}
	return req
}

// login runs the launch URL and keeps the session cookie.
func (h *harness) login() {
	h.t.Helper()
	resp, _ := h.do(h.newReq(http.MethodGet, strings.TrimPrefix(h.launch, h.base), nil))
	if resp.StatusCode != http.StatusSeeOther {
		h.t.Fatalf("login status = %d, want 303", resp.StatusCode)
	}
	h.cookie = sessionCookie(resp, h.s.cookieName)
	if h.cookie == nil {
		h.t.Fatal("login set no session cookie")
	}
}

// sessionCookie returns the response's cookie with this name, or nil.
func sessionCookie(resp *http.Response, name string) *http.Cookie {
	for _, c := range resp.Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func (h *harness) csrf() string {
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	return h.s.csrf
}

// get sends the session cookie and the CSRF header, as app.js does; mods can
// remove either.
func (h *harness) get(path string, mods ...func(*http.Request)) (*http.Response, []byte) {
	h.t.Helper()
	req := h.newReq(http.MethodGet, path, nil)
	if h.cookie != nil {
		req.AddCookie(h.cookie)
	}
	req.Header.Set(csrfHeaderName, h.csrf())
	for _, m := range mods {
		m(req)
	}
	return h.do(req)
}

func noCSRF(r *http.Request) { r.Header.Del(csrfHeaderName) }

// post sends a request that passes every check; mods can break one.
func (h *harness) post(path, body string, mods ...func(*http.Request)) (*http.Response, []byte) {
	h.t.Helper()
	req := h.newReq(http.MethodPost, path, strings.NewReader(body))
	if h.cookie != nil {
		req.AddCookie(h.cookie)
	}
	req.Header.Set("Origin", h.base)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(csrfHeaderName, h.csrf())
	for _, m := range mods {
		m(req)
	}
	return h.do(req)
}

func decodeAPIError(t *testing.T, body []byte) APIError {
	t.Helper()
	var e APIError
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("body %q is not an APIError: %v", body, err)
	}
	return e
}

// captureSlog sends slog output to a buffer for the rest of the test.
func captureSlog(t *testing.T) *syncBuf {
	t.Helper()
	buf := &syncBuf{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func decodesTo32Bytes(t *testing.T, what, v string) {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(v)
	if err != nil {
		t.Fatalf("%s %q is not base64url: %v", what, v, err)
	}
	if len(raw) != tokenBytes {
		t.Fatalf("%s decodes to %d bytes, want %d", what, len(raw), tokenBytes)
	}
}

// ---- tests ----

func TestServeDefaults(t *testing.T) {
	got := withServeDefaults(ServeOptions{})
	if got.IdleTimeout != 30*time.Minute || got.MaxLifetime != 8*time.Hour || got.Log != os.Stderr {
		t.Fatalf("defaults = %+v", got)
	}
	custom := ServeOptions{IdleTimeout: time.Second, MaxLifetime: time.Minute, Log: io.Discard}
	if got := withServeDefaults(custom); got != custom {
		t.Fatalf("custom options changed: %+v", got)
	}
}

func TestNewReviewServerRejectsNonLoopback(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	if _, err := newReviewServer(ln, nil, ServeOptions{}, serveDeps{}); err == nil {
		t.Fatal("nil backend accepted")
	}
	if _, err := newReviewServer(fakeListener{addr: &net.TCPAddr{IP: net.IPv4zero, Port: 8080}}, &fakeBackend{}, ServeOptions{}, serveDeps{}); err == nil {
		t.Fatal("0.0.0.0 listener accepted")
	}
	if _, err := newReviewServer(fakeListener{addr: &net.TCPAddr{IP: net.IPv6loopback, Port: 8080}}, &fakeBackend{}, ServeOptions{}, serveDeps{}); err == nil {
		t.Fatal("::1 listener accepted")
	}
}

type fakeListener struct{ addr net.Addr }

func (fakeListener) Accept() (net.Conn, error) { return nil, errors.New("fake") }
func (fakeListener) Close() error              { return nil }
func (l fakeListener) Addr() net.Addr          { return l.addr }

func TestEmbeddedAssets(t *testing.T) {
	for _, name := range []string{"web/index.html", "web/app.js", "web/app.css"} {
		if _, err := fs.Stat(webFS, name); err != nil {
			t.Errorf("%s not embedded: %v", name, err)
		}
	}
	// index.html is served as-is: no template actions, no CSRF slot.
	raw, err := fs.ReadFile(webFS, "web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"{{", "csrf-token"} {
		if bytes.Contains(raw, []byte(bad)) {
			t.Errorf("index.html contains %q", bad)
		}
	}
}

func TestOpenBrowserRejectsForeignURLs(t *testing.T) {
	for _, u := range []string{
		"https://example.com/",
		"http://localhost:1234/login?t=x",
		"file:///etc/passwd",
		"http://127.0.0.1:1/login?t=a&calc",
		"http://127.0.0.1:1/ x",
		"",
	} {
		if err := openBrowser(u); err == nil {
			t.Errorf("openBrowser(%q) accepted", u)
		}
	}
}

func TestLaunchURLAndLoginFlow(t *testing.T) {
	b := &fakeBackend{status: StatusView{Me: "Pat", DrafterMode: ModeBare}}
	h := startHarness(t, b, harnessOpts{opts: ServeOptions{OpenBrowser: true}})

	decodesTo32Bytes(t, "launch secret", h.secret)
	if opened := h.openedURLs(); len(opened) != 1 || opened[0] != h.launch {
		t.Fatalf("browser opened %v, want [%s]", opened, h.launch)
	}

	// Before login nothing but /login works.
	if resp, _ := h.get("/api/status"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("pre-login /api/status = %d", resp.StatusCode)
	}

	resp, _ := h.do(h.newReq(http.MethodGet, strings.TrimPrefix(h.launch, h.base), nil))
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login = %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/#csrf="+h.csrf() {
		t.Fatalf("Location = %q, want the CSRF token in the fragment", loc)
	}
	c := sessionCookie(resp, h.s.cookieName)
	if c == nil {
		t.Fatalf("no %s cookie", h.s.cookieName)
	}
	if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" {
		t.Fatalf("cookie attributes: HttpOnly=%v SameSite=%v Path=%q", c.HttpOnly, c.SameSite, c.Path)
	}
	decodesTo32Bytes(t, "session cookie", c.Value)
	if c.Value == h.secret {
		t.Fatal("session cookie equals the launch secret")
	}
	decodesTo32Bytes(t, "csrf token", h.csrf())
	h.cookie = c

	resp, body := h.get("/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Fatalf("GET / Content-Type = %q", ct)
	}
	if bytes.Contains(body, []byte(h.secret)) {
		t.Fatal("index page contains the launch secret")
	}
	if bytes.Contains(body, []byte(h.csrf())) {
		t.Fatal("index page contains the CSRF token")
	}

	resp, body = h.get("/api/status")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/api/status = %d", resp.StatusCode)
	}
	var st StatusView
	if err := json.Unmarshal(body, &st); err != nil || st.Me != "Pat" || st.DrafterMode != ModeBare {
		t.Fatalf("status = %+v, %v", st, err)
	}

	// The cookie and CSRF token authorize a POST.
	if resp, body := h.post("/api/scan", `{}`); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /api/scan = %d %s", resp.StatusCode, body)
	}
}

func TestStartupRescan(t *testing.T) {
	b := &fakeBackend{}
	h := startHarness(t, b, harnessOpts{})
	h.login()
	h.get("/api/status")
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.rescans != 1 {
		t.Fatalf("rescans at startup = %d, want 1", b.rescans)
	}
	if b.ctxErrs[0] != nil {
		t.Fatalf("startup Rescan got a done context: %v", b.ctxErrs[0])
	}
}

func TestLoginRejects(t *testing.T) {
	tests := []struct {
		name   string
		ttl    time.Duration
		sleep  time.Duration
		first  bool // log in once successfully before the attempt
		token  func(h *harness) string
		method string
		want   int
		reason string
	}{
		{name: "reused", first: true, token: func(h *harness) string { return h.secret }, want: 403, reason: "reused"},
		{name: "expired", ttl: 50 * time.Millisecond, sleep: 150 * time.Millisecond, token: func(h *harness) string { return h.secret }, want: 403, reason: "expired"},
		{name: "wrong", token: func(h *harness) string { return strings.Repeat("A", len(h.secret)) }, want: 403, reason: "invalid"},
		{name: "prefix", token: func(h *harness) string { return h.secret[:len(h.secret)-1] }, want: 403, reason: "invalid"},
		{name: "empty", token: func(*harness) string { return "" }, want: 403, reason: "invalid"},
		{name: "post", token: func(h *harness) string { return h.secret }, method: http.MethodPost, want: 405},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs := captureSlog(t)
			h := startHarness(t, &fakeBackend{}, harnessOpts{deps: serveDeps{launchTTL: tt.ttl}})
			if tt.first {
				h.login()
				h.cookie = nil
			}
			time.Sleep(tt.sleep)
			method := tt.method
			if method == "" {
				method = http.MethodGet
			}
			resp, _ := h.do(h.newReq(method, "/login?t="+tt.token(h), nil))
			if resp.StatusCode != tt.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.want)
			}
			for _, c := range resp.Cookies() {
				if strings.HasPrefix(c.Name, sessionCookiePrefix) {
					t.Fatal("rejected login set a session cookie")
				}
			}
			if loc := resp.Header.Get("Location"); loc != "" {
				t.Fatalf("rejected login redirected to %q", loc)
			}
			if tt.reason != "" {
				out := logs.String()
				if !strings.Contains(out, "launch token rejected") || !strings.Contains(out, "reason="+tt.reason) {
					t.Fatalf("missing warning with reason %q in %q", tt.reason, out)
				}
				if strings.Contains(out, h.secret) {
					t.Fatal("log contains the launch secret")
				}
			}
			if !tt.first {
				// No session came out of the failed attempt.
				h.s.mu.Lock()
				active := h.s.sessionActive
				h.s.mu.Unlock()
				if active {
					t.Fatal("rejected login activated the session")
				}
			}
		})
	}
}

func TestWrongTokenDoesNotBurnSecret(t *testing.T) {
	h := startHarness(t, &fakeBackend{}, harnessOpts{})
	if resp, _ := h.do(h.newReq(http.MethodGet, "/login?t=nope", nil)); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong token = %d", resp.StatusCode)
	}
	h.login() // still works
}

func TestHostCheck(t *testing.T) {
	h := startHarness(t, &fakeBackend{}, harnessOpts{})
	_, port, _ := net.SplitHostPort(h.host)
	other := strconv.Itoa(mustAtoi(t, port) + 1)
	hosts := []string{
		"localhost:" + port,
		"127.0.0.1",
		"127.0.0.1:" + other,
		"evil.example",
		"evil.example:" + port,
		"127.0.0.1:" + port + ".",
		"[::1]:" + port,
		"127.000.000.001:" + port,
		"127.0.0.1:0" + port,
	}
	for _, host := range hosts {
		for _, path := range []string{"/login?t=" + h.secret, "/", "/api/status"} {
			t.Run(host+path, func(t *testing.T) {
				req := h.newReq(http.MethodGet, path, nil)
				req.Host = host
				resp, _ := h.do(req)
				if resp.StatusCode != http.StatusForbidden {
					t.Fatalf("Host %q %s = %d, want 403", host, path, resp.StatusCode)
				}
				assertSecurityHeaders(t, resp)
			})
		}
	}
	// The secret survived all the bad-Host attempts.
	h.login()

	// A logged-in cookie does not help with a bad Host.
	req := h.newReq(http.MethodGet, "/api/status", nil)
	req.Host = "localhost:" + port
	req.AddCookie(h.cookie)
	if resp, _ := h.do(req); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("bad Host with cookie = %d", resp.StatusCode)
	}
}

func mustAtoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

var wantHeaders = map[string]string{
	"Content-Security-Policy":      "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'",
	"X-Content-Type-Options":       "nosniff",
	"Referrer-Policy":              "no-referrer",
	"Cache-Control":                "no-store",
	"X-Frame-Options":              "DENY",
	"Cross-Origin-Opener-Policy":   "same-origin",
	"Cross-Origin-Resource-Policy": "same-origin",
}

func assertSecurityHeaders(t *testing.T, resp *http.Response) {
	t.Helper()
	for k, v := range wantHeaders {
		if got := resp.Header.Get(k); got != v {
			t.Errorf("%s %s: header %s = %q, want %q", resp.Request.Method, resp.Request.URL.Path, k, got, v)
		}
	}
}

func TestSecurityHeadersOnEveryResponse(t *testing.T) {
	h := startHarness(t, &fakeBackend{}, harnessOpts{})

	check := func(name string, resp *http.Response, want int) {
		t.Helper()
		if resp.StatusCode != want {
			t.Errorf("%s: status %d, want %d", name, resp.StatusCode, want)
		}
		assertSecurityHeaders(t, resp)
	}
	r, _ := h.get("/")
	check("no-cookie /", r, 401)
	r, _ = h.get("/api/items")
	check("no-cookie api", r, 401)
	r, _ = h.do(h.newReq(http.MethodGet, "/login?t=bad", nil))
	check("bad login", r, 403)
	r, _ = h.do(h.newReq(http.MethodGet, strings.TrimPrefix(h.launch, h.base), nil))
	check("login", r, 303)
	h.cookie = sessionCookie(r, h.s.cookieName)
	r, _ = h.get("/")
	check("index", r, 200)
	r, _ = h.get("/static/app.js")
	check("js", r, 200)
	r, _ = h.get("/static/app.css")
	check("css", r, 200)
	r, _ = h.get("/api/status")
	check("status", r, 200)
	r, _ = h.get("/api/status", noCSRF)
	check("status without CSRF", r, 403)
	r, _ = h.get("/nope")
	check("404", r, 404)
	r, _ = h.do(h.newReq(http.MethodDelete, "/api/scan", nil))
	check("405", r, 405)
	r, _ = h.post("/api/scan", `{}`, func(r *http.Request) { r.Header.Del("Origin") })
	check("bad origin", r, 403)
	r, _ = h.post("/api/scan", `{}`)
	check("scan", r, 202)
}

func TestUnauthenticated(t *testing.T) {
	b := &fakeBackend{}
	h := startHarness(t, b, harnessOpts{})
	before := b.mutatingCalls()

	forged := &http.Cookie{Name: h.s.cookieName, Value: base64.RawURLEncoding.EncodeToString(make([]byte, 32))} // #nosec G124 -- test forges a client-side cookie
	// The real cookie value, presented before login activates the session.
	h.s.mu.Lock()
	early := &http.Cookie{Name: h.s.cookieName, Value: h.s.session} // #nosec G124 -- test forges a client-side cookie
	h.s.mu.Unlock()

	cookies := map[string]*http.Cookie{"none": nil, "forged": forged, "pre-login": early}
	for cname, c := range cookies {
		for _, path := range []string{"/", "/static/app.js", "/nope"} {
			t.Run(cname+path, func(t *testing.T) {
				h.cookie = c
				resp, body := h.get(path)
				if resp.StatusCode != http.StatusUnauthorized {
					t.Fatalf("status %d, want 401", resp.StatusCode)
				}
				if !bytes.Contains(body, []byte("Session expired - run webex-mcp --review again")) {
					t.Fatalf("body %q", body)
				}
			})
		}
		for _, path := range []string{"/api/status", "/api/items", "/api/nope"} {
			t.Run(cname+path, func(t *testing.T) {
				h.cookie = c
				resp, body := h.get(path)
				if resp.StatusCode != http.StatusUnauthorized {
					t.Fatalf("status %d, want 401", resp.StatusCode)
				}
				if e := decodeAPIError(t, body); e.Code != "unauthorized" {
					t.Fatalf("code %q", e.Code)
				}
			})
		}
		for _, path := range []string{"/api/scan", "/api/send", "/api/close"} {
			t.Run(cname+"POST"+path, func(t *testing.T) {
				h.cookie = c
				resp, body := h.post(path, `{"room_id":"r"}`)
				if resp.StatusCode != http.StatusUnauthorized {
					t.Fatalf("status %d, want 401", resp.StatusCode)
				}
				if e := decodeAPIError(t, body); e.Code != "unauthorized" {
					t.Fatalf("code %q", e.Code)
				}
			})
		}
	}
	if got := b.mutatingCalls(); got != before {
		t.Fatalf("backend called %d times without a session", got-before)
	}
	b.mu.Lock()
	n := len(b.itemsCalls)
	b.mu.Unlock()
	if n != 0 {
		t.Fatalf("Items called %d times without a session", n)
	}
	if !h.running() {
		t.Fatal("unauthenticated /api/close stopped the server")
	}
}

func TestPostChecks(t *testing.T) {
	b := &fakeBackend{}
	h := startHarness(t, b, harnessOpts{})
	h.login()

	setH := func(k, v string) func(*http.Request) { return func(r *http.Request) { r.Header.Set(k, v) } }
	delH := func(k string) func(*http.Request) { return func(r *http.Request) { r.Header.Del(k) } }
	addH := func(k, v string) func(*http.Request) { return func(r *http.Request) { r.Header.Add(k, v) } }

	tests := []struct {
		name     string
		path     string
		body     string
		mod      func(*http.Request)
		want     int
		wantCode string
		calls    int // backend calls expected
	}{
		{name: "baseline", path: "/api/scan", body: `{}`, want: 202, calls: 1},
		{name: "no Sec-Fetch-Site is fine", path: "/api/scan", body: `{}`, mod: delH("Sec-Fetch-Site"), want: 202, calls: 1},
		{name: "json with charset", path: "/api/scan", body: `{}`, mod: setH("Content-Type", "application/json; charset=utf-8"), want: 202, calls: 1},
		{name: "empty scan body", path: "/api/scan", body: ``, want: 202, calls: 1},

		{name: "missing Origin", path: "/api/scan", body: `{}`, mod: delH("Origin"), want: 403, wantCode: codeForbidden},
		{name: "localhost Origin", path: "/api/scan", body: `{}`, mod: setH("Origin", strings.Replace(h.base, "127.0.0.1", "localhost", 1)), want: 403, wantCode: codeForbidden},
		{name: "https Origin", path: "/api/scan", body: `{}`, mod: setH("Origin", strings.Replace(h.base, "http:", "https:", 1)), want: 403, wantCode: codeForbidden},
		{name: "Origin with slash", path: "/api/scan", body: `{}`, mod: setH("Origin", h.base+"/"), want: 403, wantCode: codeForbidden},
		{name: "null Origin", path: "/api/scan", body: `{}`, mod: setH("Origin", "null"), want: 403, wantCode: codeForbidden},
		{name: "evil Origin", path: "/api/scan", body: `{}`, mod: setH("Origin", "http://evil.example"), want: 403, wantCode: codeForbidden},
		{name: "two Origins", path: "/api/scan", body: `{}`, mod: addH("Origin", "http://evil.example"), want: 403, wantCode: codeForbidden},

		{name: "same-site", path: "/api/scan", body: `{}`, mod: setH("Sec-Fetch-Site", "same-site"), want: 403, wantCode: codeForbidden},
		{name: "cross-site", path: "/api/scan", body: `{}`, mod: setH("Sec-Fetch-Site", "cross-site"), want: 403, wantCode: codeForbidden},
		{name: "fetch-site none", path: "/api/scan", body: `{}`, mod: setH("Sec-Fetch-Site", "none"), want: 403, wantCode: codeForbidden},
		{name: "empty fetch-site", path: "/api/scan", body: `{}`, mod: setH("Sec-Fetch-Site", ""), want: 403, wantCode: codeForbidden},

		{name: "missing CSRF", path: "/api/scan", body: `{}`, mod: delH(csrfHeaderName), want: 403, wantCode: codeForbidden},
		{name: "wrong CSRF", path: "/api/scan", body: `{}`, mod: setH(csrfHeaderName, base64.RawURLEncoding.EncodeToString(make([]byte, 32))), want: 403, wantCode: codeForbidden},
		{name: "CSRF is not the cookie", path: "/api/scan", body: `{}`, mod: setH(csrfHeaderName, h.cookie.Value), want: 403, wantCode: codeForbidden},

		{name: "missing Content-Type", path: "/api/scan", body: `{}`, mod: delH("Content-Type"), want: 415, wantCode: codeUnsupportedMedia},
		{name: "text/plain", path: "/api/scan", body: `{}`, mod: setH("Content-Type", "text/plain"), want: 415, wantCode: codeUnsupportedMedia},
		{name: "form", path: "/api/scan", body: `{}`, mod: setH("Content-Type", "application/x-www-form-urlencoded"), want: 415, wantCode: codeUnsupportedMedia},
		{name: "multipart", path: "/api/scan", body: `{}`, mod: setH("Content-Type", "multipart/form-data; boundary=x"), want: 415, wantCode: codeUnsupportedMedia},
		{name: "json-ish", path: "/api/scan", body: `{}`, mod: setH("Content-Type", "application/jsonx"), want: 415, wantCode: codeUnsupportedMedia},

		{name: "oversize", path: "/api/dismiss", body: `{"room_id":"` + strings.Repeat("x", 70<<10) + `","reason":"other"}`, want: 413, wantCode: codeBodyTooLarge},
		{name: "oversize trailing", path: "/api/scan", body: `{}` + strings.Repeat(" ", 70<<10), want: 413, wantCode: codeBodyTooLarge},
		{name: "unknown field", path: "/api/dismiss", body: `{"room_id":"r","reason":"other","extra":1}`, want: 400, wantCode: codeBadRequest},
		{name: "unknown field scan", path: "/api/scan", body: `{"force":true}`, want: 400, wantCode: codeBadRequest},
		{name: "unknown field send", path: "/api/send", body: `{"room_id":"r","text":"hi","to_person_email":"x@y"}`, want: 400, wantCode: codeBadRequest},
		{name: "trailing object", path: "/api/dismiss", body: `{"room_id":"r","reason":"other"}{}`, want: 400, wantCode: codeBadRequest},
		{name: "trailing junk", path: "/api/scan", body: `{} x`, want: 400, wantCode: codeBadRequest},
		{name: "invalid JSON", path: "/api/dismiss", body: `{"room_id":`, want: 400, wantCode: codeBadRequest},
		{name: "wrong type", path: "/api/snooze", body: `{"room_id":"r","hours":"4"}`, want: 400, wantCode: codeBadRequest},
		{name: "empty dismiss body", path: "/api/dismiss", body: ``, want: 400, wantCode: codeBadRequest},
		{name: "trailing whitespace ok", path: "/api/dismiss", body: "{\"room_id\":\"r\",\"reason\":\"other\"}\n  \n", want: 200, calls: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := b.mutatingCalls()
			var mods []func(*http.Request)
			if tt.mod != nil {
				mods = append(mods, tt.mod)
			}
			resp, body := h.post(tt.path, tt.body, mods...)
			if resp.StatusCode != tt.want {
				t.Fatalf("status = %d, want %d (%s)", resp.StatusCode, tt.want, body)
			}
			if tt.wantCode != "" {
				if e := decodeAPIError(t, body); e.Code != tt.wantCode {
					t.Fatalf("code = %q, want %q", e.Code, tt.wantCode)
				}
			}
			if got := b.mutatingCalls() - before; got != tt.calls {
				t.Fatalf("backend calls = %d, want %d", got, tt.calls)
			}
		})
	}
}

func TestMethods(t *testing.T) {
	h := startHarness(t, &fakeBackend{}, harnessOpts{})
	h.login()
	tests := []struct {
		method, path string
		want         int
	}{
		{http.MethodPut, "/api/scan", 405},
		{http.MethodDelete, "/api/scan", 405},
		{http.MethodPatch, "/api/send", 405},
		{http.MethodHead, "/", 405},
		{http.MethodOptions, "/api/status", 405},
		{http.MethodGet, "/api/scan", 405},
		{http.MethodGet, "/api/send", 405},
		{http.MethodGet, "/api/close", 405},
		{http.MethodPost, "/api/status", 405},
		{http.MethodPost, "/", 405},
		{http.MethodPost, "/api/nope", 404},
	}
	for _, tt := range tests {
		t.Run(tt.method+tt.path, func(t *testing.T) {
			var resp *http.Response
			if tt.method == http.MethodPost {
				resp, _ = h.post(tt.path, `{}`)
			} else {
				req := h.newReq(tt.method, tt.path, nil)
				req.AddCookie(h.cookie)
				req.Header.Set(csrfHeaderName, h.csrf())
				resp, _ = h.do(req)
			}
			if resp.StatusCode != tt.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.want)
			}
			if tt.want == 405 && resp.Header.Get("Allow") == "" {
				t.Fatal("405 without Allow")
			}
		})
	}
	if !h.running() {
		t.Fatal("server stopped")
	}
}

func TestGetRoutes(t *testing.T) {
	jsBody, _ := fs.ReadFile(webFS, "web/app.js")
	cssBody, _ := fs.ReadFile(webFS, "web/app.css")
	tests := []struct {
		path   string
		want   int
		ctype  string
		body   []byte
		isJSON bool
	}{
		{path: "/static/app.js", want: 200, ctype: "text/javascript; charset=utf-8", body: jsBody},
		{path: "/static/app.css", want: 200, ctype: "text/css; charset=utf-8", body: cssBody},
		{path: "/static/", want: 404},
		{path: "/static/index.html", want: 404},
		{path: "/static/../web/index.html", want: 404},
		{path: "/web/app.js", want: 404},
		{path: "/index.html", want: 404},
		{path: "/favicon.ico", want: 404},
		{path: "/api/nope", want: 404, isJSON: true},
		{path: "/api/", want: 404, isJSON: true},
	}
	h := startHarness(t, &fakeBackend{}, harnessOpts{})
	h.login()
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			resp, body := h.get(tt.path)
			if resp.StatusCode != tt.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.want)
			}
			if tt.ctype != "" && resp.Header.Get("Content-Type") != tt.ctype {
				t.Fatalf("Content-Type = %q, want %q", resp.Header.Get("Content-Type"), tt.ctype)
			}
			if tt.body != nil && !bytes.Equal(body, tt.body) {
				t.Fatal("body differs from the embedded file")
			}
			if tt.isJSON {
				if e := decodeAPIError(t, body); e.Code != codeNotFound {
					t.Fatalf("code = %q", e.Code)
				}
			}
		})
	}
}

func TestItems(t *testing.T) {
	tests := []struct {
		name    string
		query   string
		items   []ItemView
		wantAll bool
		want    string
	}{
		{name: "nil is empty array", query: "", items: nil, want: `{"items":[]}`},
		{name: "empty", query: "?all=0", items: []ItemView{}, want: `{"items":[]}`},
		{name: "all", query: "?all=1", items: nil, wantAll: true, want: `{"items":[]}`},
		{name: "all=true is not all", query: "?all=true", items: nil, want: `{"items":[]}`},
		{name: "one item", query: "", items: []ItemView{{RoomID: "r1", Title: "Ana", State: StatePending}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := &fakeBackend{items: tt.items}
			h := startHarness(t, b, harnessOpts{})
			h.login()
			resp, body := h.get("/api/items" + tt.query)
			if resp.StatusCode != 200 {
				t.Fatalf("status %d", resp.StatusCode)
			}
			if ct := resp.Header.Get("Content-Type"); ct != "application/json; charset=utf-8" {
				t.Fatalf("Content-Type %q", ct)
			}
			if tt.want != "" && strings.TrimSpace(string(body)) != tt.want {
				t.Fatalf("body %s, want %s", body, tt.want)
			}
			var got struct{ Items []ItemView }
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatal(err)
			}
			if len(got.Items) != len(tt.items) || (len(tt.items) > 0 && got.Items[0].RoomID != tt.items[0].RoomID) {
				t.Fatalf("items %+v", got.Items)
			}
			b.mu.Lock()
			defer b.mu.Unlock()
			if len(b.itemsCalls) != 1 || b.itemsCalls[0] != tt.wantAll {
				t.Fatalf("Items calls %v, want [%v]", b.itemsCalls, tt.wantAll)
			}
		})
	}
}

func TestScanRoute(t *testing.T) {
	b := &fakeBackend{}
	h := startHarness(t, b, harnessOpts{})
	h.login()
	resp, body := h.post("/api/scan", `{}`)
	if resp.StatusCode != 202 || strings.TrimSpace(string(body)) != `{"ok":true}` {
		t.Fatalf("scan = %d %s", resp.StatusCode, body)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.rescans != 2 { // startup + this one
		t.Fatalf("rescans = %d, want 2", b.rescans)
	}
	if b.ctxErrs[1] != nil {
		t.Fatalf("Rescan got a done context: %v", b.ctxErrs[1])
	}
}

func TestDraftRoute(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		backend   *APIError
		want      int
		wantCode  string
		wantCall  bool
		wantInstr string
	}{
		{name: "ok", body: `{"room_id":"r1","instruction":"shorter"}`, want: 202, wantCall: true, wantInstr: "shorter"},
		{name: "no instruction", body: `{"room_id":"r1"}`, want: 202, wantCall: true},
		{name: "instruction at limit", body: `{"room_id":"r1","instruction":"` + strings.Repeat("a", 1000) + `"}`, want: 202, wantCall: true, wantInstr: strings.Repeat("a", 1000)},
		{name: "instruction too long", body: `{"room_id":"r1","instruction":"` + strings.Repeat("a", 1001) + `"}`, want: 400, wantCode: CodeTooLong},
		{name: "instruction too long in bytes", body: `{"room_id":"r1","instruction":"` + strings.Repeat("é", 501) + `"}`, want: 400, wantCode: CodeTooLong},
		{name: "missing room", body: `{"instruction":"x"}`, want: 400, wantCode: codeBadRequest},
		{name: "not pending", body: `{"room_id":"r1"}`, backend: &APIError{Code: CodeNotPending, Detail: "gone"}, want: 404, wantCode: CodeNotPending, wantCall: true},
		{name: "rate limited", body: `{"room_id":"r1"}`, backend: &APIError{Code: CodeRateLimited}, want: 429, wantCode: CodeRateLimited, wantCall: true},
		{name: "drafter off", body: `{"room_id":"r1"}`, backend: &APIError{Code: CodeDrafterOff, Detail: "no claude"}, want: 400, wantCode: CodeDrafterOff, wantCall: true},
		{name: "busy", body: `{"room_id":"r1","instruction":"shorter"}`, backend: &APIError{Code: CodeBusy, Detail: "drafting"}, want: 409, wantCode: CodeBusy, wantCall: true, wantInstr: "shorter"},
		{name: "signed out", body: `{"room_id":"r1"}`, backend: &APIError{Code: CodeSignedOut, Detail: "logged out"}, want: 409, wantCode: CodeSignedOut, wantCall: true},
		{name: "other error", body: `{"room_id":"r1"}`, backend: &APIError{Code: "store", Detail: "disk"}, want: 400, wantCode: "store", wantCall: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := &fakeBackend{draftErr: tt.backend}
			h := startHarness(t, b, harnessOpts{})
			h.login()
			resp, body := h.post("/api/draft", tt.body)
			if resp.StatusCode != tt.want {
				t.Fatalf("status %d, want %d (%s)", resp.StatusCode, tt.want, body)
			}
			if tt.wantCode != "" {
				e := decodeAPIError(t, body)
				if e.Code != tt.wantCode {
					t.Fatalf("code %q, want %q", e.Code, tt.wantCode)
				}
				if tt.backend != nil && e.Detail != tt.backend.Detail {
					t.Fatalf("detail %q not passed through", e.Detail)
				}
			} else if strings.TrimSpace(string(body)) != `{"ok":true}` {
				t.Fatalf("body %s", body)
			}
			b.mu.Lock()
			defer b.mu.Unlock()
			if !tt.wantCall {
				if len(b.drafts) != 0 {
					t.Fatalf("Draft called: %v", b.drafts)
				}
				return
			}
			if len(b.drafts) != 1 || b.drafts[0] != [2]string{"r1", tt.wantInstr} {
				t.Fatalf("Draft calls %v", b.drafts)
			}
		})
	}
}

func TestSendRoute(t *testing.T) {
	req := SendRequest{RoomID: "r1", HeadID: "head-1", Text: "hello", TextSHA256: strings.Repeat("ab", 32), AckSanitized: true}
	reqJSON, _ := json.Marshal(req)
	if !strings.Contains(string(reqJSON), `"head_id":"head-1"`) {
		t.Fatalf("request JSON %s lacks head_id", reqJSON)
	}
	tests := []struct {
		name     string
		body     string
		id       string
		status   int
		apiErr   *APIError
		want     int
		wantBody string
		wantCall bool
	}{
		{name: "ok", body: string(reqJSON), id: "m-123", status: 200, want: 200, wantBody: `{"message_id":"m-123"}`, wantCall: true},
		{name: "ok ignores status", body: string(reqJSON), id: "m-9", status: 0, want: 200, wantBody: `{"message_id":"m-9"}`, wantCall: true},
		{name: "sanitized passthrough", body: string(reqJSON), status: 409,
			apiErr: &APIError{Code: CodeSanitized, Detail: "changed", SanitizedText: "hello [link removed]"},
			want:   409, wantBody: `{"code":"sanitized","detail":"changed","sanitized_text":"hello [link removed]"}`, wantCall: true},
		{name: "new messages", body: string(reqJSON), status: 409, apiErr: &APIError{Code: CodeNewMessages}, want: 409, wantCall: true},
		{name: "too long", body: string(reqJSON), status: 400, apiErr: &APIError{Code: CodeTooLong}, want: 400, wantCall: true},
		{name: "rate limited", body: string(reqJSON), status: 429, apiErr: &APIError{Code: CodeRateLimited}, want: 429, wantCall: true},
		{name: "send unknown", body: string(reqJSON), status: 502, apiErr: &APIError{Code: CodeSendUnknown}, want: 502, wantCall: true},
		{name: "bad status becomes 500", body: string(reqJSON), status: 200, apiErr: &APIError{Code: "weird"}, want: 500, wantCall: true},
		{name: "zero status becomes 500", body: string(reqJSON), status: 0, apiErr: &APIError{Code: "weird"}, want: 500, wantCall: true},
		{name: "signed out is 409", body: string(reqJSON), status: 400, apiErr: &APIError{Code: CodeSignedOut, Detail: "logged out"}, want: 409, wantCall: true},
		{name: "busy is 409", body: string(reqJSON), status: 500, apiErr: &APIError{Code: CodeBusy}, want: 409, wantCall: true},
		{name: "drafter off is 400", body: string(reqJSON), status: 0, apiErr: &APIError{Code: CodeDrafterOff}, want: 400, wantCall: true},
		{name: "missing room", body: `{"text":"hi"}`, want: 400},
		{name: "browser cannot pick a target", body: `{"room_id":"r1","text":"hi","parent_id":"p"}`, want: 400},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := &fakeBackend{sendID: tt.id, sendStatus: tt.status, sendErr: tt.apiErr}
			h := startHarness(t, b, harnessOpts{})
			h.login()
			resp, body := h.post("/api/send", tt.body)
			if resp.StatusCode != tt.want {
				t.Fatalf("status %d, want %d (%s)", resp.StatusCode, tt.want, body)
			}
			if tt.wantBody != "" && strings.TrimSpace(string(body)) != tt.wantBody {
				t.Fatalf("body %s, want %s", body, tt.wantBody)
			}
			if tt.apiErr != nil {
				if e := decodeAPIError(t, body); e != *tt.apiErr {
					t.Fatalf("error %+v, want %+v", e, *tt.apiErr)
				}
			}
			b.mu.Lock()
			defer b.mu.Unlock()
			if !tt.wantCall {
				if len(b.sends) != 0 {
					t.Fatalf("Send called: %v", b.sends)
				}
				return
			}
			if len(b.sends) != 1 || b.sends[0] != req {
				t.Fatalf("Send calls %+v, want [%+v]", b.sends, req)
			}
		})
	}
}

func TestDismissRoute(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		backend  *APIError
		want     int
		wantCode string
		wantCall [2]string
	}{
		{name: DismissNoReplyNeeded, body: `{"room_id":"r1","reason":"no_reply_needed"}`, want: 200, wantCall: [2]string{"r1", DismissNoReplyNeeded}},
		{name: DismissRepliedElsewhr, body: `{"room_id":"r1","reason":"replied_elsewhere"}`, want: 200, wantCall: [2]string{"r1", DismissRepliedElsewhr}},
		{name: DismissReacted, body: `{"room_id":"r1","reason":"reacted"}`, want: 200, wantCall: [2]string{"r1", DismissReacted}},
		{name: DismissOther, body: `{"room_id":"r1","reason":"other"}`, want: 200, wantCall: [2]string{"r1", DismissOther}},
		{name: "unknown reason", body: `{"room_id":"r1","reason":"bored"}`, want: 400, wantCode: codeBadRequest},
		{name: "empty reason", body: `{"room_id":"r1"}`, want: 400, wantCode: codeBadRequest},
		{name: "case matters", body: `{"room_id":"r1","reason":"OTHER"}`, want: 400, wantCode: codeBadRequest},
		{name: "missing room", body: `{"reason":"other"}`, want: 400, wantCode: codeBadRequest},
		{name: "not pending", body: `{"room_id":"r1","reason":"other"}`, backend: &APIError{Code: CodeNotPending}, want: 404, wantCode: CodeNotPending, wantCall: [2]string{"r1", DismissOther}},
		{name: "backend error", body: `{"room_id":"r1","reason":"other"}`, backend: &APIError{Code: "store"}, want: 400, wantCode: "store", wantCall: [2]string{"r1", DismissOther}},
		{name: "signed out", body: `{"room_id":"r1","reason":"other"}`, backend: &APIError{Code: CodeSignedOut}, want: 409, wantCode: CodeSignedOut, wantCall: [2]string{"r1", DismissOther}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := &fakeBackend{dismissErr: tt.backend}
			h := startHarness(t, b, harnessOpts{})
			h.login()
			resp, body := h.post("/api/dismiss", tt.body)
			if resp.StatusCode != tt.want {
				t.Fatalf("status %d, want %d (%s)", resp.StatusCode, tt.want, body)
			}
			if tt.wantCode != "" {
				if e := decodeAPIError(t, body); e.Code != tt.wantCode {
					t.Fatalf("code %q, want %q", e.Code, tt.wantCode)
				}
			} else if strings.TrimSpace(string(body)) != `{"ok":true}` {
				t.Fatalf("body %s", body)
			}
			b.mu.Lock()
			defer b.mu.Unlock()
			if tt.wantCall == ([2]string{}) {
				if len(b.dismisses) != 0 {
					t.Fatalf("Dismiss called: %v", b.dismisses)
				}
				return
			}
			if len(b.dismisses) != 1 || b.dismisses[0] != tt.wantCall {
				t.Fatalf("Dismiss calls %v, want [%v]", b.dismisses, tt.wantCall)
			}
		})
	}
}

func TestSnoozeRoute(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		backend  *APIError
		want     int
		wantCode string
		wantCall *snoozeCall
	}{
		{name: "1h", body: `{"room_id":"r1","hours":1}`, want: 200, wantCall: &snoozeCall{"r1", 1}},
		{name: "168h", body: `{"room_id":"r1","hours":168}`, want: 200, wantCall: &snoozeCall{"r1", 168}},
		{name: "24h", body: `{"room_id":"r1","hours":24}`, want: 200, wantCall: &snoozeCall{"r1", 24}},
		{name: "0h", body: `{"room_id":"r1","hours":0}`, want: 400, wantCode: codeBadRequest},
		{name: "missing hours", body: `{"room_id":"r1"}`, want: 400, wantCode: codeBadRequest},
		{name: "negative", body: `{"room_id":"r1","hours":-5}`, want: 400, wantCode: codeBadRequest},
		{name: "169h", body: `{"room_id":"r1","hours":169}`, want: 400, wantCode: codeBadRequest},
		{name: "fractional", body: `{"room_id":"r1","hours":1.5}`, want: 400, wantCode: codeBadRequest},
		{name: "missing room", body: `{"hours":2}`, want: 400, wantCode: codeBadRequest},
		{name: "not pending", body: `{"room_id":"r1","hours":2}`, backend: &APIError{Code: CodeNotPending}, want: 404, wantCode: CodeNotPending, wantCall: &snoozeCall{"r1", 2}},
		{name: "signed out", body: `{"room_id":"r1","hours":2}`, backend: &APIError{Code: CodeSignedOut}, want: 409, wantCode: CodeSignedOut, wantCall: &snoozeCall{"r1", 2}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := &fakeBackend{snoozeErr: tt.backend}
			h := startHarness(t, b, harnessOpts{})
			h.login()
			resp, body := h.post("/api/snooze", tt.body)
			if resp.StatusCode != tt.want {
				t.Fatalf("status %d, want %d (%s)", resp.StatusCode, tt.want, body)
			}
			if tt.wantCode != "" {
				if e := decodeAPIError(t, body); e.Code != tt.wantCode {
					t.Fatalf("code %q, want %q", e.Code, tt.wantCode)
				}
			}
			b.mu.Lock()
			defer b.mu.Unlock()
			if tt.wantCall == nil {
				if len(b.snoozes) != 0 {
					t.Fatalf("Snooze called: %v", b.snoozes)
				}
				return
			}
			if len(b.snoozes) != 1 || b.snoozes[0] != *tt.wantCall {
				t.Fatalf("Snooze calls %v, want [%v]", b.snoozes, *tt.wantCall)
			}
		})
	}
}

// TestCSRFOnlyInLoginFragment: anyone holding the cookie gets GET /, so the
// page must not carry the token; it arrives once, in the /login redirect.
func TestCSRFOnlyInLoginFragment(t *testing.T) {
	h := startHarness(t, &fakeBackend{}, harnessOpts{})
	resp, _ := h.do(h.newReq(http.MethodGet, strings.TrimPrefix(h.launch, h.base), nil))
	loc := resp.Header.Get("Location")
	token, ok := strings.CutPrefix(loc, "/#csrf=")
	if !ok || token == "" {
		t.Fatalf("Location = %q, want /#csrf=<token>", loc)
	}
	decodesTo32Bytes(t, "fragment token", token)
	if token != h.csrf() {
		t.Fatal("fragment token differs from the session token")
	}
	h.cookie = sessionCookie(resp, h.s.cookieName)

	// GET / needs only the cookie, and does not hand out the token.
	resp, body := h.get("/", noCSRF)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / = %d", resp.StatusCode)
	}
	if bytes.Contains(body, []byte(token)) {
		t.Fatal("index page contains the CSRF token")
	}
	for _, path := range []string{"/static/app.js", "/static/app.css"} {
		if resp, _ := h.get(path, noCSRF); resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s without CSRF = %d", path, resp.StatusCode)
		}
	}
	// The fragment token is the one the API needs.
	setTok := func(r *http.Request) { r.Header.Set(csrfHeaderName, token) }
	if resp, _ := h.get("/api/status", setTok); resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/status with fragment token = %d", resp.StatusCode)
	}
	if resp, _ := h.post("/api/scan", `{}`, setTok); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST with fragment token = %d", resp.StatusCode)
	}
}

// TestAPIGetsRequireCSRF: the cookie reaches this server from a page on any
// 127.0.0.1 port, so reads need the CSRF header too.
func TestAPIGetsRequireCSRF(t *testing.T) {
	b := &fakeBackend{items: []ItemView{{RoomID: "r1", Title: "Ana"}}}
	h := startHarness(t, b, harnessOpts{})
	h.login()
	mods := map[string]func(*http.Request){
		"missing": noCSRF,
		"wrong": func(r *http.Request) {
			r.Header.Set(csrfHeaderName, base64.RawURLEncoding.EncodeToString(make([]byte, 32)))
		},
		"prefix": func(r *http.Request) { r.Header.Set(csrfHeaderName, h.csrf()[:len(h.csrf())-1]) },
		"cookie": func(r *http.Request) { r.Header.Set(csrfHeaderName, h.cookie.Value) },
	}
	for name, mod := range mods {
		for _, path := range []string{"/api/status", "/api/items", "/api/items?all=1", "/api/nope"} {
			t.Run(name+path, func(t *testing.T) {
				resp, body := h.get(path, mod)
				if resp.StatusCode != http.StatusForbidden {
					t.Fatalf("status %d, want 403 (%s)", resp.StatusCode, body)
				}
				if e := decodeAPIError(t, body); e.Code != codeForbidden {
					t.Fatalf("code %q", e.Code)
				}
				if bytes.Contains(body, []byte("Ana")) {
					t.Fatal("refused response leaked inbox data")
				}
			})
		}
	}
	b.mu.Lock()
	n := len(b.itemsCalls)
	b.mu.Unlock()
	if n != 0 {
		t.Fatalf("Items called %d times without the CSRF header", n)
	}
	if resp, _ := h.get("/api/items"); resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/items with CSRF = %d", resp.StatusCode)
	}
}

// TestCookieNamedPerPort: cookies ignore ports, so the name carries it.
func TestCookieNamedPerPort(t *testing.T) {
	h := startHarness(t, &fakeBackend{}, harnessOpts{})
	_, port, err := net.SplitHostPort(h.host)
	if err != nil {
		t.Fatal(err)
	}
	if want := "rd_s_" + port; h.s.cookieName != want {
		t.Fatalf("cookie name %q, want %q", h.s.cookieName, want)
	}
	h.login()
	if h.cookie.Name != "rd_s_"+port {
		t.Fatalf("login set cookie %q", h.cookie.Name)
	}
	// The right value under another server's name does not authenticate.
	value := h.cookie.Value
	other := strconv.Itoa(mustAtoi(t, port) + 1)
	for _, name := range []string{"rd_s", "rd_s_" + other} {
		h.cookie = &http.Cookie{Name: name, Value: value} // #nosec G124 -- test forges a client-side cookie
		if resp, _ := h.get("/api/status"); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("cookie %q = %d, want 401", name, resp.StatusCode)
		}
	}
}

// TestSlowSendSurvivesWriteTimeout: a send that outlasts the server's
// WriteTimeout (re-check GETs waiting on Retry-After) must still deliver its
// result, not a dropped connection after the message went out.
func TestSlowSendSurvivesWriteTimeout(t *testing.T) {
	b := &fakeBackend{sendID: "m-slow", sendStatus: 200, sendDelay: 600 * time.Millisecond}
	h := startHarness(t, b, harnessOpts{deps: serveDeps{writeTimeout: 150 * time.Millisecond}})
	h.login()
	req := h.newReq(http.MethodPost, "/api/send", strings.NewReader(`{"room_id":"r1","head_id":"h1","text":"hi","text_sha256":"00"}`))
	req.AddCookie(h.cookie)
	req.Header.Set("Origin", h.base)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(csrfHeaderName, h.csrf())
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("slow send lost its response: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("slow send body: %v", err)
	}
	if resp.StatusCode != http.StatusOK || strings.TrimSpace(string(body)) != `{"message_id":"m-slow"}` {
		t.Fatalf("slow send = %d %s", resp.StatusCode, body)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.sends) != 1 || b.sends[0].HeadID != "h1" {
		t.Fatalf("Send calls %+v", b.sends)
	}
}

func TestServeTimeoutDefaults(t *testing.T) {
	h := startHarness(t, &fakeBackend{}, harnessOpts{deps: serveDeps{writeTimeout: 150 * time.Millisecond}})
	if h.s.writeTimeout != 150*time.Millisecond || h.s.sendWriteTimeout != defaultSendWriteTimeout {
		t.Fatalf("timeouts = %s / %s", h.s.writeTimeout, h.s.sendWriteTimeout)
	}
	d := startHarness(t, &fakeBackend{}, harnessOpts{})
	if d.s.writeTimeout != defaultWriteTimeout || d.s.sendWriteTimeout < 2*time.Minute {
		t.Fatalf("default timeouts = %s / %s", d.s.writeTimeout, d.s.sendWriteTimeout)
	}
}

func TestIdleTimeoutShutdown(t *testing.T) {
	h := startHarness(t, &fakeBackend{}, harnessOpts{opts: ServeOptions{IdleTimeout: 200 * time.Millisecond, MaxLifetime: time.Hour}})
	if err := h.wait(5 * time.Second); err != nil {
		t.Fatalf("run = %v, want nil", err)
	}
}

func TestActivityDefersIdleTimeout(t *testing.T) {
	h := startHarness(t, &fakeBackend{}, harnessOpts{opts: ServeOptions{IdleTimeout: 500 * time.Millisecond, MaxLifetime: time.Hour}})
	h.login()
	deadline := time.Now().Add(1500 * time.Millisecond)
	for i := 0; time.Now().Before(deadline); i++ {
		// User actions: opening the page and POSTs.
		if i%2 == 0 {
			if resp, _ := h.get("/"); resp.StatusCode != 200 {
				t.Fatalf("GET / %d", resp.StatusCode)
			}
		} else if resp, _ := h.post("/api/scan", `{}`); resp.StatusCode != 202 {
			t.Fatalf("POST /api/scan %d", resp.StatusCode)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !h.running() {
		t.Fatal("server stopped despite activity")
	}
	// Unauthenticated requests do not count as activity.
	h.cookie = nil
	stop := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(stop) && h.running() {
		_, _ = h.client.Get(h.base + "/api/status")
		time.Sleep(50 * time.Millisecond)
	}
	if err := h.wait(5 * time.Second); err != nil {
		t.Fatalf("run = %v", err)
	}
}

// TestPollsDoNotDeferIdleTimeout: an open tab polls status and items in the
// background; that alone must not keep the server alive.
func TestPollsDoNotDeferIdleTimeout(t *testing.T) {
	h := startHarness(t, &fakeBackend{}, harnessOpts{opts: ServeOptions{IdleTimeout: 400 * time.Millisecond, MaxLifetime: time.Hour}})
	h.login()
	start := time.Now()
	for h.running() && time.Since(start) < 3*time.Second {
		for _, path := range []string{"/api/status", "/api/items", "/static/app.js"} {
			req := h.newReq(http.MethodGet, path, nil)
			req.AddCookie(h.cookie)
			req.Header.Set(csrfHeaderName, h.csrf())
			if resp, err := h.client.Do(req); err == nil {
				_ = resp.Body.Close()
			}
		}
		time.Sleep(30 * time.Millisecond)
	}
	if h.running() {
		t.Fatal("background polls kept the server alive past the idle timeout")
	}
	if err := h.wait(5 * time.Second); err != nil {
		t.Fatalf("run = %v", err)
	}
}

func TestMaxLifetimeShutdown(t *testing.T) {
	h := startHarness(t, &fakeBackend{}, harnessOpts{opts: ServeOptions{IdleTimeout: time.Hour, MaxLifetime: 400 * time.Millisecond}})
	h.login()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			req, _ := http.NewRequest(http.MethodGet, h.base+"/api/status", nil)
			req.AddCookie(h.cookie)
			if resp, err := h.client.Do(req); err == nil {
				_ = resp.Body.Close()
			}
			time.Sleep(30 * time.Millisecond)
		}
	}()
	if err := h.wait(5 * time.Second); err != nil {
		t.Fatalf("run = %v", err)
	}
}

func TestContextCancelShutdown(t *testing.T) {
	h := startHarness(t, &fakeBackend{}, harnessOpts{})
	h.cancel()
	if err := h.wait(5 * time.Second); err != nil {
		t.Fatalf("run = %v", err)
	}
}

func TestCloseRoute(t *testing.T) {
	h := startHarness(t, &fakeBackend{}, harnessOpts{})
	h.login()
	resp, body := h.post("/api/close", `{}`)
	if resp.StatusCode != 200 || strings.TrimSpace(string(body)) != `{"ok":true}` {
		t.Fatalf("close = %d %s", resp.StatusCode, body)
	}
	if err := h.wait(5 * time.Second); err != nil {
		t.Fatalf("run = %v", err)
	}
	if _, err := h.client.Get(h.base + "/"); err == nil {
		t.Fatal("server still accepting connections after close")
	}
}

func TestCloseRequiresChecks(t *testing.T) {
	h := startHarness(t, &fakeBackend{}, harnessOpts{})
	h.login()
	resp, _ := h.post("/api/close", `{}`, func(r *http.Request) { r.Header.Set("Origin", "http://evil.example") })
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin close = %d", resp.StatusCode)
	}
	resp, _ = h.post("/api/close", `{"now":true}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("close with unknown field = %d", resp.StatusCode)
	}
	time.Sleep(50 * time.Millisecond)
	if !h.running() {
		t.Fatal("rejected close stopped the server")
	}
}

func TestServeBindsLoopbackAndStops(t *testing.T) {
	lines := make(lineWriter, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := &fakeBackend{}
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, b, ServeOptions{Log: lines}) }()
	var line string
	select {
	case line = <-lines:
	case <-time.After(5 * time.Second):
		t.Fatal("no launch URL")
	}
	m := launchRE.FindStringSubmatch(line)
	if m == nil {
		t.Fatalf("no launch URL in %q", line)
	}
	resp, err := (&http.Client{
		Timeout:       5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}).Get(m[0])
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login via Serve = %d", resp.StatusCode)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return")
	}
}

func TestBrowserFailureIsNotFatal(t *testing.T) {
	logs := captureSlog(t)
	h := startHarness(t, &fakeBackend{}, harnessOpts{
		opts: ServeOptions{OpenBrowser: true},
		deps: serveDeps{open: func(string) error { return errors.New("no display") }},
	})
	h.login()
	if !h.running() {
		t.Fatal("server stopped after browser failure")
	}
	if !strings.Contains(logs.String(), "could not open a browser") {
		t.Fatalf("no warning in %q", logs.String())
	}
}

// TestStdoutUntouched runs a full session with os.Stdout swapped for a pipe;
// stdout is the MCP channel elsewhere in this binary and must stay silent.
func TestStdoutUntouched(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	restored := false
	restore := func() {
		if !restored {
			os.Stdout = orig
			restored = true
		}
	}
	defer restore()

	b := &fakeBackend{items: []ItemView{{RoomID: "r1"}}, sendID: "m1"}
	h := startHarness(t, b, harnessOpts{opts: ServeOptions{OpenBrowser: true}})
	_, _ = h.do(h.newReq(http.MethodGet, "/login?t=wrong", nil))
	h.login()
	_, _ = h.do(h.newReq(http.MethodGet, strings.TrimPrefix(h.launch, h.base), nil)) // reuse
	h.get("/")
	h.get("/static/app.js")
	h.get("/static/app.css")
	h.get("/api/status")
	h.get("/api/items?all=1")
	h.post("/api/scan", `{}`)
	h.post("/api/draft", `{"room_id":"r1","instruction":"x"}`)
	h.post("/api/send", `{"room_id":"r1","text":"hi","text_sha256":"00"}`)
	h.post("/api/dismiss", `{"room_id":"r1","reason":"other"}`)
	h.post("/api/snooze", `{"room_id":"r1","hours":2}`)
	h.post("/api/scan", `{}`, func(r *http.Request) { r.Header.Del("Origin") })
	h.post("/api/close", `{}`)
	if err := h.wait(5 * time.Second); err != nil {
		t.Fatalf("run = %v", err)
	}

	restore()
	_ = w.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 {
		t.Fatalf("server wrote %d bytes to stdout: %q", len(out), out)
	}
}
