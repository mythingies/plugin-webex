package tools

import (
	"github.com/ecopelan/plugin-webex/internal/buffer"
	"github.com/ecopelan/plugin-webex/internal/listener"
	"github.com/ecopelan/plugin-webex/internal/router"
	"github.com/ecopelan/plugin-webex/internal/webex"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

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
