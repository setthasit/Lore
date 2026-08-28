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
    Kind  RefKind // url | ticket_key | commit_sha | file_path | pr_number
    Value string  // e.g. "https://notion.so/…", "PROJ-123", "abc123", "internal/auth/auth.go"
}
```

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
`ncruces` package — pure Go, no cgo, clean cross-compilation. The cgo pairing
(mattn/go-sqlite3 + sqlite-vec cgo bindings) remains a drop-in alternative
behind the IndexStore interface; benchmark in M1 decides if it is ever needed.

| Table | Purpose |
|---|---|
| `documents` | Normalized documents (incl. `created_at`, `updated_at`); full body retained for excerpt extraction |
| `chunks` | Embedding-sized slices of document bodies, FK → documents |
| `chunks_fts` | FTS5 virtual table over chunk text (BM25) |
| `chunk_vectors` | sqlite-vec virtual table, rowid-aligned with `chunks` |
| `edges` | Typed edge graph (src, dst, kind, confidence); indexed on both src and dst for direction-aware walks |
| `pending_refs` | RawRefs that did not resolve yet (target not ingested) |
| `cursors` | Per-connector incremental sync position |
| `sync_lock` | Single-row lease: holder, acquired_at, heartbeat_at |
| `meta` | Schema version, embedder identity (provider+model+dims) |

Notes:

- `meta` stores the embedding model identity; changing embedder invalidates
  vectors and forces re-embedding (detected at startup, surfaced as an explicit
  error with a `lore sync --reembed` remedy, never silent).
- Ref-lookup indexes: `documents.url`, ticket keys and SHA prefixes are
  resolvable via indexed columns (`external_key`, `sha_prefix`) populated at
  ingest — `ResolveRef` and the LinkResolver both use them; no table scans.

## Store portability (extending beyond SQLite)

SQLite is the v1 implementation, not an architectural commitment. Storage sits
behind one repository interface, selected by an FX provider; a second backend
(e.g. Postgres + pgvector) is one new package implementing it.

```go
type IndexStore interface {
    // Documents & chunks — batch upserts are atomic internally;
    // no transaction type leaks out of the store.
    UpsertDocuments(ctx context.Context, docs []Document) error
    ReplaceChunks(ctx context.Context, docID DocID, chunks []Chunk) error

    // Retrieval — two independently ranked lists. RRF fusion happens in the
    // service layer, so the contract never assumes SQL-side fusion.
    // Filters: source, repo_ref, doc_type, created_at range.
    SearchLexical(ctx context.Context, query string, f Filters, k int) ([]ChunkHit, error)
    SearchVector(ctx context.Context, embedding []float32, f Filters, k int) ([]ChunkHit, error)

    // Ref resolution — URL, ticket key, SHA (prefix), PR/issue number, DocID.
    // Returns all candidates; disambiguation is service-layer policy.
    ResolveRef(ctx context.Context, ref string) ([]DocumentMeta, error)

    // Graph — direction-aware traversal.
    UpsertEdges(ctx context.Context, edges []Edge) error
    Neighbors(ctx context.Context, ids []DocID, kinds []EdgeKind, dir Direction) ([]Edge, error)

    // Sync bookkeeping
    Cursor(ctx context.Context, connector string) (Cursor, error)
    SetCursor(ctx context.Context, connector string, c Cursor) error
    PendingRefs(ctx context.Context) ([]PendingRef, error)

    // Lease lock — SQLite: lease row; Postgres: advisory lock. Same semantics.
    TryAcquireLease(ctx context.Context, holder string) (bool, error)
    HeartbeatLease(ctx context.Context, holder string) error
    ReleaseLease(ctx context.Context, holder string) error

    Meta(ctx context.Context, key string) (string, error)
    SetMeta(ctx context.Context, key, value string) error
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
- **No leaked SQL types.** Batch methods are atomic internally; callers never
  see transactions, rowids, or virtual-table details.
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
| commit | Whole message = one chunk (subject weighted into title field) |
| pr / issue / ticket / page | Split on markdown headings / paragraph groups, target ~300–500 tokens, small overlap |
| review_comment / issue_comment / ticket_comment | One comment = one chunk; thread id kept in metadata so retrieval can rehydrate the thread |
| *(any other / future type)* | Default: heading/paragraph split, ~300–500 tokens, small overlap |

Every chunk carries: `doc_id`, `source`, `repo_ref`, `doc_type`, `author`,
`created_at`, `updated_at` — all filterable at query time.

## Hybrid retrieval

1. **BM25** over `chunks_fts` (exact identifiers, ticket keys, error strings
   score well lexically).
2. **Vector KNN** over `chunk_vectors` (semantic paraphrase: "why Postgres"
   matches "database selection rationale").
3. **Reciprocal Rank Fusion** in Go merges both rankings:
   `score(d) = Σ 1/(k + rank_i(d))`, k = 60.
4. Optional metadata filters pushed into SQL: `source`, `repo_ref`,
   `doc_type`, `created_at` range — the time filter is what event anchoring
   compiles down to ([05](05-query-engine.md#event-resolution)).

Retrieval returns *chunks*; the query engine immediately lifts them to their
parent documents and hands them to the shared walk/rank machinery — see
[05](05-query-engine.md).
