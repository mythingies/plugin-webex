# plugin-webex

Cisco Webex integration for Claude Code — read messages, send replies, monitor spaces in real-time, route inbound messages to agents, and automate workflows.

## Quick Start

### Prerequisites

- A Webex Personal Access Token ([generate one here](https://developer.webex.com/docs/getting-your-personal-access-token))

### Install

#### curl (Linux / macOS)

```bash
curl -fsSL https://raw.githubusercontent.com/mythingies/plugin-webex/main/install.sh | sh
```

#### PowerShell (Windows)

```powershell
irm https://raw.githubusercontent.com/mythingies/plugin-webex/main/install.ps1 | iex
```

#### Go

```bash
go install github.com/mythingies/plugin-webex/cmd/webex-mcp@latest
```

#### From Source

```bash
git clone https://github.com/mythingies/plugin-webex.git
cd plugin-webex
make build
```

### Configure

Set your Webex token:

```bash
export WEBEX_TOKEN="your-personal-access-token"
```

Add the MCP server to your Claude Code config (`.mcp.json`):

```json
{
  "webex": {
    "type": "http",
    "url": "http://localhost:3119"
  }
}
```

### Run

```bash
make run
```

The server starts on `localhost:3119` by default. Claude Code connects automatically via the MCP config.

## Features

### Core Tools (Slack Parity)

| Tool | Description |
|---|---|
| `list_spaces` | List user's Webex spaces, sorted by recent activity |
| `get_space_history` | Read recent messages from a space |
| `send_message` | Send to a space, person, or thread |
| `reply_to_thread` | Reply to a specific message thread |
| `get_users` | List members of a space |
| `get_user_profile` | Look up a person's details |
| `add_reaction` | React to a message |
| `search_messages` | Cross-space full-text search with filters |

### Beyond Slack

| Tool | Description |
|---|---|
| `get_notifications` | Drain inbound message buffer (newest-first) |
| `get_priority_inbox` | Filter buffered messages by priority level |
| `get_mentions` | Peek at @mentions with surrounding context |
| `send_adaptive_card` | Rich cards with tables, buttons, and inputs |
| `share_file` | Upload and share files to spaces |
| `get_space_analytics` | Message volume, active members, peak times |
| `listener_control` | Start/stop/status of WebSocket listener |
| `get_notification_routes` | Show agent routing configuration |

### Intelligence

| Tool | Description |
|---|---|
| `list_meetings` | Upcoming and recent Webex meetings |
| `get_meeting_transcript` | Pull transcript from a past meeting |
| `get_digest` | Activity digest for spaces over a time range |
| `get_cross_space_context` | Search a topic across all spaces, correlate results |

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
- **WebSocket mode** (toggleable via `listener_control`): Real-time inbound messages via Mercury WebSocket. Messages are buffered in memory and routed to agents.

## Configuration

### Environment Variables

| Variable | Default | Description |
|---|---|---|
| `WEBEX_TOKEN` | *(required)* | Webex Personal Access Token |
| `WEBEX_MCP_ADDR` | `:3119` | Address for the MCP HTTP server |
| `WEBEX_AGENTS_CONFIG` | `.webex-agents.yml` | Path to agent routing config |

### Agent Routing

Inbound messages (via WebSocket) are routed to agents based on `.webex-agents.yml`. Routes are evaluated top-to-bottom; first match wins.

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
    priority: high
    auto_respond: true

  - match:
      space: "*"
    agent: general
    priority: low

settings:
  buffer_size: 5000
  check_interval: 15s
  priority_levels: [critical, high, medium, low]
```

**Match fields:**

- `space` — Space name (exact match or `*` for all). Supports glob prefix matching (e.g. `"Ops*"`).
- `keywords` — List of keywords to match in message text.
- `direct` — Match direct (1:1) messages.

**Route fields:**

- `agent` — Agent name. Agent definitions live in `agents/*.md`.
- `priority` — `critical`, `high`, `medium`, or `low`.
- `auto_respond` — If `true`, Claude drafts a reply automatically.
- `action` — Optional action (e.g. `notify_dm` to send a DM notification).

## Deployment

### Releasing a New Version

This project follows [Semantic Versioning](https://semver.org/). Releases are automated via GitHub Actions.

1. **Update CHANGELOG.md** with the new version entry
2. **Tag the release** and push:

```bash
git tag v0.6.0
git push origin main --tags
```

3. The **Release** workflow automatically:
   - Cross-compiles binaries for linux/darwin/windows (amd64 + arm64)
   - Packages them as `.tar.gz` (linux/darwin) and `.zip` (windows)
   - Generates `checksums.txt` (SHA256)
   - Creates a GitHub Release with all assets

### Version Scheme

| Bump | When | Example |
|---|---|---|
| **Patch** (`0.5.x`) | Bug fixes, docs | `v0.5.0` -> `v0.5.1` |
| **Minor** (`0.x.0`) | New features, backward-compatible | `v0.5.0` -> `v0.6.0` |
| **Major** (`x.0.0`) | Breaking changes | `v0.5.0` -> `v1.0.0` |

### CI/CD Pipelines

| Workflow | Trigger | Purpose |
|---|---|---|
| **Build** | Push/PR to `main` | Compile verification |
| **Lint** | Push/PR to `main` | golangci-lint static analysis |
| **Test** | Push/PR to `main` | Unit tests with race detection + coverage |
| **Release** | Tag `v*` | Cross-platform build, package, and publish |

## Security

- **Token handling**: `WEBEX_TOKEN` is read from the environment at startup and passed in-memory to the HTTP client. It is never logged, written to disk, or exposed through MCP tool responses.
- **Local only**: The MCP server binds to `localhost` by default. No external network exposure.
- **HTTPS**: For production use, place the server behind a reverse proxy with TLS termination.

## Development

```bash
make build          # Build binary to ./bin/webex-mcp
make run            # Build and run MCP server
make test           # Run all tests
make test T=Name    # Run a single test
make lint           # Run golangci-lint
make fmt            # Format code (gofmt + goimports)
make clean          # Remove build artifacts
```

### Project Structure

```
plugin-webex/
├── cmd/webex-mcp/       # Binary entry point
├── internal/
│   ├── server/          # MCP server setup + tool registration
│   ├── webex/           # Webex REST API client
│   ├── listener/        # WebSocket listener (Mercury)
│   ├── buffer/          # Ring buffer for notifications
│   ├── router/          # Agent routing engine
│   └── tools/           # MCP tool implementations
├── commands/            # /webex slash command
├── agents/              # Agent definition files (*.md)
├── skills/              # Auto-check notification skill
├── .claude-plugin/      # Plugin manifest
├── .webex-agents.yml    # Agent routing config
└── Makefile
```

### Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for guidelines.

## Contributors

See [CONTRIBUTORS.md](CONTRIBUTORS.md) for the full list.

## License

[MIT](LICENSE) - Copyright (c) 2026 mythingies
