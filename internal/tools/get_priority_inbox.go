package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/mythingies/plugin-webex/internal/buffer"
)

func registerGetPriorityInbox(s *mcpserver.MCPServer, buf *buffer.RingBuffer) {
	tool := mcp.NewTool("get_priority_inbox",
		mcp.WithDescription("Drain buffered messages filtered by priority level. Returns matching messages newest-first and removes them from the buffer. Non-matching messages remain buffered."),
		mcp.WithString("priorities",
			mcp.Required(),
			mcp.Description("Comma-separated priority levels to retrieve (e.g., \"critical,high\")."),
		),
	)

	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		raw, err := req.RequireString("priorities")
		if err != nil {
			return mcp.NewToolResultError("priorities is required"), nil
		}

		var priorities []string
		for _, p := range strings.Split(raw, ",") {
			p = strings.TrimSpace(p)
			if p != "" {
				priorities = append(priorities, p)
			}
		}
		if len(priorities) == 0 {
			return mcp.NewToolResultError("at least one priority level is required"), nil
		}

		messages := buf.DrainByPriority(priorities)
		if len(messages) == 0 {
			return mcp.NewToolResultText(fmt.Sprintf("No messages with priority %s.", raw)), nil
		}

		text := fmt.Sprintf("%d message(s) matching priority [%s]:\n\n", len(messages), raw)
		for _, msg := range messages {
			text += fmt.Sprintf("- [%s] **%s** in **%s** (%s, agent: %s): %s\n",
				msg.Priority, msg.PersonEmail, msg.RoomTitle, msg.Created.Format("15:04:05"), msg.RoutedAgent, msg.Text)
		}
		return mcp.NewToolResultText(text), nil
	})
}
