# 2. Reply Desk: draft replies to unanswered direct messages, with human approval

Date: 2026-11-23

## Status

Accepted (2026-11-23)

## Context

The user wants Claude to go through their Webex direct messages, find the ones they have not answered, work out what is being asked, check what they have said before, and draft a reply. The user then reviews the draft in a small local web page, edits it, and approves it before anything is sent.

ADR 0001 already covers the long-term pieces: a local index, retrieval over the user's own messages, `claude -p` dispatch, and `auto_send: false`. None of it is built yet. This ADR covers the first slice: direct (1:1) messages only, built on the current architecture, and designed to become the front end of ADR 0001 later rather than be thrown away.

Three designs (incremental in-session, ADR-native daemon, trust-first separate process) were written independently and scored by three judges with different lenses (user value, security, engineering fit). The decisions below take the trust-first design as the base and fold in the best parts of the other two.

Facts the decisions rest on, all checked:

- **No read status in the public REST API.** `GET /rooms` and `GET /memberships` have no `lastSeen` field. Read status exists only in the SDKs, behind an undocumented internal service. Seen is not answered anyway, and reading on a phone would hide real to-dos.
- **The public REST GETs post no activity.** No documentation says `GET /messages`, `GET /rooms` or `GET /messages/{id}` mark anything read. This is inferred, not documented, so it gets one manual check on a test DM before release.
- **Webex has no reactions API.** A 👍 instead of a reply cannot be detected.
- **`claude -p --bare` does not read the Claude subscription login.** `claude --help` (2.1.331): "Anthropic auth is strictly ANTHROPIC_API_KEY or apiKeyHelper via --settings (OAuth and keychain are never read). Third-party providers (Bedrock/Vertex/Foundry) use their own credentials."
- **`claude -p --output-format json --json-schema …` returns validated output** in `structured_output`, with `is_error` and `subtype` in the same envelope. Checked with a live call.
- **`sandboxText` did not escape its own closing tag**, so a message containing `</external-message>` could break out of the prompt-injection wrapper. This is fixed as part of this work, and the fix applies to the existing tools too.
- **Token refresh was process-local.** `OAuthProvider.Token()` refreshed from memory with no reload and no lock, so two processes could refresh at the same moment.

## Decision

1. **Scope: direct messages only.** Group spaces, @mentions and `@all` are left for later. DMs need no mention parsing, carry no `@all` noise, and nobody else in them can answer on the user's behalf.

2. **Process: `webex-mcp --review`.** This is a separate subcommand of the same binary. It is not part of the stdio MCP server, so the UI does not die with a Claude Code session, and nothing in it can write to the MCP stdout channel. It reuses `resolveAuth()`. When the ADR 0001 daemon exists, it hosts the same handlers.

3. **Unanswered means unanswered, not unread.** For each DM room active within the lookback window (default 7 days, `--lookback`), the pending **burst** is every message from the other person since the user's last message there, newest first, up to the first message from the user. If the newest message is the user's, the room is answered.
   - Webex read state is never consulted.
   - A DM with a bot is skipped.
   - Attachment-only messages, cards and voice clips are flagged "read it yourself" rather than drafted from blindly.
   - Detection is pure Go code over the fetched messages (`replydesk.Detect`) and is unit-tested without any HTTP calls.

4. **Authority split.**
   - **Read:** Go code that only issues GETs.
   - **Draft:** a `claude -p` subprocess with **no tools** (`--tools ""`), **no MCP servers** (`--strict-mcp-config` with an empty config), `--no-session-persistence`, an empty temp working directory, the prompt on stdin, and an environment allowlist that drops every `WEBEX_*` variable. It can only return JSON.
   - **Send:** one Go handler, and only after the user confirms the exact text.
   - The model never sees Webex IDs, only local refs (`b1`, `c2`, `p3`) that the server maps back, so it cannot invent or steer an ID.

5. **Drafter modes.** `auto` (the default) resolves as follows:
   - `bare` when an API key or a Bedrock/Vertex/Foundry provider is configured in the environment;
   - otherwise `isolated`: the same flags without `--bare`, which can use the subscription login, but the user's hooks still run;
   - `off` when `claude` is not on `PATH`.

   The active mode is shown on the status bar. When it is `off`, the inbox and manual replies still work.

6. **Grounding, scoped to the asker.** Retrieval searches only the user's own earlier messages in **the same DM** (BM25 with the ADR 0001 density gate), so a draft cannot leak something said to someone else.
   - Citations are shown next to the draft.
   - When nothing relevant is found, the draft is a short holding reply or clarifying question, and facts the user must supply appear as `[[FILL: …]]`. The server refuses to send while any `[[FILL` remains. This puts ADR 0001's "no prior answer found" rule in code, not just in the prompt.
   - Widening retrieval to other conversations is left for the ADR 0001 index, behind an explicit audience check.

7. **Review UI.**
   - **Serving:** it binds `127.0.0.1:0`. A single-use launch token, valid for 2 minutes, is exchanged for an HttpOnly `SameSite=Strict` session cookie. Every request needs an exact `Host` match. Non-GET requests also need an exact `Origin`, `Sec-Fetch-Site: same-origin` when the browser sends it, a per-session CSRF header, and a JSON body of at most 64 KB.
   - **Headers:** a strict CSP, `nosniff`, no referrer, and no caching.
   - **Lifetime:** it shuts down after 30 minutes idle or 8 hours in total.
   - **Actions:** Send, Regenerate with an instruction, Dismiss with a reason (no reply needed / replied elsewhere / reacted / other), Snooze and Copy, with keyboard shortcuts.
   - **Drafting:** the top 5 items are drafted in the background after each scan, and the rest when opened.

8. **Send path.** The server computes the target from the scan: the DM room, and the thread when the burst is in one. The browser does not choose it. The request carries the SHA-256 of the text the user confirmed. Before posting, the server:
   - re-fetches the newest burst message: deleted → refuse; edited → stale;
   - re-lists the room: the user already replied, or new messages arrived → refuse and rescan;
   - checks length the same way the MCP tools do (raw bytes, at most 7439);
   - runs `Sanitize`, and if that changed the text, requires the user to confirm the changed version;
   - refuses `[[FILL`.

   It records `sending` before the POST and `sent` after. A crash leaves `send_unknown`, which is never retried automatically, because a Webex POST has no idempotency key. Sends are rate-limited to one every 2 seconds and 30 per hour.

9. **State.** `replydesk.json` lives in the user config directory: 0600, the Windows ACL, atomic writes, bound to the owner's person ID (purged on mismatch), and pruned after 30 days. It holds drafts, citations capped at 500 characters, and per-room state keyed to the newest burst message, so a new message brings back a dismissed room. `--logout` and `--switch` delete it.

10. **Shared text safety.** `internal/safetext` holds `Sandbox` (now escaping the wrapper tags), `Sanitize` (now also blocking `<@all>`), `MaxMessageLen` and `Prepare`. The MCP tools and Reply Desk use the same code.

11. **Cross-process token refresh.** The refresh runs under a lock file in the config directory. Inside the lock it reloads the stored tokens and refreshes only if they are still stale.

## Consequences

- The user gets the requested workflow without the ADR 0001 daemon. Detection can be checked on real data first with `webex-mcp --review --dry-run`, which uses no LLM.
- Drafting depends on the `claude` CLI at runtime. This is the same optional dependency ADR 0001 accepts for dispatch; everything else works without it.
- In `isolated` mode the user's Claude Code hooks run inside the drafter. That is weaker than `bare`, but the drafter still has no tools, and its only output is JSON a human reviews.
- Reactions and replies sent from other devices are invisible until the next scan. The Dismiss reasons and the pre-send re-check cover both cases.
- Only DM history with the asker is searched, so roughly 1 in 8 asks will have a usable earlier answer (the ADR 0001 spike). Most drafts will be holding replies with `[[FILL]]` gaps. The value is mostly the triaged inbox and the summary of each ask.
- A second loopback HTTP surface exists while `--review` runs. `THREAT_MODEL.md` records it.
- Group spaces, the persistent index, pre-drafting on message arrival, and cross-conversation retrieval move to ADR 0001's daemon work.
