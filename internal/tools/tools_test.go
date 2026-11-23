package tools

import (
	"fmt"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/mythingies/plugin-webex/internal/safetext"
)

// The sanitizer and sandbox rules themselves are tested in internal/safetext.
// These tests check that the tools are wired to them.

func TestSanitizeCardBody(t *testing.T) {
	card := map[string]interface{}{
		"type": "AdaptiveCard",
		"body": []interface{}{
			map[string]interface{}{"type": "TextBlock", "text": "line one\nclick javascript:alert(1)"},
			map[string]interface{}{"type": "TextBlock", "text": "<@all> heads up"},
			map[string]interface{}{"type": "Input.Text", "placeholder": "data:text/html,x", "id": "javascript:keep-id"},
		},
		"actions": []interface{}{
			map[string]interface{}{"type": "Action.OpenUrl", "title": "go wmcp://x", "url": "https://example.com"},
		},
		"fallbackText": "javascript:not-a-sanitized-key",
	}
	sanitizeCardBody(card)

	body := card["body"].([]interface{})
	actions := card["actions"].([]interface{})
	tests := []struct {
		name, got, want string
	}{
		{"text keeps newline, blocks scheme", body[0].(map[string]interface{})["text"].(string), "line one\nclick [blocked-url]"},
		{"text blocks all mention", body[1].(map[string]interface{})["text"].(string), "[blocked-mention] heads up"},
		{"placeholder", body[2].(map[string]interface{})["placeholder"].(string), "[blocked-url]"},
		{"non-text key untouched", body[2].(map[string]interface{})["id"].(string), "javascript:keep-id"},
		{"action title", actions[0].(map[string]interface{})["title"].(string), "go [blocked-url]"},
		{"action url untouched", actions[0].(map[string]interface{})["url"].(string), "https://example.com"},
		{"top-level other key untouched", card["fallbackText"].(string), "javascript:not-a-sanitized-key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("got %q, want %q", tt.got, tt.want)
			}
		})
	}
}

func TestMessageBodyErrors(t *testing.T) {
	req := func(args map[string]any) mcp.CallToolRequest {
		var r mcp.CallToolRequest
		r.Params.Arguments = args
		return r
	}
	long := strings.Repeat("a", safetext.MaxMessageLen+1)
	tooLong := fmt.Sprintf("message too long (%d chars, max %d)", len(long), safetext.MaxMessageLen)
	tests := []struct {
		name     string
		args     map[string]any
		required bool
		wantErr  string
	}{
		{"required empty", nil, true, "text or markdown is required"},
		{"required empty strings", map[string]any{"text": "", "markdown": ""}, true, "text or markdown is required"},
		{"text too long", map[string]any{"text": long}, true, tooLong},
		{"markdown too long optional", map[string]any{"markdown": long}, false, tooLong},
		{"optional empty ok", nil, false, ""},
		{"at limit ok", map[string]any{"text": long[1:]}, true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, res := messageBody(req(tt.args), tt.required)
			if tt.wantErr == "" {
				if res != nil {
					t.Fatalf("unexpected error result: %+v", res)
				}
				return
			}
			if res == nil || !res.IsError {
				t.Fatalf("want error %q, got %+v", tt.wantErr, res)
			}
			if len(res.Content) != 1 {
				t.Fatalf("want one content item, got %d", len(res.Content))
			}
			tc, ok := res.Content[0].(mcp.TextContent)
			if !ok {
				t.Fatalf("content is %T, want mcp.TextContent", res.Content[0])
			}
			if tc.Text != tt.wantErr {
				t.Errorf("error = %q, want %q", tc.Text, tt.wantErr)
			}
		})
	}
}

func TestMessageBodySanitizesMentions(t *testing.T) {
	var r mcp.CallToolRequest
	r.Params.Arguments = map[string]any{"text": "<@ALL> ship it", "markdown": "**<@all>**\nnow"}
	text, md, res := messageBody(r, true)
	if res != nil || text != "[blocked-mention] ship it" || md != "**[blocked-mention]**\nnow" {
		t.Errorf("messageBody = %q, %q, %v", text, md, res)
	}
}
