package services

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"lore/internal/entities"
	"lore/internal/errors/internalerror"
	mock_embedder "lore/internal/mocks/embedder"
	mock_entities "lore/internal/mocks/entities"
	mock_repositories "lore/internal/mocks/repositories"
	"lore/internal/repositories"
)

const (
	testHeartbeat = 2 * time.Millisecond

	// Large enough that a stalled tick still lands inside the heartbeatGrace budget.
	tolerantHeartbeat = 25 * time.Millisecond

	heartbeatHolder   = "test-host/1"
	heartbeatIdentity = "openai/text-embedding-3-small/1536"
)

var errHeartbeatStore = errors.New("store is on fire")

var errLeaseTakenOver = fmt.Errorf("sqlite: sync lease is not held by %q: %w", "daemon/1", repositories.ErrLeaseLost)

type heartbeatLinks struct{ pending atomic.Int64 }

func (l *heartbeatLinks) Link(context.Context, []entities.Document) error { return nil }

func (l *heartbeatLinks) LinkPending(context.Context) error {
	l.pending.Add(1)

	return nil
}

type heartbeatMocks struct {
	store *mock_repositories.MockIndexStore
	emb   *mock_embedder.MockEmbedder
	links *heartbeatLinks
}

func recordHeartbeats(store *mock_repositories.MockIndexStore, err error) <-chan struct{} {
	beats := make(chan struct{}, 1)
	store.EXPECT().HeartbeatLease(gomock.Any(), gomock.Any()).AnyTimes().DoAndReturn(
		func(context.Context, string) error {
			select {
			case beats <- struct{}{}:
			default:
			}

			return err
		})

	return beats
}

// Fails the first beat the way a busy SQLite writer would, then reports every beat that succeeds.
func recoveringHeartbeats(store *mock_repositories.MockIndexStore) <-chan struct{} {
	var calls atomic.Int64

	beats := make(chan struct{}, 1)
	store.EXPECT().HeartbeatLease(gomock.Any(), gomock.Any()).AnyTimes().DoAndReturn(
		func(context.Context, string) error {
			if calls.Add(1) == 1 {
				return errHeartbeatStore
			}

			select {
			case beats <- struct{}{}:
			default:
			}

			return nil
		})

	return beats
}

func awaitBeats(t *testing.T, beats <-chan struct{}, n int) {
	t.Helper()

	for i := range n {
		select {
		case <-beats:
		case <-time.After(5 * time.Second):
			t.Fatalf("waited for %d heartbeats, got %d", n, i)
		}
	}
}

// A round that takes a free lease and finds its own embedder identity; the caller declares ReleaseLease.
func newHeartbeatRound(t *testing.T, connectors ...entities.Connector) (*syncOrchestrator, heartbeatMocks) {
	t.Helper()

	ctrl := gomock.NewController(t)
	m := heartbeatMocks{
		store: mock_repositories.NewMockIndexStore(ctrl),
		emb:   mock_embedder.NewMockEmbedder(ctrl),
		links: &heartbeatLinks{},
	}

	m.emb.EXPECT().Identity().Return(heartbeatIdentity).AnyTimes()
	m.store.EXPECT().Lease(gomock.Any()).Return(nil, nil)
	m.store.EXPECT().TryAcquireLease(gomock.Any(), gomock.Any()).Return(true, nil)
	m.store.EXPECT().Meta(gomock.Any(), metaKeyEmbedderIdentity).Return(heartbeatIdentity, nil)

	return &syncOrchestrator{
		store:      m.store,
		connectors: connectors,
		emb:        m.emb,
		links:      m.links,
		holder:     heartbeatHolder,
		heartbeat:  testHeartbeat,
	}, m
}

func heartbeatConnector(t *testing.T) *mock_entities.MockConnector {
	t.Helper()

	conn := mock_entities.NewMockConnector(gomock.NewController(t))
	conn.EXPECT().Name().Return("github").AnyTimes()

	return conn
}

func changes(conn *mock_entities.MockConnector, stream func(context.Context, func(entities.Batch, error) bool)) {
	conn.EXPECT().Changes(gomock.Any(), gomock.Any()).DoAndReturn(
		func(ctx context.Context, _ entities.Cursor) iter.Seq2[entities.Batch, error] {
			return func(yield func(entities.Batch, error) bool) { stream(ctx, yield) }
		})
}

func TestHeartbeatLeaseKeepsBeatingWhileTheStoreAgrees(t *testing.T) {
	t.Parallel()

	store := mock_repositories.NewMockIndexStore(gomock.NewController(t))
	beats := recordHeartbeats(store, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- heartbeatLease(ctx, store, heartbeatHolder, testHeartbeat) }()

	awaitBeats(t, beats, 3)
	cancel()

	if err := <-done; err != nil {
		t.Fatalf("heartbeatLease() = %v, want nil after three beats and a cancel", err)
	}
}

func TestHeartbeatLeaseStopsAtOnceWhenTheLeaseIsTakenOver(t *testing.T) {
	t.Parallel()

	store := mock_repositories.NewMockIndexStore(gomock.NewController(t))
	// Exactly one call is declared: a lost lease must not be given a second chance.
	store.EXPECT().HeartbeatLease(gomock.Any(), heartbeatHolder).Return(errLeaseTakenOver)

	err := heartbeatLease(context.Background(), store, heartbeatHolder, testHeartbeat)
	if got := internalerror.KindOf(err); got != internalerror.KindPrecondition {
		t.Fatalf("heartbeatLease() error kind = %v, want %v (%v)", got, internalerror.KindPrecondition, err)
	}
	if !errors.Is(err, repositories.ErrLeaseLost) {
		t.Errorf("heartbeatLease() = %v, want it to wrap ErrLeaseLost", err)
	}
}

func TestHeartbeatLeaseGivesUpOnAStoreThatStaysUnreachable(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64

	store := mock_repositories.NewMockIndexStore(gomock.NewController(t))
	store.EXPECT().HeartbeatLease(gomock.Any(), heartbeatHolder).AnyTimes().DoAndReturn(
		func(context.Context, string) error {
			calls.Add(1)

			return errHeartbeatStore
		})

	err := heartbeatLease(context.Background(), store, heartbeatHolder, tolerantHeartbeat)
	if got := internalerror.KindOf(err); got != internalerror.KindInternal {
		t.Fatalf("heartbeatLease() error kind = %v, want %v (%v)", got, internalerror.KindInternal, err)
	}
	if !errors.Is(err, errHeartbeatStore) {
		t.Errorf("heartbeatLease() = %v, want it to carry the transient store error", err)
	}
	if got := calls.Load(); got < heartbeatGrace {
		t.Errorf("gave up after %d failed beats, want at least the %d-tick grace", got, heartbeatGrace)
	}
}

func TestHeartbeatLeaseStopsWithoutAnErrorWhenTheRoundEnds(t *testing.T) {
	t.Parallel()

	store := mock_repositories.NewMockIndexStore(gomock.NewController(t))
	store.EXPECT().HeartbeatLease(gomock.Any(), gomock.Any()).AnyTimes().Return(errHeartbeatStore)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := heartbeatLease(ctx, store, heartbeatHolder, time.Hour); err != nil {
		t.Fatalf("heartbeatLease(cancelled) = %v, want nil", err)
	}
}

func TestHeartbeatLeaseIgnoresTheStoreCallTheRoundCancelled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	inFlight := make(chan struct{})
	store := mock_repositories.NewMockIndexStore(gomock.NewController(t))
	store.EXPECT().HeartbeatLease(gomock.Any(), heartbeatHolder).AnyTimes().DoAndReturn(
		func(beat context.Context, _ string) error {
			close(inFlight)
			<-beat.Done()

			return fmt.Errorf("sqlite: heartbeat sync lease for %q: %w", heartbeatHolder, beat.Err())
		})

	done := make(chan error, 1)
	go func() { done <- heartbeatLease(ctx, store, heartbeatHolder, testHeartbeat) }()

	<-inFlight
	// Holds the call past the grace budget, so a context error would abort the round if it counted as a failure.
	time.Sleep(2 * heartbeatGrace * testHeartbeat)
	cancel()

	if err := <-done; err != nil {
		t.Fatalf("heartbeatLease() = %v, want nil for the call the round's own cancel killed", err)
	}
}

func TestSyncFailsWithTheHeartbeatErrorNotTheCancellationItCaused(t *testing.T) {
	t.Parallel()

	conn := heartbeatConnector(t)
	round, m := newHeartbeatRound(t, conn)
	recordHeartbeats(m.store, errLeaseTakenOver)
	m.store.EXPECT().ReleaseLease(gomock.Any(), gomock.Any()).Return(nil)
	m.store.EXPECT().Cursor(gomock.Any(), "github").Return(nil, nil)

	// No writes are declared: the strict controller fails any the round attempts after losing the lease.
	changes(conn, func(ctx context.Context, yield func(entities.Batch, error) bool) {
		<-ctx.Done()
		yield(entities.Batch{}, ctx.Err())
	})

	_, err := round.Sync(context.Background(), SyncOptions{})

	if got := internalerror.KindOf(err); got != internalerror.KindPrecondition {
		t.Fatalf("Sync() error kind = %v, want %v (%v)", got, internalerror.KindPrecondition, err)
	}
	if errors.Is(err, context.Canceled) {
		t.Errorf("Sync() = %v, want the heartbeat failure, not the cancellation it caused", err)
	}
	if !errors.Is(err, repositories.ErrLeaseLost) {
		t.Errorf("Sync() = %v, want it to wrap ErrLeaseLost", err)
	}
	if m.links.pending.Load() != 0 {
		t.Error("the round finished its work after losing the lease")
	}
}

func TestSyncRidesOutATransientHeartbeatFailure(t *testing.T) {
	t.Parallel()

	conn := heartbeatConnector(t)
	round, m := newHeartbeatRound(t, conn)
	round.heartbeat = tolerantHeartbeat

	beats := recoveringHeartbeats(m.store)
	cursor := entities.Cursor{"page": "1"}

	m.store.EXPECT().ReleaseLease(gomock.Any(), gomock.Any()).Return(nil)
	m.store.EXPECT().Cursor(gomock.Any(), "github").Return(nil, nil)
	m.store.EXPECT().UpsertDocuments(gomock.Any(), nil).Return(nil)
	m.store.EXPECT().SetCursor(gomock.Any(), "github", cursor).Return(nil)

	changes(conn, func(_ context.Context, yield func(entities.Batch, error) bool) {
		awaitBeats(t, beats, 1)
		yield(entities.Batch{Cursor: cursor}, nil)
	})

	if _, err := round.Sync(context.Background(), SyncOptions{}); err != nil {
		t.Fatalf("Sync() = %v, want a round that outlives one unreachable-store beat", err)
	}
	if m.links.pending.Load() != 1 {
		t.Errorf("LinkPending called %d times, want 1", m.links.pending.Load())
	}
}

func TestSyncJoinsItsHeartbeatBeforeReleasingTheLease(t *testing.T) {
	t.Parallel()

	var (
		beating  atomic.Bool
		first    sync.Once
		inFlight = make(chan struct{})
		release  = make(chan struct{})
	)

	conn := heartbeatConnector(t)
	round, m := newHeartbeatRound(t, conn)
	cursor := entities.Cursor{"page": "1"}

	m.store.EXPECT().HeartbeatLease(gomock.Any(), heartbeatHolder).AnyTimes().DoAndReturn(
		func(context.Context, string) error {
			beating.Store(true)
			defer beating.Store(false)

			first.Do(func() { close(inFlight) })
			<-release

			return nil
		})
	m.store.EXPECT().ReleaseLease(gomock.Any(), gomock.Any()).DoAndReturn(
		func(context.Context, string) error {
			if beating.Load() {
				t.Error("the lease was released while a heartbeat was still in flight")
			}

			return nil
		})
	m.store.EXPECT().Cursor(gomock.Any(), "github").Return(nil, nil)
	m.store.EXPECT().UpsertDocuments(gomock.Any(), nil).Return(nil)
	m.store.EXPECT().SetCursor(gomock.Any(), "github", cursor).Return(nil)

	changes(conn, func(_ context.Context, yield func(entities.Batch, error) bool) {
		<-inFlight
		yield(entities.Batch{Cursor: cursor}, nil)
	})

	type outcome struct {
		result SyncResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := round.Sync(context.Background(), SyncOptions{})
		done <- outcome{result: result, err: err}
	}()

	<-inFlight
	// A round that never joined its heartbeat has long since released the lease by now.
	time.Sleep(5 * testHeartbeat)
	close(release)

	got := <-done
	if got.err != nil {
		t.Fatalf("Sync() = %v, want nil", got.err)
	}
	if got.result.TookOverFrom != nil {
		t.Errorf("Sync() took over from %v, want nil on a free lease", got.result.TookOverFrom)
	}
	if m.links.pending.Load() != 1 {
		t.Errorf("LinkPending called %d times, want 1", m.links.pending.Load())
	}
}
