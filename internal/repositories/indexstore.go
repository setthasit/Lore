package repositories

import (
	"context"
	"errors"

	"lore/internal/entities"
)

// ErrLeaseLost reports that the caller is no longer the sync lease's holder:
// another process took the lease over. Implementations wrap it so a caller can
// tell "your round must stop" apart from "the store could not be reached".
var ErrLeaseLost = errors.New("sync lease lost to another holder")

// Errors are returned raw with context; classifying them is the service layer's
// job.
type IndexStore interface {
	// Timestamps persist at second precision, UTC. Document.Refs are not
	// persisted.
	UpsertDocuments(ctx context.Context, docs []entities.Document) error

	// DocumentsByID returns metadata for the named documents. Ids the index
	// does not hold are silently omitted (an unsynced document is an ordinary
	// gap, not a failure); result order is unspecified. Empty ids yields no
	// metadata and no error.
	DocumentsByID(ctx context.Context, ids []entities.DocID) ([]entities.DocumentMeta, error)

	// Replaces the document's whole chunk set; nil clears it. The parent
	// document must already exist.
	ReplaceChunks(ctx context.Context, docID entities.DocID, chunks []entities.Chunk) error

	// WipeChunks removes every chunk and its derived lexical and vector rows in
	// one transaction: re-embedding rebuilds the chunk layer from the stored
	// documents. Documents, edges, pending refs, cursors and meta are untouched.
	WipeChunks(ctx context.Context) error

	// query is arbitrary user text, never an expression: text with no searchable
	// word returns no hits rather than an error.
	SearchLexical(ctx context.Context, query string, f entities.Filters, k int) ([]entities.ChunkHit, error)

	// embedding must have the store's vector width. Chunks stored without an
	// embedding are unreachable here.
	SearchVector(ctx context.Context, embedding []float32, f entities.Filters, k int) ([]entities.ChunkHit, error)

	// Nil Cursor means never checkpointed — start a full sync.
	Cursor(ctx context.Context, connector string) (entities.Cursor, error)

	SetCursor(ctx context.Context, connector string, c entities.Cursor) error

	// Unset keys read as "", not an error.
	Meta(ctx context.Context, key string) (string, error)

	// Keys naming the index's own identity are refused, not overwritten.
	SetMeta(ctx context.Context, key, value string) error

	// Never blocks. The lease expires 60s after its last heartbeat, after which
	// any caller may take it over; re-acquiring one already held restarts it.
	TryAcquireLease(ctx context.Context, holder string) (bool, error)

	// Fails with an error wrapping ErrLeaseLost when holder is no longer the
	// holder — how a round learns to stop. Any other error is the store failing.
	HeartbeatLease(ctx context.Context, holder string) error

	// No-op when the lease was already taken over or released.
	ReleaseLease(ctx context.Context, holder string) error

	Close() error
}
