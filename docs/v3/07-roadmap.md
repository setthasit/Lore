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

### M8 — Plugin contract & official plugins

`sdk/` public contract (data types, `Connector` / `Embedder` / `Completer` /
`CodeRepo`, manifest, host) with `httpx` / `refs` / `conform` moved beside it;
every connector and provider relocated to `plugins/` and importing nothing
internal; depguard boundaries; registry with manifest validation; `app`
package and a composition-root `cmd/lore`; instance-based `sources:` config
with manifest-driven validation, `lore init` scaffolds and `lore source add`
prompts generated from manifests; `KindCode` for local clones.

*Demo: two Jira instances in one workspace, and a source added to `lore.yaml`
whose validation errors were never written by hand.*

### M9 — Provider drivers & presets

`providers:` instances and `embedder:` / `llm:` role bindings; capability
checks at load; native drivers for OpenAI, Anthropic and Ollama; the
OpenAI-compatible driver with its preset catalog; model-to-dimensions
knowledge moved out of the engine and into the drivers.

*Demo: reach OpenRouter and Kimi with a configuration change and no new code.*

### M10 — External plugins

`plugexec` host implementing the NDJSON protocol ([09](09-plugin-protocol.md)),
local-path plugin declarations, `lore plugin list` / `verify`, and
`sdk/conform` passing against an out-of-process plugin written in another
language. The protocol freezes when this milestone lands.

*Demo: a Python connector syncs into a workspace with no rebuild of `lore`.*

### M11 — Distribution & trust

Coordinates (`github.com/owner/repo@vX.Y.Z`, direct URLs), release-asset
resolution, `lore.lock` with per-platform digests, `lore plugin install` /
`update` / `remove`, optional signature verification, `lore build --with` for
custom binaries, and the plugin index behind `lore plugin search`.

*Demo: install a third-party plugin by coordinate on a machine with no Go
toolchain, and watch a tampered digest refuse to launch.*

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
| Third-party plugin holds a source token | a compromised or malicious plugin exfiltrates data | per-plugin secret injection (a plugin sees only what its manifest declared), mandatory digest pinning, explicit installation, optional signatures, `lore plugin verify`; WASM sandbox named as the enforcement tier for untrusted authors |
| Plugin contract churn after third parties exist | ecosystem breakage | `APIVersion` checked at registration and in the protocol handshake; the wire protocol freezes only when M10 ships, and the contract evolves additively afterwards |
| External plugin crashes mid-stream | partial sync | the last persisted cursor is authoritative, so a crash loses no committed work and the next round resumes from it |

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

M1–M11 are implemented and covered by tests; that part of the list is the record
of the build order, not outstanding work. The wire protocol
([09](09-plugin-protocol.md)) froze with M10 and evolves additively from here.
The named risks stay in force — they describe operating conditions, not open
tasks.
