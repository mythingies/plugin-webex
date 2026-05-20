package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withPlaybookDir runs fn from a temp working directory containing
// agents/<file>=<content> entries, restoring CWD afterward.
func withPlaybookDir(t *testing.T, files map[string]string, fn func()) {
	t.Helper()
	origWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, playbookDir), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for name, body := range files {
		path := filepath.Join(tmp, playbookDir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(origWD) })

	resetPlaybookCache()
	fn()
}

func TestLoadPlaybook_Missing(t *testing.T) {
	withPlaybookDir(t, nil, func() {
		if got := loadPlaybook("alert-triage"); got != "" {
			t.Fatalf("expected empty for missing playbook, got %q", got)
		}
	})
}

func TestLoadPlaybook_ReadsAndTrims(t *testing.T) {
	withPlaybookDir(t, map[string]string{
		"alert-triage.md": "  do the thing  \n\n",
	}, func() {
		got := loadPlaybook("alert-triage")
		if got != "do the thing" {
			t.Fatalf("expected trimmed content, got %q", got)
		}
	})
}

func TestLoadPlaybook_TruncatesOversized(t *testing.T) {
	big := strings.Repeat("a", maxPlaybookSize*2)
	withPlaybookDir(t, map[string]string{
		"big.md": big,
	}, func() {
		got := loadPlaybook("big")
		if len(got) > maxPlaybookSize {
			t.Fatalf("expected ≤ %d bytes, got %d", maxPlaybookSize, len(got))
		}
		if got == "" {
			t.Fatal("expected truncated content, got empty")
		}
	})
}

func TestLoadPlaybook_RejectsUnsafeNames(t *testing.T) {
	cases := []string{
		"",
		"../etc/passwd",
		"../../secret",
		"foo/bar",
		`foo\bar`,
		".hidden",
		"-leading-dash",
		"with space",
		"with.dot",
		strings.Repeat("a", 65),
	}
	withPlaybookDir(t, nil, func() {
		for _, name := range cases {
			if got := loadPlaybook(name); got != "" {
				t.Errorf("expected empty for unsafe name %q, got %q", name, got)
			}
		}
	})
}

func TestLoadPlaybook_CachesResult(t *testing.T) {
	withPlaybookDir(t, map[string]string{
		"cached.md": "first",
	}, func() {
		first := loadPlaybook("cached")
		if first != "first" {
			t.Fatalf("expected %q, got %q", "first", first)
		}

		// Replace file content; cache should still return original.
		path := filepath.Join(playbookDir, "cached.md")
		if err := os.WriteFile(path, []byte("second"), 0o600); err != nil {
			t.Fatalf("rewrite: %v", err)
		}
		second := loadPlaybook("cached")
		if second != "first" {
			t.Fatalf("expected cached %q, got %q", "first", second)
		}
	})
}

func TestRenderPlaybooks_DedupesAndSkipsMissing(t *testing.T) {
	withPlaybookDir(t, map[string]string{
		"alert-triage.md": "alerts go here",
		"escalation.md":   "escalate fast",
	}, func() {
		out := renderPlaybooks([]string{"alert-triage", "alert-triage", "escalation", "missing"})
		if !strings.Contains(out, "### alert-triage") {
			t.Errorf("missing alert-triage section: %s", out)
		}
		if !strings.Contains(out, "### escalation") {
			t.Errorf("missing escalation section: %s", out)
		}
		if strings.Contains(out, "missing") {
			t.Errorf("missing playbook should not appear: %s", out)
		}
		if strings.Count(out, "### alert-triage") != 1 {
			t.Errorf("alert-triage should appear exactly once: %s", out)
		}
	})
}

func TestRenderPlaybooks_EmptyWhenAllMissing(t *testing.T) {
	withPlaybookDir(t, nil, func() {
		if got := renderPlaybooks([]string{"a", "b"}); got != "" {
			t.Fatalf("expected empty when no playbooks found, got %q", got)
		}
	})
}
