# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.5.0] - 2026-03-03

### Added
- Cross-platform installer scripts (`install.sh` for Linux/macOS, `install.ps1` for Windows)
- SHA256 checksum verification in both installers
- `checksums.txt` generation in release workflow
- Archive packaging in release workflow (`.tar.gz` for linux/darwin, `.zip` for windows)
- `CONTRIBUTORS.md`
- Deployment documentation in README

### Changed
- Migrated repository to `mythingies/plugin-webex`
- Updated module path to `github.com/mythingies/plugin-webex`
- README install section now includes curl, PowerShell, and `go install` methods
- Release workflow produces properly named archives instead of raw binaries

## [1.0.0] - 2026-03-02

### Added
- Comprehensive README.md for marketplace submission
- GitHub Actions CI/CD: lint, test, build, and cross-platform release workflows
- golangci-lint configuration (`.golangci.yml`)

### Changed
- Backfilled CHANGELOG with all prior release entries

## [0.3.0] - 2026-02-28

### Added
- `list_meetings` tool — list upcoming and recent Webex meetings
- `get_meeting_transcript` tool — pull transcript from a past meeting
- `get_digest` tool — activity digest for spaces over a time range
- `get_cross_space_context` tool — search a topic across all spaces and correlate results

## [0.2.0] - 2026-02-25

### Added
- WebSocket listener via Mercury for real-time inbound messages
- In-memory ring buffer for notification storage
- Agent routing engine with `.webex-agents.yml` configuration
- `get_notifications` tool — drain inbound message buffer
- `get_priority_inbox` tool — filter messages by priority level
- `get_mentions` tool — peek at @mentions with context
- `send_adaptive_card` tool — rich Adaptive Cards (tables, buttons, inputs)
- `share_file` tool — file upload/share to spaces (stub)
- `get_space_analytics` tool — message volume, active members, peak times
- `listener_control` tool — start/stop/status of WebSocket listener
- `get_notification_routes` tool — display agent routing configuration

## [0.1.0] - 2026-02-22

### Added
- Initial project scaffolding and plugin manifest
- MCP server with HTTP transport (mark3labs/mcp-go)
- Webex REST API client
- `list_spaces` tool — list user's Webex spaces
- `get_space_history` tool — read messages from a space
- `send_message` tool — send to a space, person, or thread
- `reply_to_thread` tool — reply to a specific message thread
- `get_users` tool — list members of a space
- `get_user_profile` tool — look up a person's details
- `add_reaction` tool — react to a message
- `search_messages` tool — cross-space full-text search
- `/webex` slash command
- Agent routing framework with `.webex-agents.yml`
- Makefile with build, test, lint, fmt, run targets
