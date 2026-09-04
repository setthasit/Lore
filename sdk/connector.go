package lore

import (
	"context"
	"iter"
)

// Connector is the contract every source plugin implements. Connectors fetch,
// paginate, retry and normalize; reference resolution is the host's job.
type Connector interface {
	// Name is the instance id the host built this connector for: the sync cursor
	// key, the value of every Document.Source, and the DocID prefix.
	Name() string

	// Changes streams batches of documents modified since cursor, oldest-first.
	// Must be resumable and idempotent.
	Changes(ctx context.Context, cursor Cursor) iter.Seq2[Batch, error]
}
