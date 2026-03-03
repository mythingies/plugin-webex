package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/mythingies/plugin-webex/internal/webex"
)

func registerSendAdaptiveCard(s *mcpserver.MCPServer, client *webex.Client) {
	tool := mcp.NewTool("send_adaptive_card",
		mcp.WithDescription("Send a rich Adaptive Card to a Webex space or person. Cards support tables, buttons, inputs, and other interactive elements."),
		mcp.WithString("room_id",
			mcp.Description("The ID of the space to send the card to."),
		),
		mcp.WithString("to_person_email",
			mcp.Description("The email of the person to send the card to (for direct messages)."),
		),
		mcp.WithString("card_json",
			mcp.Required(),
			mcp.Description("The Adaptive Card body as a JSON string. Must be a valid Adaptive Card schema (type: AdaptiveCard)."),
		),
	)

	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		roomID := req.GetString("room_id", "")
		toPersonEmail := req.GetString("to_person_email", "")

		if roomID == "" && toPersonEmail == "" {
			return mcp.NewToolResultError("one of room_id or to_person_email is required"), nil
		}

		cardJSON, err := req.RequireString("card_json")
		if err != nil {
			return mcp.NewToolResultError("card_json is required"), nil
		}

		var card interface{}
		if err := json.Unmarshal([]byte(cardJSON), &card); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("invalid card_json: %v", err)), nil
		}

		msg, err := client.SendAdaptiveCard(roomID, toPersonEmail, card)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to send adaptive card: %v", err)), nil
		}

		return mcp.NewToolResultText(fmt.Sprintf("Adaptive Card sent (id: %s, created: %s)", msg.ID, msg.Created)), nil
	})
}
