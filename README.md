# plugin-webex

Cisco Webex integration for Claude Code — read messages, send replies, monitor spaces in real-time, route inbound messages to agents, and automate workflows.

## Quick Start

### Prerequisites

- A Webex Personal Access Token ([generate one here](https://developer.webex.com/docs/getting-your-personal-access-token))

### Option A: Skill (no build required)

Copy the skill into your Claude Code skills directory. Only needs `curl`, `jq`, and `WEBEX_TOKEN`.

```bash
# Linux / macOS
mkdir -p ~/.claude/skills/webex
curl -fsSL https://raw.githubusercontent.com/mythingies/plugin-webex/main/skills/webex/SKILL.md \
  -o ~/.claude/skills/webex/SKILL.md

# Windows (PowerShell)
New-Item -ItemType Directory -Path "$env:USERPROFILE\.claude\skills\webex" -Force
irm https://raw.githubusercontent.com/mythingies/plugin-webex/main/skills/webex/SKILL.md |
  Set-Content "$env:USERPROFILE\.claude\skills\webex\SKILL.md"
```

Set your token:

```bash
export WEBEX_TOKEN="your-personal-access-token"
```

That's it — Claude Code picks up the skill automatically.

### Option B: MCP Server (full feature set)

The MCP server adds real-time WebSocket listener, notification buffer, agent routing, and priority inbox on top of the REST API.

#### Install

**curl (Linux / macOS)**

```bash
curl -fsSL https://raw.githubusercontent.com/mythingies/plugin-webex/main/install.sh | sh
```

**PowerShell (Windows)**

```powershell
irm https://raw.githubusercontent.com/mythingies/plugin-webex/main/install.ps1 | iex
```

**Go**

```bash
go install github.com/mythingies/plugin-webex/cmd/webex-mcp@latest
```

**From Source**

```bash
git clone https://github.com/mythingies/plugin-webex.git
cd plugin-webex
make build
```

#### Configure

Run the setup wizard:

```bash
webex-mcp --setup
```

This opens a browser-based setup UI where you can choose:
- **OAuth** (recommended) — persistent tokens that auto-refresh
- **Manual PAT** — quick start with a 12-hour personal access token

The wizard validates your credentials, writes `.mcp.json`, and registers the `wmcp://` protocol handler automatically.

Claude Code launches the server over stdio when it detects the `.mcp.json` config.

#### Manual Configuration

Alternatively, create `.mcp.json` manually (or copy `.mcp.json.example`):

**Personal Access Token:**
```json
{
  "mcpServers": {
    "webex": {
      "command": "webex-mcp",
      "env": {
        "WEBEX_TOKEN": "your-personal-access-token"
      }
    }
  }
}
```

**OAuth Integration** (auto-refreshing tokens):
```json
{
  "mcpServers": {
    "webex": {
      "command": "webex-mcp",
      "env": {
        "WEBEX_CLIENT_ID": "your-client-id",
        "WEBEX_CLIENT_SECRET": "your-client-secret"
      }
    }
  }
}
```

For OAuth, create an integration at [developer.webex.com/my-apps](https://developer.webex.com/my-apps/new/integration) with redirect URI `wmcp://oauth-callback` and scopes: `spark:messages_read`, `spark:messages_write`, `spark:rooms_read`, `spark:memberships_read`, `spark:people_read`, `meeting:schedules_read`, `meeting:transcripts_read`.

## Skill vs MCP

| | Skill | MCP Server |
|---|---|---|
| **Setup** | Copy one file + set `WEBEX_TOKEN` | Install binary + `.mcp.json` |
| **Dependencies** | `curl`, `jq` | None (single binary) |
| **REST API** | All endpoints | All endpoints |
| **WebSocket listener** | No | Yes |
| **Notification buffer** | No | Yes (in-memory ring buffer) |
| **Agent routing** | No | Yes (`.webex-agents.yml`) |
| **Priority inbox** | No | Yes |
| **OAuth / auto-refresh** | No | Yes |
| **Adaptive Cards** | Yes | Yes |
| **Meetings / transcripts** | Yes | Yes |

## Features

### Core Tools

| Tool | Description |
|---|---|
| `list_spaces` | List user's Webex spaces, sorted by recent activity |
| `get_space_history` | Read recent messages from a space |
| `send_message` | Send to a space, person, or thread |
| `reply_to_thread` | Reply to a specific message thread |
| `get_users` | List members of a space |
| `get_user_profile` | Look up a person's details |
| `search_messages` | Cross-space full-text search |

### Advanced (MCP only)

| Tool | Description |
|---|---|
| `get_notifications` | Drain inbound message buffer (newest-first) |
| `get_priority_inbox` | Filter buffered messages by priority level |
| `get_mentions` | Peek at @mentions with surrounding context |
| `send_adaptive_card` | Rich cards with tables, buttons, and inputs |
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
         ↕ stdio (MCP)                    ↕ WebSocket (Mercury)
      Claude Code                      Webex Cloud
```

Claude Code launches the server process and communicates over stdin/stdout using the MCP protocol.

## Configuration

### Environment Variables

| Variable | Default | Description |
|---|---|---|
| `WEBEX_TOKEN` | — | Webex Personal Access Token |
| `WEBEX_CLIENT_ID` | — | OAuth Client ID (alternative to PAT) |
| `WEBEX_CLIENT_SECRET` | — | OAuth Client Secret |
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

## Development

```bash
make build          # Build binary to ./bin/webex-mcp
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
│   ├── auth/            # PAT + OAuth (PKCE) authentication
│   ├── webex/           # Webex REST API client
│   ├── listener/        # WebSocket listener (Mercury)
│   ├── buffer/          # Ring buffer for notifications
│   ├── router/          # Agent routing engine
│   └── tools/           # MCP tool implementations
├── commands/            # /webex slash command
├── agents/              # Agent definition files (*.md)
├── skills/
│   ├── webex/           # Standalone skill (curl-based, no MCP)
│   └── webex-monitor/   # Auto-check notification skill
├── .claude-plugin/      # Plugin manifest
├── .mcp.json.example    # MCP config template
├── .webex-agents.yml    # Agent routing config
└── Makefile
```

## Security

- **Token handling**: Tokens are passed via environment variables and held in-memory only. Never logged or written to disk (except encrypted OAuth token store at `~/.config/webex-mcp/tokens.json` with 0600 permissions).
- **OAuth PKCE**: Authorization code flow with S256 code challenge. No client secret exposed to the browser.
- **Custom URI scheme**: `wmcp://` callback uses file-based IPC — no localhost HTTP server needed.
- **Redirect validation**: All redirect hops are checked against the original host to prevent token leakage.
- **Input limits**: Message text capped at 7,439 chars, card JSON at 28,000 bytes, callback params at 4,096 bytes.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for guidelines.

## License

[MIT](LICENSE)
