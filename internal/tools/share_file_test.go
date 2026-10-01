package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

func TestReadShareable(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	write := func(dir, name, data string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	roots := []string{root}

	name, data, err := readShareable(write(root, "sub/SKILL.md", "hi"), roots)
	if err != nil || name != "SKILL.md" || string(data) != "hi" {
		t.Fatalf("allowed file: %q %q %v", name, data, err)
	}

	for label, p := range map[string]string{
		"outside the roots": write(outside, "notes.md", "x"),
		"hidden directory":  write(root, ".ssh/config", "x"),
		"ssh identity":      write(root, "id_ed25519", "x"),
		"pem key":           write(root, "server.pem", "x"),
		"env file":          write(root, ".env.local", "x"),
		"credentials file":  write(root, "aws-credentials.json", "x"),
		"directory":         filepath.Join(root, "sub"),
		"missing file":      filepath.Join(root, "nope.md"),
		"parent escape":     filepath.Join(root, "..", filepath.Base(outside), "notes.md"),
	} {
		if _, _, err := readShareable(p, roots); err == nil {
			t.Errorf("%s: %s was allowed", label, p)
		}
	}

	link := filepath.Join(root, "link.md")
	if err := os.Symlink(write(outside, "target.md", "x"), link); err == nil {
		if _, _, err := readShareable(link, roots); err == nil {
			t.Error("a symlink out of the root was allowed")
		}
	}
}

func TestMessageBody(t *testing.T) {
	req := func(args map[string]any) mcp.CallToolRequest {
		var r mcp.CallToolRequest
		r.Params.Arguments = args
		return r
	}
	if _, _, res := messageBody(req(nil), true); res == nil {
		t.Error("required body with neither text nor markdown was accepted")
	}
	if _, _, res := messageBody(req(nil), false); res != nil {
		t.Error("optional body without text or markdown was refused")
	}
	if _, _, res := messageBody(req(map[string]any{"markdown": strings.Repeat("a", maxMessageLen+1)}), true); res == nil {
		t.Error("over-long markdown was accepted")
	}
	text, md, res := messageBody(req(map[string]any{"text": "a\nb", "markdown": "**x**\n[y](javascript:alert(1))"}), true)
	if res != nil || text != "a\nb" || md != "**x**\n[y]([blocked-url]" {
		t.Errorf("messageBody = %q, %q, %v", text, md, res)
	}
}
