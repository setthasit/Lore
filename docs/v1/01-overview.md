# 01 — Overview

## Problem

Codebase RAG tools answer *what* code is: "where is the auth logic", "what calls this
function". The question that actually burns hours every day is *why*:

- Why does this function have a weird retry with a 250ms sleep?
- Who added this check, what bug forced it, and is it still needed?
- Why did the team choose Postgres over Mongo for this service?
- Which repos are affected by decision X?

The answers exist — scattered across git blame, commit messages, PR descriptions,
review threads, linked issues, and design docs in Notion/Confluence/Jira. Nobody
reads that trail manually. Git alone is often insufficient: a commit message that
says only `JIRA-4521` carries no reasoning; the reasoning lives in the ticket.

## Concept

**Lore** indexes the *decision trail*, not (primarily) the code. It:

1. Ingests documents from configured sources (GitHub, Notion in v1) into a
   normalized store.
2. Resolves cross-source references (ticket keys, URLs, SHAs, file paths) into a
   typed **edge graph**: commit → PR → review thread → issue → design doc.
3. Answers provenance questions by combining **graph walks** (from `git blame`
   outward) with **hybrid semantic retrieval** (BM25 + vectors), returning an
   **EvidenceBundle** where every claim carries a source URL.

Honesty is a feature: when the trail ends, Lore says so
("trail ends at commit `abc123`, no linked discussion") instead of fabricating.

## Differentiators

| Typical codebase RAG | Lore |
|---|---|
| Indexes code (the *what*) | Indexes decisions (the *why*) |
| Single repo | Multi-repo workspace, GitHub + GitLab, public + private |
| Git-only context | Optional connectors: Notion, Jira, Confluence, ClickUp, … |
| Returns similar chunks | Returns a cited provenance *chain* (graph walk + retrieval) |
| Server synthesizes prose with its own LLM | MCP returns structured evidence; the calling agent synthesizes. No LLM key needed on the server |

## Landscape (why this niche is open)

Saturated: "index codebase → vector DB → semantic search" MCP servers —
[project-rag](https://github.com/Brainwires/project-rag),
[code-graph-rag](https://github.com/vitali87/code-graph-rag),
[claude-context](https://github.com/zilliztech/claude-context),
[codebase-memory-mcp](https://github.com/DeusData/codebase-memory-mcp).
All index the *what*. Cross-source decision provenance with citation-grounded
evidence is not covered by any of them.

## Goals

- Answer "why does this code exist" with cited, verifiable evidence.
- Work across multiple repositories (GitHub and GitLab; public and private).
- Treat every non-git source as an optional connector; a workspace with only
  GitHub + Notion is fully supported.
- Be useful from an AI agent (MCP), a terminal (CLI), and programmatically
  (gRPC, future web UI).
- Local-first: index lives in a single SQLite file; private credentials never
  leave the machine; fully-local mode via Ollama embeddings.
- Ship as a single static-ish Go binary.

## Non-goals (v1)

- Not a general codebase semantic-search tool (the saturated space).
- No hosted multi-tenant service; server modes (HTTP/gRPC) are self-hosted.
- No web UI in v1 — but the gRPC API is designed so a UI can be added without
  core changes.
- No write operations against any source: Lore is strictly read-only toward
  GitHub/Notion/etc.

## Name

Working name **`lore`** — short, meaningful ("accumulated tribal knowledge"),
excellent CLI ergonomics (`lore why`, `lore sync`, `lore ask`). Check for name
collisions (GitHub, package registries) before first public release.
