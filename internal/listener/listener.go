package listener

import (
	"context"
	"fmt"
	"html"
	"io"
	"log/slog"
	"sync"
	"time"

	wmh "github.com/3rg0n/webex-message-handler/go"

	"github.com/mythingies/plugin-webex/internal/buffer"
	"github.com/mythingies/plugin-webex/internal/router"
	"github.com/mythingies/plugin-webex/internal/webex"
)

const (
	// maxCacheSize limits the space name cache to prevent unbounded growth.
	maxCacheSize = 2000
	// rateLimit is the max messages processed per second.
	rateLimit = 100
)

// ListenerStatus reports the current state of the WebSocket listener.
type ListenerStatus struct {
	Connected        bool   `json:"connected"`
	Status           string `json:"status"` // "stopped", "connected", "connecting", "reconnecting", "disconnected"
	MessagesReceived int64  `json:"messagesReceived"`
	Errors           int64  `json:"errors"`
}

// Listener wraps webex-message-handler for real-time inbound messages.
type Listener struct {
	mu sync.Mutex

	token   string
	client  *webex.Client
	buf     *buffer.RingBuffer
	rtr     *router.Router
	handler *wmh.WebexMessageHandler

	selfPersonID string
	spaceNames   map[string]string // roomID → title cache

	running  bool
	cancel   context.CancelFunc
	received int64
	errors   int64

	// Rate limiting: token bucket.
	rateMu      sync.Mutex
	rateTokens  int
	rateResetAt time.Time
}

// New creates a Listener. Call Start() to begin receiving messages.
func New(token string, client *webex.Client, buf *buffer.RingBuffer, rtr *router.Router) *Listener {
	return &Listener{
		token:      token,
		client:     client,
		buf:        buf,
		rtr:        rtr,
		spaceNames: make(map[string]string),
		rateTokens: rateLimit,
	}
}

// Start connects to the Webex Mercury WebSocket and begins routing messages.
func (l *Listener) Start(ctx context.Context) error {
	l.mu.Lock()
	if l.running {
		l.mu.Unlock()
		return fmt.Errorf("listener already running")
	}
	l.mu.Unlock()

	// Resolve authenticated user for loop detection.
	me, err := l.client.GetMe()
	if err != nil {
		return fmt.Errorf("getting authenticated user: %w", err)
	}
	l.selfPersonID = me.ID

	// Use a no-op logger to prevent token leakage from the wmh library.
	handler, err := wmh.New(wmh.Config{
		Token:  l.token,
		Logger: wmh.NewSlogLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	})
	if err != nil {
		return fmt.Errorf("creating WebSocket handler: %w", err)
	}

	handler.OnMessageCreated(l.onMessage)
	handler.OnError(func(err error) {
		l.mu.Lock()
		l.errors++
		l.mu.Unlock()
		slog.Error("webex listener error", "error", err)
	})

	connCtx, cancel := context.WithCancel(ctx)
	if err := handler.Connect(connCtx); err != nil {
		cancel()
		return fmt.Errorf("connecting WebSocket: %w", err)
	}

	l.mu.Lock()
	l.handler = handler
	l.cancel = cancel
	l.running = true
	l.mu.Unlock()

	return nil
}

// Stop disconnects the WebSocket listener.
func (l *Listener) Stop(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if !l.running {
		return fmt.Errorf("listener not running")
	}

	if l.handler != nil {
		if err := l.handler.Disconnect(ctx); err != nil {
			slog.Warn("error disconnecting WebSocket handler", "error", err)
		}
	}
	if l.cancel != nil {
		l.cancel()
	}
	l.running = false
	l.handler = nil
	return nil
}

// Connected reports whether the WebSocket is currently connected.
func (l *Listener) Connected() bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.handler == nil {
		return false
	}
	return l.handler.Connected()
}

// Status returns the current listener status.
func (l *Listener) Status() ListenerStatus {
	l.mu.Lock()
	defer l.mu.Unlock()

	st := ListenerStatus{
		MessagesReceived: l.received,
		Errors:           l.errors,
	}

	if !l.running {
		st.Status = "stopped"
		return st
	}

	if l.handler != nil {
		hs := l.handler.Status()
		st.Connected = hs.WebSocketOpen
		st.Status = string(hs.Status)
	}
	return st
}

// rateLimitAllow implements a simple token bucket rate limiter.
func (l *Listener) rateLimitAllow() bool {
	l.rateMu.Lock()
	defer l.rateMu.Unlock()

	now := time.Now()
	if now.After(l.rateResetAt) {
		l.rateTokens = rateLimit
		l.rateResetAt = now.Add(time.Second)
	}
	if l.rateTokens <= 0 {
		return false
	}
	l.rateTokens--
	return true
}

// onMessage processes an inbound message: enriches, routes, and buffers it.
func (l *Listener) onMessage(msg wmh.DecryptedMessage) {
	// Rate limiting.
	if !l.rateLimitAllow() {
		slog.Warn("message rate limit exceeded, dropping message")
		return
	}

	// Loop detection: ignore messages from self.
	if msg.PersonID == l.selfPersonID {
		return
	}

	// Resolve space name (cached).
	roomTitle := l.resolveSpaceName(msg.RoomID)

	// Sanitize inbound text before routing.
	sanitizedText := html.EscapeString(msg.Text)

	// Route the message.
	inbound := router.InboundMessage{
		RoomTitle: roomTitle,
		RoomType:  msg.RoomType,
		Text:      sanitizedText,
	}

	var priority, agent string
	result := l.rtr.Route(inbound)
	if result != nil {
		priority = result.Priority
		agent = result.Agent
	} else {
		priority = "low"
		agent = ""
	}

	created, err := time.Parse(time.RFC3339, msg.Created)
	if err != nil {
		slog.Warn("failed to parse message timestamp", "error", err, "created", msg.Created)
		created = time.Now()
	}

	notif := buffer.NotificationMessage{
		ID:          msg.ID,
		RoomID:      msg.RoomID,
		RoomTitle:   roomTitle,
		PersonID:    msg.PersonID,
		PersonEmail: msg.PersonEmail,
		Text:        sanitizedText,
		HTML:        html.EscapeString(msg.HTML),
		Created:     created,
		Priority:    priority,
		RoutedAgent: agent,
	}

	l.buf.Push(notif)

	l.mu.Lock()
	l.received++
	l.mu.Unlock()
}

// resolveSpaceName looks up a space title by room ID, caching results.
func (l *Listener) resolveSpaceName(roomID string) string {
	l.mu.Lock()
	if title, ok := l.spaceNames[roomID]; ok {
		l.mu.Unlock()
		return title
	}
	l.mu.Unlock()

	// Fetch from API.
	spaces, err := l.client.ListSpaces(1000)
	if err != nil {
		return roomID // fallback to ID
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	// Evict cache if too large to prevent unbounded growth.
	if len(l.spaceNames) > maxCacheSize {
		l.spaceNames = make(map[string]string)
	}

	for _, sp := range spaces {
		l.spaceNames[sp.ID] = sp.Title
	}
	title := l.spaceNames[roomID]

	if title == "" {
		return roomID
	}
	return title
}
