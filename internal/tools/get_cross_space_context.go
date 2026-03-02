package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/ecopelan/plugin-webex/internal/webex"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

func registerGetCrossSpaceContext(s *mcpserver.MCPServer, client *webex.Client) {
	tool := mcp.NewTool("get_cross_space_context",
		mcp.WithDescription("Search for a topic across all spaces and correlate results. Returns matches grouped by space with surrounding context messages, enabling cross-space analysis."),
		mcp.WithString("query",
			mcp.Required(),
			mcp.Description("The topic or text to search for across spaces."),
		),
		mcp.WithNumber("max_spaces",
			mcp.Description("Maximum number of spaces to search (default 10)."),
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

		maxSpaces := req.GetInt("max_spaces", 10)
		maxMessages := req.GetInt("max_messages", 100)
		queryLower := strings.ToLower(query)

		spaces, err := client.ListSpaces(maxSpaces)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to list spaces: %v", err)), nil
		}

		type match struct {
			Before  *webex.Message
			Message webex.Message
			After   *webex.Message
		}

		type spaceResult struct {
			Title   string
			Matches []match
		}

		var results []spaceResult
		totalMatches := 0

		for _, sp := range spaces {
			messages, err := client.GetMessages(sp.ID, maxMessages)
			if err != nil {
				continue
			}

			var matches []match
			for i, msg := range messages {
				if !strings.Contains(strings.ToLower(msg.Text), queryLower) {
					continue
				}

				m := match{Message: msg}

				// Include surrounding context (messages are newest-first from API).
				if i > 0 {
					m.After = &messages[i-1] // newer message (comes after chronologically)
				}
				if i < len(messages)-1 {
					m.Before = &messages[i+1] // older message (comes before chronologically)
				}

				matches = append(matches, m)
			}

			if len(matches) > 0 {
				totalMatches += len(matches)
				results = append(results, spaceResult{
					Title:   sp.Title,
					Matches: matches,
				})
			}
		}

		if len(results) == 0 {
			return mcp.NewToolResultText(fmt.Sprintf("No mentions of \"%s\" found across %d spaces.", query, len(spaces))), nil
		}

		text := fmt.Sprintf("## Cross-Space Context: \"%s\"\n\n", query)
		text += fmt.Sprintf("Found **%d match(es)** across **%d space(s)**:\n\n", totalMatches, len(results))

		for _, sr := range results {
			text += fmt.Sprintf("### %s (%d match%s)\n\n", sr.Title, len(sr.Matches), pluralize(len(sr.Matches)))
			for _, m := range sr.Matches {
				if m.Before != nil {
					text += fmt.Sprintf("  _%s (%s)_: %s\n", m.Before.PersonEmail, m.Before.Created, truncate(m.Before.Text, 100))
				}
				text += fmt.Sprintf("  **%s (%s): %s** ← match\n", m.Message.PersonEmail, m.Message.Created, truncate(m.Message.Text, 150))
				if m.After != nil {
					text += fmt.Sprintf("  _%s (%s)_: %s\n", m.After.PersonEmail, m.After.Created, truncate(m.After.Text, 100))
				}
				text += "\n"
			}
		}

		return mcp.NewToolResultText(text), nil
	})
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

func pluralize(n int) string {
	if n == 1 {
		return ""
	}
	return "es"
}
