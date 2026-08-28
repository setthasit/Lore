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

**Design phase — v3.** Supersedes [v1](../v1/); v2 was abandoned research and
is not a basis for anything. Implementation planning starts after this set is
approved.

## Reading order

| Doc | Contents |
|-----|----------|
| [00 — Design deltas](00-design-deltas.md) | Every change vs v1, with rationale — read this first if you know v1 |
| [01 — Overview](01-overview.md) | Problem, concept, differentiators, goals / non-goals, landscape |
| [02 — Architecture](02-architecture.md) | Layers, transports, request flows, key design decisions, testing strategy |
| [03 — Data Model & Storage](03-data-model.md) | Document / Edge model, SQLite schema, chunking, hybrid retrieval |
| [04 — Connectors & Sync](04-connectors-and-sync.md) | Connector contract, GitHub / Notion / Jira connectors, scheduler, sync lock, link resolver |
| [05 — Query Engine](05-query-engine.md) | Unified pipeline, anchors, `find_decision` / `why` / `trace` / `impact_of` / `history_of`, synthesis |
| [06 — Interfaces & Config](06-interfaces-and-config.md) | MCP tools, CLI commands, gRPC API + mTLS, `lore.yaml` reference |
| [07 — Roadmap & Risks](07-roadmap.md) | Build milestones (ask-first ordering), named risks, open questions |

## Decisions locked so far

- Language: **Go**, layered architecture (transport → service → repository/connector), Uber FX for DI.
- Storage: **single SQLite file per workspace** (FTS5 + sqlite-vec, RRF fusion in Go).
  Default build is **pure Go** via ncruces/go-sqlite3 WASM bindings — no cgo.
- Transports: MCP **stdio**, MCP **Streamable HTTP**, **gRPC with mTLS**, CLI. gRPC is the
  programmatic API (future web UI), *not* an MCP transport.
- Query engine: **one pipeline, four seed modes** (retrieval / blame / ref / log).
  Every tool returns the same `EvidenceBundle` with `Chains` and `Gaps`.
- v1 connectors: **GitHub + Notion + Jira**; connector contract keeps
  GitLab / Confluence / ClickUp / Slack as drop-in additions. Every connector —
  and the repository list itself — is optional per workspace.
- Embeddings: pluggable provider, **cloud default** (OpenAI), Ollama for fully-local operation.
- LLM synthesis: pluggable and **optional** — MCP responses return structured evidence and never
  require an LLM key on the server.
- Sync: background scheduler + manual trigger sharing a lease lock; connectors stream
  **checkpointable batches** so interrupted backfills resume mid-source.
- Tool surface: five narrow MCP verbs (`find_decision`, `why`, `trace`, `impact_of`,
  `history_of`) over one stable `EvidenceBundle`; tools deprecate in place, the bundle
  changes only additively. CLI `lore ask` maps to `find_decision`.
- Plugin ecosystem: deferred; the connector seam stays plugin-ready via three
  disciplines (entities-only imports, additive schemas, conformance suite).
