# 00 — Design Deltas (v1 → v3)

Every substantive change from the v1 design, with rationale. v2 was abandoned
research and contributed nothing here.

## Driving requirement

v1 framed Lore as a *codebase* provenance engine: the rich pipeline (graph
walk, chains, gaps) existed only behind `git blame`. The product goal is
broader: a **working assistant for engineering decisions** that must answer

- "Why did we choose option B instead of A when incident X happened?"
- "At the moment of X, why did we do A — and what impact did we get?"

from Jira/Notion alone, **with zero repositories configured**. Code anchoring
stays, as one anchor type among several.

## Deltas

| # | Change | Was (v1) | Now (v3) | Rationale |
|---|--------|----------|----------|-----------|
| Δ1 | Product framing | "Provenance engine for codebases; answers why *code* exists" | Decision-provenance engine; anchors = query, code span, document, time window | Framing drove design; every rich feature hung off `git blame`. See [01](01-overview.md). |
| Δ2 | Zero-repo workspaces | `repos:` implicitly required; `why` validation assumed a registered repo | `repos:` explicitly optional; workspace valid with none; `why`/`history_of` fail with a clear "code anchoring disabled" error; all other tools unaffected | Ask-only (Jira/Notion) is a primary configuration. See [06](06-interfaces-and-config.md). |
| Δ3 | Pipeline asymmetry | `why` got walk + chains + gaps; `find_decision` got retrieval + 1 hop, no chains, unspecified "clustering" | One engine, four seed modes (retrieval / blame / ref / log); **every** tool returns `Chains` + `Gaps`; clustering deleted | The ask path — the primary path — received the weakest machinery. See [05](05-query-engine.md). |
| Δ4 | Event/time anchoring | None; agent had to guess dates | `find_decision` takes `around` (free text or date) → event resolution → time window; ranking uses proximity-to-anchor instead of recency when anchored | "When incident X happened" is unanswerable without it. See [05](05-query-engine.md#event-resolution). |
| Δ5 | Impact queries | Not expressible | New `impact_of` tool: forward-in-time walk (incoming references after anchor time) + time-filtered semantic expansion, chronological timeline | "What impact did we get?" is a target question. No new edge kinds needed — direction + time on existing edges. |
| Δ6 | Jira | Post-v1 aspiration | v1 connector (Cloud: new `/search/jql` endpoint, `updated` watermark, ADF→text) | User's ask-only scenario names Jira explicitly; ticket-key linking is the classic provenance case. See [04](04-connectors-and-sync.md#jiraconnector-v1). |
| Δ7 | `Anchor` shape | Code-only (repo, file, lines, SHAs) | Union: query / code span / document / time window | Generalizing later would break the bundle contract on every surface. |
| Δ8 | `Document.CreatedAt` | Only `UpdatedAt` | Both | Event time ≠ last-edit time. A postmortem edited yesterday still belongs to last year's incident. Timelines, event resolution, and impact filtering all key on `CreatedAt`. |
| Δ9 | Config conflation | `repos:` doubled as GitHub ingest list (via `remote:`) and local-clone registry | `sources.github.repos` = what to ingest; top-level `repos:` = local clones for blame only | Zero-repo workspaces must still ingest GitHub; enrichment mapping via `remote:` when both exist. |
| Δ10 | `Connector.Changes` signature | Returned final `Cursor` *before* the stream was consumed — contradicted "checkpoint every batch" | Streams `Batch{Docs, Cursor}`; orchestrator commits batch, then persists its cursor | v1 signature made the documented crash-safe resume unimplementable. |
| Δ11 | `authored_follow_up` edge | Declared in `EdgeKind`, produced by nothing | Deleted; `supersedes` added (ADR "supersedes" text pattern) | Orphan removed; supersession is real signal for "B instead of A" questions. |
| Δ12 | `Neighbors` direction | Unspecified (trace claimed "both directions") | Explicit `dir` parameter (out / in / both) in the store contract | Impact walking requires incoming-edge traversal; contracts must say so. |
| Δ13 | SQLite build | cgo (mattn) assumed; "pure-Go fallback if pain appears" | Default **ncruces/go-sqlite3 WASM** bindings (pure Go, sqlite-vec embedded); cgo variant kept as a benchmark alternative behind the same interface | Official WASM bindings exist ([sqlite-vec-go-bindings](https://github.com/asg017/sqlite-vec-go-bindings/)); removes the cross-compile risk instead of mitigating it. |
| Δ14 | Milestone order | Blame-provenance at M2; Notion at M3; Jira absent | Ask-first: retrieval core → provenance engine (`find_decision`/`trace`/`impact_of`) → Notion + Jira → code anchoring | Ship the assistant before the code explorer; matches stated priority. |
| Δ15 | Tool surface | `find_decision` returned flat clusters; no impact verb | `find_decision` kept and upgraded to the full engine; `impact_of` added as a narrow fifth verb; CLI `lore ask` still maps to `find_decision` | Verb honesty: MCP returns evidence, not answers — a rename to `ask` was considered and rejected in review as overselling. Narrow verbs route better in LLM hosts than modal params. |
| Δ16 | Contract stability | Implicit | `EvidenceBundle` = the stable, additively-evolving contract; tools = disposable verbs with in-place MCP deprecation | Changing param semantics silently breaks agent prompts; adding/retiring tools is cheap. |
| Δ17 | Plugin readiness | Not considered | Connector seam kept plugin-viable by three disciplines (entities-only imports, additive schemas, conformance suite); plugin transport itself deferred | Δ10 is the cautionary tale: freezing a wire protocol before the contract stops moving turns fixes into ecosystem breaks. |
| Δ18 | Connector/provider construction | A hardcoded switch per source and per provider in the DI module; adding either meant editing core | Plugin registry + manifest; the engine holds no source or provider name, and registration validates that a manifest matches what it built | "Adding a source is one package" was true only if you were us. See [08](08-extensibility.md). |
| Δ19 | Source configuration | Closed `sources:` struct, one typed field per source, strict decode rejecting everything else | `sources:` is a list of instances (`id` / `use` / `with`); `with:` is validated against the plugin's manifest and decoded strictly by the plugin | A config format that cannot express an unknown source cannot configure a plugin. Multiple instances of one plugin (two Jira sites) become expressible as a side effect. |
| Δ20 | Where implementations live | Connectors and providers under `internal/`, unimportable by anyone | `sdk/` (public contract, stdlib only) + `plugins/` (official plugins), both outside `internal/`; boundaries enforced by depguard, not by convention | A plugin nobody can import is not a plugin. Official plugins now hold no privilege a third party lacks. |
| Δ21 | AI providers | One Go package per vendor (`openai`, `anthropic`, `zai`, `ollama`) | Native drivers only where the wire format differs; every OpenAI-compatible vendor (Z.AI, OpenRouter, Moonshot, DeepSeek, Groq, …) is a preset row of one driver | The Z.AI package was 19 lines wrapping the OpenAI client — configuration wearing a package costume. |
| Δ22 | Embedder identity | The provider self-reported `"provider/model/dims"` | The plugin reports `Dimensions()`; the host composes the identity from the manifest name, the configured model and that width | A plugin must not be able to claim another's vector space and silently poison an index. |
| Δ23 | Local git access | A bespoke interface, deliberately not a connector | A third plugin kind (`KindCode`) behind the same registry and manifest | Mercurial, Jujutsu and remote forges are real alternative implementations of a two-method contract. |
| Δ24 | Unknown `RefKind` from a connector | Resolved to an empty rule: every such reference vanished with no error | Rejected at ingest, naming the known kinds | Silent drops are undebuggable for a plugin author; `DocType` stays open because unknown types degrade to ordinary evidence, which is honest. |

Δ17 is superseded by Δ18–Δ24: the seam it protected is now the shipped
contract, and the transport it deferred is specified in
[09](09-plugin-protocol.md), which shipped with external plugins and is now
frozen — it evolves additively from here.

## Unchanged (deliberately)

- Layering, FX wiring, error-handling policy, testing strategy shape.
- D1: MCP returns evidence, not prose; zero LLM key server-side.
- Synthesis as an optional final step for non-AI surfaces.
- Single SQLite file per workspace; RRF fusion in Go; store portability rules.
- Lease-lock sync (heartbeat + TTL takeover); read-only connectors; env-only secrets.
- `Gaps` honesty invariant — extended to every tool rather than weakened.
