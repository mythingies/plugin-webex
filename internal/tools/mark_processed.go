package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/mythingies/plugin-webex/internal/triage"
)

func registerMarkProcessed(s *mcpserver.MCPServer, tri *triage.Store) {
	tool := mcp.NewTool("mark_processed",
		mcp.WithDescription("Mark one or more pending items as processed once you've handled them. This is the only way an item leaves the pending list — it never clears automatically, on read, or on reply. Local only: never sends anything to Webex or other users."),
		mcp.WithString("ids",
			mcp.Required(),
			mcp.Description("Comma-separated message ID(s) to mark processed (the `id` shown by get_pending)."),
		),
	)

	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		raw, err := req.RequireString("ids")
		if err != nil {
			return mcp.NewToolResultError("ids is required"), nil
		}

		var ids []string
		for _, id := range strings.Split(raw, ",") {
			if id = strings.TrimSpace(id); id != "" {
				ids = append(ids, id)
			}
		}
		if len(ids) == 0 {
			return mcp.NewToolResultError("at least one message ID is required"), nil
		}

		notFound, err := tri.MarkProcessed(ids...)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to mark processed: %v", err)), nil
		}

		processed := len(ids) - len(notFound)
		auditLog("mark_processed", "processed", "count", processed, "not_found", len(notFound))

		text := fmt.Sprintf("Marked %d item(s) as processed. %d remaining.", processed, tri.PendingCount())
		if len(notFound) > 0 {
			text += fmt.Sprintf("\nNot found (already pruned or never tracked): %s", strings.Join(notFound, ", "))
		}
		return mcp.NewToolResultText(text), nil
	})
}
