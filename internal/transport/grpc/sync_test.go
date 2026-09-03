package grpc

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"go.uber.org/mock/gomock"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/timestamppb"

	lorev1 "github.com/setthasit/Lore/api/proto/lore/v1"
	"github.com/setthasit/Lore/internal/entities"
	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/internal/services"
	"github.com/setthasit/Lore/internal/transport"
)

const (
	leaseHolder = "host-9/1234"
	watchQueue  = 4
)

func (f rpcFixture) trigger(t *testing.T, in *lorev1.TriggerRequest) (*lorev1.TriggerResponse, error) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()

	return f.syncs.Trigger(ctx, in)
}

func (f rpcFixture) indexStatus(t *testing.T) *lorev1.StatusResponse {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()

	res, err := f.syncs.Status(ctx, &lorev1.StatusRequest{})
	if err != nil {
		t.Fatalf("Status() = %v, want the index stats", err)
	}

	return res
}

func (f rpcFixture) expectSubscribe() (chan<- entities.SyncEvent, <-chan struct{}) {
	published := make(chan entities.SyncEvent, watchQueue)
	unsubscribed := make(chan struct{}, 1)

	var events <-chan entities.SyncEvent = published
	f.sync.EXPECT().Subscribe().Return(events, func() {
		select {
		case unsubscribed <- struct{}{}:
		default:
		}
	})

	return published, unsubscribed
}

func awaitUnsubscribe(t *testing.T, unsubscribed <-chan struct{}) {
	t.Helper()

	select {
	case <-unsubscribed:
	case <-time.After(rpcTimeout):
		t.Error("Watch returned without unsubscribing from the event bus")
	}
}

func TestTriggerPassesTheRequestedSyncOptions(t *testing.T) {
	f := newRPCFixture(t)
	f.sync.EXPECT().
		Sync(gomock.Any(), services.SyncOptions{Source: "notion", Reembed: true}).
		Return(services.SyncResult{}, nil)

	res, err := f.trigger(t, &lorev1.TriggerRequest{Source: "notion", Reembed: true})
	if err != nil {
		t.Fatalf("Trigger() = %v, want an acknowledgment", err)
	}

	if res.GetSynced() != "notion" {
		t.Errorf("synced = %q, want notion", res.GetSynced())
	}
	if res.GetTookOverFrom() != nil {
		t.Errorf("took_over_from = %v, want it absent", res.GetTookOverFrom())
	}
}

func TestTriggerReportsTheHolderItTookOver(t *testing.T) {
	acquired := rpcCreatedAt.Add(-5 * time.Minute)
	heartbeat := rpcCreatedAt.Add(-90 * time.Second)

	f := newRPCFixture(t)
	f.sync.EXPECT().Sync(gomock.Any(), services.SyncOptions{}).Return(services.SyncResult{
		TookOverFrom: &entities.LeaseState{Holder: leaseHolder, AcquiredAt: acquired, HeartbeatAt: heartbeat},
	}, nil)

	res, err := f.trigger(t, &lorev1.TriggerRequest{})
	if err != nil {
		t.Fatalf("Trigger() = %v, want an acknowledgment", err)
	}

	assertSameProto(t, res.GetTookOverFrom(), &lorev1.LeaseState{
		Holder:      leaseHolder,
		AcquiredAt:  timestamppb.New(acquired),
		HeartbeatAt: timestamppb.New(heartbeat),
	})
}

func TestTriggerSurfacesAHeldLeaseWithItsHolder(t *testing.T) {
	const held = "cannot run a sync round — " + leaseHolder + " (last heartbeat 1m30s ago) is already writing " +
		"this index; retry later, or wait out the 120s lease TTL if that holder crashed"

	f := newRPCFixture(t)
	f.sync.EXPECT().Sync(gomock.Any(), gomock.Any()).
		Return(services.SyncResult{}, internalerror.NewPreconditionError(held, services.ErrSyncLocked))

	_, err := f.trigger(t, &lorev1.TriggerRequest{})

	st := rpcStatus(t, err)
	if st.Code() != codes.FailedPrecondition {
		t.Errorf("code = %v, want %v", st.Code(), codes.FailedPrecondition)
	}
	if st.Message() != held {
		t.Errorf("message = %q, want %q", st.Message(), held)
	}
}

func TestStatusMapsTheIndexStats(t *testing.T) {
	github := rpcCreatedAt.Add(-30 * time.Second)
	notion := rpcCreatedAt.Add(-2 * time.Hour)
	acquired := rpcCreatedAt.Add(-5 * time.Minute)

	f := newRPCFixture(t)
	f.status.EXPECT().Status(gomock.Any()).Return(entities.IndexStats{
		Documents: 412,
		Chunks:    3120,
		Edges:     877,
		Cursors: []entities.CursorAge{
			{Connector: "github", UpdatedAt: github},
			{Connector: "notion", UpdatedAt: notion},
		},
		Lease: &entities.LeaseState{Holder: leaseHolder, AcquiredAt: acquired, HeartbeatAt: rpcCreatedAt},
	}, nil)

	assertSameProto(t, f.indexStatus(t), &lorev1.StatusResponse{
		Documents: 412,
		Chunks:    3120,
		Edges:     877,
		Cursors: []*lorev1.CursorAge{
			{Source: "github", UpdatedAt: timestamppb.New(github)},
			{Source: "notion", UpdatedAt: timestamppb.New(notion)},
		},
		Lease: &lorev1.LeaseState{
			Holder:      leaseHolder,
			AcquiredAt:  timestamppb.New(acquired),
			HeartbeatAt: timestamppb.New(rpcCreatedAt),
		},
	})
}

func TestStatusReportsAFreeLeaseAndANeverSyncedIndex(t *testing.T) {
	f := newRPCFixture(t)
	f.status.EXPECT().Status(gomock.Any()).Return(entities.IndexStats{}, nil)

	res := f.indexStatus(t)

	assertSameProto(t, res, &lorev1.StatusResponse{})
	if res.GetLease() != nil {
		t.Errorf("lease = %v, want it absent", res.GetLease())
	}
}

func TestWatchStreamsEventsAndStopsWhenTheClientLeaves(t *testing.T) {
	f := newRPCFixture(t)
	published, unsubscribed := f.expectSubscribe()

	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()

	stream, err := f.syncs.Watch(ctx, &lorev1.WatchRequest{})
	if err != nil {
		t.Fatalf("Watch() = %v, want a stream", err)
	}

	published <- entities.SyncEvent{
		Source:    "github",
		Phase:     entities.SyncPhaseChunksIndexed,
		Documents: 12,
		Chunks:    47,
		At:        rpcCreatedAt,
	}

	event, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv() = %v, want an event", err)
	}
	assertSameProto(t, event, &lorev1.SyncEvent{
		Source:    "github",
		Phase:     lorev1.SyncPhase_SYNC_PHASE_CHUNKS_INDEXED,
		Documents: 12,
		Chunks:    47,
		At:        timestamppb.New(rpcCreatedAt),
	})

	cancel()

	if _, err := stream.Recv(); codes.Canceled != rpcStatus(t, err).Code() {
		t.Errorf("Recv() after cancel = %v, want a cancelled stream", err)
	}
	awaitUnsubscribe(t, unsubscribed)
}

func TestWatchEndsTheStreamWhenTheServerStops(t *testing.T) {
	f := newRPCFixture(t)
	published, unsubscribed := f.expectSubscribe()

	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()

	stream, err := f.syncs.Watch(ctx, &lorev1.WatchRequest{})
	if err != nil {
		t.Fatalf("Watch() = %v, want a stream", err)
	}

	published <- entities.SyncEvent{Phase: entities.SyncPhaseRoundStarted, At: rpcCreatedAt}
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("Recv() = %v, want an event", err)
	}

	f.stop()

	if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
		t.Errorf("Recv() = %v, want io.EOF from a stream the server ended itself", err)
	}
	awaitUnsubscribe(t, unsubscribed)
}

func TestWatchClassifiesTheErrorOfAFailedRound(t *testing.T) {
	const remediation = "the workspace sync lease is no longer held by this round"

	f := newRPCFixture(t)
	published, _ := f.expectSubscribe()

	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()

	stream, err := f.syncs.Watch(ctx, &lorev1.WatchRequest{})
	if err != nil {
		t.Fatalf("Watch() = %v, want a stream", err)
	}

	for _, failure := range []error{
		internalerror.NewPreconditionError(remediation, nil),
		errors.New(rpcCause),
	} {
		published <- entities.SyncEvent{Phase: entities.SyncPhaseFailed, Err: failure, At: rpcCreatedAt}
	}

	classified, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv() = %v, want the failed event", err)
	}
	if classified.GetError() != remediation {
		t.Errorf("error = %q, want %q", classified.GetError(), remediation)
	}

	hidden, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv() = %v, want the failed event", err)
	}
	if hidden.GetError() != transport.InternalErrorMessage {
		t.Errorf("error = %q, want %q", hidden.GetError(), transport.InternalErrorMessage)
	}
	if strings.Contains(hidden.GetError(), rpcCause) {
		t.Errorf("error %q leaks the cause", hidden.GetError())
	}
	if hidden.GetPhase() != lorev1.SyncPhase_SYNC_PHASE_FAILED {
		t.Errorf("phase = %v, want %v", hidden.GetPhase(), lorev1.SyncPhase_SYNC_PHASE_FAILED)
	}
}
