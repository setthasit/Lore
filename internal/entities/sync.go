package entities

import "time"

type IndexStats struct {
	Documents int64
	Chunks    int64
	Edges     int64

	Sources []SourceState
	Clones  []CloneState

	// Nil means no holder; a lease lapsed past its TTL is still reported.
	Lease *LeaseState
}

type SourceState struct {
	ID             string
	Configured     bool
	Documents      int64
	LastCheckpoint time.Time // Zero means never checkpointed.
}

type CloneState struct {
	Name   string
	Synced bool
}

type DeclaredWorkspace struct {
	Sources []string
	Clones  []DeclaredClone
}

type DeclaredClone struct {
	Name   string
	Synced bool
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
