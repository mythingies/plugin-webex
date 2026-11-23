package tools

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	pluginwebex "github.com/mythingies/plugin-webex"
)

// playbookDir is the directory holding per-agent markdown playbooks,
// resolved relative to the process working directory.
const playbookDir = "agents"

// builtinPlaybooks holds the playbooks shipped with the plugin, used when
// agents/<name>.md is absent from the working directory. A file on disk
// always wins, so users can override or blank out a shipped playbook.
// Tests swap it out.
var builtinPlaybooks fs.FS = pluginwebex.Playbooks

// maxPlaybookSize caps each playbook to keep tool results sane.
const maxPlaybookSize = 4 * 1024

// validAgentNameChar mirrors router/config.go's allowlist for agent names.
// Duplicated here (instead of imported) so the loader doesn't depend on the
// router package and stays callable from any tool handler.
var validAgentNameChar = func() map[rune]bool {
	m := make(map[rune]bool)
	for _, c := range "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_" {
		m[c] = true
	}
	return m
}()

var playbookCache = struct {
	mu sync.Mutex
	m  map[string]string
}{m: make(map[string]string)}

// loadPlaybook returns the contents of agents/<name>.md from the working
// directory, else the built-in copy, or "" if neither exists, it is
// unreadable, or the name is rejected by the safety guard. Results are cached.
func loadPlaybook(name string) string {
	if name == "" {
		return ""
	}
	if !safeAgentName(name) {
		return ""
	}

	playbookCache.mu.Lock()
	if cached, ok := playbookCache.m[name]; ok {
		playbookCache.mu.Unlock()
		return cached
	}
	playbookCache.mu.Unlock()

	f, err := openPlaybook(name)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()

	data, err := io.ReadAll(io.LimitReader(f, maxPlaybookSize))
	if err != nil {
		return ""
	}

	content := strings.TrimSpace(string(data))

	playbookCache.mu.Lock()
	playbookCache.m[name] = content
	playbookCache.mu.Unlock()

	return content
}

// openPlaybook opens agents/<name>.md from the working directory, falling
// back to the built-in copy only when the file does not exist there. name
// must already have passed safeAgentName.
func openPlaybook(name string) (io.ReadCloser, error) {
	path := filepath.Join(playbookDir, name+".md")
	f, err := os.Open(path) // #nosec G304 -- name passed safeAgentName ([A-Za-z0-9_-]), so path stays under playbookDir
	if err == nil {
		return f, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	// embed.FS paths always use forward slashes.
	return builtinPlaybooks.Open(playbookDir + "/" + name + ".md")
}

// safeAgentName rejects anything that could escape the agents/ directory
// or pull in unintended files: path separators, dots, leading dashes, etc.
func safeAgentName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	if name[0] == '-' {
		return false
	}
	for _, c := range name {
		if !validAgentNameChar[c] {
			return false
		}
	}
	return true
}

// renderPlaybooks builds the "## Agent playbooks" section for a set of agent
// names. Returns "" if no playbooks are found so callers can append it
// unconditionally.
func renderPlaybooks(agents []string) string {
	seen := make(map[string]bool)
	var sections []string
	for _, name := range agents {
		if seen[name] {
			continue
		}
		seen[name] = true
		body := loadPlaybook(name)
		if body == "" {
			continue
		}
		sections = append(sections, "### "+name+"\n"+body)
	}
	if len(sections) == 0 {
		return ""
	}
	return "\n## Agent playbooks\n\n" + strings.Join(sections, "\n\n") + "\n"
}

// resetPlaybookCache clears the cache. Test-only helper.
func resetPlaybookCache() {
	playbookCache.mu.Lock()
	playbookCache.m = make(map[string]string)
	playbookCache.mu.Unlock()
}
