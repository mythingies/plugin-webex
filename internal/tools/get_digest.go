package tools

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/mythingies/plugin-webex/internal/webex"
)

func registerGetDigest(s *mcpserver.MCPServer, client *webex.Client) {
	tool := mcp.NewTool("get_digest",
		mcp.WithDescription("Generate an activity digest for one or more Webex spaces over a time range. Returns structured stats and message highlights that can be used to produce a summary."),
		mcp.WithString("room_id",
			mcp.Description("Limit to a specific space. If omitted, digests across the most recent spaces."),
		),
		mcp.WithNumber("hours_back",
			mcp.Description("Number of hours to look back (default 24)."),
		),
		mcp.WithNumber("max_spaces",
			mcp.Description("Maximum number of spaces to include when room_id is omitted (default 10)."),
		),
		mcp.WithNumber("max_messages",
			mcp.Description("Maximum messages to scan per space (default 200)."),
		),
	)

	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		roomID := req.GetString("room_id", "")
		hoursBack := req.GetInt("hours_back", 24)
		maxSpaces := req.GetInt("max_spaces", 10)
		maxMessages := req.GetInt("max_messages", 200)

		cutoff := time.Now().UTC().Add(-time.Duration(hoursBack) * time.Hour)

		type spaceDigest struct {
			Title      string
			Messages   int
			Authors    map[string]int
			Highlights []string
			FirstMsg   time.Time
			LastMsg    time.Time
		}

		var spaces []webex.Space
		if roomID != "" {
			spaces = []webex.Space{{ID: roomID, Title: roomID}}
			// Try to resolve the title.
			allSpaces, err := client.ListSpaces(1000)
			if err == nil {
				for _, sp := range allSpaces {
					if sp.ID == roomID {
						spaces[0].Title = sp.Title
						break
					}
				}
			}
		} else {
			var err error
			spaces, err = client.ListSpaces(maxSpaces)
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("failed to list spaces: %v", err)), nil
			}
		}

		var digests []spaceDigest
		totalMessages := 0

		for _, sp := range spaces {
			messages, err := client.GetMessages(sp.ID, maxMessages)
			if err != nil {
				continue
			}

			d := spaceDigest{
				Title:   sp.Title,
				Authors: make(map[string]int),
			}

			for _, msg := range messages {
				t, err := time.Parse(time.RFC3339, msg.Created)
				if err != nil {
					continue
				}
				if t.Before(cutoff) {
					continue
				}

				d.Messages++
				d.Authors[msg.PersonEmail]++

				if d.FirstMsg.IsZero() || t.Before(d.FirstMsg) {
					d.FirstMsg = t
				}
				if t.After(d.LastMsg) {
					d.LastMsg = t
				}

				// Capture first few messages as highlights.
				if len(d.Highlights) < 5 {
					preview := msg.Text
					if len(preview) > 120 {
						preview = preview[:120] + "..."
					}
					d.Highlights = append(d.Highlights, fmt.Sprintf("**%s**: %s", msg.PersonEmail, preview))
				}
			}

			if d.Messages > 0 {
				totalMessages += d.Messages
				digests = append(digests, d)
			}
		}

		if len(digests) == 0 {
			return mcp.NewToolResultText(fmt.Sprintf("No activity found in the last %d hours.", hoursBack)), nil
		}

		text := fmt.Sprintf("## Activity Digest — last %d hours\n\n", hoursBack)
		text += fmt.Sprintf("**%d space(s)** with **%d total messages**\n\n", len(digests), totalMessages)

		for _, d := range digests {
			text += fmt.Sprintf("### %s\n", d.Title)
			text += fmt.Sprintf("- Messages: %d\n", d.Messages)
			text += fmt.Sprintf("- Active authors: %d\n", len(d.Authors))

			// Top authors.
			topAuthors := topN(d.Authors, 3)
			if len(topAuthors) > 0 {
				text += fmt.Sprintf("- Top contributors: %s\n", strings.Join(topAuthors, ", "))
			}

			// Time range.
			if !d.FirstMsg.IsZero() {
				text += fmt.Sprintf("- Activity: %s → %s\n", d.FirstMsg.Format("15:04"), d.LastMsg.Format("15:04 UTC"))
			}

			// Highlights.
			if len(d.Highlights) > 0 {
				text += "- Recent:\n"
				for _, h := range d.Highlights {
					text += fmt.Sprintf("  - %s\n", h)
				}
			}
			text += "\n"
		}

		return mcp.NewToolResultText(text), nil
	})
}

// topN returns the top n keys from a count map, formatted as "key (count)".
func topN(counts map[string]int, n int) []string {
	type kv struct {
		Key   string
		Count int
	}

	var sorted []kv
	for k, v := range counts {
		sorted = append(sorted, kv{k, v})
	}
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Count > sorted[j].Count
	})

	if n > len(sorted) {
		n = len(sorted)
	}

	result := make([]string, n)
	for i := 0; i < n; i++ {
		result[i] = fmt.Sprintf("%s (%d)", sorted[i].Key, sorted[i].Count)
	}
	return result
}
