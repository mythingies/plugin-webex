package router

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config represents the top-level .webex-agents.yml file.
type Config struct {
	Routes   []Route        `yaml:"routes"`
	Settings SettingsConfig `yaml:"settings"`
}

// Route maps a match condition to an agent with priority and behavior.
type Route struct {
	Match       MatchCondition `yaml:"match"`
	Agent       string         `yaml:"agent"`
	Priority    string         `yaml:"priority"`
	AutoRespond bool           `yaml:"auto_respond"`
	Action      string         `yaml:"action,omitempty"`
}

// MatchCondition defines how an inbound message is matched to a route.
type MatchCondition struct {
	Space    string   `yaml:"space,omitempty"`
	Keywords []string `yaml:"keywords,omitempty"`
	Direct   bool     `yaml:"direct,omitempty"`
}

// SettingsConfig holds global routing settings.
type SettingsConfig struct {
	BufferSize     int      `yaml:"buffer_size"`
	CheckInterval  string   `yaml:"check_interval"`
	PriorityLevels []string `yaml:"priority_levels"`
}

// LoadConfig reads and parses a .webex-agents.yml file.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}

	// Apply defaults.
	if cfg.Settings.BufferSize <= 0 {
		cfg.Settings.BufferSize = 5000
	}
	if cfg.Settings.CheckInterval == "" {
		cfg.Settings.CheckInterval = "15s"
	}
	if len(cfg.Settings.PriorityLevels) == 0 {
		cfg.Settings.PriorityLevels = []string{"critical", "high", "medium", "low"}
	}

	return &cfg, nil
}

// DefaultConfig returns a config with sensible defaults and no routes.
func DefaultConfig() *Config {
	return &Config{
		Settings: SettingsConfig{
			BufferSize:     5000,
			CheckInterval:  "15s",
			PriorityLevels: []string{"critical", "high", "medium", "low"},
		},
	}
}
