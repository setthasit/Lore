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

// IndexStats is a point-in-time report on the workspace index: what it holds,
// how fresh each connector's checkpoint is, and whether a sync round is
// running. Counts are of what the index currently holds; nothing here is
// derived from the sources.
type IndexStats struct {
	Documents int64
	Chunks    int64

	// Cursors holds one entry per connector that has ever checkpointed, ordered
	// by connector name so a rendered report is stable between runs.
	Cursors []CursorAge

	// Lease is the sync lease's current holder, or nil when no process holds
	// it. A lease whose heartbeat has lapsed past the TTL is still reported: it
	// is the operator's evidence of a crashed round, and only the next acquirer
	// clears it.
	Lease *LeaseState
}

// CursorAge dates a connector's last checkpoint. UpdatedAt is when the position
// was recorded, not a timestamp the connector chose, so "how stale is this
// source" is answerable without interpreting an opaque Cursor.
type CursorAge struct {
	Connector string
	UpdatedAt time.Time
}

// LeaseState is the sync lease as held: who has it, when they took it, and when
// they last proved they were alive.
type LeaseState struct {
	Holder      string
	AcquiredAt  time.Time
	HeartbeatAt time.Time
}
