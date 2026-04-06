package router

import (
	"log/slog"
	"strings"
	"sync"
)

// RoutingResult holds the outcome of routing a message.
type RoutingResult struct {
	Agent    string
	Priority string
}

// InboundMessage contains the fields needed for route matching.
type InboundMessage struct {
	RoomTitle string
	RoomType  string // "group" or "direct"
	Text      string
}

// Router evaluates inbound messages against the route config.
type Router struct {
	mu         sync.RWMutex
	cfg        *Config
	configPath string
}

// NewRouter creates a router from a loaded config.
func NewRouter(cfg *Config, configPath string) *Router {
	return &Router{
		cfg:        cfg,
		configPath: configPath,
	}
}

// Route evaluates the message against routes top-to-bottom. First match wins.
// Returns nil if no route matches.
func (r *Router) Route(msg InboundMessage) *RoutingResult {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, route := range r.cfg.Routes {
		if matchRoute(route.Match, msg) {
			return &RoutingResult{
				Agent:    route.Agent,
				Priority: route.Priority,
			}
		}
	}
	return nil
}

// Routes returns a copy of the current route config for display.
func (r *Router) Routes() []Route {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]Route, len(r.cfg.Routes))
	copy(out, r.cfg.Routes)
	return out
}

// Reload re-reads the YAML config from disk.
func (r *Router) Reload() error {
	if r.configPath == "" {
		return nil
	}

	cfg, err := LoadConfig(r.configPath)
	if err != nil {
		return err
	}

	r.mu.Lock()
	oldRouteCount := len(r.cfg.Routes)
	r.cfg = cfg
	r.mu.Unlock()

	slog.Info("router config reloaded", "routes", len(cfg.Routes), "previous_routes", oldRouteCount)
	return nil
}

// matchRoute checks whether a message matches a route's conditions.
func matchRoute(cond MatchCondition, msg InboundMessage) bool {
	// Check direct message condition.
	if cond.Direct {
		if msg.RoomType != "direct" {
			return false
		}
		// Direct match passes — also check keywords if specified.
		if len(cond.Keywords) > 0 {
			return matchKeywords(cond.Keywords, msg.Text)
		}
		return true
	}

	// Check space name pattern.
	if cond.Space != "" {
		if !matchGlob(cond.Space, msg.RoomTitle) {
			return false
		}
	}

	// Check keywords.
	if len(cond.Keywords) > 0 {
		if !matchKeywords(cond.Keywords, msg.Text) {
			return false
		}
	}

	return true
}

// matchGlob performs simple glob matching: "*" matches anything,
// "Prefix*" matches prefix, and exact match otherwise.
func matchGlob(pattern, value string) bool {
	if pattern == "*" {
		return true
	}
	if strings.HasSuffix(pattern, "*") {
		prefix := strings.TrimSuffix(pattern, "*")
		return strings.HasPrefix(strings.ToLower(value), strings.ToLower(prefix))
	}
	return strings.EqualFold(pattern, value)
}

// matchKeywords returns true if any keyword appears as a case-insensitive
// substring in the text.
func matchKeywords(keywords []string, text string) bool {
	lower := strings.ToLower(text)
	for _, kw := range keywords {
		if strings.Contains(lower, strings.ToLower(kw)) {
			return true
		}
	}
	return false
}
