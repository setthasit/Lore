package entities

import (
	"context"
	"iter"
)

// Cursor is an opaque per-connector sync position; only the connector that
// produced it interprets its keys.
type Cursor map[string]string

// Batch is the checkpoint unit of a sync round: Cursor becomes durable once Docs
// are durably committed.
type Batch struct {
	Docs   []Document
	Cursor Cursor
}

// Connector is the contract every source package implements. Connectors fetch,
// paginate, retry and normalize; reference resolution is the LinkResolver's job.
type Connector interface {
	Name() string // "github", "notion", "jira", …

	// Changes streams batches of documents modified since cursor, oldest-first.
	// Must be resumable and idempotent.
	Changes(ctx context.Context, cursor Cursor) iter.Seq2[Batch, error]
}
