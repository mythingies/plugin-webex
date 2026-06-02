package tools

import (
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/mythingies/plugin-webex/internal/buffer"
)

func registerGetNotifications(s *mcpserver.MCPServer, buf *buffer.RingBuffer) {
	tool := mcp.NewTool("get_notifications",
		mcp.WithDescription("Peek at buffered inbound messages from the WebSocket listener. Returns messages newest-first WITHOUT removing them — reading does not consume the buffer or clear any reminder. Use mark_processed to clear items from your pending list. Requires the listener to be active."),
		mcp.WithNumber("max",
			mcp.Description("Maximum number of messages to return (default 1000)."),
		),
	)

	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if !toolRateAllow("get_notifications") {
			return mcp.NewToolResultError("rate limited: wait before calling get_notifications again"), nil
		}

		max := clampInt(req.GetInt("max", 1000), 1, 5000)
		messages := buf.Peek(max)
		if len(messages) == 0 {
			return mcp.NewToolResultText("No new notifications."), nil
		}

		auditLog("get_notifications", "peeked", "count", len(messages))
		text := fmt.Sprintf("%d notification(s):\n\n", len(messages))
		agents := make([]string, 0, len(messages))
		for _, msg := range messages {
			text += fmt.Sprintf("- [%s] **%s** in **%s** (%s, agent: %s): %s\n",
				msg.Priority, maskEmail(msg.PersonEmail), msg.RoomTitle, msg.Created.Format("15:04:05"), msg.RoutedAgent, sandboxText(msg.Text))
			if msg.RoutedAgent != "" {
				agents = append(agents, msg.RoutedAgent)
			}
		}
		text += renderPlaybooks(agents)
		return mcp.NewToolResultText(text), nil
	})
}
