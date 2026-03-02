package tools

import (
	"github.com/ecopelan/plugin-webex/internal/webex"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// Register adds all v0.1 MCP tools to the server.
func Register(s *mcpserver.MCPServer, client *webex.Client) {
	registerListSpaces(s, client)
	registerGetSpaceHistory(s, client)
	registerSendMessage(s, client)
	registerReplyToThread(s, client)
	registerGetUsers(s, client)
	registerGetUserProfile(s, client)
	registerSearchMessages(s, client)
}
