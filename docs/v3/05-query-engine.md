# 05 — Query Engine

## One pipeline, four seed modes

Every tool runs the same pipeline; only the **seed** differs
([02 — D3](02-architecture.md#key-design-decisions)):

```
resolve anchor → seed docs → graph walk → semantic expansion → rank → EvidenceBundle
```

| Tool | Anchor | Seed | Walk direction | Semantic expansion |
|---|---|---|---|---|
| `find_decision` | query (± time window) | top-k retrieval hits → parent docs | both | none (seeding *is* retrieval) |
| `why` | code span | blamed commits | both | question + code span + commit subjects |
| `trace` | document | resolved doc | `direction` param, default both | none (mechanical neighborhood) |
| `impact_of` | document + its time | resolved doc | forward-in-time (see below) | question, filtered `created_at > T` |
| `history_of` | file path | `git log --follow` commits | both, 1 hop per commit | none |

Shared machinery:

- **Graph walk**: breadth-first through `edges`, depth ≤ 3 (config-capped),
  cycle-guarded; confidence multiplies along the path; tails below the
  confidence floor (start: 0.3) are pruned.
- **Ranking**: `score = graph proximity (fewer hops = higher) × path confidence
  × retrieval relevance (when a question exists) × time prior`.
  Time prior: **unanchored** → mild recency boost; **time-anchored** →
  proximity to the anchor window (recency would be actively wrong for
  "at the moment of X" questions).
- **Chains**: assembled from walk paths for every tool.
- **Gaps**: explicit honesty for every tool — dead-end seeds, unresolved
  events, empty impact windows.

## EvidenceBundle — the one result shape

```go
type EvidenceBundle struct {
    Question string          // normalized restatement of the query
    Anchor   Anchor          // how the question was grounded (union, below)
    Nodes    []EvidenceNode  // ordered by relevance (impact_of: chronological)
    Chains   [][]DocID       // provenance paths, e.g. [ticket, page, pr, commit]
    Gaps     []string        // "trail ends at PROJ-4521; no linked follow-up"
}

type Anchor struct {
    Kind   AnchorKind  // query | code_span | document | time_window (combinable: Code+Window, Doc+Window)
    Query  string      // find_decision: the question as used for retrieval
    Code   *CodeAnchor // why/history_of: repo, file, line span, blamed SHAs
    Doc    *DocRef     // trace/impact_of: the resolved anchor document
    Window *TimeWindow // event resolution result: from, to, how it was derived
}

type EvidenceNode struct {
    Doc      DocumentMeta // id, source, type, title, author, url, created_at, updated_at
    Excerpt  string       // the relevant span, not the whole body
    Role     string       // "seed" | "blamed_commit" | "review_thread" | "linked_ticket" |
                          // "design_doc" | "follow_up" | "semantic_match"
    Score    float32
    Via      []Edge       // how this node was reached (graph) — empty for pure retrieval hits
}
```

Invariants:

- **Every node carries a real URL.** No URL → not evidence → not returned.
- **Gaps are explicit.** A dead-end trail is reported, never papered over.
- **Every tool fills `Chains` and `Gaps`** — the ask-only path is not a
  second-class citizen.
- Excerpts are extracted spans; full bodies are available via `trace` on the
  node's ID, keeping default responses token-cheap for MCP clients.

## Event resolution

`find_decision` accepts `around` — a free-text event ("incident X", "the March outage")
or an ISO date. It compiles to a `TimeWindow`:

1. **Date given** → window = date ± `event_window` (default 30d, configurable).
2. **Free text** → hybrid retrieval of the event text; take the top hits'
   `CreatedAt`. If the top hits agree (span ≤ 2 × `event_window`), anchor time
   = the earliest agreeing hit; window = anchor ± `event_window`.
3. **Ambiguous** (top hits scattered in time) → no window; proceed unwindowed
   and record a Gap: `"could not resolve event 'X' to a time — candidates:
   <top 3 with dates and URLs>"`. The calling agent can retry with a date or a
   more specific phrase.

The window is applied as a `created_at` range filter on seed retrieval and
switches the ranking time prior to proximity mode. The resolved window — and
the document that anchored it — is returned in `Anchor.Window`, so answers are
auditable ("interpreted 'incident X' as 2025-03-12 via INC-201").

## Tool algorithms

### `find_decision(question, around?, filters?)`

The primary assistant entry point. Zero code required.

1. **Event resolution** (when `around` given) → time window.
2. **Seed**: hybrid retrieval (BM25 + vectors + RRF) of `question`, with
   filters (`source`, `repo`, `doc_type`, `since`/`until`, resolved window);
   lift top-k chunks to parent documents.
3. **Graph walk** from each seed, depth ≤ 3, both directions: a matching
   ticket pulls in its design doc, the PR that implemented it, the review
   thread that debated it — and anything that later referenced *it*.
4. **Rank** (question relevance × proximity × confidence × time prior);
   assemble `Chains`; record `Gaps` for seeds with no edges ("PROJ-4521
   stands alone; no linked discussion") and for unresolved events.

Example: `find_decision("why did we choose option B instead of A?", around="incident X")`
→ window from INC-201 → seeds: decision page + ticket debating A vs B →
chains: incident ticket → decision page → implementing PR. Retrieval also
surfaces documents discussing the *rejected* alternative A — reachable only
lexically/semantically, exactly what pure graph tools miss.

### `why(repo, file, line_start, line_end, question?)`

Code-anchored variant; requires a registered local clone.

1. **Blame** the span on the local clone → contributing commit SHAs per line
   (GitConnector).
2. **Walk** from each blamed commit: commit → PR → review threads → linked
   tickets/issues → linked pages.
3. **Semantic expansion**: embed `question` (when given) + the blamed code
   span + commit subjects; hybrid retrieval over decision-type docs; drop hits
   already found by the walk; keep top-k as `semantic_match` nodes (catches
   discussions never formally linked).
4. **Rank + Chains + Gaps** as shared.

`question` is optional: blame + walk need only the line span. Absent →
defaults to "why does `<file>:<L1>-<L2>` exist in its current form".

### `trace(ref, direction?, depth?)`

Accepts a commit SHA, PR/issue number, ticket key, or document URL/ID →
`ResolveRef` → exactly one document (ambiguous → error listing candidates) →
returns its provenance neighborhood (depth ≤ 2, `direction` = out / in / both,
default both) plus its **full body**, nodes ordered chronologically. The
drill-down companion: breadth from `find_decision`/`why`, depth from `trace`.

### `impact_of(ref_or_query, question?)`

Answers "what happened because of this decision?".

1. **Resolve anchor**: a ref resolves via `ResolveRef` (ambiguity → error with
   candidates); a free-text query resolves via retrieval, top document, with
   the interpretation recorded in `Anchor.Doc`. Anchor time `T` =
   `Doc.CreatedAt`.
2. **Forward walk**: traverse edges from the anchor, both directions
   *mechanically* but keep only nodes with `CreatedAt > T` — dominated in
   practice by **incoming** `references_doc`/`mentions_commit` edges (later
   documents citing the decision) and `supersedes` chains. Depth ≤ 3.
3. **Semantic expansion**: retrieval of `question` (default: "consequences,
   follow-ups, incidents related to <anchor title>") + anchor excerpt, hard
   filter `created_at > T`; drop already-found nodes; keep top-k as
   `semantic_match` — catches the postmortem that never linked back.
4. **Return chronologically** (a timeline, not a relevance list); `Chains` =
   anchor → follow-up paths; `Gaps` when nothing exists after `T`
   ("no follow-up evidence after 2025-03-12") — itself a useful answer.

### `history_of(path)`

1. `git log --follow` on the path (rename-aware) → commit sequence. Requires a
   registered local clone.
2. Attach each commit's PR/ticket/doc layer via `edges` (1 hop per commit).
3. Return a chronological timeline; large histories are windowed
   (`limit` + `before` cursor pagination).

## SynthesisService (non-AI surfaces only)

Input: `EvidenceBundle` + original question. Behavior:

- Prompt = system instruction ("answer ONLY from the provided evidence; cite
  the node URL for every claim; state gaps explicitly; respect the timeline
  ordering for impact questions") + serialized bundle.
- Output: markdown prose with inline `[n]` citations mapped to node URLs,
  ending with a source list.
- Provider = user-configured `LLMConnector` (OpenAI / Anthropic / Z.AI /
  Ollama). Missing LLM config → clear error naming the remedy; MCP surfaces
  are unaffected because they never call synthesis.

## Query-time validation (service layer)

- Workspace exists and is initialized; embedder identity in `meta` matches
  config (mismatch → explicit re-embed error).
- `find_decision` / `impact_of` / `trace`: no repo required, ever.
- `why` / `history_of`: at least one repo registered — otherwise precondition
  error `"no repositories registered — code anchoring disabled for this
  workspace"`; then repo registered, file exists at HEAD of local clone, line
  span valid.
- `trace` / `impact_of` ref: resolves to exactly one document (ambiguous SHA
  prefix → error listing candidates).
- All limits (walk depth, top-k, excerpt size, timeline window) capped
  server-side; MCP clients cannot request unbounded output.
