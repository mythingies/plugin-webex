package replydesk

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// webFS holds the review UI assets, served as-is. index.html carries no
// secrets: the CSRF token reaches the page only in the /login redirect's URL
// fragment (see handleLogin).
//
//go:embed web
var webFS embed.FS

// ServeOptions configures the review UI server.
type ServeOptions struct {
	OpenBrowser bool          // open the launch URL in the default browser
	IdleTimeout time.Duration // default 30m
	MaxLifetime time.Duration // default 8h
	Log         io.Writer     // where the launch URL is printed; default os.Stderr
}

const (
	defaultServeIdle     = 30 * time.Minute
	defaultServeLifetime = 8 * time.Hour
	// defaultLaunchTTL is how long the launch secret in the printed URL can be
	// exchanged for the session cookie.
	defaultLaunchTTL = 2 * time.Minute

	// The session cookie is named sessionCookiePrefix + "_" + PORT. Cookies
	// ignore ports, so every server on 127.0.0.1 receives every rd_s_*
	// cookie; the suffix keeps one review server from accepting, or
	// overwriting, another's session.
	sessionCookiePrefix = "rd_s"
	csrfHeaderName      = "X-ReplyDesk-CSRF"
	// csrfFragment is the URL fragment key /login redirects with. A fragment
	// is never sent to a server or in a Referer, and app.js removes it.
	csrfFragment      = "csrf"
	tokenBytes        = 32
	maxRequestBody    = 64 << 10
	maxInstructionLen = 1000
	minSnoozeHours    = 1
	maxSnoozeHours    = 168
	shutdownGrace     = 5 * time.Second

	// defaultWriteTimeout bounds writing an ordinary response.
	defaultWriteTimeout = 30 * time.Second
	// defaultSendWriteTimeout replaces it for /api/send, whose pre-send
	// re-checks can wait out Retry-After twice before the POST to Webex. If
	// the shorter deadline passed first, the message would be posted but the
	// browser would be told the server was unreachable.
	defaultSendWriteTimeout = 3 * time.Minute
)

// Error codes the server itself returns (the Backend returns the Code*
// constants in types.go).
const (
	codeUnauthorized     = "unauthorized"
	codeForbidden        = "forbidden"
	codeBadRequest       = "bad_request"
	codeUnsupportedMedia = "unsupported_media_type"
	codeBodyTooLarge     = "body_too_large"
	codeMethodNotAllowed = "method_not_allowed"
	codeNotFound         = "not_found"
	codeInternal         = "internal"
)

// securityHeaders are set on every response, including errors.
var securityHeaders = [][2]string{
	{"Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"},
	{"X-Content-Type-Options", "nosniff"},
	{"Referrer-Policy", "no-referrer"},
	{"Cache-Control", "no-store"},
	{"X-Frame-Options", "DENY"},
	{"Cross-Origin-Opener-Policy", "same-origin"},
	{"Cross-Origin-Resource-Policy", "same-origin"},
}

var validDismissReasons = map[string]bool{
	DismissNoReplyNeeded:  true,
	DismissRepliedElsewhr: true,
	DismissReacted:        true,
	DismissOther:          true,
}

const expiredPage = `<!doctype html>
<html lang="en">
<head><meta charset="utf-8"><title>Reply Desk</title></head>
<body><p>Session expired - run webex-mcp --review again</p></body>
</html>
`

const forbiddenPage = `<!doctype html>
<html lang="en">
<head><meta charset="utf-8"><title>Reply Desk</title></head>
<body><p>This launch link is invalid, expired, or already used - run webex-mcp --review again</p></body>
</html>
`

// Serve runs the loopback review UI until ctx is done, the idle or lifetime
// limit passes, or the user closes it. It returns nil on those normal
// shutdowns.
func Serve(ctx context.Context, b Backend, opts ServeOptions) error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("replydesk: listen: %w", err)
	}
	s, err := newReviewServer(ln, b, opts, serveDeps{})
	if err != nil {
		_ = ln.Close()
		return err
	}
	return s.run(ctx)
}

// serveDeps are the seams tests replace.
type serveDeps struct {
	open             func(url string) error // default openBrowser
	launchTTL        time.Duration          // default defaultLaunchTTL
	writeTimeout     time.Duration          // default defaultWriteTimeout
	sendWriteTimeout time.Duration          // default defaultSendWriteTimeout
}

// reviewServer is one review UI process: one launch secret, one session.
type reviewServer struct {
	b      Backend
	ln     net.Listener
	host   string // "127.0.0.1:PORT", the only accepted Host header
	origin string // "http://127.0.0.1:PORT", the only accepted Origin
	opts   ServeOptions
	open   func(string) error
	index  []byte
	js     []byte
	css    []byte

	cookieName       string // sessionCookiePrefix + "_" + PORT
	writeTimeout     time.Duration
	sendWriteTimeout time.Duration

	idle     time.Duration
	lifetime time.Duration
	started  time.Time

	closeOnce sync.Once
	closeCh   chan struct{}

	mu            sync.Mutex
	secret        string
	secretExpires time.Time
	secretUsed    bool
	session       string // cookie value; valid only once sessionActive
	csrf          string
	sessionActive bool
	lastActive    time.Time
}

func withServeDefaults(opts ServeOptions) ServeOptions {
	if opts.IdleTimeout <= 0 {
		opts.IdleTimeout = defaultServeIdle
	}
	if opts.MaxLifetime <= 0 {
		opts.MaxLifetime = defaultServeLifetime
	}
	if opts.Log == nil {
		opts.Log = os.Stderr
	}
	return opts
}

// newRandomToken returns tokenBytes of crypto/rand, base64url without padding.
func newRandomToken() (string, error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("replydesk: random token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func newReviewServer(ln net.Listener, b Backend, opts ServeOptions, deps serveDeps) (*reviewServer, error) {
	if b == nil {
		return nil, errors.New("replydesk: nil backend")
	}
	tcp, ok := ln.Addr().(*net.TCPAddr)
	if !ok || !tcp.IP.Equal(net.IPv4(127, 0, 0, 1)) || tcp.Port == 0 {
		return nil, fmt.Errorf("replydesk: refusing to serve on %s: not 127.0.0.1", ln.Addr())
	}
	opts = withServeDefaults(opts)
	if deps.open == nil {
		deps.open = openBrowser
	}
	if deps.launchTTL <= 0 {
		deps.launchTTL = defaultLaunchTTL
	}
	if deps.writeTimeout <= 0 {
		deps.writeTimeout = defaultWriteTimeout
	}
	if deps.sendWriteTimeout <= 0 {
		deps.sendWriteTimeout = defaultSendWriteTimeout
	}

	index, err := fs.ReadFile(webFS, "web/index.html")
	if err != nil {
		return nil, fmt.Errorf("replydesk: read index.html: %w", err)
	}
	js, err := fs.ReadFile(webFS, "web/app.js")
	if err != nil {
		return nil, fmt.Errorf("replydesk: read app.js: %w", err)
	}
	css, err := fs.ReadFile(webFS, "web/app.css")
	if err != nil {
		return nil, fmt.Errorf("replydesk: read app.css: %w", err)
	}

	secret, err := newRandomToken()
	if err != nil {
		return nil, err
	}
	session, err := newRandomToken()
	if err != nil {
		return nil, err
	}
	csrf, err := newRandomToken()
	if err != nil {
		return nil, err
	}

	host := net.JoinHostPort("127.0.0.1", strconv.Itoa(tcp.Port))
	now := time.Now()
	return &reviewServer{
		b:                b,
		ln:               ln,
		host:             host,
		origin:           "http://" + host,
		opts:             opts,
		open:             deps.open,
		index:            index,
		js:               js,
		css:              css,
		cookieName:       sessionCookiePrefix + "_" + strconv.Itoa(tcp.Port),
		writeTimeout:     deps.writeTimeout,
		sendWriteTimeout: deps.sendWriteTimeout,
		idle:             opts.IdleTimeout,
		lifetime:         opts.MaxLifetime,
		started:          now,
		closeCh:          make(chan struct{}),
		secret:           secret,
		secretExpires:    now.Add(deps.launchTTL),
		session:          session,
		csrf:             csrf,
		lastActive:       now,
	}, nil
}

func (s *reviewServer) launchURL() string {
	return s.origin + "/login?t=" + s.secret
}

// requestClose asks run to shut down; safe to call more than once.
func (s *reviewServer) requestClose() {
	s.closeOnce.Do(func() { close(s.closeCh) })
}

func (s *reviewServer) touch() {
	s.mu.Lock()
	s.lastActive = time.Now()
	s.mu.Unlock()
}

// deadline is when the server stops unless there is more activity.
func (s *reviewServer) deadline() (time.Time, string) {
	s.mu.Lock()
	idleAt := s.lastActive.Add(s.idle)
	s.mu.Unlock()
	lifeAt := s.started.Add(s.lifetime)
	if lifeAt.Before(idleAt) {
		return lifeAt, "max lifetime reached"
	}
	return idleAt, "idle timeout"
}

func (s *reviewServer) run(ctx context.Context) error {
	srv := &http.Server{
		Handler:           s,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      s.writeTimeout,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          slog.NewLogLogger(slog.NewTextHandler(os.Stderr, nil), slog.LevelWarn),
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(s.ln) }()

	url := s.launchURL()
	if _, err := fmt.Fprintf(s.opts.Log, "Reply Desk is running. Open this link within %s (single use):\n  %s\n",
		time.Until(s.secretExpiry()).Round(time.Second), url); err != nil {
		slog.Warn("replydesk: could not print the launch URL", "err", err)
	}
	if s.opts.OpenBrowser {
		if err := s.open(url); err != nil {
			slog.Warn("replydesk: could not open a browser; open the printed URL yourself", "err", err)
		}
	}
	s.b.Rescan(context.Background())

	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	var reason string
loop:
	for {
		at, why := s.deadline()
		d := time.Until(at)
		if d <= 0 {
			reason = why
			break
		}
		timer.Reset(d)
		select {
		case <-ctx.Done():
			reason = "context done"
			break loop
		case <-s.closeCh:
			reason = "closed by user"
			break loop
		case err := <-errCh:
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return fmt.Errorf("replydesk: serve: %w", err)
		case <-timer.C:
		}
	}

	slog.Info("replydesk: review UI stopping", "reason", reason)
	shutCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		slog.Warn("replydesk: graceful shutdown incomplete", "err", err)
		_ = srv.Close()
	}
	if err := <-errCh; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("replydesk: serve: %w", err)
	}
	return nil
}

func (s *reviewServer) secretExpiry() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.secretExpires
}

// ServeHTTP applies the transport-level checks, then routes.
func (s *reviewServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	for _, kv := range securityHeaders {
		h.Set(kv[0], kv[1])
	}

	// DNS rebinding: a page on evil.example resolved to 127.0.0.1 still sends
	// its own Host.
	if r.Host != s.host {
		writePlain(w, http.StatusForbidden, "forbidden\n")
		return
	}
	isAPI := strings.HasPrefix(r.URL.Path, "/api/")

	if r.URL.Path == "/login" {
		s.handleLogin(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		h.Set("Allow", "GET, POST")
		s.writeErr(w, isAPI, http.StatusMethodNotAllowed, codeMethodNotAllowed, "method not allowed")
		return
	}
	if !s.authenticated(r) {
		if isAPI {
			writeJSON(w, http.StatusUnauthorized, &APIError{Code: codeUnauthorized, Detail: "session expired - run webex-mcp --review again"})
			return
		}
		writeHTML(w, http.StatusUnauthorized, []byte(expiredPage))
		return
	}

	// Every API call, reads included, needs the CSRF header: the cookie
	// alone reaches this server from any page on any 127.0.0.1 port.
	if isAPI && !s.checkCSRF(w, r) {
		return
	}
	if r.Method == http.MethodPost && !s.checkPost(w, r) {
		return
	}
	// Only what the user does counts as activity: opening the page and
	// actions. The page's background polls must not keep the server alive
	// while a forgotten tab sits open.
	if r.Method == http.MethodPost || r.URL.Path == "/" {
		s.touch()
	}
	s.route(w, r, isAPI)
}

func (s *reviewServer) route(w http.ResponseWriter, r *http.Request, isAPI bool) {
	type route struct {
		method string
		h      func(http.ResponseWriter, *http.Request)
	}
	var rt route
	switch r.URL.Path {
	case "/":
		rt = route{http.MethodGet, s.handleIndex}
	case "/static/app.js":
		rt = route{http.MethodGet, func(w http.ResponseWriter, _ *http.Request) {
			writeBytes(w, http.StatusOK, "text/javascript; charset=utf-8", s.js)
		}}
	case "/static/app.css":
		rt = route{http.MethodGet, func(w http.ResponseWriter, _ *http.Request) {
			writeBytes(w, http.StatusOK, "text/css; charset=utf-8", s.css)
		}}
	case "/api/status":
		rt = route{http.MethodGet, s.handleStatus}
	case "/api/items":
		rt = route{http.MethodGet, s.handleItems}
	case "/api/scan":
		rt = route{http.MethodPost, s.handleScan}
	case "/api/draft":
		rt = route{http.MethodPost, s.handleDraft}
	case "/api/send":
		rt = route{http.MethodPost, s.handleSend}
	case "/api/dismiss":
		rt = route{http.MethodPost, s.handleDismiss}
	case "/api/snooze":
		rt = route{http.MethodPost, s.handleSnooze}
	case "/api/close":
		rt = route{http.MethodPost, s.handleClose}
	default:
		s.writeErr(w, isAPI, http.StatusNotFound, codeNotFound, "not found")
		return
	}
	if r.Method != rt.method {
		w.Header().Set("Allow", rt.method)
		s.writeErr(w, isAPI, http.StatusMethodNotAllowed, codeMethodNotAllowed, "method not allowed")
		return
	}
	rt.h(w, r)
}

// handleLogin exchanges the single-use launch secret for the session cookie.
func (s *reviewServer) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writePlain(w, http.StatusMethodNotAllowed, "method not allowed\n")
		return
	}
	tok := r.URL.Query().Get("t")

	s.mu.Lock()
	match := subtle.ConstantTimeCompare([]byte(tok), []byte(s.secret)) == 1
	var reason string
	switch {
	case !match:
		reason = "invalid"
	case s.secretUsed:
		reason = "reused"
	case !time.Now().Before(s.secretExpires):
		reason = "expired"
	}
	if reason != "" {
		s.mu.Unlock()
		// Never log the token itself.
		slog.Warn("replydesk: launch token rejected", "reason", reason, "remote", r.RemoteAddr) // #nosec G706 -- reason is a fixed string and RemoteAddr is the TCP peer set by net/http
		writeHTML(w, http.StatusForbidden, []byte(forbiddenPage))
		return
	}
	s.secretUsed = true
	s.sessionActive = true
	s.lastActive = time.Now()
	session, csrf := s.session, s.csrf
	s.mu.Unlock()

	// Secure is not set: the UI is plain http on loopback, and some browsers
	// drop Secure cookies on http://127.0.0.1.
	http.SetCookie(w, &http.Cookie{ // #nosec G124 -- loopback-only http origin; HttpOnly and SameSite=Strict are set
		Name:     s.cookieName,
		Value:    session,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
	// The CSRF token reaches the page once, in the fragment, and never in
	// the HTML: GET / answers anyone who presents the cookie.
	w.Header().Set("Location", "/#"+csrfFragment+"="+csrf)
	w.WriteHeader(http.StatusSeeOther)
}

func (s *reviewServer) authenticated(r *http.Request) bool {
	c, err := r.Cookie(s.cookieName)
	if err != nil {
		return false
	}
	s.mu.Lock()
	active, session := s.sessionActive, s.session
	s.mu.Unlock()
	return active && subtle.ConstantTimeCompare([]byte(c.Value), []byte(session)) == 1
}

// checkCSRF requires the per-session CSRF header. It writes the error
// response and returns false on failure.
func (s *reviewServer) checkCSRF(w http.ResponseWriter, r *http.Request) bool {
	s.mu.Lock()
	csrf := s.csrf
	s.mu.Unlock()
	if subtle.ConstantTimeCompare([]byte(r.Header.Get(csrfHeaderName)), []byte(csrf)) != 1 {
		writeJSON(w, http.StatusForbidden, &APIError{Code: codeForbidden, Detail: "bad or missing CSRF token"})
		return false
	}
	return true
}

// checkPost enforces the cross-site defenses on state-changing requests and
// caps the body. It writes the error response and returns false on failure.
func (s *reviewServer) checkPost(w http.ResponseWriter, r *http.Request) bool {
	origins := r.Header.Values("Origin")
	if len(origins) != 1 || origins[0] != s.origin {
		writeJSON(w, http.StatusForbidden, &APIError{Code: codeForbidden, Detail: "bad or missing Origin"})
		return false
	}
	if sites := r.Header.Values("Sec-Fetch-Site"); len(sites) > 0 && (len(sites) != 1 || sites[0] != "same-origin") {
		writeJSON(w, http.StatusForbidden, &APIError{Code: codeForbidden, Detail: "cross-site request"})
		return false
	}
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		writeJSON(w, http.StatusUnsupportedMediaType, &APIError{Code: codeUnsupportedMedia, Detail: "Content-Type must be application/json"})
		return false
	}
	if !s.checkCSRF(w, r) {
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	return true
}

// decodeBody reads exactly one JSON value into v, rejecting unknown fields
// and trailing data. allowEmpty accepts an empty body (v is left as is).
// It writes the error response and returns false on failure.
func decodeBody(w http.ResponseWriter, r *http.Request, v any, allowEmpty bool) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if allowEmpty && errors.Is(err, io.EOF) {
			return true
		}
		writeDecodeErr(w, err, "invalid JSON body")
		return false
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeDecodeErr(w, err, "unexpected data after the JSON body")
		return false
	}
	return true
}

func writeDecodeErr(w http.ResponseWriter, err error, detail string) {
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		writeJSON(w, http.StatusRequestEntityTooLarge, &APIError{Code: codeBodyTooLarge, Detail: "request body exceeds 64 KB"})
		return
	}
	writeJSON(w, http.StatusBadRequest, &APIError{Code: codeBadRequest, Detail: detail})
}

// ---- handlers ----

func (s *reviewServer) handleIndex(w http.ResponseWriter, _ *http.Request) {
	writeHTML(w, http.StatusOK, s.index)
}

func (s *reviewServer) handleStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.b.Status())
}

// refuseSignedOut answers 409 signed_out when --logout/--switch ran while
// this process was open, so the page locks and the signed-out account's
// DMs are neither listed nor rescanned. It returns true when it wrote the
// response.
func (s *reviewServer) refuseSignedOut(w http.ResponseWriter) bool {
	if !s.b.SignedOut() {
		return false
	}
	writeJSON(w, http.StatusConflict, signedOutErr())
	return true
}

func (s *reviewServer) handleItems(w http.ResponseWriter, r *http.Request) {
	if s.refuseSignedOut(w) {
		return
	}
	items := s.b.Items(r.URL.Query().Get("all") == "1")
	if items == nil {
		items = []ItemView{}
	}
	writeJSON(w, http.StatusOK, struct {
		Items []ItemView `json:"items"`
	}{items})
}

var okBody = struct {
	OK bool `json:"ok"`
}{true}

func (s *reviewServer) handleScan(w http.ResponseWriter, r *http.Request) {
	if !decodeBody(w, r, &struct{}{}, true) {
		return
	}
	if s.refuseSignedOut(w) {
		return
	}
	s.b.Rescan(context.Background())
	writeJSON(w, http.StatusAccepted, okBody)
}

func (s *reviewServer) handleDraft(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RoomID      string `json:"room_id"`
		Instruction string `json:"instruction"`
	}
	if !decodeBody(w, r, &req, false) {
		return
	}
	if req.RoomID == "" {
		writeJSON(w, http.StatusBadRequest, &APIError{Code: codeBadRequest, Detail: "room_id is required"})
		return
	}
	if len(req.Instruction) > maxInstructionLen {
		writeJSON(w, http.StatusBadRequest, &APIError{Code: CodeTooLong, Detail: fmt.Sprintf("instruction exceeds %d bytes", maxInstructionLen)})
		return
	}
	// Drafting runs in the background, past the end of this request.
	if apiErr := s.b.Draft(context.Background(), req.RoomID, req.Instruction); apiErr != nil {
		writeJSON(w, backendErrStatus(apiErr), apiErr)
		return
	}
	writeJSON(w, http.StatusAccepted, okBody)
}

func (s *reviewServer) handleSend(w http.ResponseWriter, r *http.Request) {
	var req SendRequest
	if !decodeBody(w, r, &req, false) {
		return
	}
	if req.RoomID == "" {
		writeJSON(w, http.StatusBadRequest, &APIError{Code: codeBadRequest, Detail: "room_id is required"})
		return
	}
	// The server-wide WriteTimeout is too short for a send whose re-checks
	// wait on Retry-After: the message would go out and the browser would
	// see a dropped connection instead of the result.
	if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(s.sendWriteTimeout)); err != nil {
		slog.Warn("replydesk: could not extend the send write deadline", "err", err)
	}
	// A closed tab must not abort a send halfway: the POST to Webex has no
	// idempotency key.
	id, status, apiErr := s.b.Send(context.WithoutCancel(r.Context()), req)
	if apiErr != nil {
		if fixed, ok := fixedErrStatus[apiErr.Code]; ok {
			status = fixed
		} else if status < 400 || status > 599 {
			status = http.StatusInternalServerError
		}
		writeJSON(w, status, apiErr)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		MessageID string `json:"message_id"`
	}{id})
}

func (s *reviewServer) handleDismiss(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RoomID string `json:"room_id"`
		Reason string `json:"reason"`
	}
	if !decodeBody(w, r, &req, false) {
		return
	}
	if req.RoomID == "" {
		writeJSON(w, http.StatusBadRequest, &APIError{Code: codeBadRequest, Detail: "room_id is required"})
		return
	}
	if !validDismissReasons[req.Reason] {
		writeJSON(w, http.StatusBadRequest, &APIError{Code: codeBadRequest, Detail: "unknown dismiss reason"})
		return
	}
	if apiErr := s.b.Dismiss(req.RoomID, req.Reason); apiErr != nil {
		writeJSON(w, backendErrStatus(apiErr), apiErr)
		return
	}
	writeJSON(w, http.StatusOK, okBody)
}

func (s *reviewServer) handleSnooze(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RoomID string `json:"room_id"`
		Hours  int    `json:"hours"`
	}
	if !decodeBody(w, r, &req, false) {
		return
	}
	if req.RoomID == "" {
		writeJSON(w, http.StatusBadRequest, &APIError{Code: codeBadRequest, Detail: "room_id is required"})
		return
	}
	if req.Hours < minSnoozeHours || req.Hours > maxSnoozeHours {
		writeJSON(w, http.StatusBadRequest, &APIError{Code: codeBadRequest, Detail: fmt.Sprintf("hours must be %d..%d", minSnoozeHours, maxSnoozeHours)})
		return
	}
	if apiErr := s.b.Snooze(req.RoomID, req.Hours); apiErr != nil {
		writeJSON(w, backendErrStatus(apiErr), apiErr)
		return
	}
	writeJSON(w, http.StatusOK, okBody)
}

func (s *reviewServer) handleClose(w http.ResponseWriter, r *http.Request) {
	if !decodeBody(w, r, &struct{}{}, true) {
		return
	}
	writeJSON(w, http.StatusOK, okBody)
	// Shutdown waits for this response to finish.
	s.requestClose()
}

// fixedErrStatus is the status for Backend error codes that mean the same
// thing on every endpoint, Send included.
var fixedErrStatus = map[string]int{
	CodeBusy:       http.StatusConflict,
	CodeDrafterOff: http.StatusBadRequest,
	CodeSignedOut:  http.StatusConflict,
	CodeLockLost:   http.StatusConflict,
}

// backendErrStatus maps a Backend error from Draft, Dismiss, or Snooze to an
// HTTP status.
func backendErrStatus(e *APIError) int {
	if st, ok := fixedErrStatus[e.Code]; ok {
		return st
	}
	switch e.Code {
	case CodeNotPending:
		return http.StatusNotFound
	case CodeRateLimited:
		return http.StatusTooManyRequests
	default:
		return http.StatusBadRequest
	}
}

// ---- response helpers ----

func (s *reviewServer) writeErr(w http.ResponseWriter, isAPI bool, status int, code, detail string) {
	if isAPI {
		writeJSON(w, status, &APIError{Code: code, Detail: detail})
		return
	}
	writePlain(w, status, detail+"\n")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		slog.Error("replydesk: encode response", "err", err)
		status = http.StatusInternalServerError
		body = []byte(`{"code":"` + codeInternal + `","detail":"could not encode response"}`)
	}
	writeBytes(w, status, "application/json; charset=utf-8", append(body, '\n'))
}

func writeHTML(w http.ResponseWriter, status int, body []byte) {
	writeBytes(w, status, "text/html; charset=utf-8", body)
}

func writePlain(w http.ResponseWriter, status int, body string) {
	writeBytes(w, status, "text/plain; charset=utf-8", []byte(body))
}

func writeBytes(w http.ResponseWriter, status int, contentType string, body []byte) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
