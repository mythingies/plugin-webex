# plugin-webex

Cisco Webex integration for Claude Code — read messages, send replies, monitor spaces in real-time, route inbound messages to agents, and automate workflows.

> **Heads up:** Cisco now ships an official Webex MCP server on the [Webex Agentic Platform](https://developer.webex.com/mcp/docs/webex-agentic-mcp-servers). For basic use (send/read messages, list spaces, meeting transcripts), that is the simplest path — no local binary, no setup wizard, native HTTPS transport.
>
> **Use the official Webex MCP if** you want quick request/response access to Webex from Claude Code and don't need real-time push or local routing.
>
> **Use plugin-webex if** you need any of the following, which Webex MCP does not currently provide:
> - **Real-time inbound** via WebSocket (Mercury) with a local ring buffer — agents can drain messages that arrived while idle
> - **Agent routing** with `.webex-agents.yml` — classify inbound messages by space/keywords/DM, assign priorities, isolate per-agent
> - **Priority inbox** and **@mention filtering** on the local buffer
> - **Aggregation tools** like `get_digest` (per-space activity summary) and `get_cross_space_context` (search-with-context across all your spaces)
> - **Local-only operation** with PAT (no WCIT, no integration registration, no traffic through Cisco's MCP infra)
> - **Skill-only mode** — pure curl wrapper, no binary, useful for sandboxed environments
>
> The two can also coexist — register both and Claude Code uses whichever tool fits the request.

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
        "WEBEX_CLIENT_ID": "your-client-id"
      }
    }
  }
}
```

The `WEBEX_CLIENT_SECRET` and OAuth access/refresh tokens are stored in the OS keychain (Windows Credential Manager, macOS Keychain, Linux Secret Service) — never in `.mcp.json` or on disk in plaintext. Run `webex-mcp --setup` and the wizard handles writing the secret to the keychain.

If your Linux environment has no Secret Service backend (headless servers, WSL, minimal containers), the binary falls back to a `0600` file in `~/.config/webex-mcp/`. Setting `WEBEX_CLIENT_SECRET` in the environment overrides the keychain — useful for CI and one-shot debugging.

For OAuth, create an integration at [developer.webex.com/my-apps](https://developer.webex.com/my-apps/new/integration) with redirect URI `wmcp://oauth-callback` and scopes: `spark:all`, `meeting:schedules_read`, `meeting:transcripts_read`.

> **Why `spark:all`?** The real-time WebSocket listener registers a Mercury device with Webex's WDM service (`wdm-a.wbx2.com`). That endpoint rejects granular-scoped tokens (`spark:messages_read`, `spark:rooms_read`, …) with **HTTP 403** — `spark:all` is the only public OAuth scope that grants it, matching what a Personal Access Token carries. If you only need the REST tools (no real-time listener), the granular scopes still work; drop `spark:all` and list `spark:messages_read spark:messages_write spark:rooms_read spark:memberships_read spark:people_read` instead. The `meeting:*` scopes are a separate family and are required either way for the meeting tools.

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
| `get_notifications` | Peek at inbound message buffer, newest-first (non-destructive) |
| `get_priority_inbox` | Filter buffered messages by priority level (non-destructive) |
| `get_mentions` | Peek at @mentions with surrounding context |
| `get_pending` | List messages still to process — a durable reminder that survives restarts |
| `mark_processed` | Clear handled items from the pending list (local only; never signals senders) |
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

## Pending reminders (triage)

When the WebSocket listener is active, every inbound message is recorded in a
durable "still to process" list. This is a **private, local reminder** — it
never touches Webex's unread/read state and never signals anything to the
sender.

The workflow it supports:

1. A message arrives → it's added as **pending**.
2. `get_pending` (or `get_notifications` / `get_priority_inbox`) lets you **peek
   at what's being asked** as often as you like — reading never clears it.
3. When you've actually handled the item, call `mark_processed` to remove it.

Because reading never clears a reminder, you don't lose track of what's
outstanding just by opening a message — and because "processed" is local-only,
a sender never sees a "read" they'd mistake for "seen and ignored." The list
persists across restarts at `~/.config/webex-mcp/pending.json` (0600).

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
| `WEBEX_CLIENT_SECRET` | — | OAuth Client Secret. Optional override; normally stored in OS keychain by setup. |
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

  - match:
      space: "*"
    agent: general
    priority: low

settings:
  buffer_size: 5000
  check_interval: 15s
  priority_levels: [critical, high, medium, low]
```

### Per-agent playbooks

When `get_notifications`, `get_priority_inbox`, or `get_mentions` returns
messages, each one carries its routed agent name. The MCP server inlines
`agents/<agent-name>.md` (relative to the working directory) into the tool
result so Claude has the right playbook in-context for every drained message.

For example, if `agents/alert-triage.md` contains your triage runbook and a
message routes to `alert-triage`, the tool result looks like:

```
2 notification(s):

- [critical] **al***@example.com** in **Production Alerts** ... agent: alert-triage): <external-message>...</external-message>
- [high] **bo***@example.com** in **DMs** ... agent: dm-responder): <external-message>...</external-message>

## Agent playbooks

### alert-triage
<contents of agents/alert-triage.md>

### dm-responder
<contents of agents/dm-responder.md>
```

Notes:
- Each playbook is capped at 4 KB to keep tool results reasonable.
- Agent names are validated (`a-z 0-9 - _`) and resolved relative to the
  working directory; path traversal is rejected.
- Missing playbooks are silently skipped — the routing config and the
  `agents/` files are independent.
- Multiple rooms can route to the same agent without duplicating playbook
  content.

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
