package tools

import (
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/mythingies/plugin-webex/internal/webex"
)

func registerSendMessage(s *mcpserver.MCPServer, client *webex.Client) {
	tool := mcp.NewTool("send_message",
		mcp.WithDescription("Send a message to a Webex space or person. Provide exactly one of: room_id, to_person_id, or to_person_email."),
		mcp.WithString("room_id",
			mcp.Description("The ID of the space to send the message to."),
		),
		mcp.WithString("to_person_id",
			mcp.Description("The ID of the person to send a direct message to."),
		),
		mcp.WithString("to_person_email",
			mcp.Description("The email of the person to send a direct message to."),
		),
		mcp.WithString("text",
			mcp.Required(),
			mcp.Description("The message text to send."),
		),
	)

	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		text, err := req.RequireString("text")
		if err != nil {
			return mcp.NewToolResultError("text is required"), nil
		}

		roomID := req.GetString("room_id", "")
		toPersonID := req.GetString("to_person_id", "")
		toPersonEmail := req.GetString("to_person_email", "")

		if roomID == "" && toPersonID == "" && toPersonEmail == "" {
			return mcp.NewToolResultError("one of room_id, to_person_id, or to_person_email is required"), nil
		}

		msg, err := client.SendMessage(roomID, toPersonID, toPersonEmail, "", text)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to send message: %v", err)), nil
		}

		return mcp.NewToolResultText(fmt.Sprintf("Message sent (id: %s, created: %s)", msg.ID, msg.Created)), nil
	})
}

func registerReplyToThread(s *mcpserver.MCPServer, client *webex.Client) {
	tool := mcp.NewTool("reply_to_thread",
		mcp.WithDescription("Reply to a specific message thread in a Webex space."),
		mcp.WithString("room_id",
			mcp.Required(),
			mcp.Description("The ID of the space containing the thread."),
		),
		mcp.WithString("parent_id",
			mcp.Required(),
			mcp.Description("The ID of the parent message to reply to."),
		),
		mcp.WithString("text",
			mcp.Required(),
			mcp.Description("The reply text."),
		),
	)

	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		roomID, err := req.RequireString("room_id")
		if err != nil {
			return mcp.NewToolResultError("room_id is required"), nil
		}
		parentID, err := req.RequireString("parent_id")
		if err != nil {
			return mcp.NewToolResultError("parent_id is required"), nil
		}
		text, err := req.RequireString("text")
		if err != nil {
			return mcp.NewToolResultError("text is required"), nil
		}

		msg, err := client.SendMessage(roomID, "", "", parentID, text)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to reply: %v", err)), nil
		}

		return mcp.NewToolResultText(fmt.Sprintf("Reply sent (id: %s, thread: %s)", msg.ID, parentID)), nil
	})
}
