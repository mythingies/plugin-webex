package tools

import (
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/mythingies/plugin-webex/internal/webex"
)

// messageOptions are the text and markdown parameters every sending tool takes.
func messageOptions(what string) []mcp.ToolOption {
	return []mcp.ToolOption{
		mcp.WithString("text",
			mcp.Description("The "+what+" in plain text. With markdown, it is the fallback for clients without rich text."),
		),
		mcp.WithString("markdown",
			mcp.Description("The "+what+" in Markdown, which Webex renders."),
		),
	}
}

// messageBody reads, checks, and sanitizes text and markdown. A non-nil result is the
// error to return to the caller.
func messageBody(req mcp.CallToolRequest, required bool) (text, markdown string, errResult *mcp.CallToolResult) {
	text, markdown = req.GetString("text", ""), req.GetString("markdown", "")
	if required && text == "" && markdown == "" {
		return "", "", mcp.NewToolResultError("text or markdown is required")
	}
	for _, s := range []string{text, markdown} {
		if len(s) > maxMessageLen {
			return "", "", mcp.NewToolResultError(fmt.Sprintf("message too long (%d chars, max %d)", len(s), maxMessageLen))
		}
	}
	// Strip dangerous URL protocols.
	return sanitizeOutboundText(text), sanitizeOutboundText(markdown), nil
}

func registerSendMessage(s *mcpserver.MCPServer, client *webex.Client) {
	opts := append([]mcp.ToolOption{
		mcp.WithDescription("Send a message to a Webex space or person. Provide exactly one of: room_id, to_person_id, or to_person_email, and text, markdown, or both."),
		mcp.WithString("room_id",
			mcp.Description("The ID of the space to send the message to."),
		),
		mcp.WithString("to_person_id",
			mcp.Description("The ID of the person to send a direct message to."),
		),
		mcp.WithString("to_person_email",
			mcp.Description("The email of the person to send a direct message to."),
		),
	}, messageOptions("message")...)
	tool := mcp.NewTool("send_message", opts...)

	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		text, markdown, errResult := messageBody(req, true)
		if errResult != nil {
			return errResult, nil
		}

		roomID := req.GetString("room_id", "")
		toPersonID := req.GetString("to_person_id", "")
		toPersonEmail := req.GetString("to_person_email", "")

		if roomID == "" && toPersonID == "" && toPersonEmail == "" {
			return mcp.NewToolResultError("one of room_id, to_person_id, or to_person_email is required"), nil
		}

		msg, err := client.SendMessage(roomID, toPersonID, toPersonEmail, "", text, markdown)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to send message: %v", err)), nil
		}

		auditLog("send_message", "sent", "msg_id", msg.ID, "room_id", roomID)
		return mcp.NewToolResultText(fmt.Sprintf("Message sent (id: %s, created: %s)", msg.ID, msg.Created)), nil
	})
}

func registerReplyToThread(s *mcpserver.MCPServer, client *webex.Client) {
	opts := append([]mcp.ToolOption{
		mcp.WithDescription("Reply to a specific message thread in a Webex space, with text, markdown, or both."),
		mcp.WithString("room_id",
			mcp.Required(),
			mcp.Description("The ID of the space containing the thread."),
		),
		mcp.WithString("parent_id",
			mcp.Required(),
			mcp.Description("The ID of the parent message to reply to."),
		),
	}, messageOptions("reply")...)
	tool := mcp.NewTool("reply_to_thread", opts...)

	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		roomID, err := req.RequireString("room_id")
		if err != nil {
			return mcp.NewToolResultError("room_id is required"), nil
		}
		parentID, err := req.RequireString("parent_id")
		if err != nil {
			return mcp.NewToolResultError("parent_id is required"), nil
		}
		text, markdown, errResult := messageBody(req, true)
		if errResult != nil {
			return errResult, nil
		}

		msg, err := client.SendMessage(roomID, "", "", parentID, text, markdown)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to reply: %v", err)), nil
		}

		auditLog("reply_to_thread", "sent", "msg_id", msg.ID, "room_id", roomID, "parent_id", parentID)
		return mcp.NewToolResultText(fmt.Sprintf("Reply sent (id: %s, thread: %s)", msg.ID, parentID)), nil
	})
}
