package listener

import (
	"testing"
	"time"

	"github.com/mythingies/plugin-webex/internal/buffer"
	"github.com/mythingies/plugin-webex/internal/router"
	"github.com/mythingies/plugin-webex/internal/triage"

	wmh "github.com/3rg0n/webex-message-handler/go"
)

// staticToken is a test helper that implements webex.TokenProvider.
type staticToken string

func (t staticToken) Token() (string, error) { return string(t), nil }

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
		tokenProvider: staticToken("fake"),
		buf:           buf,
		rtr:           rtr,
		selfPersonID:  "self-id",
		spaceNames:    map[string]string{"room1": "Alerts", "room2": "General Chat"},
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
		tokenProvider: staticToken("fake"),
		buf:           buf,
		rtr:           rtr,
		selfPersonID:  "my-person-id",
		spaceNames:    map[string]string{"room1": "Test"},
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
		tokenProvider: staticToken("fake"),
		buf:           buf,
		rtr:           rtr,
		selfPersonID:  "self-id",
		spaceNames:    map[string]string{"room1": "Unrouted Space"},
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

// TestOnMessageRecordsTriagePending verifies that, when a triage store is
// attached, an inbound message is recorded as a durable PENDING reminder, and
// that self-messages are not.
func TestOnMessageRecordsTriagePending(t *testing.T) {
	cfg := &router.Config{
		Routes: []router.Route{
			{Match: router.MatchCondition{Space: "*"}, Agent: "general", Priority: "low"},
		},
	}
	rtr := router.NewRouter(cfg, "")
	buf := buffer.New(100)
	tri, err := triage.NewWithPath("") // in-memory
	if err != nil {
		t.Fatalf("triage.NewWithPath: %v", err)
	}

	l := &Listener{
		tokenProvider: staticToken("fake"),
		buf:           buf,
		rtr:           rtr,
		triage:        tri,
		selfPersonID:  "self-id",
		spaceNames:    map[string]string{"room1": "General"},
	}

	l.onMessage(wmh.DecryptedMessage{
		ID:          "msg-pending",
		RoomID:      "room1",
		PersonID:    "user1",
		PersonEmail: "user@test.com",
		Text:        "can you look at this?",
		Created:     time.Now().Format(time.RFC3339),
		RoomType:    "group",
	})

	if tri.PendingCount() != 1 {
		t.Fatalf("expected 1 pending triage item, got %d", tri.PendingCount())
	}
	if it, ok := tri.Get("msg-pending"); !ok || it.Status != triage.StatusPending {
		t.Errorf("expected msg-pending recorded as pending, got %+v (ok=%v)", it, ok)
	}

	// Self message must not create a reminder.
	l.onMessage(wmh.DecryptedMessage{
		ID:          "msg-self",
		RoomID:      "room1",
		PersonID:    "self-id",
		PersonEmail: "me@test.com",
		Text:        "my own message",
		Created:     time.Now().Format(time.RFC3339),
		RoomType:    "group",
	})
	if tri.PendingCount() != 1 {
		t.Errorf("self-message created a triage item; PendingCount=%d, want 1", tri.PendingCount())
	}
}

// TestEffectiveMessageID verifies the activity-ID fallback. Live Mercury
// messages leave msg.ID (Object.ID) empty but populate the activity ID
// (Raw.ID); without the fallback, triage.Add rejects every real message.
func TestEffectiveMessageID(t *testing.T) {
	tests := []struct {
		name string
		msg  wmh.DecryptedMessage
		want string
	}{
		{
			name: "object id present wins",
			msg:  wmh.DecryptedMessage{ID: "obj-id", Raw: &wmh.MercuryActivity{ID: "act-id"}},
			want: "obj-id",
		},
		{
			name: "falls back to activity id when object id empty (live case)",
			msg:  wmh.DecryptedMessage{ID: "", Raw: &wmh.MercuryActivity{ID: "act-id"}},
			want: "act-id",
		},
		{
			name: "empty when neither present",
			msg:  wmh.DecryptedMessage{ID: "", Raw: nil},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := effectiveMessageID(tt.msg); got != tt.want {
				t.Errorf("effectiveMessageID() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestOnMessageTriageUsesActivityIDFallback is a regression test for the live
// bug where inbound messages never created triage reminders: msg.ID arrives
// empty (Object.ID), so triage.Add rejected the item. With the Raw.ID fallback
// the reminder must be recorded and keyed by the activity ID.
func TestOnMessageTriageUsesActivityIDFallback(t *testing.T) {
	cfg := &router.Config{
		Routes: []router.Route{
			{Match: router.MatchCondition{Space: "*"}, Agent: "general", Priority: "low"},
		},
	}
	rtr := router.NewRouter(cfg, "")
	buf := buffer.New(100)
	tri, err := triage.NewWithPath("") // in-memory
	if err != nil {
		t.Fatalf("triage.NewWithPath: %v", err)
	}

	l := &Listener{
		tokenProvider: staticToken("fake"),
		buf:           buf,
		rtr:           rtr,
		triage:        tri,
		selfPersonID:  "self-id",
		spaceNames:    map[string]string{"room1": "General"},
	}

	// Live-shaped message: empty ID, activity ID only in Raw.
	l.onMessage(wmh.DecryptedMessage{
		ID:          "",
		RoomID:      "room1",
		PersonID:    "user1",
		PersonEmail: "user@test.com",
		Text:        "can you look at this?",
		Created:     time.Now().Format(time.RFC3339),
		RoomType:    "group",
		Raw:         &wmh.MercuryActivity{ID: "act-123"},
	})

	if tri.PendingCount() != 1 {
		t.Fatalf("expected 1 pending triage item via activity-ID fallback, got %d", tri.PendingCount())
	}
	if it, ok := tri.Get("act-123"); !ok || it.Status != triage.StatusPending {
		t.Errorf("expected reminder keyed by activity ID act-123, got %+v (ok=%v)", it, ok)
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
				Match:    router.MatchCondition{Direct: true},
				Agent:    "dm-responder",
				Priority: "high",
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
		tokenProvider: staticToken("fake"),
		buf:           buf,
		rtr:           rtr,
		selfPersonID:  "self-id",
		spaceNames:    map[string]string{"dm-room": "DM Room"},
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
