package router

import (
	"testing"
)

func testConfig() *Config {
	return &Config{
		Routes: []Route{
			{
				Match:    MatchCondition{Space: "Production Alerts"},
				Agent:    "alert-triage",
				Priority: "critical",
			},
			{
				Match:    MatchCondition{Keywords: []string{"outage", "incident", "P1"}, Space: "*"},
				Agent:    "escalation",
				Priority: "critical",
				Action:   "notify_dm",
			},
			{
				Match:       MatchCondition{Direct: true},
				Agent:       "dm-responder",
				Priority:    "high",
				AutoRespond: true,
			},
			{
				Match:    MatchCondition{Space: "Ops*"},
				Agent:    "ops-summarizer",
				Priority: "medium",
			},
			{
				Match:    MatchCondition{Space: "*"},
				Agent:    "general",
				Priority: "low",
			},
		},
	}
}

func TestRouteExactSpace(t *testing.T) {
	r := NewRouter(testConfig(), "")

	result := r.Route(InboundMessage{
		RoomTitle: "Production Alerts",
		RoomType:  "group",
		Text:      "server is healthy",
	})

	if result == nil {
		t.Fatal("expected a route match")
	}
	if result.Agent != "alert-triage" {
		t.Errorf("expected agent alert-triage, got %s", result.Agent)
	}
	if result.Priority != "critical" {
		t.Errorf("expected priority critical, got %s", result.Priority)
	}
}

func TestRouteKeyword(t *testing.T) {
	r := NewRouter(testConfig(), "")

	result := r.Route(InboundMessage{
		RoomTitle: "Random Space",
		RoomType:  "group",
		Text:      "We have a P1 incident in production",
	})

	if result == nil {
		t.Fatal("expected a route match")
	}
	if result.Agent != "escalation" {
		t.Errorf("expected agent escalation, got %s", result.Agent)
	}
}

func TestRouteKeywordCaseInsensitive(t *testing.T) {
	r := NewRouter(testConfig(), "")

	result := r.Route(InboundMessage{
		RoomTitle: "Any Space",
		RoomType:  "group",
		Text:      "there was an OUTAGE last night",
	})

	if result == nil {
		t.Fatal("expected a route match")
	}
	if result.Agent != "escalation" {
		t.Errorf("expected agent escalation, got %s", result.Agent)
	}
}

func TestRouteDirect(t *testing.T) {
	r := NewRouter(testConfig(), "")

	result := r.Route(InboundMessage{
		RoomTitle: "",
		RoomType:  "direct",
		Text:      "hey, are you there?",
	})

	if result == nil {
		t.Fatal("expected a route match")
	}
	if result.Agent != "dm-responder" {
		t.Errorf("expected agent dm-responder, got %s", result.Agent)
	}
	if !result.AutoRespond {
		t.Error("expected auto_respond true")
	}
}

func TestRouteGlobPrefix(t *testing.T) {
	r := NewRouter(testConfig(), "")

	result := r.Route(InboundMessage{
		RoomTitle: "Ops-Monitoring",
		RoomType:  "group",
		Text:      "dashboard updated",
	})

	if result == nil {
		t.Fatal("expected a route match")
	}
	if result.Agent != "ops-summarizer" {
		t.Errorf("expected agent ops-summarizer, got %s", result.Agent)
	}
}

func TestRouteWildcardFallthrough(t *testing.T) {
	r := NewRouter(testConfig(), "")

	result := r.Route(InboundMessage{
		RoomTitle: "Random Chat",
		RoomType:  "group",
		Text:      "hello everyone",
	})

	if result == nil {
		t.Fatal("expected a route match")
	}
	if result.Agent != "general" {
		t.Errorf("expected agent general, got %s", result.Agent)
	}
}

func TestFirstMatchWins(t *testing.T) {
	// "Production Alerts" matches the first route (exact space)
	// even though it would also match the wildcard routes.
	r := NewRouter(testConfig(), "")

	result := r.Route(InboundMessage{
		RoomTitle: "Production Alerts",
		RoomType:  "group",
		Text:      "outage detected", // also matches keyword route
	})

	if result == nil {
		t.Fatal("expected a route match")
	}
	// First-match-wins: exact space match beats keyword match.
	if result.Agent != "alert-triage" {
		t.Errorf("expected first match alert-triage, got %s", result.Agent)
	}
}

func TestNoRoutes(t *testing.T) {
	r := NewRouter(&Config{}, "")

	result := r.Route(InboundMessage{
		RoomTitle: "Any Space",
		RoomType:  "group",
		Text:      "hello",
	})

	if result != nil {
		t.Errorf("expected nil result for no routes, got agent=%s", result.Agent)
	}
}

func TestRoutesReturnsACopy(t *testing.T) {
	r := NewRouter(testConfig(), "")
	routes := r.Routes()

	if len(routes) != 5 {
		t.Fatalf("expected 5 routes, got %d", len(routes))
	}

	// Modifying the copy should not affect the router.
	routes[0].Agent = "modified"
	original := r.Routes()
	if original[0].Agent == "modified" {
		t.Error("modifying returned routes should not affect router")
	}
}

func TestMatchGlob(t *testing.T) {
	tests := []struct {
		pattern string
		value   string
		want    bool
	}{
		{"*", "anything", true},
		{"Ops*", "Ops-Monitoring", true},
		{"Ops*", "ops-monitoring", true}, // case insensitive
		{"Ops*", "NotOps", false},
		{"Production Alerts", "Production Alerts", true},
		{"Production Alerts", "production alerts", true}, // case insensitive
		{"Production Alerts", "Staging Alerts", false},
	}

	for _, tt := range tests {
		got := matchGlob(tt.pattern, tt.value)
		if got != tt.want {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", tt.pattern, tt.value, got, tt.want)
		}
	}
}

func TestMatchKeywords(t *testing.T) {
	tests := []struct {
		keywords []string
		text     string
		want     bool
	}{
		{[]string{"outage"}, "there was an outage", true},
		{[]string{"outage"}, "OUTAGE detected", true},
		{[]string{"P1", "SEV"}, "this is a P1 incident", true},
		{[]string{"P1", "SEV"}, "normal message", false},
		{[]string{}, "anything", false},
	}

	for _, tt := range tests {
		got := matchKeywords(tt.keywords, tt.text)
		if got != tt.want {
			t.Errorf("matchKeywords(%v, %q) = %v, want %v", tt.keywords, tt.text, got, tt.want)
		}
	}
}
