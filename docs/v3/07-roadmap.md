# 07 — Roadmap & Risks

## Milestones

Ordering principle: **assistant first, code explorer second**. The ask-only
path (the primary persona) is demoable by M2 and real by M3; code anchoring
lands at M4 without touching the engine. Each milestone ships something
demoable.

### M1 — Retrieval core (usable end-to-end)

Config loading + validation (incl. zero-repo workspaces), FX wiring, SQLite
IndexStore via ncruces/go-sqlite3 (documents, chunks, FTS5, sqlite-vec,
cursors) **+ WASM-vs-cgo benchmark to lock the driver choice**,
GitHubConnector (commits, PRs, reviews, issues — batch-checkpoint contract),
chunking + embedding (OpenAI provider), hybrid retrieval with RRF,
retrieval-only `find_decision` (no walk yet; bundle with seeds + gaps), the
connector conformance suite, MCP stdio server, `lore init` / `lore sync` /
`lore status` / `lore ask` CLI (pretty-print).

*Demo: point at a public repo's PRs/issues — no clone — and ask a decision
question from Claude Code.*

### M2 — Provenance engine (the differentiator)

LinkResolver + `edges`/`pending_refs`, direction-aware graph walk, full `find_decision`
(chains + gaps), **event resolution** (`around` → time window, proximity
ranking), `trace`, `impact_of` (forward-in-time timeline), `ResolveRef`.

*Demo: "why did we choose B over A when incident X happened?" and "what
impact did it have?" → cited chains and a chronological timeline, still zero
repos.*

### M3 — Notion + Jira connectors (ask-only for real)

NotionConnector (`root_pages` scoping), JiraConnector (`/search/jql`,
`updated` watermark, ADF flattening), cross-source edges (ticket ↔ page ↔ PR),
ticket-key resolution exercised for real, deferred ref resolution
(`pending_refs`) under multi-source ingestion.

*Demo: a Jira + Notion **only** workspace answers both target questions with
cross-source citation chains.*

### M4 — Code anchoring

`repos:` config + GitConnector (blame/log via `git` shell-out), `why` +
`history_of` (MCP + CLI pretty-print), enrichment mapping (`remote:` →
source repos), precondition errors on ask-only workspaces.

*Demo: `why` on a weird line in a known OSS repo → cited chain commit → PR →
issue → design doc.*

### M5 — Scheduler + serve mode

Lease lock (heartbeat + TTL takeover), scheduler with skip-on-held-lock,
`sync_now` / `sync_status` MCP tools, MCP Streamable HTTP via `lore serve`.

### M6 — gRPC + mTLS + synthesis

`lore.v1` proto (FindDecision / Why / Trace / ImpactOf / HistoryOf + SyncService
including `Watch` streaming), mTLS listener + `make certs.dev`, loopback/TLS
startup enforcement, SynthesisService + LLMConnector providers (`--explain`,
synthesized `lore ask`).

### M7 — Polish & showcase

README with two demo walkthroughs — ask-only (Jira/Notion) and code-anchored
(OSS repo) — quickstart for Claude Code/Cursor, Ollama fully-local guide,
source setup guide, version stamping with a cross-compile matrix, and the
GitLab connector as the proof that the connector abstraction holds.

## Named risks

| Risk | Impact | Mitigation |
|---|---|---|
| GitHub rate limits on large-repo backfill | first sync slow/fails | GraphQL batching, batch-level cursor checkpoints (resumable), backoff on secondary limits |
| Jira/Notion API slow pagination | long backfill | project/subtree scoping; backfill is async and batch-resumable by design |
| Jira API churn (legacy search endpoints deprecated) | connector breakage | build on the new `/search/jql` + `nextPageToken` from day one; connector isolated in one package |
| Event resolution picks the wrong "incident X" | wrong time window → misleading evidence | agreement check across top hits; ambiguity → explicit Gap with candidates instead of a silent guess; resolved anchor always returned in `Anchor.Window` for auditability |
| Timestamp skew (docs edited long after the event) | wrong timeline ordering | `CreatedAt` (event time) drives windows/timelines; `UpdatedAt` only drives sync/freshness |
| Sparse linking (no PR discipline, tickets never referenced) | thin chains, weak demo | `Gaps` reporting keeps answers honest; retrieval seeding still finds unlinked discussions; docs set expectations |
| Private data → cloud embedder/LLM | privacy concern | pluggable providers; Ollama = fully local; documented loudly |
| WASM SQLite slower than cgo | query latency | M1 benchmark on realistic corpus; cgo pairing is a drop-in behind IndexStore if needed |
| Embedding model change invalidates vectors | silent quality loss | embedder identity in `meta`; startup mismatch check; explicit `--reembed` |
| `lore` name collision | rename cost | checked before release: kept, module path `github.com/setthasit/Lore` — see [01 — Name](01-overview.md#name) |

## Open questions

1. Embedding batch size / concurrency defaults per provider.
2. Graph-walk scoring constants (depth penalty, confidence floor 0.3, RRF
   k = 60) and `event_window` default (30d) — start with the values in
   [05](05-query-engine.md), tune against a real workspace.
3. Whether `impact_of` should also accept an explicit `until` (bounded impact
   windows for "what happened in the following quarter").
4. Jira Data Center auth mode (PAT) — post-v1, same package.
5. `lore ask` conversational follow-ups (thread context) — v1 is single-shot;
   revisit after web UI.
6. Whether M7 includes the GitLab connector or it moves to post-v1 — **settled:
   included**, merge requests mapping onto the existing `pr` document types.

## Status

M1–M7 are implemented and covered by tests; the milestone list above is kept as
the record of the build order, not as outstanding work. The named risks stay in
force — they describe operating conditions, not open tasks.
