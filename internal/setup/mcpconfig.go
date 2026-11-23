package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/mythingies/plugin-webex/internal/auth"
)

// mcpConfigFile is the project-scoped Claude Code MCP config, written to the
// current directory so the server loads only for sessions started there.
const mcpConfigFile = ".mcp.json"

// mcpServerName is the key of our entry under "mcpServers".
const mcpServerName = "webex"

// authEnvKeys are the env vars that select an auth mode. Writing a new entry
// replaces all of them, so a stale WEBEX_TOKEN can't shadow a new OAuth
// session (PAT wins in resolveAuth). Other env vars, such as
// WEBEX_SHARE_DIRS, are kept.
var authEnvKeys = []string{"WEBEX_TOKEN", "WEBEX_CLIENT_ID", "WEBEX_CLIENT_SECRET"}

var (
	// ErrNoClientID means the stored OAuth session can't be tied to a client
	// ID: the tokens predate v0.9.6 and neither WEBEX_CLIENT_ID nor an
	// existing .mcp.json names one.
	ErrNoClientID = errors.New("OAuth client ID unknown for the stored session")

	// ErrNoClientSecret means no client secret is stored for the client ID,
	// so the server could not refresh tokens; the wizard has to collect it.
	ErrNoClientSecret = errors.New("no OAuth client secret stored for this client ID")
)

// ConfigResult reports what EnsureMCPConfig did.
type ConfigResult struct {
	Path    string // absolute path of .mcp.json
	Changed bool   // false when the existing entry was already correct
	Display string // the written entry, secrets masked
	// Warning is set when the write put a secret into a file that is
	// likely shared (see secretWarning); it is also logged to stderr.
	Warning string
}

// EnsureMCPConfig makes the current directory's .mcp.json point at this
// binary for the existing OAuth session. --setup calls it when a session is
// already stored, so running setup in a new directory registers the server
// there, and running it again repairs a stale binary path.
func EnsureMCPConfig(e *ExistingAuth) (*ConfigResult, error) {
	existing, err := readMCPConfig()
	if err != nil {
		return nil, err
	}

	clientID := resolveClientID(os.Getenv("WEBEX_CLIENT_ID"), e, existing)
	if clientID == "" {
		return nil, ErrNoClientID
	}
	if os.Getenv("WEBEX_CLIENT_SECRET") == "" {
		if _, err := auth.LoadClientSecret(clientID); err != nil {
			return nil, ErrNoClientSecret
		}
	}
	return writeMCPConfig(oauthEnv(clientID), existing)
}

// patEnv is the auth env for a Personal Access Token entry.
func patEnv(token string) map[string]string {
	return map[string]string{"WEBEX_TOKEN": token}
}

// oauthEnv is the auth env for an OAuth entry. The client secret is stored in
// the OS keychain, never in .mcp.json.
func oauthEnv(clientID string) map[string]string {
	return map[string]string{"WEBEX_CLIENT_ID": clientID}
}

// resolveClientID picks the client ID for the stored session: an explicit
// env var first, then the one saved with the tokens, then the one already in
// this directory's .mcp.json.
func resolveClientID(env string, e *ExistingAuth, existing []byte) string {
	if env != "" {
		return env
	}
	if e != nil && e.ClientID != "" {
		return e.ClientID
	}
	if entry := serverEntry(existing); entry != nil {
		if envMap, ok := entry["env"].(map[string]any); ok {
			if id, ok := envMap["WEBEX_CLIENT_ID"].(string); ok {
				return id
			}
		}
	}
	return ""
}

// readMCPConfig returns the current .mcp.json, or nil if there is none.
func readMCPConfig() ([]byte, error) {
	data, err := os.ReadFile(mcpConfigFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", mcpConfigFile, err)
	}
	return data, nil
}

// saveMCPConfig reads ./.mcp.json, merges our entry into it, and returns the
// masked display copy the wizard shows.
func saveMCPConfig(authEnv map[string]string) (string, error) {
	existing, err := readMCPConfig()
	if err != nil {
		return "", err
	}
	res, err := writeMCPConfig(authEnv, existing)
	if err != nil {
		return "", err
	}
	return res.Display, nil
}

// writeMCPConfig writes our server entry, pointing at this binary with
// authEnv, into ./.mcp.json, keeping any other servers and top-level keys
// already there. existing is the file's current contents (nil if absent).
func writeMCPConfig(authEnv map[string]string, existing []byte) (*ConfigResult, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolving executable path: %w", err)
	}
	exePath, err := filepath.Abs(exe)
	if err != nil {
		return nil, err
	}

	merged, changed, err := mergeMCPConfig(existing, exePath, authEnv)
	if err != nil {
		return nil, err
	}

	absPath, err := filepath.Abs(mcpConfigFile)
	if err != nil {
		return nil, err
	}

	res := &ConfigResult{
		Path:    absPath,
		Changed: changed,
		Display: displayEntry(exePath, authEnv),
	}
	if changed {
		if err := writeConfigFile(absPath, merged); err != nil {
			return nil, err
		}
		if w := secretWarning(existing, authEnv); w != "" && !isGitIgnored() {
			res.Warning = w
			slog.Warn(w, "path", absPath)
		}
	}
	return res, nil
}

// writeConfigFile replaces path with data through a 0600 temp file in the
// same directory and a rename, so the result is owner-only even when an
// existing file was world-readable (os.WriteFile keeps an existing file's
// mode), and a crash never leaves a half-written config.
func writeConfigFile(path string, data []byte) (err error) {
	f, err := os.CreateTemp(filepath.Dir(path), mcpConfigFile+".*.tmp")
	if err != nil {
		return fmt.Errorf("writing %s: %w", mcpConfigFile, err)
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmp) // best effort; the write already failed
		}
	}()
	if _, err := f.Write(data); err != nil {
		_ = f.Close() // the write error is the one to report
		return fmt.Errorf("writing %s: %w", mcpConfigFile, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close() // the sync error is the one to report
		return fmt.Errorf("writing %s: %w", mcpConfigFile, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("writing %s: %w", mcpConfigFile, err)
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return fmt.Errorf("restricting %s: %w", mcpConfigFile, err)
	}
	// On Windows, 0600 has no effect; strip inherited ACEs so other users on
	// the machine can't read an embedded PAT. The rename keeps the ACL.
	if err := auth.RestrictFileAccess(tmp); err != nil {
		return fmt.Errorf("locking %s ACL: %w", mcpConfigFile, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replacing %s: %w", mcpConfigFile, err)
	}
	return nil
}

// secretWarning returns a warning when authEnv puts a Personal Access Token
// into a .mcp.json that also configures other servers: such a file is often
// shared or committed, and it now holds a secret.
func secretWarning(existing []byte, authEnv map[string]string) string {
	if authEnv["WEBEX_TOKEN"] == "" || len(existing) == 0 {
		return ""
	}
	var root map[string]any
	if json.Unmarshal(existing, &root) != nil {
		return ""
	}
	servers, _ := root["mcpServers"].(map[string]any)
	for name := range servers {
		if name != mcpServerName {
			return mcpConfigFile + " configures other servers and now also holds your Webex access token; do not commit or share it (add it to .gitignore, or use OAuth, which keeps secrets in the OS keychain)"
		}
	}
	return ""
}

// isGitIgnored reports whether git ignores ./.mcp.json, in which case the
// secretWarning is not needed. A variable so tests don't depend on the
// directory they run in.
var isGitIgnored = func() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// Exit status 0 means ignored; anything else (not ignored, not a repo, no
	// git) means warn.
	return exec.CommandContext(ctx, "git", "check-ignore", "-q", "--", mcpConfigFile).Run() == nil
}

// mergeMCPConfig returns existing with mcpServers.webex set to command plus
// authEnv. Other servers, other top-level keys, and non-auth env vars on our
// entry are kept. changed is false when the result equals existing
// semantically, so callers can skip the write. Invalid JSON is an error
// rather than something to overwrite: the file may hold other servers.
func mergeMCPConfig(existing []byte, command string, authEnv map[string]string) ([]byte, bool, error) {
	root := map[string]any{}
	if len(bytes.TrimSpace(existing)) > 0 {
		if err := json.Unmarshal(existing, &root); err != nil {
			return nil, false, fmt.Errorf("existing %s is not valid JSON (fix or remove it, then rerun --setup): %w", mcpConfigFile, err)
		}
		if root == nil {
			root = map[string]any{}
		}
	}

	servers, _ := root["mcpServers"].(map[string]any)
	if servers == nil {
		if _, present := root["mcpServers"]; present {
			return nil, false, fmt.Errorf("existing %s has a non-object \"mcpServers\"", mcpConfigFile)
		}
		servers = map[string]any{}
	}

	env := map[string]any{}
	if old, ok := servers[mcpServerName].(map[string]any); ok {
		if oldEnv, ok := old["env"].(map[string]any); ok {
			for k, v := range oldEnv {
				env[k] = v
			}
		}
	}
	for _, k := range authEnvKeys {
		delete(env, k)
	}
	for k, v := range authEnv {
		env[k] = v
	}

	before, _ := json.Marshal(servers[mcpServerName])
	// Only command and env: args from a `go run` dev entry would break
	// the binary invocation.
	servers[mcpServerName] = map[string]any{
		"command": command,
		"env":     env,
	}
	root["mcpServers"] = servers
	after, _ := json.Marshal(servers[mcpServerName])

	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil, false, err
	}
	out = append(out, '\n')

	changed := len(bytes.TrimSpace(existing)) == 0 || !bytes.Equal(before, after)
	return out, changed, nil
}

// serverEntry returns mcpServers.webex from a .mcp.json body, or nil.
func serverEntry(data []byte) map[string]any {
	if len(data) == 0 {
		return nil
	}
	var root map[string]any
	if json.Unmarshal(data, &root) != nil {
		return nil
	}
	servers, _ := root["mcpServers"].(map[string]any)
	entry, _ := servers[mcpServerName].(map[string]any)
	return entry
}

// displayEntry renders our entry for the wizard and CLI with secrets masked.
func displayEntry(command string, authEnv map[string]string) string {
	masked := map[string]string{}
	for k, v := range authEnv {
		masked[k] = maskValue(v)
	}
	display, _ := json.MarshalIndent(map[string]any{
		"mcpServers": map[string]any{
			mcpServerName: map[string]any{"command": command, "env": masked},
		},
	}, "", "  ")
	return string(display)
}

// maskValue shows the first and last four characters of a credential, or
// only asterisks when it is too short to reveal any of it safely.
func maskValue(v string) string {
	if len(v) > 8 {
		return v[:4] + "..." + v[len(v)-4:]
	}
	return "****"
}
