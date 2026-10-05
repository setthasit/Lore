# 01 — Overview

## Problem

Engineering teams lose their *why*. Decisions are made in incident channels,
Jira tickets, Notion pages, PR reviews — then the people rotate, the context
scatters, and six months later nobody can answer:

- Why did we choose option B instead of A when incident X happened?
- At the moment of X, why did we do A — and what impact did we get?
- Why does this function have a weird retry with a 250ms sleep?
- Who added this check, what bug forced it, and is it still needed?
- Which repos and services were affected by decision X?

The answers exist — scattered across tickets, design docs, postmortems, PR
descriptions, review threads, commit messages. Nobody reads that trail
manually. No single source is sufficient: a commit that says only `JIRA-4521`
carries no reasoning; the reasoning lives in the ticket; the *consequences*
live in documents written weeks later.

Codebase RAG tools answer *what* code is. Lore answers *why decisions were
made and what happened next* — whether or not the question touches code at all.

## Concept

**Lore** indexes the *decision trail*. It:

1. Ingests documents from configured sources (GitHub, GitLab, Notion and Jira
   ship as official plugins, `plugins/plugins.go`, `Official`) into
   a normalized store. Sources are independent; **any subset works**, including
   workspaces with no repository at all.
2. Resolves cross-source references (ticket keys, URLs, SHAs, file paths) into
   a typed **edge graph**: ticket → design doc → PR → review thread → commit,
   in any direction the links actually exist.
3. Answers provenance questions through **one query pipeline** that combines
   graph walks with **hybrid semantic retrieval** (BM25 + vectors) and
   **time anchoring** ("when incident X happened"), returning an
   **EvidenceBundle** where every claim carries a source URL.

The pipeline is seeded four ways — a free-text question (`find_decision`), a code span
(`why`, via `git blame`), a specific document (`trace`, `impact_of`), or a file
history (`history_of`). Same walk, same bundle shape. The ranking differs by
tool, and [05](05-query-engine.md) says how.

Honesty is a feature: when the trail ends, Lore says so in the bundle's gaps
rather than fabricating. When the index holds at least one link, a standalone
seed is reported as title then DocID:
`Move session auth to JWT (jira:ticket:PROJ-4521) stands alone; no linked discussion`.
When the index holds no links at all, those per-result lines are replaced by
one index-level line:
`the index holds no links between documents, so no result has linked discussion. Add a source whose documents reference each other, or register a code clone under repos`

No standalone gaps means no index-level line. Other gap kinds are unchanged,
including unresolved events, "no follow-up evidence after 2025-03-12" and
"trail ends at commit 4f2b91c0d3ae, not synced from a source".

## Two personas, one engine

| Ask-only workspace (no repos) | Code-anchored workspace |
|---|---|
| Sources: Jira + Notion (± GitHub issues/PRs) | Same, plus local clones registered |
| "Why B over A during incident X?" → `find_decision` | "Why does auth.go:40-55 exist?" → `why` |
| "What impact did decision A have?" → `impact_of` | "How did this file evolve?" → `history_of` |
| Anchors: query, document, time window | Additional anchor: code span (blame) |

Nothing about the first column is degraded: `find_decision`, `trace`, and `impact_of`
run the full walk with chains and gaps. The second column is the first column
plus one extra anchor type.

## Differentiators

| Typical codebase RAG | Lore |
|---|---|
| Indexes code (the *what*) | Indexes decisions (the *why* and the *what happened next*) |
| Requires a repository | Repository optional; Jira/Notion-only workspaces are first-class |
| Single repo | Multi-repo, multi-source workspace; cross-source edges |
| Returns similar chunks | Returns a cited provenance *chain* (graph walk + retrieval + time anchor) |
| No temporal reasoning | Event resolution ("when incident X happened") and forward-in-time impact tracing |
| Server synthesizes prose with its own LLM | MCP returns structured evidence; the calling agent synthesizes. No LLM key needed on the server |

## Landscape (why this niche is open)

Saturated: "index codebase → vector DB → semantic search" MCP servers —
[project-rag](https://github.com/Brainwires/project-rag),
[code-graph-rag](https://github.com/vitali87/code-graph-rag),
[claude-context](https://github.com/zilliztech/claude-context),
[codebase-memory-mcp](https://github.com/DeusData/codebase-memory-mcp).
All index the *what*, all require code. Cross-source decision provenance with
citation-grounded evidence, temporal anchoring, and impact tracing is not
covered by any of them.

## Goals

- Answer "why was this decided" and "what impact did it have" with cited,
  verifiable evidence — with or without a codebase.
- Treat every source as an optional connector; Jira-only, Notion-only,
  GitHub-only, and any combination are fully supported.
- Keep code provenance excellent: `git blame` anchoring, rename-aware history,
  commit → PR → review → issue chains.
- Be useful from an AI agent (MCP), a terminal (CLI), and programmatically
  (gRPC, future web UI).
- Local-first: the index is a single SQLite file, and a credential is written
  in `lore.yaml` as a literal or read from the environment through
  `${env:VAR}`, or, for a compiled-in plugin whose field is omitted, from its
  default variable, then handed only to the plugin whose manifest declared it
  (`internal/registry/secrets.go`, `resolveSecrets`). Each credential does
  reach the API it authenticates, so a Jira token goes to Jira,
  and with a hosted embedder the chunk text being indexed goes to that vendor
  too. Binding `embedder:` to Ollama keeps even that on the machine. The engine
  sends nothing anywhere else, having no service of its own, but an installed
  plugin is an unconfined process with its own network access
  ([08](08-extensibility.md#invariants-a-plugin-must-not-break), invariant 5).
- Ship as a single **pure-Go** binary, with no cgo. The release build sets
  `CGO_ENABLED=0` in the `Makefile`, and no package imports `C`.

## Non-goals (v1)

- Not a general codebase semantic-search tool (the saturated space).
- Not an incident-management or ticketing tool: Lore reads the trail, it never
  writes to any source. The engine cannot: `lore.Connector` and `lore.CodeRepo`
  expose no write operation, and the four official source plugins call read
  endpoints only. A third-party plugin's own HTTP calls are its own
  responsibility ([04](04-connectors-and-sync.md#connector-contract)).
- No hosted multi-tenant service; server modes (HTTP/gRPC) are self-hosted.
- No web UI in v1 — but the gRPC API is designed so a UI can be added without
  core changes.
- No conversational memory in v1 — queries are single-shot; the MCP host model
  carries conversation state itself.

## Name

**`lore`** — short, meaningful ("accumulated tribal knowledge"), excellent CLI
ergonomics (`lore ask`, `lore why`, `lore impact`). Kept after a collision
check; the Go module path is `github.com/setthasit/Lore` and the binary is
`lore`.

The name is not exclusive, and the check said so before the decision was made:

| Where | Holder | Overlap |
|---|---|---|
| GitHub | [EpicGames/lore](https://github.com/EpicGames/lore) — a version-control system | developer tooling, adjacent domain |
| Web | `lore.kernel.org` — the kernel mailing-list archive | developer tooling, different shape |
| PyPI | Instacart's `lore` ML framework | different ecosystem |
| npm | `lore` React/Redux framework | different ecosystem |
| crates.io | `lore` logic-programming crate | different ecosystem |
| Homebrew | free | the CLI formula name is available |

No package registry Lore ships to is taken, so the cost of the collision is
search-result noise, not a blocked release. Revisit only if the project
publishes to an ecosystem where the name is already occupied.
