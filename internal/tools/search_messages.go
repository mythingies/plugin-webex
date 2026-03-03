package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/mythingies/plugin-webex/internal/webex"
)

func registerSearchMessages(s *mcpserver.MCPServer, client *webex.Client) {
	tool := mcp.NewTool("search_messages",
		mcp.WithDescription("Search for messages containing specific text across one or more Webex spaces. Searches message content client-side since the Webex API does not support server-side full-text search."),
		mcp.WithString("query",
			mcp.Required(),
			mcp.Description("The text to search for in messages."),
		),
		mcp.WithString("room_id",
			mcp.Description("Limit search to a specific space. If omitted, searches across recent spaces."),
		),
		mcp.WithNumber("max_spaces",
			mcp.Description("Maximum number of spaces to search across (default 10, only used when room_id is omitted)."),
		),
		mcp.WithNumber("max_messages",
			mcp.Description("Maximum messages to scan per space (default 100)."),
		),
	)

	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		query, err := req.RequireString("query")
		if err != nil {
			return mcp.NewToolResultError("query is required"), nil
		}

		roomID := req.GetString("room_id", "")
		maxSpaces := req.GetInt("max_spaces", 10)
		maxMessages := req.GetInt("max_messages", 100)

		queryLower := strings.ToLower(query)
		var results []string

		if roomID != "" {
			// Search a single space.
			matches, err := searchSpace(client, roomID, queryLower, maxMessages)
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("search failed: %v", err)), nil
			}
			results = append(results, matches...)
		} else {
			// Search across recent spaces.
			spaces, err := client.ListSpaces(maxSpaces)
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("failed to list spaces: %v", err)), nil
			}
			for _, sp := range spaces {
				matches, err := searchSpace(client, sp.ID, queryLower, maxMessages)
				if err != nil {
					continue // skip spaces that fail
				}
				for _, m := range matches {
					results = append(results, fmt.Sprintf("[%s] %s", sp.Title, m))
				}
			}
		}

		if len(results) == 0 {
			return mcp.NewToolResultText(fmt.Sprintf("No messages found matching '%s'.", query)), nil
		}

		text := fmt.Sprintf("Found %d message(s) matching '%s':\n\n", len(results), query)
		for _, r := range results {
			text += "- " + r + "\n"
		}
		return mcp.NewToolResultText(text), nil
	})
}

func searchSpace(client *webex.Client, roomID, queryLower string, maxMessages int) ([]string, error) {
	messages, err := client.GetMessages(roomID, maxMessages)
	if err != nil {
		return nil, err
	}

	var matches []string
	for _, msg := range messages {
		if strings.Contains(strings.ToLower(msg.Text), queryLower) {
			matches = append(matches, fmt.Sprintf("**%s** (%s): %s", msg.PersonEmail, msg.Created, msg.Text))
		}
	}
	return matches, nil
}
