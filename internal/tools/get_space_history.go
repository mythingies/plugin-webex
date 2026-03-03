package tools

import (
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/mythingies/plugin-webex/internal/webex"
)

func registerGetSpaceHistory(s *mcpserver.MCPServer, client *webex.Client) {
	tool := mcp.NewTool("get_space_history",
		mcp.WithDescription("Read recent messages from a Webex space."),
		mcp.WithString("room_id",
			mcp.Required(),
			mcp.Description("The ID of the space to read messages from."),
		),
		mcp.WithNumber("max",
			mcp.Description("Maximum number of messages to return (default 20, max 1000)."),
		),
	)

	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		roomID, err := req.RequireString("room_id")
		if err != nil {
			return mcp.NewToolResultError("room_id is required"), nil
		}

		max := req.GetInt("max", 20)

		messages, err := client.GetMessages(roomID, max)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to get messages: %v", err)), nil
		}

		var text string
		for _, msg := range messages {
			thread := ""
			if msg.ParentID != "" {
				thread = fmt.Sprintf(" [thread: %s]", msg.ParentID)
			}
			text += fmt.Sprintf("- **%s** (%s)%s: %s\n", msg.PersonEmail, msg.Created, thread, msg.Text)
		}
		if text == "" {
			text = "No messages found in this space."
		}

		return mcp.NewToolResultText(text), nil
	})
}
