# 1. Local index daemon and rules-driven agent dispatch

Date: 2026-09-29

## Status

Accepted (2026-10-02)

## Context

Today `webex-mcp` is a stdio MCP server that exists only while a Claude Code session is open. Inbound messages go to an in-memory ring buffer plus `pending.json`. Every read tool (`search_messages`, `get_space_history`, `get_digest`, `get_cross_space_context`) fans out to the live REST API on each call. `.webex-agents.yml` routes only tag a message with an agent name and priority. Nothing starts an agent, and the `auto_respond` / `action` keys in the docs are silently ignored by the parser.

The goals are:

1. Keep a local index of Webex history so Claude searches a local store instead of pulling records over MCP/REST.
2. When a new message arrives, start a Claude agent chosen by a rules file that names the channel and the system prompt.
3. Draft replies grounded in what the user already said and in facts already on record.

Candidate retrieval techniques (a density gate, rank fusion, distilled Q/A records) came from an earlier prototype. They were treated as hypotheses to test, not as settled design; each decision below rests on the spike data, not on the prototype.

A throwaway spike (2026-09-29, 100 spaces, 6 months) measured:

- **Backfill:** 54,760 messages in 6m18s with `max=1000` and Link-header paging. There were zero 429s and one space skipped on a 504.
- **Corpus:** 4,856 messages are the user's own, and 84% of those pass a density gate (at least 6 words, or naming something concrete). 864 question-to-user-answer pairs: 662 threaded, 158 DM, 44 inline in group spaces.
- **Answer reuse, lexical proxy:** on the 150 most recent pairs, the best word overlap between the user's real answer and the top-5 BM25/RRF results reached ≥ .25 for 3% of pairs, against 0% for random earlier messages. Retrieval beats random, but the signal is weak.
- **Answer reuse, LLM judge:** a blind judge (`claude -p`, Haiku, candidates deduped and shuffled across arms) asked whether any top-5 candidate would have let the user give the same answer. It judged all 150 pairs. Results:

  | Arm | hit@5 | 95% CI |
  |---|---|---|
  | User's prior messages | 13% | 8–19% |
  | Answers to similar past questions | 3% | 1–7% |
  | RRF of both | 11% | 7–17% |
  | Random control | 1% | 0–5% |

  Retrieval is real, but most new questions have no reusable earlier answer. Question-to-question matching adds nothing at the lexical level.

The corporate machines this targets use full-disk encryption (BitLocker, FileVault, LUKS), so a local plaintext index is acceptable.

## Decision

1. **Process model.** A single binary with two roles. `webex-mcp daemon` is long-running, one per user, installed as a login item or service. It owns the Mercury listener, backfill, and dispatch. `webex-mcp` (no subcommand) stays the per-session stdio MCP server.
2. **Storage.** SQLite through `modernc.org/sqlite`: pure Go, no CGO, so the cross-compiled single binary survives. It runs in WAL mode so the daemon writes while MCP sessions read, and uses FTS5 for search. The file lives in the user config dir with 0600 permissions, plus the existing Windows ACL. It replaces the ring buffer and `pending.json`: pending/processed becomes a column. Retention is configurable (default 6 months). There is no app-level encryption; the design relies on OS disk encryption.
3. **Backfill.** REST `/messages` with Link-header paging, `max=1000`, and honoring `Retry-After` on 429. It runs on first start, then fills per-space gaps since the last stored message.
4. **Reads.** MCP read tools query SQLite. REST is used only for writes and cache misses.
5. **Rules live in `agents/*.md` frontmatter**, replacing `.webex-agents.yml`: `name`, `match` (`space`, `keywords`, `direct`), `priority`, `model`, `tools` (an allowlist), `mcp_servers` (extra MCP servers the agent may use), and `auto_send` (default `false`). The markdown body is the system prompt.
6. **Dispatch.** When a stored message matches a rule, the daemon runs `claude -p --bare --model <model> --tools <allowlist> --strict-mcp-config --mcp-config <webex read-only + the agent's mcp_servers> --no-session-persistence`, with the agent body as system prompt. There is a global concurrency cap and per-space debounce. A dispatch-status column makes each message fire at most once. If `claude` isn't on PATH, dispatch is disabled and indexing continues.
7. **Retrieval.** One FTS5 BM25 arm over the user's prior messages, with the density gate applied, and recency as a tie-breaker. The similar-past-question arm and RRF fusion are left out because the spike showed no gain from them. The retriever still takes ranked lists behind an interface, so a later arm, such as embeddings, can be fused in with reciprocal rank fusion (K=60) without rework. All of this is implemented from the published algorithms; no prototype code is copied.
8. **The knowledge layer is external and opt-in: [notegraph](https://github.com/3rg0n/notegraph), over MCP.** `webex-mcp` contains no knowledge code. An agent that wants curated facts (decisions, claims, entities) lists notegraph in `mcp_servers`.
   - **Why not built in.** Notegraph's store (LadybugDB) needs CGo, which would break the pure-Go single binary. It is also a typed graph, the wrong shape for raw chat, and it allows a single writer, so it runs as its own service.
   - **Why not distilled records here.** The judged hit@5 upper bound for answer reuse (19%) is under the ~30% bar that would justify distilling raw history into Q/A records inside this repo.
   - **Reads.** Agents may run notegraph `recall` before drafting; its results are untrusted input like any other retrieved text.
   - **Writes.** Extractions an agent makes land as unreviewed with the Webex message IDs as `source_capture_ids`. A question and answer are captured as accepted only after the user actually sends or approves the reply, which is the human-review signal notegraph's provenance model requires.
   - **Open.** Whether curated facts beat raw retrieval is a separate hypothesis the spike did not test; measure it with the same blind judge before recommending notegraph by default. Notegraph stores embeddings but does not compute them, and Claude Code cannot produce them, so a paraphrase (embedding) arm still needs an embedder.
9. **Trust boundary.** Both inbound messages and indexed history are untrusted input.
   - All retrieved text goes through `sandboxText`.
   - Each agent gets only its allowlisted tools, and outbound send is off unless `auto_send: true`.
   - A dispatched agent's index queries are limited to the triggering space unless its rule widens them.
   - Every dispatch is audit-logged.

## Consequences

- Search and history no longer fan out over REST, and agents run without an open Claude Code session.
- A daemon has to be installed and supervised on three operating systems, which is new operational surface.
- A plaintext copy of the user's messages lives on disk. `--logout` and `--switch` must purge the index, or the next account inherits the previous user's history.
- The Claude Code CLI becomes a runtime dependency for dispatch only, which is in tension with the marketplace "no runtime dependencies" rule. Indexing and MCP still work without it.
- Agents that opt into notegraph depend on a separately installed, always-on notegraph service; agents without it run on the index alone.
- `.webex-agents.yml` is deprecated. The daemon reads it for one release and logs a migration warning.
- Roughly 1 in 8 new questions has a findable earlier answer. Drafts must say "no prior answer found" instead of producing unsupported text, and retrieval should mainly be presented as context, not as a ready answer.
