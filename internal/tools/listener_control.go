package tools

import (
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/mythingies/plugin-webex/internal/listener"
)

func registerListenerControl(s *mcpserver.MCPServer, lst *listener.Listener) {
	tool := mcp.NewTool("listener_control",
		mcp.WithDescription("Control the WebSocket listener for real-time inbound Webex messages. Actions: start, stop, status."),
		mcp.WithString("action",
			mcp.Required(),
			mcp.Description("The action to perform: \"start\", \"stop\", or \"status\"."),
			mcp.Enum("start", "stop", "status"),
		),
	)

	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		action, err := req.RequireString("action")
		if err != nil {
			return mcp.NewToolResultError("action is required"), nil
		}

		switch action {
		case "start":
			if err := lst.Start(ctx); err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("failed to start listener: %v", err)), nil
			}
			return mcp.NewToolResultText("WebSocket listener started. Inbound messages will be buffered and routed."), nil

		case "stop":
			if err := lst.Stop(ctx); err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("failed to stop listener: %v", err)), nil
			}
			return mcp.NewToolResultText("WebSocket listener stopped."), nil

		case "status":
			st := lst.Status()
			text := fmt.Sprintf("Listener status: **%s**\n", st.Status)
			text += fmt.Sprintf("- Connected: %v\n", st.Connected)
			text += fmt.Sprintf("- Messages received: %d\n", st.MessagesReceived)
			text += fmt.Sprintf("- Errors: %d\n", st.Errors)
			return mcp.NewToolResultText(text), nil

		default:
			return mcp.NewToolResultError(fmt.Sprintf("unknown action: %s (use start, stop, or status)", action)), nil
		}
	})
}
