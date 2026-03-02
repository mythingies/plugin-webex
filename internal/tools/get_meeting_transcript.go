package tools

import (
	"context"
	"fmt"

	"github.com/ecopelan/plugin-webex/internal/webex"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

func registerGetMeetingTranscript(s *mcpserver.MCPServer, client *webex.Client) {
	tool := mcp.NewTool("get_meeting_transcript",
		mcp.WithDescription("Pull the transcript from a past Webex meeting. Returns the full transcript text."),
		mcp.WithString("meeting_id",
			mcp.Required(),
			mcp.Description("The ID of the meeting to get the transcript for."),
		),
		mcp.WithString("format",
			mcp.Description("Transcript format: \"txt\" (plain text) or \"vtt\" (WebVTT with timestamps). Default: txt."),
			mcp.Enum("txt", "vtt"),
		),
	)

	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		meetingID, err := req.RequireString("meeting_id")
		if err != nil {
			return mcp.NewToolResultError("meeting_id is required"), nil
		}

		format := req.GetString("format", "txt")

		// Find transcripts for this meeting.
		transcripts, err := client.ListTranscripts(meetingID, 10)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to list transcripts: %v", err)), nil
		}

		if len(transcripts) == 0 {
			return mcp.NewToolResultText("No transcript available for this meeting."), nil
		}

		// Download the first available transcript.
		transcript := transcripts[0]
		content, err := client.DownloadTranscript(transcript.ID, format)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to download transcript: %v", err)), nil
		}

		header := fmt.Sprintf("**Transcript: %s** (started: %s, format: %s)\n\n",
			transcript.MeetingTopic, transcript.StartTime, format)

		return mcp.NewToolResultText(header + content), nil
	})
}
