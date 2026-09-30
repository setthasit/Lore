# 03 — Data Model & Storage

## Core entities

Everything any connector produces normalizes to one shape:

```go
type Document struct {
    ID        DocID     // "<source>:<type>:<external_id>", globally unique
    Source    string    // "github", "notion", "jira", later "gitlab", "slack", …
    Type      DocType   // see below
    RepoRef   string    // optional: "github:owner/repo" — empty for non-repo docs
    Title     string
    Body      string    // normalized plain text / markdown
    Author    string
    URL       string    // canonical web URL — the citation target
    CreatedAt time.Time // when the thing happened (event time)
    UpdatedAt time.Time // last edit (freshness / sync watermark)
    Refs      []RawRef  // unresolved references found in the body (see below)
}

type DocType string
// commit, pr, pr_review, review_comment, issue, issue_comment,
// page, ticket, ticket_comment
// (open set — a new connector may introduce a type; unknown types get the
//  default chunking strategy and rank as ordinary evidence)

type RawRef struct {
    Kind     RefKind // url | ticket_key | commit_sha | file_path | pr_number
    Value    string  // e.g. "https://notion.so/…", "PROJ-123", "abc123", "internal/auth/auth.go"
    Instance string  // optional: the source instance the target lives in.
                     // Empty resolves against every instance
}
```

How `Instance` narrows a match, and what an empty one resolves against, is the
LinkResolver's rule ([04](04-connectors-and-sync.md#link-resolver),
`internal/services/linkresolver.go`, `outOfScope`).

`CreatedAt` vs `UpdatedAt` matters: a postmortem edited yesterday still
*belongs to* last year's incident. Event resolution, impact filtering, and
timeline ordering key on `CreatedAt`; sync watermarks and freshness use
`UpdatedAt`. Sources that lack a true creation time (rare) set
`CreatedAt = UpdatedAt` and the connector documents it.

Cross-source relationships are typed edges produced by the LinkResolver
([04](04-connectors-and-sync.md#link-resolver)):

```go
type Edge struct {
    Src        DocID
    Dst        DocID
    Kind       EdgeKind // commit_in_pr | pr_closes_issue | references_doc |
                        // mentions_commit | mentions_path | supersedes
    Confidence float32  // 1.0 explicit API link … 0.5 fuzzy text match
}
```

Edges are directional: `Src` is the document containing/declaring the
reference, `Dst` the referenced document. Traversal direction is a query-time
parameter (see `Neighbors` below) — *incoming* edges to a decision document,
filtered by time, are how `impact_of` finds consequences without any special
"impact" edge kind.

`RepoRef` is a metadata dimension, not a partition: one workspace index holds
many repos *and repo-less documents side by side*, so cross-repo and
cross-source questions ("which repos were affected by decision X") work for
free.

## Storage: one SQLite file per workspace

`~/.lore/<workspace>.db` (path configurable). Chosen for: zero external infra,
single-file portability, offline queries after sync, trivial integration tests.

Driver: **ncruces/go-sqlite3 (WASM build)** with sqlite-vec embedded via the
official [sqlite-vec-go-bindings](https://github.com/asg017/sqlite-vec-go-bindings/)
`ncruces` package — pure Go, no cgo, clean cross-compilation. No cgo driver
ships, and `make build.matrix` proves it by building every shipped
platform with `CGO_ENABLED=0` (`Makefile`, `PLATFORMS`). Store benchmarks live at
`internal/repositories/sqlite/bench_test.go`, and a second implementation
would be one package behind the `IndexStore` interface.

| Table | Purpose |
|---|---|
| `documents` | Normalized documents (incl. `created_at`, `updated_at`); the full body is retained and read back by `DocumentsWithBody` (`internal/repositories/sqlite/documents.go`), which `documentBody` (`internal/services/ref.go`) calls for `trace`'s whole body and `impact_of`'s anchor excerpt |
| `chunks` | Embedding-sized slices of document bodies, FK → documents |
| `chunks_fts` | FTS5 virtual table over chunk text (BM25) |
| `chunk_vectors` | sqlite-vec virtual table, rowid-aligned with `chunks`. The chunk's `id` is an explicit rowid alias, and both derived rows are written with it (`internal/repositories/sqlite/documents.go`, `ReplaceChunks`) |
| `edges` | Typed edge graph (src, dst, kind, confidence). `PRIMARY KEY (src, dst, kind)` covers the outward direction and `edges_dst_idx` the inward one, so a direction-aware walk is indexed either way (`internal/repositories/sqlite/schema.go`, `schemaDDL`) |
| `pending_refs` | RawRefs that did not resolve yet (target not ingested), keyed by source document, kind, value and instance scope |
| `cursors` | Per-connector incremental sync position |
| `sync_lock` | Single-row lease: holder, acquired_at, heartbeat_at |
| `meta` | Schema version, vector width, embedder identity (provider+model+dims), chunk format. The store owns the first two and refuses to overwrite them through `SetMeta` (`internal/repositories/sqlite/sync.go`, `Store.SetMeta`) |

Notes:

- `meta` stores the embedding model identity; changing embedder invalidates
  vectors and forces re-embedding. The next sync round detects it and refuses
  with a `lore sync --reembed` remedy
  (`internal/services/sync.go`, `reconcileIdentity`). Nothing detects it at
  startup or at query time, so queries before that round answer against the
  old vectors.
- `meta` also stores `chunk_format`, the version of the text the chunker
  writes (`internal/services/chunker.go`, `chunkFormat`, now `2`). It is a
  second compatibility rule beside the embedder identity. A sync round adopts
  it only when none is recorded and the index holds no chunks. It refuses any
  other recorded format, or none recorded on an index that holds chunks
  (`internal/services/sync.go`, `reconcileChunkFormat`). The remedy and what
  `--reembed` records are in [04](04-connectors-and-sync.md#sync-round).
  Queries keep answering from an old-format index.
- Ref-lookup indexes: `documents.url`, ticket keys and SHA prefixes are
  resolvable via indexed columns (`external_key`, `sha_prefix`) populated at
  ingest — `ResolveRef` and the LinkResolver both use them; no table scans.
  One ref shape is the exception: a bare PR or issue number names no
  repository, so it compiles to `external_key LIKE '%/pull/' || ?` over the two
  document types and scans
  (`internal/repositories/sqlite/resolve.go`, `numberRefClause`). A number
  qualified with its slug takes the indexed `IN` branch instead.
- Keeping a ref's instance scope widened the `pending_refs` key, and the
  recorded index generation moved with it
  (`internal/repositories/sqlite/schema.go`, `schemaVersion`, now `4`). Opening
  an index recorded under a different generation refuses, and the error names
  the remedy: delete the index file and re-sync. Nothing is migrated
  (`internal/repositories/sqlite/schema.go`, `ensureMeta`).

## Store portability (extending beyond SQLite)

SQLite is the v1 implementation, not an architectural commitment. Storage sits
behind one repository interface, selected by an FX provider; a second backend
(e.g. Postgres + pgvector) is one new package implementing it.

```go
// internal/repositories/indexstore.go, with package qualifiers elided:
// Document, DocID and Cursor come from sdk, the rest from internal/entities.
// Errors come back raw with context, and classifying them is the service
// layer's job.
type IndexStore interface {
    // Documents & chunks: one call is one transaction,
    // no transaction type leaks out of the store.
    UpsertDocuments(ctx context.Context, docs []Document) error
    DocumentsByID(ctx context.Context, ids []DocID) ([]DocumentMeta, error)
    DocumentsWithBody(ctx context.Context, ids []DocID) ([]Document, error)
    ReplaceChunks(ctx context.Context, docID DocID, chunks []Chunk) error
    WipeChunks(ctx context.Context) error // clears the chunk layer for --reembed

    // Retrieval — two independently ranked lists. RRF fusion happens in the
    // service layer, so the contract never assumes SQL-side fusion.
    // Filters: source, repo_ref, doc_type, created_at range.
    SearchLexical(ctx context.Context, query string, f Filters, k int) ([]ChunkHit, error)
    SearchVector(ctx context.Context, embedding []float32, f Filters, k int) ([]ChunkHit, error)

    // Ref resolution — URL, ticket key, SHA (prefix), PR/issue number, DocID.
    // Returns all candidates; disambiguation is service-layer policy.
    ResolveRef(ctx context.Context, ref string) ([]DocumentMeta, error)

    // Graph — direction-aware traversal. A re-upserted edge keeps the highest
    // confidence seen, so the graph never depends on resolution order.
    UpsertEdges(ctx context.Context, edges []Edge) error
    Neighbors(ctx context.Context, ids []DocID, kinds []EdgeKind, dir Direction) ([]Edge, error)

    // Sync bookkeeping. Cursors are keyed by instance id.
    PendingRefs(ctx context.Context) ([]PendingRef, error)
    UpsertPendingRefs(ctx context.Context, refs []PendingRef) error
    DeletePendingRefs(ctx context.Context, refs []PendingRef) error
    Cursor(ctx context.Context, instance string) (Cursor, error)
    SetCursor(ctx context.Context, instance string, c Cursor) error

    // Lease lock: one row, CHECK (id = 1) in schemaDDL, taken by the single
    // conditional statement acquireLeaseSQL (sqlite/sync.go).
    TryAcquireLease(ctx context.Context, holder string) (bool, error)
    HeartbeatLease(ctx context.Context, holder string) error
    ReleaseLease(ctx context.Context, holder string) error
    Lease(ctx context.Context) (*LeaseState, error) // nil means free

    Meta(ctx context.Context, key string) (string, error)
    SetMeta(ctx context.Context, key, value string) error

    Stats(ctx context.Context) (IndexStats, error)
    Close() error
}

type Direction int // DirOut (src→dst), DirIn (dst→src), DirBoth
```

Portability rules baked into the design:

- **Fusion in Go, not SQL.** The store returns ranked lists; RRF lives in the
  service layer. Any backend that can rank lexically and by vector distance
  qualifies (FTS5/sqlite-vec, tsvector/pgvector, Elastic, …).
- **The index is derived data.** Sources are ground truth; the index is a
  rebuildable cache. Switching backends = re-sync + re-embed against a fresh
  store — no data-migration tooling, ever.
- **No leaked SQL types.** Callers never see transactions, rowids, or
  virtual-table details. Atomicity holds per method, not per batch:
  `UpsertDocuments`, `ReplaceChunks` and `UpsertEdges` each run in one
  transaction (`internal/repositories/sqlite/documents.go` and `edges.go`), and
  a sync batch spans several such calls. See
  [04](04-connectors-and-sync.md#sync-round) for what a crash mid-batch leaves
  behind.
- Config gains a `store:` key (`sqlite` default) when a second backend lands —
  not before (YAGNI).

Honest cost of a new backend: schema + the interface implementation +
integration tests + vector-dimension handling. Bounded to one package, but not
free.

## Chunking

Decision-trail documents are short and structured — the strategy is
type-aware, with a defined default for types the table does not name:

| DocType | Strategy |
|---|---|
| commit | Whole message = one chunk |
| pr / issue / ticket / page | Split on markdown headings / paragraph groups, target ~300–500 tokens, small overlap (`minChunkTokens`, `maxChunkTokens`, `overlapTokens`). Each chunk begins with the heading lines of its enclosing sections |
| review_comment / issue_comment / ticket_comment | One comment = one chunk, carrying the thread id its `DocID` prefix names (`threadID`) |
| *(any other / future type)* | Default: heading/paragraph split, ~300–500 tokens, small overlap. Each chunk begins with the heading lines of its enclosing sections |

Strategy selection is the type switch in `internal/services/chunker.go`,
`chunker.Chunk`, and an unnamed type falls through to the default branch.

A split chunk's stored text is its heading path, a blank line, the overlap
from the previous chunk, and then its own content (`splitBody`). The path
lists the enclosing section headings outermost first, each as the line
written in the body (`## Rollout`). A chunk outside every section has no path.
The first chunk has no overlap. A heading the chunk itself starts with is
not repeated in its path. Only ATX headings count: one to six `#` followed by
a space or tab (`headingLevel`). A `#` line inside a CommonMark code fence is
code, not a heading (`nextFence`). The path does not count toward
`maxChunkTokens`. It is not carried into the next chunk's overlap. Commit
and comment chunks carry no path.

A chunk is body text only. There is no title field to weight a commit subject
into: `entities.Chunk` carries no title, and `chunks_fts` is declared
`fts5(text)` over that one column
(`internal/repositories/sqlite/schema.go`, `schemaDDL`). The document's title
lives in `documents.title`, which no search reads, so a decision whose title
names the thing and whose body does not is unreachable lexically.

Every chunk carries `doc_id`, `source`, `repo_ref`, `doc_type`, `author`,
`created_at`, `updated_at` and `thread_id` (`chunkOf`). Four of those are
filterable: `entities.Filters` exposes `source`, `repo_ref`, `doc_type` and a
`created_at` range, and nothing else reaches SQL
(`internal/repositories/sqlite/search.go`, `filterClause`). The chunk's
`author`, `updated_at` and `thread_id` come back on a `ChunkHit` and no query
path reads them: a bundle's author and timestamps come from `documents` through
`DocumentMeta`, and nothing rehydrates a comment thread from `thread_id` today.

## Hybrid retrieval

1. **BM25** over `chunks_fts` (exact identifiers, ticket keys, error strings
   score well lexically).
2. **Vector KNN** over `chunk_vectors` (semantic paraphrase: "why Postgres"
   matches "database selection rationale").
3. **Reciprocal Rank Fusion** in Go merges both rankings:
   `score(d) = Σ 1/(k + rank_i(d))`, k = 60 (`internal/services/rrf.go`,
   `rrfK`, `fuse`).
4. Optional metadata filters pushed into SQL: `source`, `repo_ref`,
   `doc_type`, `created_at` range — the time filter is what event anchoring
   compiles down to ([05](05-query-engine.md#event-resolution)). The lexical
   query filters in the outer statement and the vector query as a rowid
   candidate set, because a filter outside a KNN would return fewer than `k`
   hits (`internal/repositories/sqlite/search.go`, `searchVectorSQL`).

Retrieval returns *chunks*; the query engine immediately lifts them to their
parent documents and hands them to the shared walk/rank machinery — see
[05](05-query-engine.md).
