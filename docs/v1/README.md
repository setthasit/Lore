# Lore — Design Documents

Lore is a provenance engine for codebases: an MCP server, CLI, and gRPC service
that answers *why* code exists — not *what* it is — by indexing the decision trail
(commits, pull requests, review threads, issues, Notion pages, …) across multiple
repositories and knowledge sources, and returning citation-grounded evidence.

## Status

**Design phase.** These documents are the source of truth for the design review.
Implementation planning starts after this set is approved.

## Reading order

| Doc | Contents |
|-----|----------|
| [01 — Overview](01-overview.md) | Problem, concept, differentiators, goals / non-goals, landscape |
| [02 — Architecture](02-architecture.md) | Layers, transports, request flows, key design decisions, testing strategy |
| [03 — Data Model & Storage](03-data-model.md) | Document / Edge model, SQLite schema, chunking, hybrid retrieval |
| [04 — Connectors & Sync](04-connectors-and-sync.md) | Connector contract, GitHub / Notion connectors, scheduler, sync lock, link resolver |
| [05 — Query Engine](05-query-engine.md) | EvidenceBundle, `why` / `trace` / `find_decision` / `history_of` algorithms, synthesis |
| [06 — Interfaces & Config](06-interfaces-and-config.md) | MCP tools, CLI commands, gRPC API + mTLS, `lore.yaml` reference |
| [07 — Roadmap & Risks](07-roadmap.md) | Build milestones, named risks, open questions |

## Decisions locked so far

- Language: **Go**, layered architecture (transport → service → repository/connector), Uber FX for DI.
- Storage: **single SQLite file per workspace** (FTS5 + sqlite-vec, RRF fusion).
- Transports: MCP **stdio**, MCP **Streamable HTTP**, **gRPC with mTLS**, CLI. gRPC is the
  programmatic API (future web UI), *not* an MCP transport.
- v1 connectors: **GitHub + Notion**; connector contract keeps GitLab / Jira / Confluence / ClickUp
  as drop-in additions. Every connector is optional per workspace.
- Embeddings: pluggable provider, **cloud default** (OpenAI), Ollama for fully-local operation.
- LLM synthesis: pluggable and **optional** — MCP responses return structured evidence and never
  require an LLM key on the server.
- Sync: background scheduler + manual trigger sharing a lease lock; scheduler skips a round when
  the lock is held.
