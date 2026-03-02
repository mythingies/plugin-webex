package router

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	yaml := `
routes:
  - match:
      space: "Production Alerts"
    agent: alert-triage
    priority: critical
  - match:
      keywords: ["outage", "P1"]
      space: "*"
    agent: escalation
    priority: critical
    action: notify_dm
  - match:
      direct: true
    agent: dm-responder
    priority: high
    auto_respond: true
settings:
  buffer_size: 3000
  check_interval: 30s
  priority_levels: [critical, high, medium, low]
`
	path := writeTemp(t, yaml)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if len(cfg.Routes) != 3 {
		t.Fatalf("expected 3 routes, got %d", len(cfg.Routes))
	}

	if cfg.Routes[0].Agent != "alert-triage" {
		t.Errorf("expected agent alert-triage, got %s", cfg.Routes[0].Agent)
	}
	if cfg.Routes[1].Action != "notify_dm" {
		t.Errorf("expected action notify_dm, got %s", cfg.Routes[1].Action)
	}
	if !cfg.Routes[2].AutoRespond {
		t.Error("expected auto_respond true for route 3")
	}
	if cfg.Routes[2].Match.Direct != true {
		t.Error("expected direct true for route 3")
	}

	if cfg.Settings.BufferSize != 3000 {
		t.Errorf("expected buffer_size 3000, got %d", cfg.Settings.BufferSize)
	}
	if cfg.Settings.CheckInterval != "30s" {
		t.Errorf("expected check_interval 30s, got %s", cfg.Settings.CheckInterval)
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	yaml := `
routes:
  - match:
      space: "*"
    agent: general
    priority: low
`
	path := writeTemp(t, yaml)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if cfg.Settings.BufferSize != 5000 {
		t.Errorf("expected default buffer_size 5000, got %d", cfg.Settings.BufferSize)
	}
	if cfg.Settings.CheckInterval != "15s" {
		t.Errorf("expected default check_interval 15s, got %s", cfg.Settings.CheckInterval)
	}
	if len(cfg.Settings.PriorityLevels) != 4 {
		t.Errorf("expected 4 default priority levels, got %d", len(cfg.Settings.PriorityLevels))
	}
}

func TestLoadConfigMissing(t *testing.T) {
	_, err := LoadConfig("/nonexistent/path.yml")
	if err == nil {
		t.Error("expected error for missing file")
	}
}

func TestLoadConfigInvalid(t *testing.T) {
	path := writeTemp(t, "{{invalid yaml")
	_, err := LoadConfig(path)
	if err == nil {
		t.Error("expected error for invalid YAML")
	}
}

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Settings.BufferSize != 5000 {
		t.Errorf("expected 5000, got %d", cfg.Settings.BufferSize)
	}
	if len(cfg.Routes) != 0 {
		t.Errorf("expected 0 routes, got %d", len(cfg.Routes))
	}
}

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "agents.yml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing temp file: %v", err)
	}
	return path
}
