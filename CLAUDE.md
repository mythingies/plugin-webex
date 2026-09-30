# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

**plugin-webex** is a Claude Code plugin that connects Claude Code to a user's Cisco Webex workspace. Claude Code launches a single Go binary (`webex-mcp`) and speaks MCP to it over **stdio** — there is no listening HTTP port. The binary proxies the Webex REST API as MCP tools and can optionally hold a Mercury WebSocket for real-time inbound messages, which it classifies, routes to agent playbooks, and records as durable local reminders.

There are two install flavors (see README "Skill vs MCP"):
- **`skills/webex/SKILL.md`** — standalone curl + jq skill, PAT (`WEBEX_TOKEN`) only, no binary.
- **MCP server** (`cmd/webex-mcp`) — full feature set, PAT or OAuth.

## Commands

```bash
make build            # go build -o bin/webex-mcp ./cmd/webex-mcp/
make test             # go test -v ./...
make test T=TestName  # go test -v -run 'TestName' ./...
make lint             # golangci-lint run ./...
make fmt              # gofmt -w . && goimports -w .
make clean
```

- Go version is pinned in `go.mod`; CI uses the matching `go-version` in `.github/workflows/*.yml`.
- CI's test job runs `go test -race -coverprofile=coverage.out ./...` — run with `-race` locally before pushing concurrency changes (listener, buffer, triage, token refresh).
- Lint is golangci-lint v2 (`.golangci.yml`): govet, errcheck, staticcheck, unused, ineffassign, **gosec**. goimports uses `local-prefixes: github.com/mythingies/plugin-webex`, so project imports go in their own group.
- The Makefile has no `run` target. To exercise the server, build and point a `.mcp.json` (template: `.mcp.json.example`; the real `.mcp.json` is gitignored) at the binary.

### Binary subcommands (`cmd/webex-mcp/main.go`)

Checked as `os.Args[1]` before the server starts:
- `--setup [--force]` — detect-first: if a usable OAuth session exists it prints the identity and exits; otherwise opens the local browser wizard (`internal/setup`, `setup.html`) on a random localhost port, which writes `.mcp.json` and the keychain entries. `--force` re-authenticates anyway.
- `--switch` — clear stored OAuth tokens, then run setup for a different account (client secret is kept).
- `--logout` — clear stored OAuth tokens only.
- `--register-protocol` / `--oauth-callback <url>` — register and handle the `wmcp://oauth-callback` URI scheme used by the PKCE flow.

With no subcommand it resolves auth, loads routing config from `WEBEX_AGENTS_CONFIG` (default `.webex-agents.yml`, relative to CWD; falls back to `router.DefaultConfig()` on missing/invalid), and serves MCP on stdio.

## Authentication

`resolveAuth()` in `cmd/webex-mcp/main.go` picks the mode, in priority order:

1. **PAT** — `WEBEX_TOKEN` set → `auth.NewStaticProvider`. Expires in ~12h.
2. **OAuth, env secret** — `WEBEX_CLIENT_ID` + `WEBEX_CLIENT_SECRET` both set. For CI/debugging; an env secret always overrides the keychain.
3. **OAuth, keychain secret** — only `WEBEX_CLIENT_ID` set; secret read from the OS keychain. The normal end-user path written by `--setup`.

OAuth is PKCE with auto-refresh. Everything implements `webex.TokenProvider`.

Credential storage (`internal/auth/`): access/refresh tokens and the client secret live in the OS keychain via `zalando/go-keyring` (service `webex-mcp`, accounts `oauth-tokens` and `oauth-client-secret-<clientID>`). `.mcp.json` holds only the non-secret client ID and binary path.
- `keyringAvailable()` probes the backend; with no Secret Service (headless Linux, WSL, containers) it falls back to 0600 files in `~/.config/webex-mcp/`. On Windows the fallback file also gets an explicit ACL (`store_acl_windows.go`, icacls `/inheritance:r`); `store_acl_other.go` is the non-Windows stub.
- `NewOAuthProvider` auto-migrates on first launch: legacy `tokens.json` → keychain then deleted (`MigrateTokensFromFile`); an env `WEBEX_CLIENT_SECRET` is copied into the keychain (`MigrateClientSecretFromEnv`, idempotent).
- `internal/setup/status.go`: `CurrentAuth()` does session detection independent of client ID; the decision logic is the pure `evalExistingAuth` so it can be unit-tested without keychain/HTTP.

## Architecture

`server.New()` (`internal/server/server.go`) builds the pieces and hands them all to `tools.Register`:

```
webex.Client (REST) ─┐
buffer.RingBuffer ───┤
router.Router ───────┼──> tools.Register(s, client, buf, rtr, lst, tri)
listener.Listener ───┤
triage.Store ────────┘   (triage failure is non-fatal: logs a warning, runs without persistence)
```

- **REST mode** (always on): one file per MCP tool in `internal/tools/`, each calling `internal/webex/client.go`. Tool names are the `mcp.NewTool("…")` strings; grep for them rather than trusting a list.
- **WebSocket mode** (toggled via the `listener_control` tool / `/webex connect`): `internal/listener` wraps `github.com/3rg0n/webex-message-handler/go`.

### Inbound message path (`listener.onMessage`)

1. Drop messages from self (`selfPersonID`) to prevent loops; per-second rate limit.
2. `router.Route()` matches against `.webex-agents.yml` routes → priority + agent name (unmatched → `low`).
3. Push a `buffer.NotificationMessage` into the in-memory ring buffer (lost on restart).
4. `triage.Add()` records the message as PENDING in `~/.config/webex-mcp/pending.json` (0600). `Add` is idempotent and never resets status. Messages without a usable ID skip triage — see `effectiveMessageID`.

Read-side semantics matter: `get_notifications`, `get_priority_inbox`, `get_mentions`, and `get_pending` are **non-destructive peeks**. Only `mark_processed` clears a reminder, and it is local-only. Nothing is ever sent back to Webex as a read receipt. This is deliberate — reading must not clear the user's to-do signal or show the sender a "read".

### Agent playbooks (`internal/tools/playbooks.go`)

`get_notifications`, `get_priority_inbox`, and `get_mentions` (not `get_pending`) call `renderPlaybooks()` to inline `agents/<agent>.md` for each distinct routed agent into the tool result. Paths resolve **relative to the process CWD** (`playbookDir = "agents"`), capped at 4 KB each, cached, and silently skipped if missing. Agent names are validated against `[A-Za-z0-9_-]`. That allowlist is intentionally duplicated from `router/config.go` so the tools package does not import router. Keep the two in sync.

### Security invariants in `internal/tools/tools.go`

These come from the MAESTRO threat model (`THREAT_MODEL.md`). New tools must follow them:
- Wrap any Webex-sourced text in `sandboxText()` (`<external-message>…</external-message>`) as a prompt-injection guard.
- Mask emails in output with `maskEmail()`.
- Run outbound text through `sanitizeOutboundText()`, which only allows http/https/mailto URLs (so no `javascript:`, `data:`, or `wmcp://`). Enforce `maxMessageLen` (7439) and `maxCardJSONLen` (28000).
- Call `auditLog()` for tool actions; drain-type tools are rate-limited (`toolRateInterval`).
- Tokens must never be logged. stdout is the MCP channel, so all logging goes to stderr via `slog`.

## Plugin assets

- `.claude-plugin/plugin.json`: plugin manifest. `commands/webex.md` is the `/webex` slash command.
- `agents/*.md`: the playbooks above. `.webex-agents.yml` is the routing config users edit.
- `skills/webex-monitor/SKILL.md`: auto-checks notifications while the listener is connected.
- `install.sh` / `install.ps1` download release binaries from GitHub Releases. `release.yml` builds them with `-trimpath -ldflags="-s -w"`.

## Conventions

- `CHANGELOG.md` follows Keep a Changelog + SemVer. Add entries under `[Unreleased]` or a new version heading, and record security results (govulncheck, CodeQL) in a `### Security` subsection as previous releases do.
- Target: official Claude Code plugin marketplace. That means single-binary distribution with no runtime deps, and CI lint + test + build must pass.
