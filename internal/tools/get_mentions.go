package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/mythingies/plugin-webex/internal/buffer"
)

func registerGetMentions(s *mcpserver.MCPServer, buf *buffer.RingBuffer) {
	tool := mcp.NewTool("get_mentions",
		mcp.WithDescription("Peek at buffered messages that contain @mentions. Returns mentions with context without removing messages from the buffer."),
		mcp.WithNumber("max",
			mcp.Description("Maximum number of messages to scan (default 100)."),
		),
	)

	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		max := req.GetInt("max", 100)

		messages := buf.Peek(max)
		if len(messages) == 0 {
			return mcp.NewToolResultText("No messages in buffer."), nil
		}

		var mentions []string
		for _, msg := range messages {
			if len(msg.Mentions) > 0 || containsMention(msg.Text) {
				mentionList := strings.Join(msg.Mentions, ", ")
				if mentionList == "" {
					mentionList = extractMentions(msg.Text)
				}
				mentions = append(mentions, fmt.Sprintf("- **%s** in **%s** (%s) mentioned [%s]: %s",
					msg.PersonEmail, msg.RoomTitle, msg.Created.Format("15:04:05"), mentionList, msg.Text))
			}
		}

		if len(mentions) == 0 {
			return mcp.NewToolResultText("No @mentions found in recent messages."), nil
		}

		text := fmt.Sprintf("%d mention(s) found:\n\n%s\n", len(mentions), strings.Join(mentions, "\n"))
		return mcp.NewToolResultText(text), nil
	})
}

// containsMention checks if text has @mention patterns.
func containsMention(text string) bool {
	return strings.Contains(text, "@")
}

// extractMentions finds @word patterns in text.
func extractMentions(text string) string {
	var found []string
	words := strings.Fields(text)
	for _, w := range words {
		if strings.HasPrefix(w, "@") && len(w) > 1 {
			found = append(found, w)
		}
	}
	if len(found) == 0 {
		return "@unknown"
	}
	return strings.Join(found, ", ")
}
