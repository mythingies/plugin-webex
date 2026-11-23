// Package pluginwebex embeds the routing config and agent playbooks that ship
// with the plugin, so the binary works the same from any directory. Files in
// the process CWD (.webex-agents.yml, agents/<name>.md) still take precedence;
// these are the fallback when they are absent.
package pluginwebex

import "embed"

// DefaultRoutes is the shipped .webex-agents.yml.
//
//go:embed .webex-agents.yml
var DefaultRoutes []byte

// Playbooks holds the shipped agents/*.md files, addressed as
// "agents/<name>.md".
//
//go:embed agents/*.md
var Playbooks embed.FS
