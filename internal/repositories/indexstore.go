package repositories

import (
	"context"

	"lore/internal/entities"
)

// IndexStore is the workspace index: the derived, rebuildable cache every query
// reads and every sync round writes. SQLite is the current implementation, not
// an architectural commitment, so the contract is deliberately backend-neutral:
//
//   - No leaked SQL types. Batch methods are atomic internally; callers never
//     see transactions, rowids or virtual-table details.
//   - Fusion in Go, not SQL. The two retrieval methods return independently
//     ranked lists; RRF lives in the service layer, so any backend that can rank
//     lexically and by vector distance qualifies.
//   - Ranking convention: ChunkHit.Score is "higher is more relevant" in every
//     retrieval method, whatever the backend's native direction (BM25 and vector
//     distance are both smaller-is-better and get negated). Scores are
//     comparable within one result list only — never across the two methods, and
//     never across backends — which is exactly what rank-based fusion needs.
//     Hits carry no Embedding: callers rank and cite chunks, they do not
//     re-embed them.
//   - The index is derived data. Sources are ground truth; switching backends is
//     a re-sync plus re-embed against a fresh store, never a data migration.
//
// Implementations return raw errors with context; classification into
// internalerror kinds is the service layer's job.
//
// The interface grows additively: ref resolution, edges and pending refs land in
// later change-sets.
type IndexStore interface {
	// UpsertDocuments writes docs idempotently, keyed by Document.ID. The whole
	// batch commits or nothing does. Document.Refs are not persisted here; the
	// link-resolver wave owns pending refs.
	// Timestamps persist at second precision, UTC.
	UpsertDocuments(ctx context.Context, docs []entities.Document) error

	// ReplaceChunks makes chunks the complete chunk set of docID: previous
	// chunks and their derived lexical and vector rows are removed first. A nil
	// or empty slice clears the document's chunks. Chunks whose Embedding is nil
	// are indexed lexically only. The parent document must already exist.
	ReplaceChunks(ctx context.Context, docID entities.DocID, chunks []entities.Chunk) error

	// SearchLexical returns the k best chunks for query ranked by lexical
	// relevance, best first, honouring f. query is arbitrary user text:
	// implementations must accept anything a person can type — quotes,
	// boolean-looking words, pasted punctuation — and search for its words
	// rather than failing. Text with no searchable word matches nothing and
	// returns no hits, not an error.
	SearchLexical(ctx context.Context, query string, f entities.Filters, k int) ([]entities.ChunkHit, error)

	// SearchVector returns the k chunks nearest to embedding, nearest first,
	// honouring f. embedding must have the store's vector width. Chunks indexed
	// without an embedding are unreachable here and lexically retrievable only.
	SearchVector(ctx context.Context, embedding []float32, f entities.Filters, k int) ([]entities.ChunkHit, error)

	// Cursor returns the connector's last checkpointed sync position, or a nil
	// Cursor when it has never checkpointed. A nil Cursor is a full sync's
	// starting point, so an unknown connector is not an error.
	Cursor(ctx context.Context, connector string) (entities.Cursor, error)

	// SetCursor durably records the connector's sync position. It is called
	// after the batch's documents are committed, never before.
	SetCursor(ctx context.Context, connector string, c entities.Cursor) error

	// Meta returns the value stored under key, or "" when the key is unset.
	// The store owns the keys describing the index's own identity (schema
	// generation, vector width); embedder identity is the service layer's.
	Meta(ctx context.Context, key string) (string, error)

	// SetMeta stores value under key. Keys the implementation reserves for the
	// index's identity are refused rather than silently overwritten.
	SetMeta(ctx context.Context, key, value string) error

	// TryAcquireLease takes the workspace's single sync lease for holder and
	// reports whether holder now has it. It never blocks: a scheduler tick that
	// loses to a manual run skips its round.
	//
	// The lease is held while its heartbeat is younger than the 60s TTL. Past
	// that the holder is presumed dead and the next caller takes the lease over,
	// so a crashed sync cannot wedge the scheduler forever. Re-acquiring a lease
	// one already holds succeeds and restarts its clock.
	TryAcquireLease(ctx context.Context, holder string) (bool, error)

	// HeartbeatLease keeps holder's lease alive; sync rounds call it well inside
	// the TTL. It fails if holder is not the current holder, which is how a
	// holder learns its lease was taken over and its round must stop.
	HeartbeatLease(ctx context.Context, holder string) error

	// ReleaseLease frees the lease if holder still holds it. Releasing a lease
	// that was already taken over or released is a no-op, so a deferred release
	// can neither fail nor disturb the next holder.
	ReleaseLease(ctx context.Context, holder string) error

	// Close releases the store's resources. It is safe to call once.
	Close() error
}
