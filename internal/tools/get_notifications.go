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
		mcp.WithDescription("Drain all buffered inbound messages from the WebSocket listener. Returns messages newest-first and removes them from the buffer. Requires the listener to be active."),
	)

	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		messages := buf.Drain()
		if len(messages) == 0 {
			return mcp.NewToolResultText("No new notifications."), nil
		}

		text := fmt.Sprintf("%d notification(s):\n\n", len(messages))
		for _, msg := range messages {
			text += fmt.Sprintf("- [%s] **%s** in **%s** (%s, agent: %s): %s\n",
				msg.Priority, msg.PersonEmail, msg.RoomTitle, msg.Created.Format("15:04:05"), msg.RoutedAgent, msg.Text)
		}
		return mcp.NewToolResultText(text), nil
	})
}
