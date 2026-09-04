# Lore — Design Documents (v3)

Lore is a **decision-provenance engine for engineering teams**: an MCP server,
CLI, and gRPC service that answers *why* — why a decision was made, what
alternatives were rejected, what happened afterwards — by indexing the decision
trail (tickets, design docs, incidents, pull requests, review threads, commits)
across sources and returning citation-grounded evidence.

Code is **one anchor among several**, not the center: a workspace with only
Jira + Notion — no repository at all — is a first-class configuration.
Questions like *"why did we choose option B over A when incident X happened?"*
and *"what impact did decision A have?"* are primary use cases; `git blame`
anchoring remains fully supported as an optional enrichment.

## Status

**Implemented.** This set is the source of truth for the shipped system and is
kept in step with the code; it supersedes [v1](../v1/), and v2 was abandoned
research that nothing builds on.

## Reading order

| Doc | Contents |
|-----|----------|
| [00 — Design deltas](00-design-deltas.md) | Every change vs v1, with rationale — read this first if you know v1 |
| [01 — Overview](01-overview.md) | Problem, concept, differentiators, goals / non-goals, landscape |
| [02 — Architecture](02-architecture.md) | Layers, transports, request flows, key design decisions, testing strategy |
| [03 — Data Model & Storage](03-data-model.md) | Document / Edge model, SQLite schema, chunking, hybrid retrieval |
| [04 — Connectors & Sync](04-connectors-and-sync.md) | Connector contract, GitHub / GitLab / Notion / Jira connectors, scheduler, sync lock, link resolver |
| [05 — Query Engine](05-query-engine.md) | Unified pipeline, anchors, `find_decision` / `why` / `trace` / `impact_of` / `history_of`, synthesis |
| [06 — Interfaces & Config](06-interfaces-and-config.md) | MCP tools, CLI commands, gRPC API + mTLS, `lore.yaml` reference |
| [07 — Roadmap & Risks](07-roadmap.md) | Build milestones (ask-first ordering), named risks, open questions |
| [08 — Extensibility](08-extensibility.md) | Plugin kinds, the `sdk` contract, manifests, registry, instance configuration, provider roles |
| [09 — Plugin Protocol](09-plugin-protocol.md) | NDJSON wire protocol for out-of-process plugins: ops, streaming, cancellation, errors |
| [10 — Plugin Distribution](10-plugin-distribution.md) | Coordinates, install and lockfile, signatures, trust model, custom binaries |

## Decisions locked so far

- Language: **Go**, layered architecture (transport → service → repository/plugin), Uber FX for DI.
- Storage: **single SQLite file per workspace** (FTS5 + sqlite-vec, RRF fusion in Go).
  Default build is **pure Go** via ncruces/go-sqlite3 WASM bindings — no cgo.
- Transports: MCP **stdio**, MCP **Streamable HTTP**, **gRPC with mTLS**, CLI. gRPC is the
  programmatic API (future web UI), *not* an MCP transport.
- Query engine: **one pipeline, four seed modes** (retrieval / blame / ref / log).
  Every tool returns the same `EvidenceBundle` with `Chains` and `Gaps`.
- Sources: **GitHub + GitLab + Notion + Jira** ship as official plugins; the contract
  keeps Confluence / ClickUp / Slack as drop-in additions that need not live in this
  repository. Every source — and the repository list itself — is optional per workspace,
  and one plugin may be configured as several instances.
- Embeddings: provider plugin, **cloud default** (OpenAI), Ollama for fully-local operation.
- LLM synthesis: provider plugin and **optional** — MCP responses return structured evidence
  and never require an LLM key on the server. OpenAI-compatible vendors are presets, not code.
- Sync: background scheduler + manual trigger sharing a lease lock; connectors stream
  **checkpointable batches** so interrupted backfills resume mid-source.
- Tool surface: five narrow MCP verbs (`find_decision`, `why`, `trace`, `impact_of`,
  `history_of`) over one stable `EvidenceBundle`; tools deprecate in place, the bundle
  changes only additively. CLI `lore ask` maps to `find_decision`.
- Extensibility: sources, model providers and code access are **plugins** behind one
  registry and manifest; official plugins live in `plugins/` and hold no privilege a
  third party lacks; external plugins run out of process over NDJSON and are fetched by
  coordinate with pinned digests. The SQLite IndexStore stays built in.
