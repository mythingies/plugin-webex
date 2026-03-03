package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/mythingies/plugin-webex/internal/webex"
)

func registerGetUsers(s *mcpserver.MCPServer, client *webex.Client) {
	tool := mcp.NewTool("get_users",
		mcp.WithDescription("List members of a Webex space."),
		mcp.WithString("room_id",
			mcp.Required(),
			mcp.Description("The ID of the space to list members from."),
		),
		mcp.WithNumber("max",
			mcp.Description("Maximum number of members to return (default 100)."),
		),
	)

	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		roomID, err := req.RequireString("room_id")
		if err != nil {
			return mcp.NewToolResultError("room_id is required"), nil
		}

		max := req.GetInt("max", 100)

		members, err := client.ListMembers(roomID, max)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to list members: %v", err)), nil
		}

		var text string
		for _, m := range members {
			email := ""
			if len(m.Emails) > 0 {
				email = m.Emails[0]
			}
			text += fmt.Sprintf("- **%s** (%s, id: %s)\n", m.DisplayName, email, m.ID)
		}
		if text == "" {
			text = "No members found."
		}

		return mcp.NewToolResultText(text), nil
	})
}

func registerGetUserProfile(s *mcpserver.MCPServer, client *webex.Client) {
	tool := mcp.NewTool("get_user_profile",
		mcp.WithDescription("Get detailed profile information for a Webex user."),
		mcp.WithString("person_id",
			mcp.Required(),
			mcp.Description("The ID of the person to look up. Use 'me' for the authenticated user."),
		),
	)

	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		personID, err := req.RequireString("person_id")
		if err != nil {
			return mcp.NewToolResultError("person_id is required"), nil
		}

		var person *webex.Person
		if strings.EqualFold(personID, "me") {
			person, err = client.GetMe()
		} else {
			person, err = client.GetPerson(personID)
		}
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to get profile: %v", err)), nil
		}

		email := ""
		if len(person.Emails) > 0 {
			email = person.Emails[0]
		}

		text := fmt.Sprintf("**%s** (%s)\n- Email: %s\n- Status: %s\n- Type: %s\n- Created: %s\n",
			person.DisplayName, person.NickName, email, person.Status, person.Type, person.Created)

		return mcp.NewToolResultText(text), nil
	})
}
