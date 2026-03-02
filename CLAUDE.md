# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

**plugin-webex** is a Claude Code plugin that integrates Cisco Webex into Claude Code — designed to surpass the official Slack plugin. It connects Claude Code to a user's Webex workspace through a locally-run HTTP MCP server, enabling Claude to read/send messages, monitor spaces in real-time, route inbound messages to specialized agents, send rich Adaptive Cards, and automate workflows — all from within Claude Code.

## Tech Stack

- **Language:** Go 1.22+
- **MCP SDK:** `mark3labs/mcp-go` (HTTP transport)
- **WebSocket:** `webex-message-handler` Go implementation (for real-time inbound)
- **Lint:** `golangci-lint`
- **Test:** `go test`
- **Build:** `go build` / Makefile

## Authentication

Users authenticate via a **Webex Personal Access Token (PAT)** generated at https://developer.webex.com/docs/getting-your-personal-access-token. Set as `WEBEX_TOKEN` environment variable. The MCP server reads it on startup.

## Architecture

```
┌───────────────────────────────────────────────────────────────┐
│  webex-mcp server (local HTTP MCP server)                     │
│                                                               │
│  ┌──────────────┐     ┌────────────────────────────────────┐ │
│  │ REST proxy    │     │ WebSocket listener (toggleable)    │ │
│  │ (MCP tools)   │     │ webex-message-handler              │ │
│  │               │     │ → in-memory ring buffer            │ │
│  │ Webex REST API│     │ → agent router (.webex-agents.yml) │ │
│  │ calls only    │     │ → priority classification          │ │
│  └──────────────┘     └────────────────────────────────────┘ │
└───────────────────────────────────────────────────────────────┘
         ↕ HTTP (MCP)                    ↕ WebSocket (Mercury)
      Claude Code                      Webex Cloud
```

**Two modes, one binary:**
- **REST mode** (always on): Stateless proxy. Claude calls MCP tools → Webex REST API.
- **WebSocket mode** (toggleable via `/webex connect`): Real-time inbound messages via `webex-message-handler`. Messages buffered in memory, routed to agents.

## MCP Tools

### Core (v0.1 — Slack parity)
| Tool | Description |
|---|---|
| `list_spaces` | List user's Webex spaces |
| `get_space_history` | Read messages from a space |
| `send_message` | Send to a space, person, or thread |
| `reply_to_thread` | Reply to a specific thread (parentId) |
| `get_users` | List people in a space |
| `get_user_profile` | Look up a person's details |
| `add_reaction` | React to a message |
| `search_messages` | Cross-space search with filters |

### Beyond Slack (v0.2+)
| Tool | Description |
|---|---|
| `get_notifications` | Drain inbound message buffer |
| `get_priority_inbox` | Classified/prioritized inbound messages |
| `get_mentions` | All @mentions with context |
| `send_adaptive_card` | Rich formatted cards (tables, buttons, inputs) |
| `share_file` | Upload/share files to spaces |
| `get_space_analytics` | Message volume, active members, peak times |
| `listener_control` | Start/stop/status of WebSocket listener |
| `get_notification_routes` | Show agent routing config |

### Intelligence (v0.3+)
| Tool | Description |
|---|---|
| `list_meetings` | Upcoming Webex meetings |
| `get_meeting_transcript` | Pull transcript from a past meeting |
| `get_digest` | AI-generated summary of spaces over time range |
| `get_cross_space_context` | Search topic across all spaces, correlate |

## Agent Routing

Inbound messages (via WebSocket) are routed to agents based on `.webex-agents.yml`:

```yaml
routes:
  - match:
      space: "Production Alerts"
    agent: alert-triage
    priority: critical
  - match:
      keywords: ["outage", "incident", "P1"]
      space: "*"
    agent: escalation
    priority: critical
    action: notify_dm
  - match:
      direct: true
    agent: dm-responder
    auto_respond: true
    priority: high
  - match:
      space: "*"
    agent: general
    priority: low
```

Agent definitions live in `agents/*.md` — markdown files with instructions Claude follows when messages match a route.

## Plugin Structure

```
plugin-webex/
├── .claude-plugin/
│   └── plugin.json              # Plugin manifest
├── .mcp.json                    # MCP server config (HTTP, localhost)
├── commands/
│   └── webex.md                 # /webex slash command
├── agents/
│   ├── alert-triage.md          # Alert summarization + action
│   ├── dm-responder.md          # Context-aware DM replies
│   ├── ops-summarizer.md        # Operational summaries
│   ├── escalation.md            # Severity detection + notification
│   ├── digest-builder.md        # Compile space activity digests
│   └── general.md               # Categorize + summarize
├── skills/
│   └── webex-monitor/
│       └── SKILL.md             # Auto-check notifications when listener active
├── cmd/
│   └── webex-mcp/
│       └── main.go              # Binary entry point
├── internal/
│   ├── server/                  # MCP server setup + tool registration
│   ├── webex/                   # Webex REST API client
│   ├── listener/                # WebSocket listener (webex-message-handler)
│   ├── buffer/                  # Ring buffer for notifications
│   ├── router/                  # Agent routing engine
│   └── tools/                   # MCP tool implementations
├── .webex-agents.yml            # Agent routing config (user-editable)
├── Makefile
├── go.mod
├── go.sum
├── .gitignore
├── CHANGELOG.md
├── CONTRIBUTING.md
└── LICENSE                      # MIT
```

## Development Commands

```bash
make build                       # Build binary to ./bin/webex-mcp
make run                         # Build and run MCP server locally
make test                        # Run all tests
make test T=TestName             # Run a single test
make lint                        # Run golangci-lint
make fmt                         # Format code (gofmt + goimports)
make clean                       # Remove build artifacts
```

## Quality Gates

### Local (pre-commit)
1. `make lint`
2. `make test`

### GitHub Actions (on PR)
- **Lint** — golangci-lint
- **Build** — `go build` verification
- **Release** — Semantic versioning with release tags

## Release Roadmap

| Version | Milestone |
|---|---|
| **v0.1** | Core MCP tools (Slack parity), `/webex` command, plugin scaffolding |
| **v0.2** | WebSocket listener, notification buffer, agent routing, adaptive cards |
| **v0.3** | Watchdogs, digests, meeting integration, priority inbox |
| **v1.0** | Marketplace submission, cross-space intelligence, context bridge |

## Plugin Submission

Targets the official Claude Code plugin marketplace. Requirements:
- Passing CI/CD (lint + test + build)
- README.md, CHANGELOG.md, CONTRIBUTING.md, LICENSE (MIT)
- Secure credential handling (WEBEX_TOKEN never logged or exposed)
- Plugin manifest in `.claude-plugin/plugin.json`
- Single binary distribution (no runtime dependencies)
