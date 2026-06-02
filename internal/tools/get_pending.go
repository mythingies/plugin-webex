package tools

import (
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/mythingies/plugin-webex/internal/triage"
)

func registerGetPending(s *mcpserver.MCPServer, tri *triage.Store) {
	tool := mcp.NewTool("get_pending",
		mcp.WithDescription("List inbound messages still marked as 'to process' — a durable personal reminder that survives restarts. Reading this does NOT clear anything; items stay pending until explicitly marked processed via mark_processed. Newest-first."),
		mcp.WithNumber("max",
			mcp.Description("Maximum number of pending items to return (default 100)."),
		),
	)

	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		max := clampInt(req.GetInt("max", 100), 1, 5000)

		items := tri.ListPending()
		if len(items) == 0 {
			return mcp.NewToolResultText("Nothing pending — you're all caught up."), nil
		}
		if len(items) > max {
			items = items[:max]
		}

		text := fmt.Sprintf("%d item(s) still to process:\n\n", len(items))
		for _, it := range items {
			text += fmt.Sprintf("- `%s` [%s] **%s** in **%s** (%s): %s\n",
				it.ID, it.Priority, maskEmail(it.PersonEmail), it.RoomTitle,
				it.Created.Format("Jan 2 15:04"), sandboxText(it.Text))
		}
		text += "\nMark items done with mark_processed once you've handled them."
		return mcp.NewToolResultText(text), nil
	})
}
