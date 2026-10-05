# 05 — Query Engine

## One pipeline, four seed modes

Every query tool walks the same stages in the same order
([02 — D3](02-architecture.md#key-design-decisions)). The seed differs by tool,
and so do two later stages: semantic expansion runs for `why` and `impact_of`
only, and the time-prior ranking formula for `find_decision` only. The table
says which, and the walk and rank notes under it name the symbol each tool
reaches:

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

- **Graph walk**: breadth-first through `edges`, cycle-guarded by a visited set
  so a node is reached once (`internal/services/walk.go`, `walkGraph`).
  Depth is `query.walk_depth`, default 3 when the key is unset or non-positive
  (`walkOptions.hops`), while `trace` and `history_of` pass their own instead.
  Config validation refuses a negative depth and caps nothing above it, so an
  operator who writes `walk_depth: 40` gets a 40-hop walk. Confidence
  multiplies along the path and a tail below the floor is pruned. The floor is a
  fixed 0.3 and not a setting, because no caller passes
  `walkOptions.MinConfidence` (`walkOptions.confidenceFloor`).
- **Ranking**: `find_decision` scores
  `graph proximity (fewer hops = higher) × path confidence × retrieval relevance
  × time prior` in `rank` (`internal/services/rank.go`), whose only caller is
  `queryService.FindDecision` (`internal/services/query.go`).
  Time prior (`timePrior.of`): **unanchored** → an age penalty of at most
  `RecencyPenalty` (0.2) across a `RecencyHorizon` of one year, so a newer
  document keeps a neutral 1 and an older one is discounted toward 0.8.
  **Time-anchored** → proximity to the centre of the resolved window, because
  recency would be actively wrong for "at the moment of X" questions.
  The other four tools do **not** reach `rank`: they score a walked node
  `proximity × path confidence` and a semantic match by its fused retrieval
  rank alone, with no time prior at all
  (`internal/services/rank.go`, `nodeSet.addWalked` and `nodeSet.addMatches`).
  For a reader that means an age discount applies to `find_decision` results and
  to nothing else, and that a `why` or `trace` bundle ranks a decade-old thread
  level with last week's.
- **Chains**: every query tool assembles them from its walk paths
  (`internal/services/rank.go`, `assembleChains`, called from `query.go`,
  `why.go`, `trace.go`, `impact.go` and `history.go`). A chain ends at its last
  cited node, so it never names a document the bundle omits (`citedChain`).
- **Gaps**: explicit honesty for every query tool. A seed no chain reached
  produces a standalone gap (`standaloneSeedGaps`, same five callers). When
  the index holds no links at all, standalone gaps collapse to one index-level
  line. No standalone gaps means no index-level line. Other gap kinds are
  unchanged: a blamed commit no source ingested
  (`internal/services/coderepo.go`, `unsyncedCommitGap`), an unresolved event
  (`internal/services/event.go`, `resolveEvent`), an empty impact window
  (`internal/services/impact.go`, `impactGaps`) and a `trace` focus that
  matched no passage (`internal/services/trace.go`, `traceExcerpt`).

## EvidenceBundle — the one result shape

The per-result gap shown below is the linked-index case (at least one link
anywhere in the index). When the index holds no links at all, nonempty
per-result gaps are replaced by one index-level line:
`the index holds no links between documents, so no result has linked discussion. Add a source whose documents reference each other, or register a code clone under repos`

```go
type EvidenceBundle struct {
    Question string          // normalized restatement of the query
    Anchor   Anchor          // how the question was grounded (union, below)
    Nodes    []EvidenceNode  // ordered by relevance (impact_of: chronological)
    Chains   [][]DocID       // provenance paths, e.g. [ticket, page, pr, commit]
    Gaps     []string        // linked index: "<title> (<doc id>) stands alone; no linked discussion"
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
                          // "linked_change" | "design_doc" | "follow_up" | "semantic_match"
    Score    float32
    Via      []Edge       // how this node was reached (graph) — empty for pure retrieval hits
}
```

Invariants:

- **Every node carries a real URL.** No URL → not evidence → not returned, and
  the drop is silent: the node set skips a URL-less document
  (`internal/services/rank.go`, `nodeSet.add`), retrieval drops a URL-less seed
  (`internal/services/retrieve.go`, `liftDocuments`), and a ref that resolves to
  one is a not-found error rather than an uncitable anchor
  (`internal/services/ref.go`, `resolveOneRef`).
- **Gaps are explicit.** A dead-end trail is reported, never papered over.
- **Every query tool fills `Chains` and `Gaps`** — the ask-only path is not a
  second-class citizen. Confirmed by the five callers of `assembleChains` and
  `standaloneSeedGaps` named above.
- Excerpts are extracted spans. That keeps default responses token-cheap for
  MCP clients. `trace` on the node's ID returns more of the document: its body
  up to 8,000 characters, or the passages matching a `focus` question.

## Event resolution

`find_decision` accepts `around` — a free-text event ("incident X", "the March outage")
or an ISO date. It compiles to a `TimeWindow`:

1. **Date given** → window = date ± `event_window` (default 30d, configurable).
2. **Free text** → hybrid retrieval of the event text, taking the top five
   hits' `CreatedAt` (`internal/services/event.go`, `eventCandidates`,
   `eventTopHits`). If they agree (span ≤ 2 × `event_window`), anchor time is
   the earliest agreeing hit and the window is anchor ± `event_window`
   (`resolveEvent`).
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
   lift top-k chunks to parent documents. The BM25 arm of every hybrid
   retrieval double-quotes each query token and OR-joins them. A run of
   letters and digits joined by single `.`, `_`, `-` or `/` stays one token,
   so the quoting makes FTS5 match `15.5` or `fast_forward` as a phrase rather
   than as separate terms (`internal/repositories/sqlite/search.go`,
   `ftsMatchExpr`).
3. **Graph walk** from each seed, `query.walk_depth` hops, both directions: a matching
   ticket pulls in its design doc, the PR that implemented it, the review
   thread that debated it — and anything that later referenced *it*.
4. **Rank** (question relevance × proximity × confidence × time prior).
   Assemble `Chains`. With at least one link anywhere in the index, record
   per-result `Gaps` for standalone seeds (the seed's title and DocID, then
   "stands alone; no linked discussion"). When the index holds no links at
   all, replace those lines with one index-level line:
   `the index holds no links between documents, so no result has linked discussion. Add a source whose documents reference each other, or register a code clone under repos`

   No standalone gaps means no index-level line. Unresolved event gaps are
   unchanged.

Example: `find_decision("why did we choose option B instead of A?", around="incident X")`
→ window from INC-201 → seeds: decision page + ticket debating A vs B →
chains: incident ticket → decision page → implementing PR. Retrieval also
surfaces documents discussing the *rejected* alternative A — reachable only
lexically/semantically, exactly what pure graph tools miss.

### `why(file, line_start, repo?, line_end?, question?)`

Code-anchored variant; requires a registered local clone.

1. **Blame** the span on the local clone → contributing commit SHAs per line
   (code plugin).
2. **Walk** from each blamed commit: commit → PR → review threads → linked
   tickets/issues → linked pages.
3. **Semantic expansion**: embed `question` (when given) + the blamed code
   span + commit subjects. Retrieval covers the **whole** index with no
   document-type restriction, because `whyService.whyMatches` passes an empty
   `entities.Filters` (`internal/services/why.go`), so a commit or a code-review
   comment competes with a decision page for the same slots. Hits the walk
   already produced are dropped when the node set dedupes by document id
   (`internal/services/rank.go`, `nodeSet.add`), and the rest become
   `semantic_match` nodes (catches discussions never formally linked).
4. **Chains + Gaps** as shared. Scoring is the non-`rank` path above.

`question` is optional: blame + walk need only the line span. Absent, it
defaults to "why does `<file>:<L1>-<L2>` exist in its current form"
(`whyQuestionOf`).

### `trace(ref, direction?, depth?, focus?)`

Accepts a commit SHA, PR/issue number, ticket key, or document URL/ID →
`ResolveRef` → exactly one document, with an ambiguous ref answered by a bad
request listing every candidate and its URL (`internal/services/ref.go`,
`resolveOneRef`, `ambiguousRef`) → returns its provenance neighborhood
(`depth` capped at `maxTraceDepth` = 2, `direction` = out / in / both, default
both) plus a **bounded excerpt of its body** (below), nodes ordered
chronologically (`byChronology`).
The drill-down companion: breadth from `find_decision`/`why`, depth from
`trace`.

The anchor node's excerpt depends on `focus`, an optional question
(`internal/services/trace.go`, `traceExcerpt`):

- **No `focus`**: the excerpt is the document body (`documentBody`). A body of
  at most 8,000 characters (`maxTraceExcerptRunes`) is returned whole. A longer
  one is cut to its first 8,000 characters, followed by a blank line and a
  marker that states the real total (`capExcerpt`):
  `[truncated: 8,000 of 70,000 characters shown. Pass focus with a question to get the passages that match it.]`
  A character here is a Unicode code point, not a byte, so the cut never
  lands inside one.
- **With `focus`**: the excerpt holds the passages of the anchor document
  that best match the question, at most 3 (`maxFocusPassages`), in document
  order. A passage is one stored chunk ([03](03-data-model.md#chunking)).
  A document with fewer than 3 matching passages returns only those. A
  focused excerpt holds only passages even when the body would fit under the
  cap in full. Adjacent passages are separated by a blank line. Passages that
  are not adjacent have a `…` line between them (`joinPassages`). The search
  is the hybrid retrieval the other tools use (`hybridSearch`). It embeds the
  focus question with the configured embedder. The `doc_id` filter restricts
  it to the anchor document (`entities.Filters.DocID`).
- **Focused excerpt over the cap**: it is cut the same way and ends with a
  short marker that carries no focus hint:
  `[truncated: 8,000 of 12,007 characters shown.]`
  The total there is the length of the joined passages. A commit or a comment
  is stored as one chunk of any length, so a long one reaches this case with
  a single passage.
- **`focus` matches no passage**: the excerpt is the one returned without
  `focus`. The bundle gains the gap
  `focus "<question>" matched no passage of <title> (<doc id>)`.
  This case arises only when the search returns nothing, for example for a
  document with no stored chunk.

`focus` is trimmed first. A whitespace-only one counts as no focus. One longer
than 1,000 characters (`maxFocusRunes`) after trimming is a bad request,
`focus must be at most 1,000 characters`, rejected before any lookup.
Neighbors and chains are the same with and without `focus`. Gaps differ only
by the no-match gap.

### `impact_of(ref_or_query, question?)`

Answers "what happened because of this decision?".

1. **Resolve anchor**: a ref resolves via the same `resolveOneRef` (ambiguity →
   error with candidates), and a free-text query resolves via retrieval, top document, with
   the interpretation recorded in `Anchor.Doc`. Anchor time `T` =
   `Doc.CreatedAt`.
2. **Forward walk**: traverse edges from the anchor, both directions
   *mechanically* but keep only nodes with `CreatedAt > T` — dominated in
   practice by **incoming** `references_doc`/`mentions_commit` edges (later
   documents citing the decision) and `supersedes` chains, `query.walk_depth`
   hops. The time filter is `walkOptions.TimeAfter`, applied in
   `walker.admits`, so an older document is neither returned nor walked through.
3. **Semantic expansion**: retrieval of `question` (default: "consequences,
   follow-ups, incidents related to <anchor title>") + anchor excerpt. The SQL
   filter is inclusive, `created_at >= T`, because timestamps persist at second
   precision, so the service re-drops anything not strictly after `T`
   (`internal/services/impact.go`, `impactService.impactMatches`). Already-found
   nodes drop out at `nodeSet.add`, and the rest become `semantic_match`, which
   catches the postmortem that never linked back.
4. **Return chronologically** (a timeline, not a relevance list); `Chains` =
   anchor → follow-up paths; `Gaps` when nothing exists after `T`
   ("no follow-up evidence after 2025-03-12") — itself a useful answer.

### `history_of(path, repo?, limit?, before?)`

1. `git log --follow` on the path (rename-aware) → commit sequence. Requires a
   registered local clone: `historyService.HistoryOf`
   (`internal/services/history.go`) refuses an empty repo set before it reads
   anything.
2. Attach each commit's PR/ticket/doc layer via `edges`, one hop per commit
   (`historyWalkDepth`).
3. Return a chronological timeline; large histories are windowed
   (`limit` + `before` cursor pagination, `fileHistory.window`), and a `before`
   that matches two commits is a bad request naming both (`fileHistory.cursorAt`).

## SynthesisService (non-AI surfaces only)

Input: `EvidenceBundle` + original question. Behavior:

- Prompt = system instruction + serialized bundle. The instruction is
  `synthesisSystem` (`internal/services/synthesis.go`): answer only from the
  numbered evidence, cite the evidence **number** inline as `[1]`, never write a
  URL or a title in place of a number, report the listed gaps, keep a
  chronological bundle in order.
- Output: markdown prose with inline `[n]` citations, then a source list that
  maps each number to its node URL. An answer that cites nothing, cites a number
  outside the bundle, or wraps a citation in a markdown link is refused rather
  than returned (`synthesisService.Synthesize`, `checkCitations`), so a
  hallucinated citation number surfaces as an error instead of a footnote.
- Provider = the provider instance bound to the `llm:` role (OpenAI,
  Anthropic, Ollama, or any OpenAI-compatible vendor, see
  [08](08-extensibility.md#provider-roles-and-drivers)). A workspace with no
  `llm:` block constructs the service with a nil model, and every call returns
  the precondition error that names the remedy (`NewSynthesisService`,
  `unconfiguredSynthesis`). MCP is unaffected because no file under
  `internal/transport/mcp/` references `SynthesisService`.

## Query-time validation (service layer)

- No query verb checks that the workspace is initialized. Opening the store
  does it once: `ensureMeta` refuses a file whose schema generation or vector
  width is not this build's (`internal/repositories/sqlite/schema.go`,
  `Store.bootstrap`), so a query runs against an index that already opened
  cleanly. Neither the embedder identity nor the chunk format is checked here.
  A sync round checks both before it ingests (`internal/services/sync.go`,
  `reconcileIdentity` and `reconcileChunkFormat`, reached from `runRound`).
  `statusService.EmbedderIdentity` reports both embedder identities without
  judging them. So a workspace whose embedder changed and has not re-synced
  answers queries against a foreign vector space, silently, until the next
  sync refuses. An index split by an older chunk format keeps answering from
  its old chunks until `lore sync --reembed` rebuilds them.
- `find_decision` / `impact_of` / `trace`: no repo required, ever. Those three
  services hold no `CodeRepo` at all (`NewQueryService`, `NewImpactService`,
  `NewTraceService`).
- `why` / `history_of`: at least one repo registered, otherwise the
  `askOnlyRefusal` precondition error. Then the file must be tracked at HEAD
  (`internal/services/coderepo.go`, `requireTrackedFile`, via `HasFileAtHEAD`)
  and `why`'s line span must be sane (`internal/services/why.go`,
  `validateLineSpan`).
- `trace` / `impact_of` ref: resolves to exactly one document (ambiguous SHA
  prefix → error listing candidates, `ambiguousRef`).
- Client-settable limits are capped server-side: `trace`'s `depth` at
  `maxTraceDepth` and `history_of`'s `limit` at `maxHistoryLimit`
  (`internal/services/trace.go`, `traceDepth`;
  `internal/services/history.go`, `historyLimit`), and an anchor excerpt at
  `anchorExcerptChars` (`internal/services/retrieve.go`, `anchorExcerpt`), so
  no MCP client can request unbounded output. `trace`'s own anchor excerpt is
  bounded in `internal/services/trace.go` instead. `maxTraceExcerptRunes`
  (8,000) cuts it. `maxFocusPassages` (3) limits a focused one. A `focus`
  over `maxFocusRunes` (1,000) is rejected rather than cut. `query.walk_depth`
  and `query.top_k` are operator settings rather than request parameters. Each
  query service constructor applies its default only when one is unset or
  non-positive (`NewQueryService`, `NewWhyService` and `NewImpactService`), and
  `walkOptions.hops` defaults again at the walk.
