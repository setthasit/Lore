# 05 — Query Engine

## EvidenceBundle — the one result shape

Every query tool produces the same structure; transports differ only in whether
`SynthesisService` turns it into prose ([02 — D1/D2](02-architecture.md#key-design-decisions)).

```go
type EvidenceBundle struct {
    Question string          // normalized restatement of the query
    Anchor   *Anchor         // for why/history_of: repo, file, line span, blamed SHAs
    Nodes    []EvidenceNode  // ordered by relevance
    Chains   [][]DocID       // provenance paths, e.g. [commit, pr, issue, page]
    Gaps     []string        // explicit honesty: "trail ends at commit abc123; no linked discussion"
}

type EvidenceNode struct {
    Doc      DocumentMeta // id, source, type, title, author, url, updated_at
    Excerpt  string       // the relevant span, not the whole body
    Role     string       // "blamed_commit" | "review_thread" | "linked_issue" |
                          // "design_doc" | "semantic_match"
    Score    float32
    Via      []Edge       // how this node was reached (graph) — empty for pure retrieval hits
}
```

Invariants:

- **Every node carries a real URL.** No URL → not evidence → not returned.
- **Gaps are explicit.** A dead-end trail is reported, never papered over.
- Excerpts are extracted spans; full bodies are available via `trace` on the
  node's ID, keeping default responses token-cheap for MCP clients.

## Tool algorithms

### `why(repo, file, line_start, line_end, question?)`

1. **Blame** the span on the local clone → contributing commit SHAs per line
   (GitConnector).
2. **Graph walk** from each blamed commit through `edges`, depth ≤ 3:
   commit → PR → review threads → linked issues → linked pages. Cycle-guarded;
   confidence multiplies along the path; low-confidence tails pruned.
3. **Semantic expansion**: embed `question` (when given) + the blamed code
   span + commit subjects; hybrid retrieval over decision-type docs; drop hits
   already found by the walk; keep top-k as `semantic_match` nodes (catches
   discussions never formally linked). A question like "why A instead of B"
   retrieves docs discussing the rejected alternative B — unreachable from
   code-span embedding alone.
4. **Rank**: graph proximity (fewer hops = higher) × edge confidence ×
   retrieval score (against the question when given) × mild recency prior.
   Assemble `Chains` from walk paths; record `Gaps` for blamed commits with no
   outgoing edges.

`question` is optional: blame + graph walk need only the line span. Absent →
defaults to "why does `<file>:<L1>-<L2>` exist in its current form". When
given, it also fills `EvidenceBundle.Question` and drives the synthesis prompt
on non-AI surfaces.

### `trace(ref)`

Accepts a commit SHA, PR/issue number, or document URL/ID → resolves to a
document → returns its full provenance neighborhood (1–2 hops, both
directions) plus its full body. The drill-down companion to `why`: an agent
gets breadth from `why`, then depth from `trace` on the interesting node.

### `find_decision(query)`

1. Hybrid retrieval (BM25 + vectors + RRF) over all sources, optionally
   filtered (`repo`, `source`, `doc_type`, time range).
2. Expand each top hit one hop through `edges` — a matching PR pulls in its
   issue and design doc.
3. Group into per-decision evidence clusters, rank, return.

Example: `find_decision("why Postgres instead of Mongo")` → ADR page in
Notion + the PR that introduced the Postgres driver + the issue debate.

### `history_of(path)`

1. `git log --follow` on the path (rename-aware) → commit sequence.
2. Attach each commit's PR/issue/doc layer via `edges`.
3. Return a chronological timeline; large histories are windowed
   (`limit` + `before` cursor pagination).

## SynthesisService (non-AI surfaces only)

Input: `EvidenceBundle` + original question. Behavior:

- Prompt = system instruction ("answer ONLY from the provided evidence; cite
  the node URL for every claim; state gaps explicitly") + serialized bundle.
- Output: markdown prose with inline `[n]` citations mapped to node URLs,
  ending with a source list.
- Provider = user-configured `LLMConnector` (OpenAI / Anthropic / Z.AI /
  Ollama). Missing LLM config → clear error naming the remedy; MCP surfaces
  are unaffected because they never call synthesis.

## Query-time validation (service layer)

- Workspace exists and is initialized; embedder identity in `meta` matches
  config (mismatch → explicit re-embed error).
- `why`: repo registered, file exists at HEAD of local clone, line span valid.
- `trace`: ref resolves to exactly one document (ambiguous SHA prefix → error
  listing candidates).
- All limits (depth, top-k, excerpt size) capped server-side; MCP clients
  cannot request unbounded output.
