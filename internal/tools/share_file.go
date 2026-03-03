package tools

import (
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/mythingies/plugin-webex/internal/webex"
)

func registerShareFile(s *mcpserver.MCPServer, client *webex.Client) {
	tool := mcp.NewTool("share_file",
		mcp.WithDescription("Upload and share a file to a Webex space. (Not yet implemented — multipart upload deferred to a future version.)"),
		mcp.WithString("room_id",
			mcp.Required(),
			mcp.Description("The ID of the space to share the file in."),
		),
		mcp.WithString("file_path",
			mcp.Required(),
			mcp.Description("The local file path to upload."),
		),
	)

	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		roomID, err := req.RequireString("room_id")
		if err != nil {
			return mcp.NewToolResultError("room_id is required"), nil
		}
		filePath, err := req.RequireString("file_path")
		if err != nil {
			return mcp.NewToolResultError("file_path is required"), nil
		}

		if err := client.ShareFile(roomID, filePath); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("%v", err)), nil
		}

		return mcp.NewToolResultText("File shared."), nil
	})
}
