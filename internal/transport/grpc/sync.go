package grpc

import (
	"context"
	"log/slog"

	lorev1 "lore/api/proto/lore/v1"
	"lore/internal/entities"
	"lore/internal/services"
	"lore/internal/transport"
)

type syncServer struct {
	lorev1.UnimplementedSyncServiceServer
	sync     services.SyncOrchestrator
	status   services.StatusService
	stopping <-chan struct{}
	log      *slog.Logger
}

var _ lorev1.SyncServiceServer = (*syncServer)(nil)

func newSyncServer(svc transport.Services, stopping <-chan struct{}, log *slog.Logger) *syncServer {
	return &syncServer{sync: svc.Sync, status: svc.Status, stopping: stopping, log: log}
}

func (s *syncServer) Trigger(ctx context.Context, in *lorev1.TriggerRequest) (*lorev1.TriggerResponse, error) {
	result, err := s.sync.Sync(ctx, services.SyncOptions{Source: in.GetSource(), Reembed: in.GetReembed()})
	if err != nil {
		return nil, rpcError(s.log, "Trigger", err)
	}

	return &lorev1.TriggerResponse{
		Synced:       in.GetSource(),
		TookOverFrom: newLeaseState(result.TookOverFrom),
	}, nil
}

func (s *syncServer) Status(ctx context.Context, _ *lorev1.StatusRequest) (*lorev1.StatusResponse, error) {
	stats, err := s.status.Status(ctx)
	if err != nil {
		return nil, rpcError(s.log, "Status", err)
	}

	cursors := make([]*lorev1.CursorAge, len(stats.Cursors))
	for i, cursor := range stats.Cursors {
		cursors[i] = &lorev1.CursorAge{
			Source:    cursor.Connector,
			UpdatedAt: newTimestamp(cursor.UpdatedAt),
		}
	}

	return &lorev1.StatusResponse{
		Documents: stats.Documents,
		Chunks:    stats.Chunks,
		Edges:     stats.Edges,
		Cursors:   cursors,
		Lease:     newLeaseState(stats.Lease),
	}, nil
}

func (s *syncServer) Watch(_ *lorev1.WatchRequest, stream lorev1.SyncService_WatchServer) error {
	events, unsubscribe := s.sync.Subscribe()
	defer unsubscribe()

	gone := stream.Context().Done()
	for {
		select {
		case <-gone:
			return nil
		case <-s.stopping:
			return nil
		case event := <-events:
			if err := stream.Send(newSyncEvent(event)); err != nil {
				return err
			}
		}
	}
}

var syncPhaseCodes = map[entities.SyncPhase]lorev1.SyncPhase{
	entities.SyncPhaseRoundStarted:      lorev1.SyncPhase_SYNC_PHASE_ROUND_STARTED,
	entities.SyncPhaseBatchStored:       lorev1.SyncPhase_SYNC_PHASE_BATCH_STORED,
	entities.SyncPhaseChunksIndexed:     lorev1.SyncPhase_SYNC_PHASE_CHUNKS_INDEXED,
	entities.SyncPhaseConnectorFinished: lorev1.SyncPhase_SYNC_PHASE_CONNECTOR_FINISHED,
	entities.SyncPhasePendingLinked:     lorev1.SyncPhase_SYNC_PHASE_PENDING_LINKED,
	entities.SyncPhaseRoundFinished:     lorev1.SyncPhase_SYNC_PHASE_ROUND_FINISHED,
	entities.SyncPhaseFailed:            lorev1.SyncPhase_SYNC_PHASE_FAILED,
}

func newSyncEvent(event entities.SyncEvent) *lorev1.SyncEvent {
	out := &lorev1.SyncEvent{
		Source:    event.Source,
		Phase:     syncPhaseCodes[event.Phase],
		Documents: event.Documents,
		Chunks:    event.Chunks,
		At:        newTimestamp(event.At),
	}
	if event.Err != nil {
		_, out.Error = transport.Classify(event.Err)
	}

	return out
}

func newLeaseState(lease *entities.LeaseState) *lorev1.LeaseState {
	if lease == nil {
		return nil
	}

	return &lorev1.LeaseState{
		Holder:      lease.Holder,
		AcquiredAt:  newTimestamp(lease.AcquiredAt),
		HeartbeatAt: newTimestamp(lease.HeartbeatAt),
	}
}
