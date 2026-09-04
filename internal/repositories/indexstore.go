package repositories

import (
	"context"
	"errors"
	"time"

	"github.com/setthasit/Lore/internal/entities"
	"github.com/setthasit/Lore/sdk"
)

// Another process took the lease over; implementations wrap it so a caller can
// tell it apart from a store that could not be reached.
var ErrLeaseLost = errors.New("sync lease lost to another holder")

const LeaseTTL = 60 * time.Second

// Errors are returned raw with context; classifying them is the service layer's
// job.
type IndexStore interface {
	// Timestamps persist at second precision, UTC. Document.Refs are not
	// persisted.
	UpsertDocuments(ctx context.Context, docs []lore.Document) error

	// Ids the index does not hold are silently omitted; result order is
	// unspecified.
	DocumentsByID(ctx context.Context, ids []lore.DocID) ([]entities.DocumentMeta, error)

	// Full documents including Body. Ids the index does not hold are silently
	// omitted; Refs are never populated.
	DocumentsWithBody(ctx context.Context, ids []lore.DocID) ([]lore.Document, error)

	// Every candidate is returned; picking one is the caller's policy. A ref
	// shape the store does not recognise yields no candidates, not an error.
	ResolveRef(ctx context.Context, ref string) ([]entities.DocumentMeta, error)

	// Replaces the document's whole chunk set; nil clears it. The parent
	// document must already exist.
	ReplaceChunks(ctx context.Context, docID lore.DocID, chunks []entities.Chunk) error

	// Documents, edges, pending refs, cursors and meta survive the wipe.
	WipeChunks(ctx context.Context) error

	// query is arbitrary user text, never an expression: text with no searchable
	// word returns no hits rather than an error.
	SearchLexical(ctx context.Context, query string, f entities.Filters, k int) ([]entities.ChunkHit, error)

	// embedding must have the store's vector width. Chunks stored without an
	// embedding are unreachable here.
	SearchVector(ctx context.Context, embedding []float32, f entities.Filters, k int) ([]entities.ChunkHit, error)

	// A re-upserted edge (src, dst, kind) keeps the highest confidence seen, so the
	// result never depends on the order refs were resolved in.
	UpsertEdges(ctx context.Context, edges []entities.Edge) error

	// Empty kinds means every kind.
	Neighbors(ctx context.Context, ids []lore.DocID, kinds []entities.EdgeKind, dir entities.Direction) ([]entities.Edge, error)

	PendingRefs(ctx context.Context) ([]entities.PendingRef, error)

	UpsertPendingRefs(ctx context.Context, refs []entities.PendingRef) error

	DeletePendingRefs(ctx context.Context, refs []entities.PendingRef) error

	// Nil Cursor means never checkpointed — start a full sync.
	Cursor(ctx context.Context, connector string) (lore.Cursor, error)

	SetCursor(ctx context.Context, connector string, c lore.Cursor) error

	// Unset keys read as "", not an error.
	Meta(ctx context.Context, key string) (string, error)

	// Keys naming the index's own identity are refused, not overwritten.
	SetMeta(ctx context.Context, key, value string) error

	// Never blocks. The lease expires LeaseTTL after its last heartbeat, after
	// which any caller may take it over; re-acquiring one already held restarts it.
	TryAcquireLease(ctx context.Context, holder string) (bool, error)

	// A non-holder fails with an error wrapping ErrLeaseLost.
	HeartbeatLease(ctx context.Context, holder string) error

	// No-op when the lease was already taken over or released.
	ReleaseLease(ctx context.Context, holder string) error

	// Nil means the lease is free; a lease lapsed past LeaseTTL is still reported.
	Lease(ctx context.Context) (*entities.LeaseState, error)

	// An empty index reports zeros and no rows, not an error.
	Stats(ctx context.Context) (entities.IndexStats, error)

	Close() error
}
