package tools

import (
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
