package setup

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testExe = "/opt/webex-mcp/bin/webex-mcp"

func decode(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, data)
	}
	return root
}

func webexEntry(t *testing.T, root map[string]any) map[string]any {
	t.Helper()
	servers, ok := root["mcpServers"].(map[string]any)
	if !ok {
		t.Fatalf("mcpServers missing: %v", root)
	}
	entry, ok := servers["webex"].(map[string]any)
	if !ok {
		t.Fatalf("webex entry missing: %v", servers)
	}
	return entry
}

func TestMergeMCPConfig_NewFile(t *testing.T) {
	out, changed, err := mergeMCPConfig(nil, testExe, map[string]string{"WEBEX_CLIENT_ID": "cid"})
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Error("expected changed for a new file")
	}
	entry := webexEntry(t, decode(t, out))
	if entry["command"] != testExe {
		t.Errorf("command = %v", entry["command"])
	}
	if env := entry["env"].(map[string]any); env["WEBEX_CLIENT_ID"] != "cid" {
		t.Errorf("env = %v", env)
	}
}

func TestMergeMCPConfig_KeepsOtherServersAndKeys(t *testing.T) {
	existing := []byte(`{
  "mcpServers": {
    "other": {"command": "other-server"},
    "webex": {"command": "/stale/path/webex-mcp", "args": ["run", "."],
              "env": {"WEBEX_TOKEN": "old-pat", "WEBEX_SHARE_DIRS": "/data"}}
  },
  "extra": true
}`)
	out, changed, err := mergeMCPConfig(existing, testExe, map[string]string{"WEBEX_CLIENT_ID": "cid"})
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Error("expected changed when the command is stale")
	}
	root := decode(t, out)
	if root["extra"] != true {
		t.Error("top-level key dropped")
	}
	servers := root["mcpServers"].(map[string]any)
	if _, ok := servers["other"]; !ok {
		t.Error("other server dropped")
	}
	entry := webexEntry(t, root)
	if entry["command"] != testExe {
		t.Errorf("stale command not replaced: %v", entry["command"])
	}
	if _, ok := entry["args"]; ok {
		t.Error("args from a go-run entry should be dropped")
	}
	env := entry["env"].(map[string]any)
	if _, ok := env["WEBEX_TOKEN"]; ok {
		t.Error("stale WEBEX_TOKEN would shadow OAuth; it must be removed")
	}
	if env["WEBEX_SHARE_DIRS"] != "/data" {
		t.Error("non-auth env var dropped")
	}
	if env["WEBEX_CLIENT_ID"] != "cid" {
		t.Errorf("client ID not set: %v", env)
	}
}

func TestMergeMCPConfig_UnchangedWhenCurrent(t *testing.T) {
	first, _, err := mergeMCPConfig(nil, testExe, map[string]string{"WEBEX_CLIENT_ID": "cid"})
	if err != nil {
		t.Fatal(err)
	}
	_, changed, err := mergeMCPConfig(first, testExe, map[string]string{"WEBEX_CLIENT_ID": "cid"})
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("expected no change when the entry is already correct")
	}
}

func TestMergeMCPConfig_RejectsInvalidJSON(t *testing.T) {
	for _, in := range []string{`{not json`, `{"mcpServers": []}`} {
		if _, _, err := mergeMCPConfig([]byte(in), testExe, nil); err == nil {
			t.Errorf("expected error for %q rather than overwriting it", in)
		}
	}
}

func TestResolveClientID(t *testing.T) {
	cwdConfig := []byte(`{"mcpServers":{"webex":{"command":"x","env":{"WEBEX_CLIENT_ID":"from-file"}}}}`)
	cases := []struct {
		name     string
		env      string
		auth     *ExistingAuth
		existing []byte
		want     string
	}{
		{"env wins", "from-env", &ExistingAuth{ClientID: "from-tokens"}, cwdConfig, "from-env"},
		{"tokens next", "", &ExistingAuth{ClientID: "from-tokens"}, cwdConfig, "from-tokens"},
		{"existing file last", "", &ExistingAuth{}, cwdConfig, "from-file"},
		{"unknown", "", &ExistingAuth{}, nil, ""},
		{"nil auth", "", nil, []byte(`{not json`), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveClientID(tc.env, tc.auth, tc.existing); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDisplayEntry_MasksSecrets(t *testing.T) {
	out := displayEntry(testExe, map[string]string{"WEBEX_TOKEN": "supersecretpersonaltoken"})
	if strings.Contains(out, "supersecretpersonaltoken") {
		t.Errorf("token not masked: %s", out)
	}
}

func TestMaskValue(t *testing.T) {
	cases := map[string]string{
		"":                   "****",
		"abc":                "****",
		"12345678":           "****",
		"C9b0123456789xyz78": "C9b0...yz78",
	}
	for in, want := range cases {
		if got := maskValue(in); got != want {
			t.Errorf("maskValue(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHandleDetect_ShortClientIDDoesNotPanic(t *testing.T) {
	// Used to slice clientID[:4] unconditionally.
	t.Setenv("WEBEX_CLIENT_ID", "abc")
	t.Setenv("WEBEX_CLIENT_SECRET", "s")
	rec := httptest.NewRecorder()
	handleDetect(rec, httptest.NewRequest(http.MethodGet, "/api/detect", nil))
	if !strings.Contains(rec.Body.String(), `"client_id":"****"`) {
		t.Errorf("unexpected response: %s", rec.Body.String())
	}
}
