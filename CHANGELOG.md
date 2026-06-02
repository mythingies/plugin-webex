# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.9.0] - 2026-06-02

### Added
- **Durable triage list** (`internal/triage`) — a local, persistent "still to process" reminder for inbound messages. Survives restarts (0600 JSON at `~/.config/webex-mcp/pending.json`, atomic temp-file+rename writes, Windows ACL via `auth.RestrictFileAccess`). Reading never changes an item's status; items stay `pending` until explicitly cleared.
- `get_pending` MCP tool — lists messages still to process (newest-first). Reading it never clears anything.
- `mark_processed` MCP tool — the only way an item leaves the pending list. Local only: never sends anything to Webex or other users; never fires automatically, on read, or on reply.
- `buffer.PeekByPriority` — non-destructive priority-filtered read.

### Changed
- `get_notifications` and `get_priority_inbox` are now **non-destructive peeks** — they no longer drain the buffer, so reading inbound messages can't silently discard them. `get_notifications` gains an optional `max` parameter.
- Inbound messages are now recorded in the durable triage list (in addition to the in-memory ring buffer) when the WebSocket listener is active.

### Notes
- This release deliberately does **not** send Webex read receipts. The plugin never touches Webex's native unread/read state — "processed" is a private, local reminder only, so a sender never sees a false "read" signal and the user never loses an unread reminder by reading a message through the plugin. (Outward read-receipt support via the internal Conversation API was investigated and declined; see prior discussion / `webex-message-handler` #23.)

## [0.8.0] - 2026-06-02

### Security
- OAuth access/refresh tokens now stored in the OS keychain (Windows Credential Manager, macOS Keychain, Linux Secret Service via `github.com/zalando/go-keyring`) instead of plaintext JSON at `~/.config/webex-mcp/tokens.json`. Closes the on-disk plaintext-token attack surface flagged as F1 (Critical) in `THREAT_MODEL.md`.
- `WEBEX_CLIENT_SECRET` now stored in the OS keychain instead of being embedded in `.mcp.json`. Setup wizard writes the secret to the keychain and emits a `.mcp.json` containing only `WEBEX_CLIENT_ID` (non-secret) plus the binary path.
- Auto-migration on first run of v0.8.0: any pre-existing `tokens.json` is imported into the keychain and the legacy file is removed. Any `WEBEX_CLIENT_SECRET` set in the environment is copied into the keychain (no-op when an entry already exists). Migration is logged via `slog`.
- Linux fallback: when Secret Service is unavailable (`go-keyring` probe fails), the binary falls back to existing 0600-file storage. PAT (`WEBEX_TOKEN`) flow is unchanged — environment variables remain the recommended channel per Claude Code convention.
- Setting `WEBEX_CLIENT_SECRET` in the environment overrides the keychain at startup (CI/testing escape hatch).

### Added
- `auth.LoadClientSecret(clientID)` / `auth.SaveClientSecret(clientID, secret)` / `auth.DeleteClientSecret(clientID)` — keychain-backed accessors with file fallback.
- `TokenStore.MigrateTokensFromFile()` and `auth.MigrateClientSecretFromEnv()` — idempotent migration helpers.
- `TokenStore.UsingKeyring()` — reports which backend is active.
- `keyringAvailable()` probe that detects Secret Service availability on Linux without crashing.
- 8 new tests covering keychain save/load, deletion, missing-key errors, file-to-keychain migration, env-secret migration idempotency, and `sanitizeID` path-injection defense.

### Changed
- `TokenStore` now construction-time probes the OS keychain and uses the file fallback only when no backend exists. Public `Save`/`Load`/`Delete`/`Path` API is unchanged; existing tests pass without modification.
- Setup wizard's OAuth flow writes `WEBEX_CLIENT_SECRET` to the keychain before generating `.mcp.json`. The generated `.mcp.json` no longer contains the secret in any form.
- `cmd/webex-mcp/main.go` resolves the OAuth client secret from the keychain when `WEBEX_CLIENT_ID` is set in the environment but `WEBEX_CLIENT_SECRET` is not.

### Removed
- `WEBEX_CLIENT_SECRET` from setup wizard's `.mcp.json` output. Field is no longer required for normal usage.

## [0.7.1] - 2026-05-20

### Security
- Setup wizard now hardens `.mcp.json` ACL on Windows after writing. Previously, `os.WriteFile` with mode `0600` had no effect on NTFS, so the file inherited parent-directory ACEs allowing `BUILTIN\Users` and `Authenticated Users` to read the embedded `WEBEX_CLIENT_SECRET`. Setup now invokes `icacls /inheritance:r /grant:r <user>:F` after writing, restricting access to the current user only. Existing installs should re-run `webex-mcp setup` or manually run `icacls .mcp.json /inheritance:r /grant:r %USERNAME%:F` to harden their on-disk file.
- Bump `github.com/buger/jsonparser` v1.1.1 → v1.1.2 (CVE-2026-32285, GHSA-6g7g-w4f8-9c9x — DoS via `Delete` on malformed JSON; transitive via mcp-go → invopop/jsonschema → wk8/go-ordered-map)
- Bump `github.com/go-jose/go-jose/v4` v4.1.3 → v4.1.4 (CVE-2026-34986, GHSA-78h2-9frx-2jm8 — JWE decryption panic on empty `encrypted_key`; transitive via webex-message-handler)

### Changed
- Exported `auth.RestrictFileAccess` so the setup package can apply the same NTFS ACL hardening as token storage

## [0.7.0] - 2026-05-20

### Added
- Inline per-agent playbooks: `get_notifications`, `get_priority_inbox`, and `get_mentions` append `agents/<routed-agent>.md` to tool results so Claude has the right playbook in-context for every drained message (4 KB cap per playbook, path-traversal-safe)
- Prompt injection sandboxing: all external Webex message content wrapped in `<external-message>` tags so the LLM treats it as data, not instructions
- CodeQL security scanning workflow (Go, security-and-quality queries, weekly schedule)
- Dependency review workflow for PRs (blocks moderate+ vulnerabilities, GPL/AGPL licenses)
- Dependabot configuration for Go modules and GitHub Actions (weekly updates)
- SECURITY.md with vulnerability reporting policy and security measures summary
- GitHub secret scanning and push protection enabled
- Dependabot vulnerability alerts and automated security fixes enabled
- MAESTRO threat model (THREAT_MODEL.md) — full 7-layer analysis with ASI threat taxonomy
- PII redaction: email addresses masked in all tool outputs (`al***@example.com`)
- Structured audit logging via `slog` for all write/drain tool operations
- Per-tool rate limiting (2s cooldown) on buffer drain operations
- Buffer overflow alerting: `slog.Warn` at capacity and 95% utilization
- Per-agent buffer isolation: `DrainByAgent` and `PeekByAgent` methods
- Output validation: outbound messages stripped of `javascript:`, `data:`, `wmcp://` URLs
- Adaptive Card body sanitization: recursive text field sanitization against prompt injection
- Per-tool OAuth scope declarations with startup scope validation
- Windows NTFS ACL enforcement for token storage via `icacls` (platform-specific)
- CycloneDX SBOM generation in release workflow
- Webex API redirect path allowlisting (`/v1/` prefix only)

### Changed
- Updated all GitHub Actions to latest versions (checkout v6, setup-go v6, upload-artifact v7, download-artifact v8, golangci-lint-action v9)
- Upgraded golangci-lint from v1.64.8 to v2.11.4 (matching v2 config format)
- Added `-trimpath -ldflags="-s -w"` to release builds for smaller, reproducible binaries
- Removed duplicate artifact upload steps in release workflow
- Config validation hardened: agent name character allowlist, priority enum enforcement, keyword bounds
- OAuth callback uses `fstat` on open file descriptor to prevent TOCTOU race

### Removed
- `auto_respond` and `action` fields from agent routing config (unimplemented, misleading)
- `HTML` field from buffer notifications (sandbox gap vector)

### Fixed
- CI lint failures caused by golangci-lint v1 not understanding v2 config format
- Deprecated Node.js 16/20 warnings in GitHub Actions
- Digest truncation ordering: `sandboxText()` now applied before truncation to prevent tag breakage

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
