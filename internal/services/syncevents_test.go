package services

import (
	"context"
	"errors"
	"iter"
	"sync"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/setthasit/Lore/internal/entities"
	"github.com/setthasit/Lore/internal/mocks/lore"
	mock_repositories "github.com/setthasit/Lore/internal/mocks/repositories"
	"github.com/setthasit/Lore/sdk"
)

const (
	busTimeout = 10 * time.Second

	// Long enough that a round never beats, so no HeartbeatLease is expected.
	quietHeartbeat = time.Hour

	busSubscribers = 8
	busEvents      = 10
)

var errEventStore = errors.New("store is on fire")

var eventAt = time.Date(2025, 3, 12, 9, 30, 0, 0, time.UTC)

func publishSequence(t *testing.T, bus *syncEventBus, count int64) {
	t.Helper()

	published := make(chan struct{})
	go func() {
		defer close(published)

		for i := range count {
			bus.publish(entities.SyncEvent{Documents: i})
		}
	}()

	select {
	case <-published:
	case <-time.After(busTimeout):
		t.Fatalf("publishing %d events to a subscriber that never reads did not finish", count)
	}
}

func drainEvents(events <-chan entities.SyncEvent) []entities.SyncEvent {
	drained := make([]entities.SyncEvent, 0, syncEventBuffer)
	for {
		select {
		case event := <-events:
			drained = append(drained, event)
		default:
			return drained
		}
	}
}

func TestSyncEventBusKeepsTheNewestEventsWhenASubscriberStopsReading(t *testing.T) {
	t.Parallel()

	bus := &syncEventBus{}
	events, unsubscribe := bus.subscribe()
	defer unsubscribe()

	const overflow = 10
	publishSequence(t, bus, syncEventBuffer+overflow)

	kept := drainEvents(events)
	if len(kept) != syncEventBuffer {
		t.Fatalf("buffered %d events, want %d", len(kept), syncEventBuffer)
	}
	if kept[0].Documents != overflow {
		t.Errorf("oldest kept event = %d, want %d: the buffer dropped the wrong end",
			kept[0].Documents, overflow)
	}
	if want := int64(syncEventBuffer + overflow - 1); kept[len(kept)-1].Documents != want {
		t.Errorf("newest kept event = %d, want %d", kept[len(kept)-1].Documents, want)
	}
}

func TestSyncEventBusNeverStallsOnASubscriberThatStoppedReading(t *testing.T) {
	t.Parallel()

	bus := &syncEventBus{}
	_, unsubscribe := bus.subscribe()
	defer unsubscribe()

	publishSequence(t, bus, syncEventBuffer*10)
}

func TestSyncEventBusUnsubscribeTwiceClosesTheChannelOnce(t *testing.T) {
	t.Parallel()

	bus := &syncEventBus{}
	events, unsubscribe := bus.subscribe()

	unsubscribe()
	unsubscribe()

	select {
	case _, open := <-events:
		if open {
			t.Error("the channel of an unsubscribed reader still carries events")
		}
	case <-time.After(busTimeout):
		t.Fatal("unsubscribe left the reader's channel open")
	}
}

func TestSyncEventBusPublishesToTheSubscribersThatStayed(t *testing.T) {
	t.Parallel()

	bus := &syncEventBus{}
	_, left := bus.subscribe()
	stayed, unsubscribe := bus.subscribe()
	defer unsubscribe()

	left()
	bus.publish(entities.SyncEvent{Documents: 1})

	if got := drainEvents(stayed); len(got) != 1 {
		t.Fatalf("the remaining subscriber received %d events, want 1", len(got))
	}
}

func TestSyncEventBusServesConcurrentSubscribers(t *testing.T) {
	t.Parallel()

	bus := &syncEventBus{}

	abandoned := t.Context().Done()

	var readers sync.WaitGroup
	for range busSubscribers {
		events, unsubscribe := bus.subscribe()
		t.Cleanup(unsubscribe)

		readers.Add(1)
		go func() {
			defer readers.Done()

			seen := make(map[int64]struct{}, busEvents)
			for len(seen) < busEvents {
				select {
				case event := <-events:
					seen[event.Documents] = struct{}{}
				case <-abandoned:
					return
				}
			}
		}()
	}

	churn := make(chan struct{})
	go func() {
		defer close(churn)

		for range busEvents {
			_, unsubscribe := bus.subscribe()
			unsubscribe()
		}
	}()

	var publishers sync.WaitGroup
	for i := range int64(busEvents) {
		publishers.Add(1)
		go func() {
			defer publishers.Done()

			bus.publish(entities.SyncEvent{Documents: i})
		}()
	}

	publishers.Wait()
	<-churn
	awaitReaders(t, &readers)
}

func awaitReaders(t *testing.T, readers *sync.WaitGroup) {
	t.Helper()

	done := make(chan struct{})
	go func() {
		defer close(done)
		readers.Wait()
	}()

	select {
	case <-done:
	case <-time.After(busTimeout):
		t.Fatal("subscribers did not finish reading")
	}
}

type roundChunker struct{ perDoc map[lore.DocID]int }

func (c roundChunker) Chunk(doc lore.Document) []entities.Chunk {
	chunks := make([]entities.Chunk, c.perDoc[doc.ID])
	for i := range chunks {
		chunks[i] = entities.Chunk{DocID: doc.ID, Ordinal: i, Text: string(doc.ID), Source: doc.Source}
	}

	return chunks
}

func eventDoc(id lore.DocID) lore.Document {
	return lore.Document{ID: id, Source: "github", Type: lore.DocTypePR, Title: string(id)}
}

func newUncontendedRound(t *testing.T, chunks map[lore.DocID]int) (*syncOrchestrator, *mock_repositories.MockIndexStore) {
	t.Helper()

	ctrl := gomock.NewController(t)
	store := mock_repositories.NewMockIndexStore(ctrl)
	emb := mock_lore.NewMockEmbedder(ctrl)

	emb.EXPECT().Embed(gomock.Any(), gomock.Any()).AnyTimes().DoAndReturn(
		func(_ context.Context, texts []string) ([][]float32, error) {
			vectors := make([][]float32, len(texts))
			for i := range vectors {
				vectors[i] = []float32{0.1}
			}

			return vectors, nil
		})

	store.EXPECT().Lease(gomock.Any()).Return(nil, nil).AnyTimes()
	store.EXPECT().TryAcquireLease(gomock.Any(), gomock.Any()).Return(true, nil).AnyTimes()
	store.EXPECT().ReleaseLease(gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	store.EXPECT().Meta(gomock.Any(), metaKeyEmbedderIdentity).Return(heartbeatIdentity.String(), nil).AnyTimes()

	return &syncOrchestrator{
		store:     store,
		chunker:   roundChunker{perDoc: chunks},
		emb:       emb,
		space:     heartbeatIdentity,
		links:     &heartbeatLinks{},
		holder:    heartbeatHolder,
		heartbeat: quietHeartbeat,
		now:       func() time.Time { return eventAt },
	}, store
}

func repeatingConnector(t *testing.T, batches ...lore.Batch) *mock_lore.MockConnector {
	t.Helper()

	conn := mock_lore.NewMockConnector(gomock.NewController(t))
	conn.EXPECT().Name().Return("github").AnyTimes()
	conn.EXPECT().Changes(gomock.Any(), gomock.Any()).AnyTimes().DoAndReturn(
		func(context.Context, lore.Cursor) iter.Seq2[lore.Batch, error] {
			return func(yield func(lore.Batch, error) bool) {
				for _, batch := range batches {
					if !yield(batch, nil) {
						return
					}
				}
			}
		})

	return conn
}

func TestSyncEventsCarryTheCumulativeTotalsOfEachRound(t *testing.T) {
	t.Parallel()

	first, second, third := eventDoc("github:pr:1"), eventDoc("github:pr:2"), eventDoc("github:pr:3")
	round, store := newUncontendedRound(t, map[lore.DocID]int{first.ID: 2, second.ID: 1, third.ID: 3})
	round.connectors = []lore.Connector{repeatingConnector(t,
		lore.Batch{Docs: []lore.Document{first, second}, Cursor: lore.Cursor{"page": "1"}},
		lore.Batch{Docs: []lore.Document{third}, Cursor: lore.Cursor{"page": "2"}},
	)}

	store.EXPECT().Cursor(gomock.Any(), "github").Return(nil, nil).AnyTimes()
	store.EXPECT().UpsertDocuments(gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	store.EXPECT().ReplaceChunks(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	store.EXPECT().SetCursor(gomock.Any(), "github", gomock.Any()).Return(nil).AnyTimes()

	events, unsubscribe := round.Subscribe()
	defer unsubscribe()

	const rounds = 2
	for range rounds {
		if _, err := round.Sync(context.Background(), SyncOptions{}); err != nil {
			t.Fatalf("Sync() = %v, want nil", err)
		}
	}

	perRound := []entities.SyncEvent{
		{Phase: entities.SyncPhaseRoundStarted, At: eventAt},
		{Source: "github", Phase: entities.SyncPhaseBatchStored, Documents: 2, At: eventAt},
		{Source: "github", Phase: entities.SyncPhaseChunksIndexed, Documents: 2, Chunks: 3, At: eventAt},
		{Source: "github", Phase: entities.SyncPhaseBatchStored, Documents: 3, Chunks: 3, At: eventAt},
		{Source: "github", Phase: entities.SyncPhaseChunksIndexed, Documents: 3, Chunks: 6, At: eventAt},
		{Source: "github", Phase: entities.SyncPhaseConnectorFinished, Documents: 3, Chunks: 6, At: eventAt},
		{Phase: entities.SyncPhasePendingLinked, Documents: 3, Chunks: 6, At: eventAt},
		{Phase: entities.SyncPhaseRoundFinished, Documents: 3, Chunks: 6, At: eventAt},
	}
	want := make([]entities.SyncEvent, 0, rounds*len(perRound))
	for range rounds {
		want = append(want, perRound...)
	}

	got := drainEvents(events)
	if len(got) != len(want) {
		t.Fatalf("published %d events, want %d (%+v)", len(got), len(want), got)
	}
	for i, event := range got {
		if event != want[i] {
			t.Errorf("event %d = %+v, want %+v", i, event, want[i])
		}
	}
}

// A round-level failure needs a step the round owns rather than an instance's, and
// the LinkResolver pass is the one the sync round runs for itself.
type failingLinks struct{ err error }

func (failingLinks) Link(context.Context, []lore.Document) error { return nil }

func (l failingLinks) LinkPending(context.Context) error { return l.err }

func TestSyncEventsEndAFailedRoundWithItsError(t *testing.T) {
	t.Parallel()

	round, _ := newUncontendedRound(t, nil)
	round.links = failingLinks{err: errEventStore}

	events, unsubscribe := round.Subscribe()
	defer unsubscribe()

	if _, err := round.Sync(context.Background(), SyncOptions{}); err == nil {
		t.Fatal("Sync() = nil, want the failure of the round's own linking pass")
	}

	got := drainEvents(events)
	if len(got) != 2 {
		t.Fatalf("published %d events, want a started and a failed one (%+v)", len(got), got)
	}
	if got[0].Phase != entities.SyncPhaseRoundStarted {
		t.Errorf("first event = %v, want %v", got[0].Phase, entities.SyncPhaseRoundStarted)
	}
	if got[1].Phase != entities.SyncPhaseFailed {
		t.Errorf("last event = %v, want %v", got[1].Phase, entities.SyncPhaseFailed)
	}
	if !errors.Is(got[1].Err, errEventStore) {
		t.Errorf("failed event error = %v, want it to wrap %v", got[1].Err, errEventStore)
	}
}

func TestSyncEventsNameTheInstanceThatFailedAndStillFinishTheRound(t *testing.T) {
	t.Parallel()

	round, store := newUncontendedRound(t, nil)
	round.connectors = []lore.Connector{repeatingConnector(t)}
	store.EXPECT().Cursor(gomock.Any(), "github").Return(nil, errEventStore)

	events, unsubscribe := round.Subscribe()
	defer unsubscribe()

	res, err := round.Sync(context.Background(), SyncOptions{})
	if err != nil {
		t.Fatalf("Sync() = %v, want the round to survive its only instance failing", err)
	}
	if len(res.Failures) != 1 || res.Failures[0].Instance != "github" {
		t.Fatalf("Sync() failures = %+v, want the github instance alone", res.Failures)
	}

	// No connector_finished: the instance never reached the end of its stream.
	want := []entities.SyncPhase{
		entities.SyncPhaseRoundStarted,
		entities.SyncPhaseFailed,
		entities.SyncPhasePendingLinked,
		entities.SyncPhaseRoundFinished,
	}
	got := drainEvents(events)
	if len(got) != len(want) {
		t.Fatalf("published %d events, want %d (%+v)", len(got), len(want), got)
	}
	for i, phase := range want {
		if got[i].Phase != phase {
			t.Errorf("event %d = %v, want %v", i, got[i].Phase, phase)
		}
	}
	if got[1].Source != "github" {
		t.Errorf("the failed event names source %q, want the failing instance", got[1].Source)
	}
	if !errors.Is(got[1].Err, errEventStore) {
		t.Errorf("the failed event error = %v, want it to wrap %v", got[1].Err, errEventStore)
	}
}
