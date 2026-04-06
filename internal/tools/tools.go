package tools

import (
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/mythingies/plugin-webex/internal/buffer"
	"github.com/mythingies/plugin-webex/internal/listener"
	"github.com/mythingies/plugin-webex/internal/router"
	"github.com/mythingies/plugin-webex/internal/webex"
)

// maxMessageLen is the maximum text length for outbound messages.
const maxMessageLen = 7439 // Webex API limit

// maxCardJSONLen is the maximum card JSON size.
const maxCardJSONLen = 28000 // Webex API limit for attachment bodies

// maxMentionsPerMessage caps extracted @mentions to prevent abuse.
const maxMentionsPerMessage = 50

// toolRateInterval is the minimum interval between drain-type tool calls.
const toolRateInterval = 2 * time.Second

// sandboxText wraps external message content so the LLM treats it as data,
// not as instructions. This is a defence-in-depth measure against prompt
// injection via Webex messages.
func sandboxText(text string) string {
	return "<external-message>" + text + "</external-message>"
}

// clampInt clamps a value between min and max bounds.
func clampInt(val, min, max int) int {
	if val < min {
		return min
	}
	if val > max {
		return max
	}
	return val
}

// maskEmail redacts an email address for PII protection.
// "alice@example.com" → "al***@example.com"
func maskEmail(email string) string {
	parts := strings.SplitN(email, "@", 2)
	if len(parts) != 2 {
		return "***"
	}
	local := parts[0]
	if len(local) <= 2 {
		local = local[:1] + "***"
	} else {
		local = local[:2] + "***"
	}
	return local + "@" + parts[1]
}

// auditLog emits a structured audit entry for tool calls.
func auditLog(tool, action string, attrs ...any) {
	args := append([]any{"tool", tool, "action", action}, attrs...)
	slog.Info("audit", args...)
}

// allowedOutboundProtocols lists URL schemes safe for outbound messages.
var allowedOutboundProtocols = map[string]bool{
	"http":   true,
	"https":  true,
	"mailto": true,
}

// sanitizeOutboundText validates outbound message text.
// Strips URLs with disallowed protocols (e.g., javascript:, data:, wmcp://).
func sanitizeOutboundText(text string) string {
	words := strings.Fields(text)
	for i, w := range words {
		if u, err := url.Parse(w); err == nil && u.Scheme != "" {
			if !allowedOutboundProtocols[strings.ToLower(u.Scheme)] {
				words[i] = "[blocked-url]"
			}
		}
	}
	return strings.Join(words, " ")
}

// toolRateLimiter provides per-tool rate limiting for drain operations.
var toolRateLimiter = struct {
	mu   sync.Mutex
	last map[string]time.Time
}{last: make(map[string]time.Time)}

// toolRateAllow returns true if the named tool can be called (rate-limited).
func toolRateAllow(tool string) bool {
	toolRateLimiter.mu.Lock()
	defer toolRateLimiter.mu.Unlock()

	now := time.Now()
	if last, ok := toolRateLimiter.last[tool]; ok {
		if now.Sub(last) < toolRateInterval {
			return false
		}
	}
	toolRateLimiter.last[tool] = now
	return true
}

// ToolScopes maps each MCP tool to its minimum required OAuth scopes.
// Tools that only read from local state (buffer, config) have no scope requirements.
var ToolScopes = map[string][]string{
	"list_spaces":             {"spark:rooms_read"},
	"get_space_history":       {"spark:messages_read"},
	"send_message":            {"spark:messages_write"},
	"reply_to_thread":         {"spark:messages_write"},
	"get_users":               {"spark:memberships_read"},
	"get_user_profile":        {"spark:people_read"},
	"search_messages":         {"spark:messages_read", "spark:rooms_read"},
	"send_adaptive_card":      {"spark:messages_write"},
	"share_file":              {"spark:messages_write"},
	"get_space_analytics":     {"spark:messages_read", "spark:rooms_read", "spark:memberships_read"},
	"listener_control":        {"spark:messages_read"},
	"list_meetings":           {"meeting:schedules_read"},
	"get_meeting_transcript":  {"meeting:transcripts_read"},
	"get_digest":              {"spark:messages_read", "spark:rooms_read"},
	"get_cross_space_context": {"spark:messages_read", "spark:rooms_read"},
}

// ValidateScopes checks that configuredScopes covers all tool requirements.
// Returns warnings for tools whose scopes are not covered.
func ValidateScopes(configuredScopes string) []string {
	scopeSet := make(map[string]bool)
	for _, s := range strings.Fields(configuredScopes) {
		scopeSet[s] = true
	}
	if scopeSet["spark:all"] {
		return nil // spark:all covers everything
	}

	var warnings []string
	for tool, required := range ToolScopes {
		for _, s := range required {
			if !scopeSet[s] {
				warnings = append(warnings, tool+" requires scope "+s)
				break
			}
		}
	}
	return warnings
}

// Register adds all MCP tools to the server.
func Register(s *mcpserver.MCPServer, client *webex.Client, buf *buffer.RingBuffer, rtr *router.Router, lst *listener.Listener) {
	// v0.1 — core tools (Slack parity).
	registerListSpaces(s, client)
	registerGetSpaceHistory(s, client)
	registerSendMessage(s, client)
	registerReplyToThread(s, client)
	registerGetUsers(s, client)
	registerGetUserProfile(s, client)
	registerSearchMessages(s, client)

	// v0.2 — beyond Slack.
	registerGetNotifications(s, buf)
	registerGetPriorityInbox(s, buf)
	registerGetMentions(s, buf)
	registerSendAdaptiveCard(s, client)
	registerShareFile(s, client)
	registerGetSpaceAnalytics(s, client)
	registerListenerControl(s, lst)
	registerGetNotificationRoutes(s, rtr)

	// v0.3 — intelligence.
	registerListMeetings(s, client)
	registerGetMeetingTranscript(s, client)
	registerGetDigest(s, client)
	registerGetCrossSpaceContext(s, client)
}
