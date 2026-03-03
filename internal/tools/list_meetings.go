package tools

import (
	"context"
	"fmt"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/mythingies/plugin-webex/internal/webex"
)

func registerListMeetings(s *mcpserver.MCPServer, client *webex.Client) {
	tool := mcp.NewTool("list_meetings",
		mcp.WithDescription("List upcoming or recent Webex meetings. Defaults to showing the next 7 days of meetings."),
		mcp.WithString("from",
			mcp.Description("Start date/time in ISO 8601 format (default: now)."),
		),
		mcp.WithString("to",
			mcp.Description("End date/time in ISO 8601 format (default: 7 days from now)."),
		),
		mcp.WithNumber("max",
			mcp.Description("Maximum number of meetings to return (default 20)."),
		),
	)

	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		from := req.GetString("from", "")
		to := req.GetString("to", "")
		max := req.GetInt("max", 20)

		// Default: now to 7 days from now.
		if from == "" {
			from = time.Now().UTC().Format(time.RFC3339)
		}
		if to == "" {
			to = time.Now().UTC().Add(7 * 24 * time.Hour).Format(time.RFC3339)
		}

		meetings, err := client.ListMeetings(from, to, max)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to list meetings: %v", err)), nil
		}

		if len(meetings) == 0 {
			return mcp.NewToolResultText("No meetings found in the specified time range."), nil
		}

		text := fmt.Sprintf("%d meeting(s):\n\n", len(meetings))
		for _, m := range meetings {
			text += fmt.Sprintf("- **%s** (%s)\n  Time: %s → %s\n  State: %s | Host: %s\n  Meeting #: %s\n",
				m.Title, m.MeetingType, m.Start, m.End, m.State, m.HostDisplayName, m.MeetingNumber)
			if m.Agenda != "" {
				text += fmt.Sprintf("  Agenda: %s\n", m.Agenda)
			}
			if m.WebLink != "" {
				text += fmt.Sprintf("  Link: %s\n", m.WebLink)
			}
			text += "\n"
		}
		return mcp.NewToolResultText(text), nil
	})
}
