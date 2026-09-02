package services

import (
	"context"
	"errors"
	"iter"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"lore/internal/connectors/embedder"
	"lore/internal/entities"
	"lore/internal/errors/internalerror"
	"lore/internal/repositories"
	"lore/internal/repositories/sqlite"
)

const (
	leaseDims      = 3
	leaseSlug      = "acme/lore"
	leaseConnector = "github"

	leaseWinnerHolder = "host-a/1001"
	leaseLoserHolder  = "host-b/2002"
	leaseDeadHolder   = "host-c/3003"

	// Bounds the fixture's channel waits so a stalled round fails by name, not by test-binary timeout.
	leaseStall = 5 * time.Second
)

// Anchored to now, so only the injected clock can age a lease past the TTL:
// a service-level check that slipped back to the wall clock would read zero.
var leaseEpoch = time.Now().UTC().Truncate(time.Second)

var (
	leaseWinnerDoc = leaseDoc("winner", "The round that holds the lease")
	leaseLoserDoc  = leaseDoc("loser", "The round that was turned away")
	leaseTakerDoc  = leaseDoc("taker", "The round that inherited a dead lease")
)

// A round shares its clock with its own heartbeat goroutine, so time moves atomically.
type leaseClock struct{ nanos atomic.Int64 }

func newLeaseClock() *leaseClock {
	var c leaseClock
	c.nanos.Store(leaseEpoch.UnixNano())

	return &c
}

func (c *leaseClock) now() time.Time { return time.Unix(0, c.nanos.Load()).UTC() }

func (c *leaseClock) advance(d time.Duration) { c.nanos.Add(int64(d)) }

func leaseStore(t *testing.T) (*sqlite.Store, *leaseClock) {
	t.Helper()

	clock := newLeaseClock()

	s, err := sqlite.Open(filepath.Join(t.TempDir(), "workspace.db"), leaseDims, sqlite.WithClock(clock.now))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	return s, clock
}

func leaseDoc(slug, title string) entities.Document {
	return entities.Document{
		ID:      entities.NewDocID("github", entities.DocTypeCommit, leaseSlug+"/commit/"+slug),
		Source:  "github",
		Type:    entities.DocTypeCommit,
		RepoRef: "github:" + leaseSlug,
		Title:   title,
		Body:    title + ", and left this body behind for the index to chunk.",
		Author:  "dana",
		URL:     "https://github.com/" + leaseSlug + "/commit/" + slug,
	}
}

type leaseEmbedder struct{}

var _ embedder.Embedder = leaseEmbedder{}

func (leaseEmbedder) Identity() string { return embedder.FormatIdentity("fake", "lease", leaseDims) }

func (leaseEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	vectors := make([][]float32, len(texts))
	for i := range vectors {
		vectors[i] = make([]float32, leaseDims)
		vectors[i][0] = 1
	}

	return vectors, nil
}

// held, when set, is closed once the batch is durable and the round then parks
// on resume, keeping its lease alive for a contender to run against.
type leaseSource struct {
	doc    entities.Document
	held   chan struct{}
	resume chan struct{}
}

func (s *leaseSource) Name() string { return leaseConnector }

func (s *leaseSource) Changes(ctx context.Context, _ entities.Cursor) iter.Seq2[entities.Batch, error] {
	return func(yield func(entities.Batch, error) bool) {
		batch := entities.Batch{
			Docs:   []entities.Document{s.doc},
			Cursor: entities.Cursor{"since": string(s.doc.ID)},
		}
		if !yield(batch, nil) || s.held == nil {
			return
		}

		close(s.held)
		select {
		case <-s.resume:
		case <-ctx.Done():
		}
	}
}

func leaseRound(store repositories.IndexStore, clock *leaseClock, holder string, source *leaseSource) *syncOrchestrator {
	return &syncOrchestrator{
		store:      store,
		connectors: []entities.Connector{source},
		chunker:    NewChunker(),
		emb:        leaseEmbedder{},
		links:      NewLinkResolver(store, nil),
		holder:     holder,
		heartbeat:  heartbeatInterval,
		now:        clock.now,
	}
}

func leaseIndexed(t *testing.T, store *sqlite.Store, ids ...entities.DocID) []entities.DocID {
	t.Helper()

	docs, err := store.DocumentsByID(context.Background(), ids)
	if err != nil {
		t.Fatalf("DocumentsByID: %v", err)
	}

	got := make([]entities.DocID, len(docs))
	for i, doc := range docs {
		got[i] = doc.ID
	}
	slices.Sort(got)

	return got
}

func raceOneRoundAgainstALiveLease(t *testing.T) (*sqlite.Store, error) {
	t.Helper()

	store, clock := leaseStore(t)
	source := &leaseSource{doc: leaseWinnerDoc, held: make(chan struct{}), resume: make(chan struct{})}

	round := make(chan error, 1)
	go func() {
		_, err := leaseRound(store, clock, leaseWinnerHolder, source).Sync(context.Background(), SyncOptions{})
		round <- err
	}()

	select {
	case <-source.held:
	case err := <-round:
		t.Fatalf("the winning round ended before it committed a batch: %v", err)
	case <-time.After(leaseStall):
		t.Fatalf("the winning round did not commit a batch within %s", leaseStall)
	}

	_, refused := leaseRound(store, clock, leaseLoserHolder, &leaseSource{doc: leaseLoserDoc}).
		Sync(context.Background(), SyncOptions{})

	close(source.resume)
	select {
	case err := <-round:
		if err != nil {
			t.Fatalf("the winning round: %v", err)
		}
	case <-time.After(leaseStall):
		t.Fatalf("the winning round did not finish within %s of being resumed", leaseStall)
	}

	return store, refused
}

func TestSyncLandsOnlyTheWinningRoundsDocuments(t *testing.T) {
	store, refused := raceOneRoundAgainstALiveLease(t)

	if got := internalerror.KindOf(refused); got != internalerror.KindPrecondition {
		t.Fatalf("the refused round's error kind = %v, want %v (%v)", got, internalerror.KindPrecondition, refused)
	}
	if !errors.Is(refused, ErrSyncLocked) {
		t.Errorf("the refused round = %v, want it to wrap ErrSyncLocked", refused)
	}

	want := []entities.DocID{leaseWinnerDoc.ID}
	got := leaseIndexed(t, store, leaseWinnerDoc.ID, leaseLoserDoc.ID)
	if !slices.Equal(got, want) {
		t.Errorf("indexed documents = %v, want only the winner's %v", got, want)
	}
}

func TestSyncTakesOverALeaseLeftDeadPastItsTTL(t *testing.T) {
	ctx := context.Background()
	store, clock := leaseStore(t)

	if acquired, err := store.TryAcquireLease(ctx, leaseDeadHolder); err != nil || !acquired {
		t.Fatalf("TryAcquireLease(%s) = %v, %v; want true, nil", leaseDeadHolder, acquired, err)
	}

	clock.advance(repositories.LeaseTTL + repositories.LeaseTTL/2)

	got, err := leaseRound(store, clock, leaseWinnerHolder, &leaseSource{doc: leaseTakerDoc}).Sync(ctx, SyncOptions{})
	if err != nil {
		t.Fatalf("Sync past the TTL of a dead lease: %v", err)
	}

	want := entities.LeaseState{Holder: leaseDeadHolder, AcquiredAt: leaseEpoch, HeartbeatAt: leaseEpoch}
	if got.TookOverFrom == nil || *got.TookOverFrom != want {
		t.Fatalf("TookOverFrom = %+v, want %+v", got.TookOverFrom, want)
	}

	indexed := leaseIndexed(t, store, leaseTakerDoc.ID)
	if !slices.Equal(indexed, []entities.DocID{leaseTakerDoc.ID}) {
		t.Errorf("indexed documents = %v, want the taker's round to have landed", indexed)
	}
}
