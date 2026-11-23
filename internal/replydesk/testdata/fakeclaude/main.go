// Command fakeclaude stands in for the claude CLI in replydesk's drafter
// tests. It checks how it was launched and prints a canned result envelope.
// A failed check is reported as an error envelope whose subtype names it, so
// the test sees it through the drafter's normal error path.
//
// The scenario comes from ANTHROPIC_FAKE_SCENARIO (an ANTHROPIC_ variable so
// it passes the drafter's environment allowlist).
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	switch os.Getenv("ANTHROPIC_FAKE_SCENARIO") {
	case "sleep":
		time.Sleep(30 * time.Second)
		return
	case "error":
		fmt.Fprintln(os.Stderr, "fake failure on stderr")
		emit(map[string]any{"type": "result", "subtype": "error_max_turns", "is_error": true, "result": "SECRET-RESULT"})
		os.Exit(1)
	}

	if failure := check(); failure != "" {
		emit(map[string]any{"type": "result", "subtype": "fake_assert_" + failure, "is_error": true})
		return
	}
	emit(map[string]any{
		"type":     "result",
		"subtype":  "success",
		"is_error": false,
		"result":   "",
		"structured_output": map[string]any{
			"needs_reply":        true,
			"needs_reply_reason": "asks for the deadline",
			"summary":            "  Wants the report deadline.  ",
			"asks":               []string{"When is the report due?"},
			"missing_info":       []string{},
			"draft":              "It is due Friday, as I said earlier.",
			"cited_refs":         []string{"p1", "b1", "zz9"},
			"no_prior_answer":    false,
		},
	})
}

// check returns "" when the launch looks right, else a short failure name.
func check() string {
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		upper := strings.ToUpper(name)
		if strings.HasPrefix(upper, "WEBEX_") {
			return "webex_env"
		}
		if upper == "CLAUDECODE" || upper == "CLAUDE_CODE_ENTRYPOINT" {
			return "claudecode_env"
		}
	}
	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		return "no_api_key"
	}

	args := os.Args[1:]
	if !hasPair(args, "--tools", "") {
		return "tools_not_empty"
	}
	for _, flag := range []string{"-p", "--bare", "--strict-mcp-config", "--no-session-persistence", "--disable-slash-commands"} {
		if !has(args, flag) {
			return "missing" + strings.ReplaceAll(flag, "-", "_")
		}
	}
	if !hasPair(args, "--output-format", "json") {
		return "output_format"
	}
	schema := value(args, "--json-schema")
	var js map[string]any
	if err := json.Unmarshal([]byte(schema), &js); err != nil {
		return "schema_not_json"
	}

	wd, err := os.Getwd()
	if err != nil {
		return "getwd"
	}
	mcp := value(args, "--mcp-config")
	if !sameDir(filepath.Dir(mcp), wd) {
		return "mcp_outside_cwd"
	}
	b, err := os.ReadFile(mcp) // #nosec G304 G703 -- test helper reading the path it was handed
	if err != nil || strings.TrimSpace(string(b)) != `{"mcpServers":{}}` {
		return "mcp_config"
	}
	sys := value(args, "--system-prompt-file")
	b, err = os.ReadFile(sys) // #nosec G304 G703 -- test helper reading the path it was handed
	if err != nil || !strings.Contains(string(b), "<external-message>") {
		return "system_prompt"
	}
	entries, err := os.ReadDir(wd)
	if err != nil || len(entries) != 2 {
		return "cwd_not_private"
	}

	stdin, err := io.ReadAll(os.Stdin)
	if err != nil {
		return "stdin"
	}
	in := string(stdin)
	if !strings.Contains(in, "Messages awaiting your reply") || !strings.Contains(in, "FAKE-MARKER-123") {
		return "prompt"
	}
	if strings.Contains(in, "do-not-leak") {
		return "secret_in_prompt"
	}
	return ""
}

func sameDir(a, b string) bool {
	ai, err := os.Stat(a) // #nosec G703 -- test helper; paths come from its own argv
	if err != nil {
		return false
	}
	bi, err := os.Stat(b) // #nosec G703 -- test helper; path is its own cwd
	return err == nil && os.SameFile(ai, bi)
}

func has(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

func hasPair(args []string, flag, want string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == want {
			return true
		}
	}
	return false
}

func value(args []string, flag string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}

func emit(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		os.Exit(2)
	}
	_, _ = os.Stdout.Write(append(b, '\n'))
}
