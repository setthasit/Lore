# 07 — Roadmap & Risks

## Milestones

Each milestone ships something demoable; the novel part (provenance) is
front-loaded right after the minimum viable core.

### M1 — Core loop (usable end-to-end)

Config loading + validation, FX wiring, SQLite IndexStore (documents, chunks,
FTS5, sqlite-vec, cursors), GitHubConnector (commits, PRs, reviews, issues),
chunking + embedding (OpenAI provider), hybrid retrieval with RRF,
`find_decision`, MCP stdio server, `lore init` / `lore sync` /
`lore status` CLI.

*Demo: point at a public repo, ask a decision question from Claude Code.*

### M2 — Provenance (the differentiator)

LinkResolver + `edges`/`pending_refs`, GitConnector (blame/log), graph walk,
`why` + `trace` + `history_of` (MCP + CLI pretty-print), `Gaps` honesty
reporting.

*Demo: `why` on a weird line in a known OSS repo → cited chain commit → PR →
issue.*

### M3 — Notion connector

Second source proves the connector abstraction; cross-source edges
(PR → Notion design doc); `root_pages` scoping; deferred ref resolution
exercised for real.

### M4 — Scheduler + serve mode

Lease lock (heartbeat + TTL takeover), scheduler with skip-on-held-lock,
`sync_now`/`sync_status` MCP tools, MCP Streamable HTTP via `lore serve`.

### M5 — gRPC + mTLS

`lore.v1` proto, QueryService + SyncService (including `Watch` streaming),
mTLS listener + `make certs.dev`, loopback/TLS startup enforcement,
SynthesisService + LLMConnector providers (`--explain`, `lore ask`).

### M6 — Polish & showcase

README with a recorded demo against a well-known OSS repo, quickstart for
Claude Code/Cursor, Ollama fully-local guide, GitLab connector if time allows
(strongest proof the abstraction holds).

## Named risks

| Risk | Impact | Mitigation |
|---|---|---|
| GitHub rate limits on large-repo backfill | first sync slow/fails | GraphQL batching, per-batch cursor checkpoints (resumable), backoff on secondary limits |
| Notion API slow pagination | long backfill | subtree scoping via `root_pages`; backfill is async by design |
| Private data → cloud embedder/LLM | privacy concern | pluggable providers; Ollama = fully local; documented loudly |
| sqlite-vec cgo complicates cross-compile | release friction | IndexStore interface hides the store; pure-Go fallback (chromem-go + bleve) if real pain appears |
| Poorly-linked repos (no PR discipline, bare commits) | thin evidence, weak demo | `Gaps` reporting keeps answers honest; semantic expansion still finds unlinked discussions; docs set expectations |
| Embedding model change invalidates vectors | silent quality loss | embedder identity in `meta`; startup mismatch check; explicit `--reembed` |
| `lore` name collision | rename cost | check registries/GitHub before first public release |

## Open questions (to settle during implementation planning)

1. Embedding batch size / concurrency defaults per provider.
2. Graph-walk scoring constants (depth penalty, confidence floor) — start with
   the values in [05](05-query-engine.md), tune against a real repo.
3. `lore ask` conversational follow-ups (thread context) — v1 is single-shot;
   revisit after web UI.
4. Whether M6 includes the GitLab connector or it moves to post-v1.

## After design review

Next step: structured implementation plan (`implementation-plan-creator`
format, phase documents with checkbox tasks) derived from these milestones,
executable phase-by-phase.
