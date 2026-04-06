package router

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// maxConfigSize limits config file size to prevent parsing abuse.
const maxConfigSize = 1024 * 1024 // 1MB

// Config represents the top-level .webex-agents.yml file.
type Config struct {
	Routes   []Route        `yaml:"routes"`
	Settings SettingsConfig `yaml:"settings"`
}

// Route maps a match condition to an agent with priority.
type Route struct {
	Match    MatchCondition `yaml:"match"`
	Agent    string         `yaml:"agent"`
	Priority string         `yaml:"priority"`
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
	// Resolve symlinks to prevent symlink-based attacks.
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, fmt.Errorf("resolving config path: %w", err)
	}

	data, err := os.ReadFile(resolved)
	if err != nil {
		return nil, fmt.Errorf("reading config %s: %w", resolved, err)
	}

	if len(data) > maxConfigSize {
		return nil, fmt.Errorf("config file too large (%d bytes, max %d)", len(data), maxConfigSize)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
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

	if err := ValidateConfig(&cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// validAgentName checks that an agent name contains only safe characters.
var validAgentNameChars = func() map[rune]bool {
	m := make(map[rune]bool)
	for _, c := range "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_" {
		m[c] = true
	}
	return m
}()

// ValidateConfig checks that a config is within safe bounds and uses valid values.
func ValidateConfig(cfg *Config) error {
	if len(cfg.Routes) > 200 {
		return fmt.Errorf("too many routes (%d, max 200)", len(cfg.Routes))
	}

	// Build allowed priority set from settings.
	allowedPriority := make(map[string]bool, len(cfg.Settings.PriorityLevels))
	for _, p := range cfg.Settings.PriorityLevels {
		allowedPriority[p] = true
	}

	for i, route := range cfg.Routes {
		if len(route.Match.Keywords) > 50 {
			return fmt.Errorf("route %d: too many keywords (%d, max 50)", i, len(route.Match.Keywords))
		}
		if strings.Count(route.Match.Space, "*") > 5 {
			return fmt.Errorf("route %d: too many wildcards in space pattern", i)
		}

		// Validate agent name: alphanumeric, dash, underscore only.
		if route.Agent == "" {
			return fmt.Errorf("route %d: agent name is required", i)
		}
		for _, c := range route.Agent {
			if !validAgentNameChars[c] {
				return fmt.Errorf("route %d: invalid character %q in agent name %q (allowed: a-z, 0-9, -, _)", i, c, route.Agent)
			}
		}

		// Validate priority against configured levels.
		if route.Priority != "" && !allowedPriority[route.Priority] {
			return fmt.Errorf("route %d: invalid priority %q (allowed: %v)", i, route.Priority, cfg.Settings.PriorityLevels)
		}

		// Validate keyword content: reject control characters.
		for j, kw := range route.Match.Keywords {
			if kw == "" {
				return fmt.Errorf("route %d: keyword %d is empty", i, j)
			}
			if len(kw) > 200 {
				return fmt.Errorf("route %d: keyword %d too long (%d chars, max 200)", i, j, len(kw))
			}
		}
	}
	return nil
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
