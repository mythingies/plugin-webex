package server

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/mythingies/plugin-webex/internal/router"
)

func TestLoadRoutingConfig(t *testing.T) {
	dir := t.TempDir()
	builtin := len(router.BuiltinConfig().Routes)
	if builtin == 0 {
		t.Fatal("built-in config has no routes; the cases below would prove nothing")
	}

	valid := filepath.Join(dir, "valid.yml")
	if err := os.WriteFile(valid, []byte("routes:\n  - match: {direct: true}\n    agent: mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	invalid := filepath.Join(dir, "invalid.yml")
	if err := os.WriteFile(invalid, []byte("routes: [not: valid: yaml"), 0o600); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		path string
		want int
	}{
		{"empty path uses built-in", "", builtin},
		{"missing file uses built-in", filepath.Join(dir, "absent.yml"), builtin},
		{"valid file is used as-is", valid, 1},
		{"invalid file routes nothing", invalid, 0},
		// A path that exists but can't be read as a file must not fall
		// back to the sample routes either.
		{"unreadable path routes nothing", dir, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := len(loadRoutingConfig(tc.path).Routes); got != tc.want {
				t.Errorf("got %d routes, want %d", got, tc.want)
			}
		})
	}
}
