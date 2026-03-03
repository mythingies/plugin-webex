package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/mythingies/plugin-webex/internal/router"
)

func registerGetNotificationRoutes(s *mcpserver.MCPServer, rtr *router.Router) {
	tool := mcp.NewTool("get_notification_routes",
		mcp.WithDescription("Display the current agent routing configuration from .webex-agents.yml. Shows how inbound messages are matched to agents."),
	)

	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		routes := rtr.Routes()
		if len(routes) == 0 {
			return mcp.NewToolResultText("No routes configured. Create a .webex-agents.yml file to define message routing."), nil
		}

		text := fmt.Sprintf("%d route(s) configured:\n\n", len(routes))
		for i, r := range routes {
			text += fmt.Sprintf("**Route %d** → agent: `%s`, priority: %s", i+1, r.Agent, r.Priority)
			if r.AutoRespond {
				text += ", auto-respond: yes"
			}
			if r.Action != "" {
				text += fmt.Sprintf(", action: %s", r.Action)
			}
			text += "\n"

			// Show match conditions.
			var conditions []string
			if r.Match.Space != "" {
				conditions = append(conditions, fmt.Sprintf("space: \"%s\"", r.Match.Space))
			}
			if r.Match.Direct {
				conditions = append(conditions, "direct: true")
			}
			if len(r.Match.Keywords) > 0 {
				conditions = append(conditions, fmt.Sprintf("keywords: [%s]", strings.Join(r.Match.Keywords, ", ")))
			}
			if len(conditions) > 0 {
				text += fmt.Sprintf("  Match: %s\n", strings.Join(conditions, ", "))
			}
			text += "\n"
		}

		return mcp.NewToolResultText(text), nil
	})
}
