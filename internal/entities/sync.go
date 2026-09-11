package entities

import "time"

type IndexStats struct {
	Documents int64
	Chunks    int64
	Edges     int64

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

// Divergence of the two sides blocks every sync round until a re-embed rebuilds
// the chunk layer.
type EmbedderIdentity struct {
	Configured string
	Indexed    string // empty until a sync records one
}

type SyncPhase string

const (
	SyncPhaseRoundStarted      SyncPhase = "round_started"
	SyncPhaseBatchStored       SyncPhase = "batch_stored"
	SyncPhaseChunksIndexed     SyncPhase = "chunks_indexed"
	SyncPhaseConnectorFinished SyncPhase = "connector_finished"
	SyncPhasePendingLinked     SyncPhase = "pending_linked"
	SyncPhaseRoundFinished     SyncPhase = "round_finished"
	SyncPhaseFailed            SyncPhase = "failed"
)

type SyncEvent struct {
	Source    string
	Phase     SyncPhase
	Documents int64
	Chunks    int64
	Err       error
	At        time.Time
}
