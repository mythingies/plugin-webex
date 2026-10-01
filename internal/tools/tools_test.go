package tools

import "testing"

func TestSanitizeOutboundText(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"keeps newlines", "line one\nline two\n\n- item", "line one\nline two\n\n- item"},
		{"keeps indentation", "```\nfunc f() {\n\treturn\n}\n```", "```\nfunc f() {\n\treturn\n}\n```"},
		{"keeps label colon", "Note: see https://example.com/a", "Note: see https://example.com/a"},
		{"keeps mailto", "mail mailto:a@example.com", "mail mailto:a@example.com"},
		{"blocks javascript", "click javascript:alert(1) now", "click [blocked-url] now"},
		{"blocks data uri", "x data:text/html;base64,AAAA y", "x [blocked-url] y"},
		{"blocks wmcp", "wmcp://oauth-callback?code=1", "[blocked-url]"},
		{"blocks scheme in markdown link", "[x](javascript:alert(1))", "[x]([blocked-url]"},
		{"blocks uppercase scheme", "JavaScript:alert(1)", "[blocked-url]"},
		{"blocks scheme on its own line", "a\njavascript:x\nb", "a\n[blocked-url]\nb"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sanitizeOutboundText(tt.in); got != tt.want {
				t.Errorf("sanitizeOutboundText(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
