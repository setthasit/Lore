package mcp

import (
	"context"
	"log/slog"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/setthasit/Lore/internal/entities"
	"github.com/setthasit/Lore/internal/services"
	"github.com/setthasit/Lore/internal/transport"
)

const (
	syncNowName    = "sync_now"
	syncStatusName = "sync_status"
)

const syncNowDescription = `Refresh the index now: pull every configured source's changes into it, or just one source, and answer once the round has finished.

Use this when sync_status shows the source you need is stale, or when the user says the thing you are looking for landed after the last checkpoint. Every other tool here reads the index; this is the only one that writes it, so reach for it to fix a thin or empty result, not as a warm-up.

Rounds are exclusive across every process sharing this workspace. A round that cannot take the lock fails naming the holder and how long ago it last checked in, and writes nothing — report that rather than retrying in a loop. Progress is checkpointed per batch, so a round that dies partway keeps what it committed and the next one resumes there.

Returns what the round covered: synced names the source, or all configured sources, and took_over_from appears only when this round reclaimed the lock from a holder that had stopped checking in. Sources fail independently, so failures lists any instance that gave up at its last checkpoint while the rest of the round committed — a populated list is a partial refresh, not a failed one.`

const syncStatusDescription = `Report the state of the index: how much is stored, how fresh each source is, and whether a sync round is writing right now.

Use this before trusting a thin or empty evidence bundle from find_decision, why, trace, impact_of or history_of — a source that last checkpointed days ago cannot hold what happened yesterday — and use sync_now to refresh it.

Every age here is whole seconds counted at the moment of this call, never a wall-clock timestamp, so staleness is readable without knowing the current time. sources lists one entry per source that has ever checkpointed, each with last_checkpoint_seconds_ago; a source missing from that list has never been synced. sync_lock.held says whether a round is running, and when it is, holder, held_for_seconds and last_heartbeat_seconds_ago describe it: a heartbeat many minutes old means that holder most likely died, and the next round will take the lock over.`

const allSources = "all configured sources"

type syncNowInput struct {
	Source string `json:"source,omitempty" jsonschema:"sync only this source instance, named by the id it has in the workspace configuration; omit it to sync every configured source"`
}

type syncStatusInput struct{}

type syncAcknowledgment struct {
	Synced       string           `json:"synced"`
	TookOverFrom *displacedHolder `json:"took_over_from,omitempty"`

	// Failures is present only when an instance gave up while the round carried
	// on: what the other instances committed is durable, so this is a partial
	// success the caller should report rather than retry blindly.
	Failures []instanceFailure `json:"failures,omitempty"`
}

type instanceFailure struct {
	Instance string `json:"instance"`
	Error    string `json:"error"`
}

type displacedHolder struct {
	Holder                  string `json:"holder"`
	LastHeartbeatSecondsAgo int64  `json:"last_heartbeat_seconds_ago"`
}

type indexStatus struct {
	Documents int64          `json:"documents"`
	Chunks    int64          `json:"chunks"`
	Edges     int64          `json:"edges"`
	Sources   []sourceStatus `json:"sources"`
	SyncLock  syncLock       `json:"sync_lock"`
}

type sourceStatus struct {
	Source                   string `json:"source"`
	LastCheckpointSecondsAgo int64  `json:"last_checkpoint_seconds_ago"`
}

// The ages are pointers: a lock taken this very second still reports both, rather than omitting a zero.
type syncLock struct {
	Held                    bool   `json:"held"`
	Holder                  string `json:"holder,omitempty"`
	HeldForSeconds          *int64 `json:"held_for_seconds,omitempty"`
	LastHeartbeatSecondsAgo *int64 `json:"last_heartbeat_seconds_ago,omitempty"`
}

type syncNowTool struct {
	sync services.SyncOrchestrator
	log  *slog.Logger
}

type syncStatusTool struct {
	status services.StatusService
	log    *slog.Logger
}

func registerSyncNow(server *sdk.Server, sync services.SyncOrchestrator, log *slog.Logger) {
	tool := syncNowTool{sync: sync, log: log}
	sdk.AddTool(server, &sdk.Tool{
		Name:        syncNowName,
		Description: syncNowDescription,
	}, tool.handle)
}

func registerSyncStatus(server *sdk.Server, status services.StatusService, log *slog.Logger) {
	tool := syncStatusTool{status: status, log: log}
	sdk.AddTool(server, &sdk.Tool{
		Name:        syncStatusName,
		Description: syncStatusDescription,
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
	}, tool.handle)
}

func (t syncNowTool) handle(ctx context.Context, _ *sdk.CallToolRequest, in syncNowInput) (*sdk.CallToolResult, syncAcknowledgment, error) {
	result, err := t.sync.Sync(ctx, services.SyncOptions{Source: in.Source})
	if err != nil {
		return nil, syncAcknowledgment{}, toolError(t.log, syncNowName, err)
	}

	return nil, newSyncAcknowledgment(in.Source, result, time.Now()), nil
}

func (t syncStatusTool) handle(ctx context.Context, _ *sdk.CallToolRequest, _ syncStatusInput) (*sdk.CallToolResult, indexStatus, error) {
	stats, err := t.status.Status(ctx)
	if err != nil {
		return nil, indexStatus{}, toolError(t.log, syncStatusName, err)
	}

	return nil, newIndexStatus(stats, time.Now()), nil
}

func newSyncAcknowledgment(source string, result services.SyncResult, now time.Time) syncAcknowledgment {
	ack := syncAcknowledgment{Synced: source}
	if source == "" {
		ack.Synced = allSources
	}
	if result.TookOverFrom != nil {
		ack.TookOverFrom = &displacedHolder{
			Holder:                  result.TookOverFrom.Holder,
			LastHeartbeatSecondsAgo: secondsAgo(now, result.TookOverFrom.HeartbeatAt),
		}
	}
	for _, failure := range result.Failures {
		_, message := transport.Classify(failure.Err)
		ack.Failures = append(ack.Failures, instanceFailure{Instance: failure.Instance, Error: message})
	}

	return ack
}

func newIndexStatus(stats entities.IndexStats, now time.Time) indexStatus {
	sources := make([]sourceStatus, len(stats.Cursors))
	for i, cursor := range stats.Cursors {
		sources[i] = sourceStatus{
			Source:                   cursor.Connector,
			LastCheckpointSecondsAgo: secondsAgo(now, cursor.UpdatedAt),
		}
	}

	return indexStatus{
		Documents: stats.Documents,
		Chunks:    stats.Chunks,
		Edges:     stats.Edges,
		Sources:   sources,
		SyncLock:  newSyncLock(stats.Lease, now),
	}
}

func newSyncLock(lease *entities.LeaseState, now time.Time) syncLock {
	if lease == nil {
		return syncLock{}
	}

	held, heartbeat := secondsAgo(now, lease.AcquiredAt), secondsAgo(now, lease.HeartbeatAt)

	return syncLock{
		Held:                    true,
		Holder:                  lease.Holder,
		HeldForSeconds:          &held,
		LastHeartbeatSecondsAgo: &heartbeat,
	}
}

// A clock the store and this process disagree about reads as "just happened", never as the future.
func secondsAgo(now, then time.Time) int64 {
	return max(0, int64(now.Sub(then).Round(time.Second).Seconds()))
}
