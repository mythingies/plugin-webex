package listener

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/ecopelan/plugin-webex/internal/buffer"
	"github.com/ecopelan/plugin-webex/internal/router"
	"github.com/ecopelan/plugin-webex/internal/webex"

	wmh "github.com/3rg0n/webex-message-handler/go"
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
}

// New creates a Listener. Call Start() to begin receiving messages.
func New(token string, client *webex.Client, buf *buffer.RingBuffer, rtr *router.Router) *Listener {
	return &Listener{
		token:      token,
		client:     client,
		buf:        buf,
		rtr:        rtr,
		spaceNames: make(map[string]string),
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

	handler, err := wmh.New(wmh.Config{
		Token:  l.token,
		Logger: wmh.NewSlogLogger(slog.Default()),
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
		_ = l.handler.Disconnect(ctx)
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

// onMessage processes an inbound message: enriches, routes, and buffers it.
func (l *Listener) onMessage(msg wmh.DecryptedMessage) {
	// Loop detection: ignore messages from self.
	if msg.PersonID == l.selfPersonID {
		return
	}

	// Resolve space name (cached).
	roomTitle := l.resolveSpaceName(msg.RoomID)

	// Route the message.
	inbound := router.InboundMessage{
		RoomTitle: roomTitle,
		RoomType:  msg.RoomType,
		Text:      msg.Text,
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

	created, _ := time.Parse(time.RFC3339, msg.Created)

	notif := buffer.NotificationMessage{
		ID:          msg.ID,
		RoomID:      msg.RoomID,
		RoomTitle:   roomTitle,
		PersonID:    msg.PersonID,
		PersonEmail: msg.PersonEmail,
		Text:        msg.Text,
		HTML:        msg.HTML,
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
	for _, sp := range spaces {
		l.spaceNames[sp.ID] = sp.Title
	}
	title := l.spaceNames[roomID]
	l.mu.Unlock()

	if title == "" {
		return roomID
	}
	return title
}
