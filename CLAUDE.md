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
- Suppress a gosec finding with `// #nosec G<rule> -- <reason>`. golangci-lint honors it, and standalone gosec does too; standalone gosec ignores `//nolint:gosec`, which older lines (mostly `internal/auth`) still use.
- The Makefile has no `run` target. To exercise the server, build and point a `.mcp.json` (template: `.mcp.json.example`; the real `.mcp.json` is gitignored) at the binary.

### Binary subcommands (`cmd/webex-mcp/main.go`)

Checked as `os.Args[1]` before the server starts:
- `--setup [--force]` — detect-first: if a usable OAuth session exists it prints the identity, then `setup.EnsureMCPConfig` writes or repairs `./.mcp.json` without a browser. The client ID comes from `WEBEX_CLIENT_ID`, then `StoredTokens.ClientID` (saved since v0.9.6), then the existing `.mcp.json`; with no ID it exits 1, and with no stored secret for it the wizard opens. Otherwise it opens the local browser wizard (`internal/setup`, `setup.html`) on a random localhost port, which writes `.mcp.json` and the keychain entries. `--force` re-authenticates anyway. Both paths go through `mergeMCPConfig` (`internal/setup/mcpconfig.go`). It replaces only `mcpServers.webex` with `command` + `env`: auth env keys are reset and other env keys are kept. Other servers stay, and it refuses to overwrite invalid JSON. `.mcp.json` lands in the CWD, which is how a user scopes the server to one directory.
- `--switch` — clear stored OAuth tokens, then run setup for a different account (client secret is kept).
- `--logout` — clear stored OAuth tokens only.
- `--register-protocol` / `--oauth-callback <url>` — register and handle the `wmcp://oauth-callback` URI scheme used by the PKCE flow.
- `--review [--dry-run] [--lookback 7d] [--drafter auto|bare|isolated|off] [--model m] [--no-browser]` — Reply Desk (`cmd/webex-mcp/review.go` → `internal/replydesk`; see below). `--logout`/`--switch` also call `replydesk.Purge()`.

With no subcommand it resolves auth, loads routing config from `WEBEX_AGENTS_CONFIG` (default `.webex-agents.yml`, relative to CWD). A missing file falls back to `router.BuiltinConfig()`, the repo's `.webex-agents.yml` embedded by the root package `embed.go`. An invalid file falls back to `router.DefaultConfig()`, which has no routes, so a broken user config is never silently swapped for the sample routes. Then it serves MCP on stdio.

### Releasing

The version is hard-coded in two places with no ldflag injection: `"version"` in `.claude-plugin/plugin.json` and `const version` in `internal/setup/setup.go` (shown in the wizard). Keep both equal to the tag. Pushing a `v*` tag runs `release.yml`, which publishes the six platform archives, `checksums.txt`, and a CycloneDX `sbom.cdx.json`.

`install.ps1` prompts with `Read-Host` (MCP server vs skill); non-interactively use `echo 1 | pwsh -File install.ps1 -Version vX.Y.Z`, or `-SkillOnly`. Windows won't overwrite a running `webex-mcp.exe` (each Claude Code session runs one), but it can be renamed aside first.

## Authentication

`resolveAuth()` in `cmd/webex-mcp/main.go` picks the mode, in priority order:

1. **PAT** — `WEBEX_TOKEN` set → `auth.NewStaticProvider`. Expires in ~12h.
2. **OAuth, env secret** — `WEBEX_CLIENT_ID` + `WEBEX_CLIENT_SECRET` both set. For CI/debugging; an env secret always overrides the keychain.
3. **OAuth, keychain secret** — only `WEBEX_CLIENT_ID` set; secret read from the OS keychain. The normal end-user path written by `--setup`.

OAuth is PKCE with auto-refresh. Everything implements `webex.TokenProvider`.

The callback has no HTTP listener. Webex redirects to `wmcp://oauth-callback`. The OS handler registered by `RegisterProtocol` (on Windows, `wmcp-callback.vbs` beside the binary, run hidden) launches `webex-mcp --oauth-callback <url>`, which writes `oauth-callback.json` (0600) to the user config dir. `AuthorizeWith` polls that file, checks `state`, and exchanges the code. `Authorize` is `AuthorizeWith(openBrowser)`. The setup wizard instead posts `popup:true` to `/api/oauth`, which streams NDJSON: `{auth_url}` first, then the result. The page loads that URL into a window it opened during the click and closes it at the end. The `window.open` call must stay synchronous in the click handler or browsers block it.

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

`get_notifications`, `get_priority_inbox`, and `get_mentions` (not `get_pending`) call `renderPlaybooks()` to inline `agents/<agent>.md` for each distinct routed agent into the tool result. Paths resolve **relative to the process CWD** (`playbookDir = "agents"`). Only when the file doesn't exist there does it fall back to the copy embedded from the repo's `agents/` (`pluginwebex.Playbooks`; tests swap `builtinPlaybooks`). Playbooks are capped at 4 KB each, cached, and silently skipped if missing from both. `TestBuiltinConfig` requires every built-in route's agent to have an embedded playbook. Agent names are validated against `[A-Za-z0-9_-]`. That allowlist is intentionally duplicated from `router/config.go` so the tools package does not import router. Keep the two in sync.

### Reply Desk (`internal/replydesk`, ADR 0002)

`webex-mcp --review` is a separate process, not part of the MCP server. It finds 1:1 DMs where the other person wrote since the user's last message (the "burst"), drafts replies, and serves a loopback review page. `types.go` is the contract between the package's parts and `web/app.js`; change it deliberately. Authority is split:
- **Read:** `Scan` / `Detect` (pure, tested without HTTP) and grounding issue only GETs. `invariant_test.go` records every request against an httptest Webex and asserts this, plus that the one `POST /messages` happens only on Send.
- **Draft:** `drafter.go` runs `claude -p` with `--tools ""`, `--strict-mcp-config` with an empty config, `--no-session-persistence`, a temp cwd, the prompt on stdin, and an env allowlist with no `WEBEX_*`. `--bare` is used when an API key or Bedrock/Vertex/Foundry is configured, because under `--bare` the CLI never reads the subscription login. The model sees local refs (`b1`, `c2`, `p3`), never Webex IDs. The system prompt is `prompts/drafter.md`. Retrieval (`Rank`, BM25 + density gate) is limited to the user's own messages in the same DM.
- **Send:** only `Service.Send`, after the user confirms. It checks, in order: state, empty text, SHA-256 of the confirmed text, `[[FILL` placeholders, byte length, `Sanitize` diff (needs `ack_sanitized`), rate limit, then a live re-check (ask deleted or edited, already replied, new messages). The target comes from the scan. A failed POST becomes `send_unknown` and is never retried.
- **Server** (`server.go`): `127.0.0.1:0`; a single-use launch token becomes an HttpOnly SameSite=Strict cookie; exact `Host` on every request; exact `Origin`, `Sec-Fetch-Site`, CSRF header (`X-ReplyDesk-CSRF`) and JSON-only bodies on POST; strict CSP, so `web/` must not use inline scripts, inline styles or `innerHTML` for data; idle and lifetime shutdown; never writes to stdout.
- **State:** `store.go`, `replydesk.json` in the config dir (0600 + ACL, owner-bound, pruned after 30 days, purged on logout). Records are keyed by room and carry the burst head they apply to, so a new message re-opens a dismissed room.

### Security invariants (`internal/safetext`, `internal/tools/tools.go`)

These come from the MAESTRO threat model (`THREAT_MODEL.md`). New tools must follow them:
- Wrap any Webex-sourced text in `safetext.Sandbox()` (`<external-message>…</external-message>`) as a prompt-injection guard. It neutralizes opening and closing wrapper tags inside the text (F19); sandbox *before* truncating, so a cut can't leave a partial tag behind.
- Mask emails in output with `maskEmail()`. The exceptions are `get_users` and `get_user_profile`, which return full addresses on purpose because looking people up is their job (`people.go`, asserted in `labels_test.go`). Labels such as display names still go through `safetext.Label`.
- Run outbound text and markdown through `safetext.Prepare` / `safetext.Sanitize`. These allow only http/https/mailto URLs (so no `javascript:`, `data:`, or `wmcp://`), replace any other `scheme:` match in place (including one inside a Markdown link, and `<@personEmail:…>` mentions), turn `<@all>` into `[blocked-mention]`, and leave whitespace and newlines alone. Tests: `internal/safetext/safetext_test.go`. Enforce `safetext.MaxMessageLen` (7439 bytes, checked on the raw input) and `maxCardJSONLen` (28000).
- Local file reads that a Webex message could steer go through a root-restricted check like `readShareable()` in `share_file.go`: regular files only (no symlinks), under CWD, the OS temp dir, or `WEBEX_SHARE_DIRS`, refusing hidden path components and key/credential/`.env` names.
- Call `auditLog()` for tool actions; drain-type tools are rate-limited (`toolRateInterval`).
- Tokens must never be logged. stdout is the MCP channel, so all logging goes to stderr via `slog`.

## Plugin assets

- `.claude-plugin/plugin.json`: plugin manifest. `commands/webex.md` is the `/webex` slash command.
- `agents/*.md`: the playbooks above. `.webex-agents.yml` is the routing config users edit. Both are also `go:embed`ded into the binary by `embed.go` (package `pluginwebex`) as the built-in defaults, so editing them changes the shipped defaults.
- `skills/webex-monitor/SKILL.md`: auto-checks notifications while the listener is connected.
- `install.sh` / `install.ps1` download release binaries from GitHub Releases. `release.yml` builds them with `-trimpath -ldflags="-s -w"`.

## Conventions

- `CHANGELOG.md` follows Keep a Changelog + SemVer. Add entries under `[Unreleased]` or a new version heading, and record security results (govulncheck, CodeQL) in a `### Security` subsection as previous releases do.
- Target: official Claude Code plugin marketplace. That means single-binary distribution with no runtime deps, and CI lint + test + build must pass.
