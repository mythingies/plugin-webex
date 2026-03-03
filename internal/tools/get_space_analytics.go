package tools

import (
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/mythingies/plugin-webex/internal/webex"
)

func registerGetSpaceAnalytics(s *mcpserver.MCPServer, client *webex.Client) {
	tool := mcp.NewTool("get_space_analytics",
		mcp.WithDescription("Get analytics for a Webex space: message count, active members, peak activity hour, and most active person over a time window."),
		mcp.WithString("room_id",
			mcp.Required(),
			mcp.Description("The ID of the space to analyze."),
		),
		mcp.WithNumber("days_back",
			mcp.Description("Number of days to look back (default 7)."),
		),
	)

	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		roomID, err := req.RequireString("room_id")
		if err != nil {
			return mcp.NewToolResultError("room_id is required"), nil
		}

		daysBack := req.GetInt("days_back", 7)

		analytics, err := client.GetSpaceAnalytics(roomID, daysBack)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to get analytics: %v", err)), nil
		}

		text := fmt.Sprintf("**%s** — %d day analytics\n\n", analytics.RoomTitle, analytics.DaysBack)
		text += fmt.Sprintf("- Messages: %d\n", analytics.MessageCount)
		text += fmt.Sprintf("- Active members: %d / %d total\n", analytics.ActiveMembers, analytics.TotalMembers)
		text += fmt.Sprintf("- Peak hour: %02d:00\n", analytics.PeakHour)
		text += fmt.Sprintf("- Most active: %s\n", analytics.MostActivePerson)

		return mcp.NewToolResultText(text), nil
	})
}
