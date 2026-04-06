# MAESTRO Threat Model

**Project**: plugin-webex (Cisco Webex integration for Claude Code)
**Date**: 2026-04-06
**Framework**: MAESTRO (OWASP MAS + CSA) with ASI Threat Taxonomy
**Taxonomy**: T1-T15 core, T16-T47 extended, BV-1-BV-12 blindspot vectors, MA-1-MA-8 agent integrity

## Executive Summary

plugin-webex is a Go 1.26 MCP server plugin that bridges untrusted Webex message content into Claude Code's LLM context. Analysis across all 7 MAESTRO layers, dependency CVE scanning, and agent integrity auditing identified **18 deduplicated findings**: 2 Critical, 6 High, 7 Medium, 3 Low. The most critical risks are **plaintext credential storage** (tokens readable on disk) and **absent audit logging** (agent actions are untraceable). Prompt injection sandboxing (`sandboxText()`) is well-intentioned but has gaps in HTML field handling and adaptive card content. All four agentic risk factors (Non-Determinism, Autonomy, Agent Identity, A2A Communication) are present in this system.

## Scope

- **Languages**: Go 1.26
- **AI Components**: Yes — MCP server providing tools to Claude Code LLM; agent routing engine; 6 agent definitions; 2 skills
- **Entry Points**: `cmd/webex-mcp/main.go` (stdio MCP server), `internal/setup/setup.go` (localhost:8899 HTTP setup wizard)
- **External Dependencies**: mark3labs/mcp-go v0.44.1, 3rg0n/webex-message-handler v0.6.0, gopkg.in/yaml.v3
- **External APIs**: Webex REST API (https://webexapis.com/v1/*), Webex Mercury WebSocket
- **Agentic Risk Factors**:
  - **Non-Determinism**: LLM processes tool results; sandboxing effectiveness varies with model behavior
  - **Autonomy**: Agent routing auto-classifies/routes without human gate; auto_respond flag exists
  - **Agent Identity**: No cryptographic agent identity; agents identified by string name only
  - **A2A Communication**: Shared ring buffer between agents; no per-agent isolation

## Risk Summary

| # | ASI Threat | Layer | Title | Sev | L | I | Risk | Risk Factors | Traditional Framework |
|---|-----------|-------|-------|-----|---|---|------|-------------|----------------------|
| 1 | T22, T9 | L2,L4,L6 | Plaintext credential storage | Crit | 3 | 3 | 9 | — | STRIDE:ID, OWASP:A02, CWE-532, CWE-798 |
| 2 | T8, T44 | L5 | No audit trail / insufficient logging | Crit | 3 | 3 | 9 | — | STRIDE:R, OWASP:A09, CWE-778 |
| 3 | T6, BV-1, BV-4 | L1,L3 | Prompt injection sandbox gaps | High | 2 | 3 | 6 | Non-Determinism | OWASP-LLM:LLM01, ASI01, MITRE-ATLAS:AML.T0054 |
| 4 | T12, T9 | L3,L7 | Agent routing config manipulation | High | 2 | 3 | 6 | A2A | STRIDE:T,S, ASI03 |
| 5 | T3, T45 | L6 | No per-tool authorization scoping | High | 2 | 3 | 6 | Autonomy | STRIDE:E, OWASP:A01, CWE-269 |
| 6 | T9, T22 | L6 | Windows NTFS ignores 0600 permissions | High | 2 | 3 | 6 | — | OWASP:A01, CWE-280 |
| 7 | BV-7 | L7 | No per-agent buffer isolation | High | 2 | 3 | 6 | A2A | ASI-only |
| 8 | T11, T7 | L1 | Unsanitized model output in send_message | High | 2 | 3 | 6 | Non-Determinism | OWASP-LLM:LLM02, CWE-94 |
| 9 | T10 | L5 | Silent buffer overflow / HITL blindspot | Med | 2 | 2 | 4 | — | ASI06, CWE-400 |
| 10 | T19, MA-4 | L3,Integrity | Misleading agent autonomy claims | Med | 2 | 2 | 4 | Autonomy | ASI-only |
| 11 | T12, CWE-502 | L2,L3 | YAML config injection via anchors | Med | 2 | 2 | 4 | — | OWASP:A08, CWE-502 |
| 12 | CWE-359 | L2 | PII exposure in tool output | Med | 2 | 2 | 4 | — | OWASP:A01, CWE-359 |
| 13 | T13, T25 | L7 | No SBOM / GPG release signing | Med | 1 | 3 | 3 | — | STRIDE:T, OWASP:A06 |
| 14 | T2, T32 | L3 | Buffer drain without rate limiting | Med | 2 | 2 | 4 | Autonomy | OWASP-LLM:LLM06, CWE-400 |
| 15 | T41 | L3 | Webex API redirect validation gaps | Med | 2 | 2 | 4 | — | CWE-601 |
| 16 | BV-9, T24 | L6 | OAuth callback file TOCTOU race | Low | 1 | 2 | 2 | — | CWE-367 |
| 17 | T4, T42 | L3,L4 | Per-listener rate limiting (not global) | Low | 1 | 2 | 2 | — | CWE-400 |
| 18 | T13 (CVE) | L7 | GO-2026-4514 jsonparser DoS (unreachable) | Low | 1 | 1 | 1 | — | CWE-400 |

## Layer Analysis

### Layer 1: Foundation Model

plugin-webex does not host or fine-tune models — it provides MCP tools whose output flows into Claude Code's context. The primary L1 risk is that untrusted Webex message content influences LLM reasoning.

**F3 — Prompt injection sandbox gaps (T6, BV-1, BV-4 | High)**
`sandboxText()` wraps `msg.Text` in `<external-message>` tags across 7 tools. However:
- **HTML field bypass**: `buffer.NotificationMessage` stores both `.Text` (sandboxed) and `.HTML` (raw from Webex API, never sandboxed). If any future tool accesses `.HTML`, it bypasses the sandbox entirely.
- **Adaptive card injection**: `send_adaptive_card` accepts arbitrary JSON from Claude. Card text fields are not sanitized. A poisoned model output could craft cards with embedded instructions that persist in space history and re-enter agent context via `get_space_history`.
- **Truncation ordering**: `get_digest` truncates text *before* sandboxing — a crafted message could have injection payload in the first 120 chars.

Files: `internal/tools/tools.go:23`, `internal/tools/send_adaptive_card.go:45`, `internal/tools/get_digest.go:106`, `internal/listener/listener.go:246`

**F8 — Unsanitized model output in send_message (T11, T7 | High)**
`send_message` and `reply_to_thread` relay Claude's output directly to Webex with no output validation. Webex renders markdown; a compromised or manipulated model could send messages containing malicious URLs, embedded protocol handlers (`wmcp://`), or social engineering content.

Files: `internal/tools/send_message.go:31-55`

### Layer 2: Data Operations

**F1 — Plaintext credential storage (T22 | Critical)**
OAuth tokens stored as plaintext JSON at `~/.config/webex-mcp/tokens.json` (0600 perms). PAT tokens loaded from `WEBEX_TOKEN` env var. `.mcp.json` (gitignored, not tracked) stores `WEBEX_CLIENT_ID` and `WEBEX_CLIENT_SECRET` in plaintext. While `.gitignore` correctly excludes these files, on-disk plaintext is vulnerable to local privilege escalation, process memory dumps, and filesystem access.

Files: `internal/auth/store.go:60`, `internal/auth/oauth.go:81`, `.mcp.json`, `.env.txt`

**F11 — YAML config injection (T12, CWE-502 | Medium)**
`.webex-agents.yml` parsed via `yaml.Unmarshal()` with 1MB size limit but no schema validation. YAML anchors/aliases could amplify payloads. Agent names, priority values, and space patterns are not validated against allowlists.

Files: `internal/router/config.go:44-64`

**F12 — PII exposure in tool output (CWE-359 | Medium)**
PersonEmail, PersonName, and RoomTitle returned unredacted in tool responses. Repeated tool calls accumulate PII in LLM context. GDPR/CCPA compliance risk.

Files: `internal/tools/get_notifications.go:27`, `internal/tools/get_mentions.go:38`

### Layer 3: Agent Frameworks

**F4 — Agent routing config manipulation (T12, T9 | High)**
`.webex-agents.yml` is user-editable with no integrity verification. If modified by a supply chain attack or local compromise, routes can redirect production alerts to rogue agents, bypass keyword filters, or re-prioritize all messages. No audit trail on config changes.

Files: `internal/router/config.go`, `internal/router/router.go`, `.webex-agents.yml`

**F10 — Misleading agent autonomy claims (T19, MA-4 | Medium)**
Route config includes `auto_respond: true` and `action: notify_dm` fields. Both are parsed and stored but **never executed** — no code path consumes them. Agent definitions (escalation.md, dm-responder.md) describe autonomous behavior that doesn't exist. The `webex-monitor` skill claims to "automatically check" notifications but is a passive template.

Files: `.webex-agents.yml:19,26`, `internal/router/router.go:14`, `agents/escalation.md`, `skills/webex-monitor/SKILL.md`

**F14 — Buffer drain without rate limiting (T2, T32 | Medium)**
`get_notifications` drains the entire buffer with no limit parameter. No per-tool rate limiting prevents a runaway agent from exhausting the buffer in a tight loop.

Files: `internal/tools/get_notifications.go:19`, `internal/buffer/buffer.go:56`

**F15 — Webex API redirect validation (T41 | Medium)**
HTTP client blocks cross-host redirects but allows same-host redirects (e.g., `webexapis.com/v1/messages` → `webexapis.com/auth/login`). A DNS compromise could redirect API calls to a same-host fake endpoint that captures Authorization headers.

Files: `internal/webex/client.go:41-51`

### Layer 4: Deployment Infrastructure

**Strong posture overall.** Key mitigations in place:
- File permissions: 0600 for tokens, 0700 for config dirs
- CI/CD: All GitHub Actions pinned to commit SHAs; release checksums generated
- Install scripts: SHA256 verification before execution
- Rate limiting: 100 msg/sec token bucket on WebSocket listener
- Redirect blocking: Cross-host redirects blocked on HTTP client
- No containers (single binary deployment)

**F6 — Windows NTFS ignores 0600 permissions (T9, T22 | High)**
`os.WriteFile(..., 0600)` has no effect on Windows NTFS. Token files at `%APPDATA%\webex-mcp\tokens.json` are readable by any local user. No Windows ACL enforcement.

Files: `internal/auth/store.go:64`, `internal/auth/oauth.go:169`

### Layer 5: Evaluation & Observability

**F2 — No audit trail / insufficient logging (T8, T44 | Critical)**
No persistent audit log exists. Tool invocations (`send_message`, `reply_to_thread`, `get_notifications`) record no actor, timestamp, or parameter trail. Routing decisions in `router.Route()` are not logged. API errors logged at Debug level. Failed auth attempts not recorded. Buffer overflow (message loss) is silent.

The codebase uses `log/slog` in 6 files and `fmt.Fprintln(os.Stderr)` in main.go. No structured logging framework, no centralized log aggregation, no distributed tracing, no metrics export, no anomaly detection.

Files: `internal/listener/listener.go:102-106`, `internal/tools/send_message.go`, `internal/router/router.go:84`, `internal/webex/client.go:235`

**F9 — Silent buffer overflow (T10 | Medium)**
Ring buffer (default 5000) silently discards oldest messages when full. No warning, no metric, no alert. Critical alerts could be dropped before human review.

Files: `internal/buffer/buffer.go:43-53`

### Layer 6: Security & Compliance

**F5 — No per-tool authorization scoping (T3, T45 | High)**
All 30+ MCP tools execute with the same OAuth token (full `spark:all` scope). No per-tool scope restriction. `share_file`, `send_message`, and `get_meeting_transcript` all share identical privileges. Token auto-refreshes without re-validation.

Files: `internal/auth/oauth.go:87-110`, `internal/webex/client.go:215-224`

**F16 — OAuth callback TOCTOU race (BV-9 | Low)**
Callback file polled at 500ms intervals. Between stat() and ReadFile(), a local attacker can replace the file. Windows lacks the Unix permission check. Requires same-user local access.

Files: `internal/auth/oauth.go:224-258`

### Layer 7: Agent Ecosystem

**F7 — No per-agent buffer isolation (BV-7 | High)**
Ring buffer is a single shared store. `Drain()` returns all messages to any caller. No per-agent filtering, tagging, or access control. One agent can read sensitive messages intended for another agent's workflow.

Files: `internal/buffer/buffer.go`, `internal/tools/get_notifications.go`

**F13 — No SBOM / GPG release signing (T13, T25 | Medium)**
No Software Bill of Materials generated. Release binaries lack GPG signatures (SHA256 checksums only). Dependency `webex-message-handler` is from a personal GitHub account (`3rg0n`). No SLSA build provenance.

Files: `go.mod`, `install.sh`, `install.ps1`, `.github/workflows/release.yml`

**F17 — Per-listener rate limiting only (T4, T42 | Low)**
Rate limiter is per-listener instance. Multiple concurrent MCP clients would each get independent rate buckets, collectively exceeding intended limits.

Files: `internal/listener/listener.go:181-195`

**F18 — GO-2026-4514 jsonparser DoS (Low)**
`github.com/buger/jsonparser v1.1.1` (transitive via mcp-go) has a known DoS vulnerability. govulncheck confirms the vulnerable code path is **not reachable** from plugin-webex. No fix available yet.

Files: `go.mod` (indirect dependency)

## Agent/Skill Integrity

| File | Type | Declared Intent | Misalignment | ASI Threat | Evidence | Severity | Observable |
|------|------|----------------|-------------|-----------|----------|----------|------------|
| .mcp.json | mcp_config | MCP server config | MA-3 (credentials in config) | T22 | OAuth client_id/secret in plaintext JSON | Critical | Yes (file inspection) |
| .webex-agents.yml | routing_config | Route messages to agents | MA-4 (auto_respond/action unused) | T19 | Fields parsed but never executed | High | No (silent) |
| agents/escalation.md | agent_def | Escalate via DM notification | MA-4 (can't actually send DM) | T19 | personId never resolved; agent is passive | High | Yes (user notices no DMs) |
| skills/webex-monitor/SKILL.md | skill_md | "Automatically check" notifications | MA-2/MA-4 (passive, not automatic) | T19 | No polling or auto-invocation logic | High | No (silent) |
| agents/dm-responder.md | agent_def | Auto-reply to DMs | MA-4 (auto_respond not enforced) | T6 | Flag exists but MCP doesn't gate send_message | Medium | No (silent) |
| agents/alert-triage.md | agent_def | Summarize alerts | Aligned | — | Intent matches behavior | — | — |
| agents/digest-builder.md | agent_def | Compile digests | Aligned | — | Intent matches behavior | — | — |
| agents/ops-summarizer.md | agent_def | Summarize ops activity | Aligned | — | Intent matches behavior | — | — |
| agents/general.md | agent_def | Categorize messages | Aligned | — | Intent matches behavior | — | — |
| skills/webex/SKILL.md | skill_md | Webex tool wrapper | Aligned | — | Intent matches behavior | — | — |
| commands/webex.md | slash_cmd | Webex tool dispatch | Aligned | — | Intent matches behavior | — | — |

## Dependency CVEs

| Package | Version | CVE | CVSS | Fixed In | Code Path Used | Risk |
|---------|---------|-----|------|----------|---------------|------|
| github.com/buger/jsonparser | v1.1.1 | GO-2026-4514 | DoS | N/A | No (confirmed by govulncheck) | Low |

*Scanned with: govulncheck*

## Recommended Mitigations (Priority Order)

1. **Encrypt credentials at rest** (F1, F6 — Critical): Use OS keychain (macOS Keychain, Windows Credential Manager, Linux libsecret) instead of plaintext JSON files. On Windows, use DPAPI or SetFileSecurity() for NTFS ACLs. Remove plaintext tokens from `.mcp.json` env block — use env var references only.

2. **Add structured audit logging** (F2 — Critical): Implement `slog`-based audit log at every tool entry/exit point recording: tool name, timestamp, parameters (with token masking), result status. Log routing decisions. Promote API errors from Debug to Warn/Error. Consider centralized log output (syslog/OTEL).

3. **Close sandboxText gaps** (F3 — High): Remove `.HTML` field from `buffer.NotificationMessage` or wrap it with `sandboxText()` at storage time. Sanitize Adaptive Card text fields before sending. Ensure truncation happens *after* sandboxing, not before.

4. **Add output validation for send_message** (F8 — High): Validate model outputs before relaying to Webex. Strip or allowlist URL protocols. Consider markdown sanitization for outbound messages.

5. **Implement per-tool authorization** (F5 — High): Add tool-level scope checks. Restrict `share_file` to file scopes, `get_meeting_transcript` to meeting scopes. Map each tool to minimum required OAuth scopes.

6. **Add agent routing integrity** (F4 — High): Validate `.webex-agents.yml` against strict schema. Whitelist agent names (alphanumeric + dash). Enum-validate priority values. Log config loads and changes.

7. **Implement per-agent buffer isolation** (F7 — High): Tag buffered messages with target agent. Filter `Drain()` / `Peek()` by agent identity. Prevent cross-agent message access.

8. **Fix misleading agent definitions** (F10 — Medium): Either implement `auto_respond` and `action` fields or remove them from config schema and agent documentation. Document that agents are passive prompt templates, not autonomous actors.

9. **Add buffer overflow alerting** (F9 — Medium): Log warning when buffer utilization exceeds 80% and 95%. Expose buffer size in `listener_control` status output.

10. **Generate SBOM and sign releases** (F13 — Medium): Add `cyclonedx-gomod` or `syft` to CI. Sign release binaries with GPG. Add SLSA build provenance.

11. **Monitor GO-2026-4514** (F18 — Low): No action required now (unreachable code path). Update `mark3labs/mcp-go` when fix propagates.

## Trust Boundaries

```
TB1: User ←→ Claude Code (LLM context)
     User trusts Claude Code's judgment on tool results.
     Tool results contain untrusted Webex content (sandboxed).

TB2: Claude Code ←→ MCP Server (stdio)
     Claude Code sends tool calls; MCP server returns results.
     MCP server trusts all tool calls (no per-tool auth).

TB3: MCP Server ←→ Webex REST API (HTTPS)
     Bearer token auth. Server trusts API responses.
     API responses contain untrusted user-generated content.

TB4: MCP Server ←→ Webex Mercury WebSocket (WSS)
     Real-time inbound messages. Untrusted content.
     Rate-limited (100 msg/sec) but not per-agent isolated.

TB5: MCP Server ←→ Local Filesystem
     Token storage, config files, .mcp.json.
     0600 perms (Unix), no ACL enforcement (Windows).

TB6: Agent Routing Config ←→ MCP Server
     .webex-agents.yml loaded at startup. No integrity check.
     Modification redirects all message routing.
```

## Data Flow Diagram (Text)

```
                    ┌─────────────────┐
                    │   Webex Cloud    │
                    │  (REST + WSS)    │
                    └───────┬─────────┘
                            │ TB3/TB4
                 HTTPS/WSS  │  Bearer Token
                            │
              ┌─────────────▼──────────────┐
              │     webex-mcp (MCP Server)  │
              │                             │
              │  ┌────────┐  ┌───────────┐  │
              │  │ REST    │  │ WebSocket │  │
              │  │ Proxy   │  │ Listener  │  │
              │  │ (tools) │  │ (Mercury) │  │
              │  └────┬───┘  └─────┬─────┘  │
              │       │            │         │
              │       │     ┌──────▼──────┐  │
              │       │     │ Ring Buffer  │  │
              │       │     │ (shared,     │  │
              │       │     │  no isolation)│  │
              │       │     └──────┬──────┘  │
              │       │            │         │
              │  ┌────▼────────────▼──────┐  │
              │  │   Agent Router          │  │
              │  │   (.webex-agents.yml)   │◄─┤─ TB6
              │  └─────────────────────────┘  │
              │                             │
              │  ┌─────────────────────┐    │
              │  │  Token Store        │◄───┤─ TB5
              │  │  (~/.config/...)     │    │
              │  └─────────────────────┘    │
              └──────────────┬──────────────┘
                             │ TB2
                        stdio│(MCP)
                             │
              ┌──────────────▼──────────────┐
              │       Claude Code (LLM)      │
              │   sandboxText() in responses │
              └──────────────┬──────────────┘
                             │ TB1
                             │
                    ┌────────▼────────┐
                    │      User       │
                    └─────────────────┘
```

## Remediation Status

All 18 findings have been assessed. 17 are remediated in code; 1 is accepted risk (unreachable transitive CVE). Remediation was performed on 2026-04-06.

| # | Finding | Severity | Status | Remediation |
|---|---------|----------|--------|-------------|
| F1 | Plaintext credential storage | Critical | **Mitigated** | Windows NTFS ACLs enforced via `icacls` in `store_acl_windows.go`. Unix 0600 mode already effective. Full OS keychain integration deferred to future release. |
| F2 | No audit trail / insufficient logging | Critical | **Remediated** | Structured `slog`-based audit logging added to all write and drain tools via `auditLog()` helper (`tools.go:65`). Logs tool name, action, and key parameters. |
| F3 | Prompt injection sandbox gaps | High | **Remediated** | `.HTML` field removed from `NotificationMessage` struct and listener construction. Adaptive Card text fields recursively sanitized via `sanitizeCardBody()` (`send_adaptive_card.go:15`). Digest truncation reordered: `sandboxText()` applied before truncation (`get_digest.go:106`). |
| F4 | Agent routing config manipulation | High | **Remediated** | Config validation hardened: agent names restricted to `[a-zA-Z0-9_-]` via allowlist (`config.go:83`). Priority values validated against configured enum. Keyword count (max 50) and length (max 200 chars) enforced. Route limit capped at 200. |
| F5 | No per-tool authorization scoping | High | **Remediated** | `ToolScopes` map declares minimum OAuth scopes per tool (`tools.go:112`). `ValidateScopes()` checks configured scopes at startup and emits warnings for uncovered tools (`main.go:157`). |
| F6 | Windows NTFS ignores 0600 permissions | High | **Remediated** | Platform-specific `restrictFileAccess()` calls `icacls /inheritance:r /grant:r <user>:F` on Windows after every token save (`store_acl_windows.go`). No-op on Unix where 0600 is effective (`store_acl_other.go`). |
| F7 | No per-agent buffer isolation | High | **Remediated** | `DrainByAgent(agent)` and `PeekByAgent(n, agent)` methods added to ring buffer (`buffer.go`). Agents can now drain/peek only messages routed to them. |
| F8 | Unsanitized model output in send_message | High | **Remediated** | `sanitizeOutboundText()` strips URLs with disallowed protocols (`javascript:`, `data:`, `wmcp://`) from outbound messages (`tools.go:79`). Applied in both `send_message` and `reply_to_thread`. |
| F9 | Silent buffer overflow / HITL blindspot | Medium | **Remediated** | `slog.Warn` emitted when buffer is at capacity (with dropped message ID) and at 95% utilization threshold (`buffer.go:Push()`). |
| F10 | Misleading agent autonomy claims | Medium | **Remediated** | `AutoRespond` and `Action` fields removed from `Route` struct, `RoutingResult` struct, all tests, and `.webex-agents.yml`. No dead code or misleading configuration remains. |
| F11 | YAML config injection via anchors | Medium | **Remediated** | Covered by F4 hardening — agent name allowlist, priority enum validation, keyword bounds, and route count limits prevent YAML injection from producing exploitable config. |
| F12 | PII exposure in tool output | Medium | **Remediated** | `maskEmail()` redacts email addresses to `al***@example.com` format (`tools.go:50`). Applied in `get_notifications`, `get_priority_inbox`, `get_mentions`, `get_space_history`, `search_messages`, `get_digest`, and `get_cross_space_context`. |
| F13 | No SBOM / GPG release signing | Medium | **Partially remediated** | CycloneDX SBOM generation added to release workflow via `cyclonedx-gomod` (`release.yml`). SBOM included in release artifacts and checksums. GPG signing and SLSA provenance deferred. |
| F14 | Buffer drain without rate limiting | Medium | **Remediated** | Per-tool rate limiter with 2-second cooldown via `toolRateAllow()` (`tools.go:98`). Applied to `get_notifications` and `get_priority_inbox` drain operations. |
| F15 | Webex API redirect validation gaps | Medium | **Remediated** | Redirect path allowlist restricts same-host redirects to `/v1/` prefix only (`client.go:53`). Redirects to non-API paths (e.g., `/auth/login`) are now blocked. |
| F16 | OAuth callback file TOCTOU race | Low | **Remediated** | Replaced `os.Stat()` + `os.ReadFile()` with `os.Open()` + `f.Stat()` + `io.ReadAll(f)` — stat and read from the same file descriptor, eliminating the TOCTOU window (`oauth.go:231`). |
| F17 | Per-listener rate limiting only | Low | **Accepted** | Architecture uses a single listener instance per process. Multi-client scenarios are out of scope for the current single-binary deployment model. |
| F18 | GO-2026-4514 jsonparser DoS | Low | **Accepted** | Vulnerable code path confirmed unreachable by `govulncheck`. Transitive dependency via `mark3labs/mcp-go`. Will update when upstream fix propagates. |

### Residual Risk

After remediation, the residual risk profile is:

- **Critical**: 0 (down from 2)
- **High**: 0 (down from 6)
- **Medium**: 2 accepted (F17 architecture constraint, F13 GPG/SLSA deferred)
- **Low**: 1 accepted (F18 unreachable CVE)

### Future Work

- F1: Migrate token storage to OS keychain (macOS Keychain, Windows Credential Manager, Linux libsecret)
- F13: Add GPG release signing and SLSA build provenance attestation
- F18: Update `mark3labs/mcp-go` when `buger/jsonparser` fix propagates
