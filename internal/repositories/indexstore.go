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
//   - Fusion in Go, not SQL. Retrieval methods (added with the search wave)
//     return independently ranked lists; RRF lives in the service layer, so any
//     backend that can rank lexically and by vector distance qualifies.
//   - The index is derived data. Sources are ground truth; switching backends is
//     a re-sync plus re-embed against a fresh store, never a data migration.
//
// Implementations return raw errors with context; classification into
// internalerror kinds is the service layer's job.
//
// The interface grows additively: retrieval, ref resolution, edges, sync
// bookkeeping and the lease lock land in later change-sets.
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

	// Close releases the store's resources. It is safe to call once.
	Close() error
}
