# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

**plugin-webex** is a Claude Code plugin that integrates Cisco Webex into Claude Code — designed to surpass the official Slack plugin. It connects Claude Code to a user's Webex workspace through a locally-run HTTP MCP server, enabling Claude to read/send messages, monitor spaces in real-time, route inbound messages to specialized agents, send rich Adaptive Cards, and automate workflows — all from within Claude Code.

## Tech Stack

- **Language:** Go (see `go.mod` for the pinned version)
- **MCP SDK:** `mark3labs/mcp-go` over **stdio** — Claude Code launches the binary and speaks MCP over stdin/stdout (`internal/server/server.go`, `NewStdioServer`). There is no listening HTTP port.
- **WebSocket:** `webex-message-handler` Go implementation (for real-time inbound)
- **Credential storage:** `zalando/go-keyring` (OS keychain) with a 0600-file fallback
- **Lint:** `golangci-lint`
- **Test:** `go test`
- **Build:** `go build` / Makefile

## Authentication

`resolveAuth()` in `cmd/webex-mcp/main.go` selects the auth mode at startup, in priority order:

1. **PAT** — `WEBEX_TOKEN` set → `auth.NewStaticProvider`. Quick start; the token expires in ~12h. This is the only mode the bundled curl-based skill (`skills/webex/`) supports.
2. **OAuth (env override)** — `WEBEX_CLIENT_ID` + `WEBEX_CLIENT_SECRET` both set → OAuth with the secret taken from the environment. Intended for CI/testing.
3. **OAuth (keychain)** — `WEBEX_CLIENT_ID` set, secret absent from env → secret loaded from the OS keychain. This is the normal end-user path written by `webex-mcp --setup`.

OAuth uses PKCE with a `wmcp://oauth-callback` custom URI scheme (`auth.RegisterProtocol`, `--register-protocol`, `--oauth-callback`). Tokens auto-refresh.

### Credential storage (`internal/auth/`)

OAuth access/refresh tokens **and** the OAuth client secret live in the OS keychain (service `webex-mcp`, accounts `oauth-tokens` and `oauth-client-secret-<clientID>`), never in plaintext on disk. `.mcp.json` holds only the non-secret `WEBEX_CLIENT_ID` plus the binary path.

- `keyringAvailable()` probes the backend at construction; when no Secret Service exists (headless Linux, WSL, minimal containers) the store falls back to 0600 files in `~/.config/webex-mcp/`. On Windows that fallback path also gets an explicit ACL via `auth.RestrictFileAccess` (icacls `/inheritance:r`).
- `NewOAuthProvider` runs auto-migration on first launch: a pre-existing `tokens.json` is imported into the keychain and deleted (`MigrateTokensFromFile`), and a `WEBEX_CLIENT_SECRET` env var is copied into the keychain (`MigrateClientSecretFromEnv`, idempotent).
- Setting `WEBEX_CLIENT_SECRET` in the env always overrides the keychain — the CI/debugging escape hatch.

## Architecture

```
┌───────────────────────────────────────────────────────────────┐
│  webex-mcp server (stdio)                                     │
│                                                               │
│  ┌──────────────┐     ┌────────────────────────────────────┐ │
│  │ REST proxy    │     │ WebSocket listener (toggleable)    │ │
│  │ (MCP tools)   │     │ webex-message-handler              │ │
│  │               │     │ → in-memory ring buffer            │ │
│  │ Webex REST API│     │ → agent router (.webex-agents.yml) │ │
│  │ calls only    │     │ → priority classification          │ │
│  └──────────────┘     └────────────────────────────────────┘ │
└───────────────────────────────────────────────────────────────┘
         ↕ stdio (MCP)                   ↕ WebSocket (Mercury)
      Claude Code                      Webex Cloud
```

**Two modes, one binary:**
- **REST mode** (always on): Stateless proxy. Claude calls MCP tools → Webex REST API.
- **WebSocket mode** (toggleable via `/webex connect`): Real-time inbound messages via `webex-message-handler`. Messages buffered in memory, routed to agents.

`server.New()` (`internal/server/server.go`) wires the Webex client, ring buffer, router, and listener together, then `tools.Register` attaches every MCP tool. `Start()` serves MCP over stdin/stdout. The `cmd/webex-mcp` binary also dispatches the `--setup`, `--register-protocol`, and `--oauth-callback` subcommands before entering the server loop.

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
| `get_notifications` | Peek inbound message buffer (non-destructive) |
| `get_priority_inbox` | Classified/prioritized inbound messages (non-destructive peek) |
| `get_mentions` | All @mentions with context |
| `get_pending` | List messages still to process (durable, restart-surviving reminder) |
| `mark_processed` | Clear handled items from the pending list (local only) |
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
├── .mcp.json                    # MCP server config (stdio; WEBEX_CLIENT_ID + binary path)
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
│   ├── buffer/                  # In-memory ring buffer for live notifications
│   ├── triage/                  # Durable "still to process" reminder store (pending.json)
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
