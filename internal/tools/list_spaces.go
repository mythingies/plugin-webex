package tools

import (
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/mythingies/plugin-webex/internal/webex"
)

func registerListSpaces(s *mcpserver.MCPServer, client *webex.Client) {
	tool := mcp.NewTool("list_spaces",
		mcp.WithDescription("List the user's Webex spaces, sorted by most recent activity."),
		mcp.WithNumber("max",
			mcp.Description("Maximum number of spaces to return (default 50, max 1000)."),
		),
	)

	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		max := req.GetInt("max", 50)

		spaces, err := client.ListSpaces(max)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to list spaces: %v", err)), nil
		}

		var text string
		for _, sp := range spaces {
			text += fmt.Sprintf("- **%s** (type: %s, id: %s, last active: %s)\n", sp.Title, sp.Type, sp.ID, sp.LastActivity)
		}
		if text == "" {
			text = "No spaces found."
		}

		return mcp.NewToolResultText(text), nil
	})
}
