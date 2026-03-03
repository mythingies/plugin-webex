package listener

import (
	"testing"
	"time"

	"github.com/mythingies/plugin-webex/internal/buffer"
	"github.com/mythingies/plugin-webex/internal/router"

	wmh "github.com/3rg0n/webex-message-handler/go"
)

// TestOnMessageRouteAndBuffer verifies that the onMessage callback
// correctly routes a message and pushes it to the buffer.
func TestOnMessageRouteAndBuffer(t *testing.T) {
	cfg := &router.Config{
		Routes: []router.Route{
			{
				Match:    router.MatchCondition{Space: "Alerts"},
				Agent:    "alert-triage",
				Priority: "critical",
			},
			{
				Match:    router.MatchCondition{Space: "*"},
				Agent:    "general",
				Priority: "low",
			},
		},
	}
	rtr := router.NewRouter(cfg, "")
	buf := buffer.New(100)

	l := &Listener{
		token:        "fake",
		buf:          buf,
		rtr:          rtr,
		selfPersonID: "self-id",
		spaceNames:   map[string]string{"room1": "Alerts", "room2": "General Chat"},
	}

	// Send a message to "Alerts" space.
	l.onMessage(wmh.DecryptedMessage{
		ID:          "msg1",
		RoomID:      "room1",
		PersonID:    "user1",
		PersonEmail: "user@test.com",
		Text:        "server is down",
		Created:     time.Now().Format(time.RFC3339),
		RoomType:    "group",
	})

	if buf.Size() != 1 {
		t.Fatalf("expected 1 message in buffer, got %d", buf.Size())
	}

	msgs := buf.Peek(1)
	if msgs[0].Priority != "critical" {
		t.Errorf("expected priority critical, got %s", msgs[0].Priority)
	}
	if msgs[0].RoutedAgent != "alert-triage" {
		t.Errorf("expected agent alert-triage, got %s", msgs[0].RoutedAgent)
	}
	if msgs[0].RoomTitle != "Alerts" {
		t.Errorf("expected room title Alerts, got %s", msgs[0].RoomTitle)
	}
}

// TestOnMessageSelfLoop verifies that messages from the authenticated
// user (self) are ignored.
func TestOnMessageSelfLoop(t *testing.T) {
	cfg := &router.Config{
		Routes: []router.Route{
			{
				Match:    router.MatchCondition{Space: "*"},
				Agent:    "general",
				Priority: "low",
			},
		},
	}
	rtr := router.NewRouter(cfg, "")
	buf := buffer.New(100)

	l := &Listener{
		token:        "fake",
		buf:          buf,
		rtr:          rtr,
		selfPersonID: "my-person-id",
		spaceNames:   map[string]string{"room1": "Test"},
	}

	// Message from self should be ignored.
	l.onMessage(wmh.DecryptedMessage{
		ID:          "msg-self",
		RoomID:      "room1",
		PersonID:    "my-person-id",
		PersonEmail: "me@test.com",
		Text:        "my own message",
		Created:     time.Now().Format(time.RFC3339),
		RoomType:    "group",
	})

	if buf.Size() != 0 {
		t.Errorf("expected 0 messages (self ignored), got %d", buf.Size())
	}
}

// TestOnMessageNoRouteMatch verifies that messages with no matching route
// get default priority "low" and empty agent.
func TestOnMessageNoRouteMatch(t *testing.T) {
	cfg := &router.Config{} // no routes
	rtr := router.NewRouter(cfg, "")
	buf := buffer.New(100)

	l := &Listener{
		token:        "fake",
		buf:          buf,
		rtr:          rtr,
		selfPersonID: "self-id",
		spaceNames:   map[string]string{"room1": "Unrouted Space"},
	}

	l.onMessage(wmh.DecryptedMessage{
		ID:          "msg2",
		RoomID:      "room1",
		PersonID:    "user2",
		PersonEmail: "user2@test.com",
		Text:        "hello",
		Created:     time.Now().Format(time.RFC3339),
		RoomType:    "group",
	})

	msgs := buf.Peek(1)
	if msgs[0].Priority != "low" {
		t.Errorf("expected default priority low, got %s", msgs[0].Priority)
	}
	if msgs[0].RoutedAgent != "" {
		t.Errorf("expected empty agent, got %s", msgs[0].RoutedAgent)
	}
}

// TestStatusStopped verifies status when listener is not running.
func TestStatusStopped(t *testing.T) {
	l := &Listener{}
	st := l.Status()
	if st.Status != "stopped" {
		t.Errorf("expected status stopped, got %s", st.Status)
	}
	if st.Connected {
		t.Error("expected not connected")
	}
}

// TestOnMessageDirectRoute verifies routing for direct messages.
func TestOnMessageDirectRoute(t *testing.T) {
	cfg := &router.Config{
		Routes: []router.Route{
			{
				Match:       router.MatchCondition{Direct: true},
				Agent:       "dm-responder",
				Priority:    "high",
				AutoRespond: true,
			},
			{
				Match:    router.MatchCondition{Space: "*"},
				Agent:    "general",
				Priority: "low",
			},
		},
	}
	rtr := router.NewRouter(cfg, "")
	buf := buffer.New(100)

	l := &Listener{
		token:        "fake",
		buf:          buf,
		rtr:          rtr,
		selfPersonID: "self-id",
		spaceNames:   map[string]string{"dm-room": "DM Room"},
	}

	l.onMessage(wmh.DecryptedMessage{
		ID:          "dm1",
		RoomID:      "dm-room",
		PersonID:    "user3",
		PersonEmail: "user3@test.com",
		Text:        "hey there",
		Created:     time.Now().Format(time.RFC3339),
		RoomType:    "direct",
	})

	msgs := buf.Peek(1)
	if msgs[0].Priority != "high" {
		t.Errorf("expected priority high for DM, got %s", msgs[0].Priority)
	}
	if msgs[0].RoutedAgent != "dm-responder" {
		t.Errorf("expected agent dm-responder, got %s", msgs[0].RoutedAgent)
	}
}
