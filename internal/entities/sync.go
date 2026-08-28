package entities

import (
	"context"
	"iter"
	"time"
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

type IndexStats struct {
	Documents int64
	Chunks    int64

	// One entry per connector that has ever checkpointed, ordered by connector name.
	Cursors []CursorAge

	// Nil means no holder; a lease lapsed past its TTL is still reported.
	Lease *LeaseState
}

// UpdatedAt is when the position was recorded, not a time the connector chose.
type CursorAge struct {
	Connector string
	UpdatedAt time.Time
}

type LeaseState struct {
	Holder      string
	AcquiredAt  time.Time
	HeartbeatAt time.Time
}
