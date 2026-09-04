package services_test

import (
	"context"
	"errors"
	"iter"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/setthasit/Lore/internal/entities"
	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/internal/mocks/lore"
	mock_repositories "github.com/setthasit/Lore/internal/mocks/repositories"
	mock_services "github.com/setthasit/Lore/internal/mocks/services"
	"github.com/setthasit/Lore/internal/services"
	"github.com/setthasit/Lore/sdk"
)

const (
	metaKeyEmbedderIdentity = "embedder_identity"
	currentIdentity         = "openai/text-embedding-3-small/1536"
	previousIdentity        = "ollama/nomic-embed-text/768"
)

var errSyncStore = errors.New("store is on fire")

type syncMocks struct {
	ctrl    *gomock.Controller
	store   *mock_repositories.MockIndexStore
	chunker *mock_services.MockChunker
	emb     *mock_lore.MockEmbedder
	links   *mock_services.MockLinkResolver
}

func newSyncMocks(t *testing.T) syncMocks {
	t.Helper()

	ctrl := gomock.NewController(t)

	return syncMocks{
		ctrl:    ctrl,
		store:   mock_repositories.NewMockIndexStore(ctrl),
		chunker: mock_services.NewMockChunker(ctrl),
		emb:     mock_lore.NewMockEmbedder(ctrl),
		links:   mock_services.NewMockLinkResolver(ctrl),
	}
}

func (m syncMocks) freeLease() {
	m.store.EXPECT().Lease(gomock.Any()).Return(nil, nil)
}

func syncHolder() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown-host"
	}

	return host + "/" + strconv.Itoa(os.Getpid())
}

func (m syncMocks) acquiredLease() {
	m.freeLease()
	m.store.EXPECT().TryAcquireLease(gomock.Any(), gomock.Any()).Return(true, nil)
	m.store.EXPECT().ReleaseLease(gomock.Any(), gomock.Any()).Return(nil)
}

func (m syncMocks) matchingIdentity() {
	m.store.EXPECT().Meta(gomock.Any(), metaKeyEmbedderIdentity).Return(currentIdentity, nil)
}

func (m syncMocks) connector(name string) *mock_lore.MockConnector {
	conn := mock_lore.NewMockConnector(m.ctrl)
	conn.EXPECT().Name().Return(name).AnyTimes()

	return conn
}

func (m syncMocks) linkedBatches(n int) {
	m.links.EXPECT().Link(gomock.Any(), gomock.Any()).Times(n).Return(nil)
}

func (m syncMocks) linkedPending() {
	m.links.EXPECT().LinkPending(gomock.Any()).Return(nil)
}

func (m syncMocks) orchestrator(connectors ...lore.Connector) services.SyncOrchestrator {
	return services.NewSyncOrchestrator(m.store, connectors, m.chunker, m.emb, m.links, currentIdentity)
}

type syncStreamItem struct {
	batch lore.Batch
	err   error
}

func syncBatch(cursor lore.Cursor, docs ...lore.Document) syncStreamItem {
	return syncStreamItem{batch: lore.Batch{Docs: docs, Cursor: cursor}}
}

func syncFailure(err error) syncStreamItem { return syncStreamItem{err: err} }

type syncStream struct {
	items  []syncStreamItem
	yields int
}

func newSyncStream(items ...syncStreamItem) *syncStream { return &syncStream{items: items} }

func (s *syncStream) seq() iter.Seq2[lore.Batch, error] {
	return func(yield func(lore.Batch, error) bool) {
		for _, item := range s.items {
			s.yields++
			if !yield(item.batch, item.err) {
				return
			}
		}
	}
}

func syncDoc(id lore.DocID) lore.Document {
	return lore.Document{
		ID:      id,
		Source:  "github",
		Type:    lore.DocTypePR,
		RepoRef: "github:acme/lore",
		Title:   "Checkpoint per batch",
		Body:    "the sync round commits, then checkpoints",
	}
}

// The instance assertion reads nothing but Source and ID, so a mislabelling
// connector is one of the two set to something the instance never owns.
func syncDocOf(source string, id lore.DocID) lore.Document {
	doc := syncDoc(id)
	doc.Source = source

	return doc
}

func syncChunks(id lore.DocID, texts ...string) []entities.Chunk {
	chunks := make([]entities.Chunk, len(texts))
	for i, text := range texts {
		chunks[i] = entities.Chunk{DocID: id, Ordinal: i, Text: text, Source: "github"}
	}

	return chunks
}

// Copies: the orchestrator fills the very slice the chunker returned, which would otherwise mutate the expectation.
func withSyncVectors(chunks []entities.Chunk, vectors [][]float32) []entities.Chunk {
	out := make([]entities.Chunk, len(chunks))
	copy(out, chunks)
	for i := range out {
		out[i].Embedding = vectors[i]
	}

	return out
}

func assertSyncKind(t *testing.T, err error, want internalerror.Kind) {
	t.Helper()

	if err == nil {
		t.Fatalf("Sync() = nil, want a %v error", want)
	}
	if got := internalerror.KindOf(err); got != want {
		t.Fatalf("Sync() error kind = %v, want %v (%v)", got, want, err)
	}
}

// Transports may show the caller nothing but Message, so it carries the assertions.
func syncMessage(t *testing.T, err error) string {
	t.Helper()

	var classified *internalerror.Error
	if !errors.As(err, &classified) {
		t.Fatalf("Sync() error %v is unclassified", err)
	}

	return classified.Message
}

// The round survives a failing instance, so its error is read off the result.
func onlySyncFailure(t *testing.T, res services.SyncResult, instance string) services.InstanceFailure {
	t.Helper()

	if len(res.Failures) != 1 {
		t.Fatalf("Sync() reported %d instance failures, want 1 (%+v)", len(res.Failures), res.Failures)
	}
	if got := res.Failures[0].Instance; got != instance {
		t.Fatalf("Sync() blamed instance %q, want %q", got, instance)
	}

	return res.Failures[0]
}

func assertSyncFailureKind(t *testing.T, failure services.InstanceFailure, want internalerror.Kind) {
	t.Helper()

	if failure.Err == nil {
		t.Fatalf("the %s instance failed with a nil error, want a %v one", failure.Instance, want)
	}
	if got := internalerror.KindOf(failure.Err); got != want {
		t.Fatalf("the %s instance failed with kind %v, want %v (%v)",
			failure.Instance, got, want, failure.Err)
	}
}

func TestSyncCheckpointsOnlyAfterTheBatchIsCommitted(t *testing.T) {
	t.Parallel()

	m := newSyncMocks(t)
	m.acquiredLease()
	m.matchingIdentity()

	doc := syncDoc("github:pr:1")
	next := lore.Cursor{"updated_at": "2024-03-02T09:30:00Z"}
	vectors := [][]float32{{0.1, 0.2}, {0.3, 0.4}}
	texts := []string{"first chunk", "second chunk"}
	stored := withSyncVectors(syncChunks(doc.ID, texts...), vectors)

	conn := m.connector("github")
	conn.EXPECT().Changes(gomock.Any(), lore.Cursor{"updated_at": "2024-03-01T00:00:00Z"}).
		Return(newSyncStream(syncBatch(next, doc)).seq())

	gomock.InOrder(
		m.store.EXPECT().Cursor(gomock.Any(), "github").
			Return(lore.Cursor{"updated_at": "2024-03-01T00:00:00Z"}, nil),
		m.store.EXPECT().UpsertDocuments(gomock.Any(), []lore.Document{doc}).Return(nil),
		m.chunker.EXPECT().Chunk(doc).Return(syncChunks(doc.ID, texts...)),
		m.emb.EXPECT().Embed(gomock.Any(), texts).Return(vectors, nil),
		m.store.EXPECT().ReplaceChunks(gomock.Any(), doc.ID, stored).Return(nil),
		m.links.EXPECT().Link(gomock.Any(), []lore.Document{doc}).Return(nil),
		m.store.EXPECT().SetCursor(gomock.Any(), "github", next).Return(nil),
		m.links.EXPECT().LinkPending(gomock.Any()).Return(nil),
	)

	if _, err := m.orchestrator(conn).Sync(context.Background(), services.SyncOptions{}); err != nil {
		t.Fatalf("Sync() = %v, want nil", err)
	}
}

func TestSyncFailureKeepsTheLastCommittedCursor(t *testing.T) {
	t.Parallel()

	doc := syncDoc("github:pr:1")
	committed := lore.Cursor{"updated_at": "1"}

	tests := []struct {
		name      string
		setup     func(m syncMocks, conn *mock_lore.MockConnector)
		want      internalerror.Kind
		wantCause error
	}{
		{
			name: "cursor unreadable, so the connector is never asked for changes",
			setup: func(m syncMocks, _ *mock_lore.MockConnector) {
				m.store.EXPECT().Cursor(gomock.Any(), "github").Return(nil, errSyncStore)
			},
			want:      internalerror.KindInternal,
			wantCause: errSyncStore,
		},
		{
			name: "connector fails on its first batch",
			setup: func(m syncMocks, conn *mock_lore.MockConnector) {
				m.store.EXPECT().Cursor(gomock.Any(), "github").Return(nil, nil)
				conn.EXPECT().Changes(gomock.Any(), nil).Return(newSyncStream(syncFailure(errSyncStore)).seq())
			},
			want:      internalerror.KindInternal,
			wantCause: errSyncStore,
		},
		{
			name: "documents cannot be stored",
			setup: func(m syncMocks, conn *mock_lore.MockConnector) {
				m.store.EXPECT().Cursor(gomock.Any(), "github").Return(nil, nil)
				conn.EXPECT().Changes(gomock.Any(), nil).Return(newSyncStream(syncBatch(committed, doc)).seq())
				m.store.EXPECT().UpsertDocuments(gomock.Any(), []lore.Document{doc}).Return(errSyncStore)
			},
			want:      internalerror.KindInternal,
			wantCause: errSyncStore,
		},
		{
			name: "embedder fails",
			setup: func(m syncMocks, conn *mock_lore.MockConnector) {
				m.store.EXPECT().Cursor(gomock.Any(), "github").Return(nil, nil)
				conn.EXPECT().Changes(gomock.Any(), nil).Return(newSyncStream(syncBatch(committed, doc)).seq())
				m.store.EXPECT().UpsertDocuments(gomock.Any(), gomock.Any()).Return(nil)
				m.chunker.EXPECT().Chunk(doc).Return(syncChunks(doc.ID, "only chunk"))
				m.emb.EXPECT().Embed(gomock.Any(), []string{"only chunk"}).Return(nil, errSyncStore)
			},
			want:      internalerror.KindInternal,
			wantCause: errSyncStore,
		},
		{
			name: "embedder answers with the wrong number of vectors",
			setup: func(m syncMocks, conn *mock_lore.MockConnector) {
				m.store.EXPECT().Cursor(gomock.Any(), "github").Return(nil, nil)
				conn.EXPECT().Changes(gomock.Any(), nil).Return(newSyncStream(syncBatch(committed, doc)).seq())
				m.store.EXPECT().UpsertDocuments(gomock.Any(), gomock.Any()).Return(nil)
				m.chunker.EXPECT().Chunk(doc).Return(syncChunks(doc.ID, "a", "b"))
				m.emb.EXPECT().Embed(gomock.Any(), []string{"a", "b"}).
					Return([][]float32{{0.1}}, nil)
			},
			want: internalerror.KindInternal,
		},
		{
			name: "chunks cannot be stored",
			setup: func(m syncMocks, conn *mock_lore.MockConnector) {
				m.store.EXPECT().Cursor(gomock.Any(), "github").Return(nil, nil)
				conn.EXPECT().Changes(gomock.Any(), nil).Return(newSyncStream(syncBatch(committed, doc)).seq())
				m.store.EXPECT().UpsertDocuments(gomock.Any(), gomock.Any()).Return(nil)
				m.chunker.EXPECT().Chunk(doc).Return(syncChunks(doc.ID, "only chunk"))
				m.emb.EXPECT().Embed(gomock.Any(), gomock.Any()).Return([][]float32{{0.1}}, nil)
				m.store.EXPECT().ReplaceChunks(gomock.Any(), doc.ID, gomock.Any()).Return(errSyncStore)
			},
			want:      internalerror.KindInternal,
			wantCause: errSyncStore,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m := newSyncMocks(t)
			m.acquiredLease()
			m.matchingIdentity()
			m.linkedPending()

			conn := m.connector("github")
			// No SetCursor is declared: the strict controller fails any checkpoint of an uncommitted batch.
			tt.setup(m, conn)

			res, err := m.orchestrator(conn).Sync(context.Background(), services.SyncOptions{})
			if err != nil {
				t.Fatalf("Sync() = %v, want the round to survive its only instance failing", err)
			}

			failure := onlySyncFailure(t, res, "github")
			assertSyncFailureKind(t, failure, tt.want)
			if tt.wantCause != nil && !errors.Is(failure.Err, tt.wantCause) {
				t.Errorf("the github instance failed with %v, want it to wrap %v", failure.Err, tt.wantCause)
			}
		})
	}
}

func TestSyncKeepsEarlierCheckpointsWhenAConnectorDiesMidStream(t *testing.T) {
	t.Parallel()

	m := newSyncMocks(t)
	m.acquiredLease()
	m.matchingIdentity()

	first := lore.Cursor{"page": "1"}
	stream := newSyncStream(
		syncBatch(first),
		syncFailure(errSyncStore),
		syncBatch(lore.Cursor{"page": "3"}),
	)

	conn := m.connector("github")
	conn.EXPECT().Changes(gomock.Any(), nil).Return(stream.seq())

	m.store.EXPECT().Cursor(gomock.Any(), "github").Return(nil, nil)
	m.store.EXPECT().UpsertDocuments(gomock.Any(), nil).Return(nil)
	m.store.EXPECT().SetCursor(gomock.Any(), "github", first).Return(nil)
	m.linkedBatches(1)
	m.linkedPending()

	res, err := m.orchestrator(conn).Sync(context.Background(), services.SyncOptions{})
	if err != nil {
		t.Fatalf("Sync() = %v, want the round to survive its only instance failing", err)
	}
	assertSyncFailureKind(t, onlySyncFailure(t, res, "github"), internalerror.KindInternal)

	if stream.yields != 2 {
		t.Errorf("connector yielded %d items, want 2 — the round must abandon the stream, not drain it", stream.yields)
	}
}

func TestSyncRejectsABatchAnInstanceMislabelled(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		doc  lore.Document
		want []string
	}{
		{
			name: "the source names another instance",
			doc:  syncDocOf("gitlab", "github:pr:1"),
			want: []string{`"github:pr:1"`, `"gitlab"`, "github"},
		},
		{
			name: "the document id lands in another instance's namespace",
			doc:  syncDocOf("github", "gitlab:pr:1"),
			want: []string{`"gitlab:pr:1"`, `"github:"`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m := newSyncMocks(t)
			m.acquiredLease()
			m.matchingIdentity()
			m.linkedPending()

			conn := m.connector("github")
			conn.EXPECT().Changes(gomock.Any(), nil).
				Return(newSyncStream(syncBatch(lore.Cursor{"page": "1"}, tt.doc)).seq())
			m.store.EXPECT().Cursor(gomock.Any(), "github").Return(nil, nil)
			// No UpsertDocuments and no SetCursor are declared: the batch must be refused
			// before it is written, and the cursor must not move past it.

			res, err := m.orchestrator(conn).Sync(context.Background(), services.SyncOptions{})
			if err != nil {
				t.Fatalf("Sync() = %v, want the round to survive its only instance failing", err)
			}

			failure := onlySyncFailure(t, res, "github")
			assertSyncFailureKind(t, failure, internalerror.KindBadRequest)

			message := syncMessage(t, failure.Err)
			for _, want := range tt.want {
				if !strings.Contains(message, want) {
					t.Errorf("the github instance failed with %q, want it to name %s", message, want)
				}
			}
		})
	}
}

func TestSyncRunsTheRemainingInstancesAfterOneFails(t *testing.T) {
	t.Parallel()

	m := newSyncMocks(t)
	m.acquiredLease()
	m.matchingIdentity()

	doc := lore.Document{ID: "notion:page:9", Source: "notion", Type: lore.DocTypePage}
	cursor := lore.Cursor{"last_edited_time": "no-1"}

	broken, healthy := m.connector("github"), m.connector("notion")
	m.store.EXPECT().Cursor(gomock.Any(), "github").Return(nil, nil)
	broken.EXPECT().Changes(gomock.Any(), nil).Return(newSyncStream(syncFailure(errSyncStore)).seq())

	m.store.EXPECT().Cursor(gomock.Any(), "notion").Return(nil, nil)
	healthy.EXPECT().Changes(gomock.Any(), nil).Return(newSyncStream(syncBatch(cursor, doc)).seq())
	m.store.EXPECT().UpsertDocuments(gomock.Any(), []lore.Document{doc}).Return(nil)
	m.chunker.EXPECT().Chunk(doc).Return(syncChunks(doc.ID, "body"))
	m.emb.EXPECT().Embed(gomock.Any(), []string{"body"}).Return([][]float32{{0.5}}, nil)
	m.store.EXPECT().ReplaceChunks(gomock.Any(), doc.ID,
		withSyncVectors(syncChunks(doc.ID, "body"), [][]float32{{0.5}})).Return(nil)
	// The only SetCursor declared is the healthy instance's: the broken one committed nothing.
	m.store.EXPECT().SetCursor(gomock.Any(), "notion", cursor).Return(nil)
	m.linkedBatches(1)
	m.linkedPending()

	res, err := m.orchestrator(broken, healthy).Sync(context.Background(), services.SyncOptions{})
	if err != nil {
		t.Fatalf("Sync() = %v, want a round that reports the failure and carries on", err)
	}

	failure := onlySyncFailure(t, res, "github")
	if !errors.Is(failure.Err, errSyncStore) {
		t.Errorf("the github instance failed with %v, want it to wrap %v", failure.Err, errSyncStore)
	}
}

func TestSyncSkipsWhenAnotherProcessHoldsTheLease(t *testing.T) {
	t.Parallel()

	const holder = "host-9/1234"

	tests := []struct {
		name      string
		lease     *entities.LeaseState
		leaseErr  error
		want      string
		wantExact string
	}{
		{
			name:  "names the holder and its heartbeat",
			lease: &entities.LeaseState{Holder: holder, HeartbeatAt: time.Now().Add(-90 * time.Second)},
			want:  holder + " (last heartbeat 1m",
		},
		{
			name:     "an unreadable lease still refuses the round",
			leaseErr: errSyncStore,
			want:     "another process",
			wantExact: "cannot run a sync round — another process is already writing this index; " +
				"retry later, or wait out the 60s lease TTL if that holder crashed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m := newSyncMocks(t)
			m.store.EXPECT().Lease(gomock.Any()).Return(tt.lease, tt.leaseErr).AnyTimes()
			m.store.EXPECT().TryAcquireLease(gomock.Any(), gomock.Any()).Return(false, nil)

			_, err := m.orchestrator(m.connector("github")).Sync(context.Background(), services.SyncOptions{})
			assertSyncKind(t, err, internalerror.KindPrecondition)

			if !errors.Is(err, services.ErrSyncLocked) {
				t.Errorf("Sync() error = %v, want it to wrap ErrSyncLocked", err)
			}
			message := syncMessage(t, err)
			if !strings.Contains(message, tt.want) {
				t.Errorf("Sync() message = %q, want it to name %q", message, tt.want)
			}
			if tt.wantExact != "" && message != tt.wantExact {
				t.Errorf("Sync() message = %q, want exactly %q", message, tt.wantExact)
			}
		})
	}
}

func TestSyncRestrictsTheRoundToTheNamedSource(t *testing.T) {
	t.Parallel()

	m := newSyncMocks(t)
	m.acquiredLease()
	m.matchingIdentity()
	m.linkedPending()

	github, notion := m.connector("github"), m.connector("notion")
	// notion declares no Cursor or Changes call: the strict controller fails the round if it is read.
	m.store.EXPECT().Cursor(gomock.Any(), "github").Return(nil, nil)
	github.EXPECT().Changes(gomock.Any(), nil).Return(newSyncStream().seq())

	_, err := m.orchestrator(github, notion).Sync(context.Background(), services.SyncOptions{Source: "github"})
	if err != nil {
		t.Fatalf("Sync() = %v, want nil", err)
	}
}

func TestSyncWithoutASourceRunsEveryConnector(t *testing.T) {
	t.Parallel()

	m := newSyncMocks(t)
	m.acquiredLease()
	m.matchingIdentity()
	m.linkedPending()

	var connectors []lore.Connector
	for _, name := range []string{"github", "notion"} {
		conn := m.connector(name)
		m.store.EXPECT().Cursor(gomock.Any(), name).Return(nil, nil)
		conn.EXPECT().Changes(gomock.Any(), nil).Return(newSyncStream().seq())
		connectors = append(connectors, conn)
	}

	if _, err := m.orchestrator(connectors...).Sync(context.Background(), services.SyncOptions{}); err != nil {
		t.Fatalf("Sync() = %v, want nil", err)
	}
}

func TestSyncRejectsAnUnknownSource(t *testing.T) {
	t.Parallel()

	m := newSyncMocks(t)
	// No lease call is declared: an unknown source is refused before the round takes the lock.
	_, err := m.orchestrator(m.connector("github"), m.connector("notion")).
		Sync(context.Background(), services.SyncOptions{Source: "gitlab"})

	assertSyncKind(t, err, internalerror.KindBadRequest)

	message := syncMessage(t, err)
	for _, want := range []string{`"gitlab"`, "github, notion"} {
		if !strings.Contains(message, want) {
			t.Errorf("Sync() message = %q, want it to name %q", message, want)
		}
	}
}

func TestSyncRefusesToReembedASingleSource(t *testing.T) {
	t.Parallel()

	m := newSyncMocks(t)
	// No store call is declared: a re-embed wipes chunks workspace-wide, so the pair dies before any of it.
	_, err := m.orchestrator(m.connector("github"), m.connector("notion")).
		Sync(context.Background(), services.SyncOptions{Source: "github", Reembed: true})

	assertSyncKind(t, err, internalerror.KindBadRequest)

	if message := syncMessage(t, err); !strings.Contains(message, "whole workspace") {
		t.Errorf("Sync() message = %q, want it to say a re-embed covers the whole workspace", message)
	}
}

func TestSyncReportsTheDeadLeaseItTookOver(t *testing.T) {
	t.Parallel()

	dead := &entities.LeaseState{
		Holder:      "host-9/1234",
		AcquiredAt:  time.Now().Add(-5 * time.Minute),
		HeartbeatAt: time.Now().Add(-3 * time.Minute),
	}
	own := &entities.LeaseState{Holder: syncHolder(), HeartbeatAt: time.Now().Add(-3 * time.Minute)}

	tests := []struct {
		name     string
		previous *entities.LeaseState
		leaseErr error
		want     *entities.LeaseState
	}{
		{name: "another holder's dead lease is reported", previous: dead, want: dead},
		{name: "a free lease is no takeover", previous: nil, want: nil},
		{name: "this process' own lapsed lease is no takeover", previous: own, want: nil},
		{
			name:     "a still-live holder's lease is no takeover",
			previous: &entities.LeaseState{Holder: "host-9/1234", HeartbeatAt: time.Now().Add(-5 * time.Second)},
			want:     nil,
		},
		{name: "an unreadable lease does not fail the round", leaseErr: errSyncStore, want: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m := newSyncMocks(t)
			m.store.EXPECT().Lease(gomock.Any()).Return(tt.previous, tt.leaseErr)
			m.store.EXPECT().TryAcquireLease(gomock.Any(), gomock.Any()).Return(true, nil)
			m.store.EXPECT().ReleaseLease(gomock.Any(), gomock.Any()).Return(nil)
			m.matchingIdentity()
			m.linkedPending()

			res, err := m.orchestrator().Sync(context.Background(), services.SyncOptions{})
			if err != nil {
				t.Fatalf("Sync() = %v, want nil", err)
			}
			if res.TookOverFrom != tt.want {
				t.Errorf("Sync() TookOverFrom = %v, want %v", res.TookOverFrom, tt.want)
			}
		})
	}
}

func TestSyncRefusesAnEmbedderIdentityMismatch(t *testing.T) {
	t.Parallel()

	m := newSyncMocks(t)
	m.acquiredLease()
	m.store.EXPECT().Meta(gomock.Any(), metaKeyEmbedderIdentity).Return(previousIdentity, nil)

	conn := m.connector("github")

	_, err := m.orchestrator(conn).Sync(context.Background(), services.SyncOptions{})
	assertSyncKind(t, err, internalerror.KindPrecondition)

	for _, want := range []string{"lore sync --reembed", previousIdentity, currentIdentity} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Sync() error = %q, want it to name %q", err, want)
		}
	}
}

func TestSyncAdoptsTheEmbedderIdentityOnFirstSync(t *testing.T) {
	t.Parallel()

	m := newSyncMocks(t)
	m.acquiredLease()

	conn := m.connector("github")
	conn.EXPECT().Changes(gomock.Any(), nil).Return(newSyncStream().seq())

	gomock.InOrder(
		m.store.EXPECT().Meta(gomock.Any(), metaKeyEmbedderIdentity).Return("", nil),
		m.store.EXPECT().SetMeta(gomock.Any(), metaKeyEmbedderIdentity, currentIdentity).Return(nil),
		m.store.EXPECT().Cursor(gomock.Any(), "github").Return(nil, nil),
	)

	m.linkedPending()

	if _, err := m.orchestrator(conn).Sync(context.Background(), services.SyncOptions{}); err != nil {
		t.Fatalf("Sync() = %v, want nil", err)
	}
}

func TestSyncReembedRewindsWipesThenRecordsIdentity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		stored string
	}{
		{name: "identity changed", stored: previousIdentity},
		{name: "identity unchanged, the caller still asked for a rebuild", stored: currentIdentity},
		{name: "identity never recorded", stored: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m := newSyncMocks(t)
			m.acquiredLease()
			m.store.EXPECT().Meta(gomock.Any(), metaKeyEmbedderIdentity).Return(tt.stored, nil)

			github, notion := m.connector("github"), m.connector("notion")
			github.EXPECT().Changes(gomock.Any(), nil).Return(newSyncStream().seq())
			notion.EXPECT().Changes(gomock.Any(), nil).Return(newSyncStream().seq())

			gomock.InOrder(
				m.store.EXPECT().SetCursor(gomock.Any(), "github", nil).Return(nil),
				m.store.EXPECT().SetCursor(gomock.Any(), "notion", nil).Return(nil),
				m.store.EXPECT().WipeChunks(gomock.Any()).Return(nil),
				m.store.EXPECT().SetMeta(gomock.Any(), metaKeyEmbedderIdentity, currentIdentity).Return(nil),
				m.store.EXPECT().Cursor(gomock.Any(), "github").Return(nil, nil),
				m.store.EXPECT().Cursor(gomock.Any(), "notion").Return(nil, nil),
			)

			m.linkedPending()

			_, err := m.orchestrator(github, notion).Sync(context.Background(), services.SyncOptions{Reembed: true})
			if err != nil {
				t.Fatalf("Sync(Reembed) = %v, want nil", err)
			}
		})
	}
}

func TestSyncReembedFailureLeavesTheIdentityAlone(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		setup func(m syncMocks)
	}{
		{
			name: "the cursor cannot be rewound",
			setup: func(m syncMocks) {
				m.store.EXPECT().SetCursor(gomock.Any(), "github", nil).Return(errSyncStore)
			},
		},
		{
			name: "the chunk layer cannot be wiped",
			setup: func(m syncMocks) {
				m.store.EXPECT().SetCursor(gomock.Any(), "github", nil).Return(nil)
				m.store.EXPECT().WipeChunks(gomock.Any()).Return(errSyncStore)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m := newSyncMocks(t)
			m.acquiredLease()
			m.store.EXPECT().Meta(gomock.Any(), metaKeyEmbedderIdentity).Return(previousIdentity, nil)
			// No SetMeta is declared: a half-finished rebuild must not claim the new identity.
			tt.setup(m)

			conn := m.connector("github")

			_, err := m.orchestrator(conn).Sync(context.Background(), services.SyncOptions{Reembed: true})
			assertSyncKind(t, err, internalerror.KindInternal)
		})
	}
}

func TestSyncProcessesConnectorsIndependently(t *testing.T) {
	t.Parallel()

	m := newSyncMocks(t)
	m.acquiredLease()
	m.matchingIdentity()

	ghDoc := syncDoc("github:pr:1")
	noDoc := lore.Document{ID: "notion:page:9", Source: "notion", Type: lore.DocTypePage}
	ghCursor := lore.Cursor{"updated_at": "gh-1"}
	noCursor := lore.Cursor{"last_edited_time": "no-1"}

	github, notion := m.connector("github"), m.connector("notion")
	github.EXPECT().Changes(gomock.Any(), lore.Cursor{"updated_at": "gh-0"}).
		Return(newSyncStream(syncBatch(ghCursor, ghDoc)).seq())
	notion.EXPECT().Changes(gomock.Any(), nil).
		Return(newSyncStream(syncBatch(noCursor, noDoc)).seq())

	m.store.EXPECT().Cursor(gomock.Any(), "github").Return(lore.Cursor{"updated_at": "gh-0"}, nil)
	m.store.EXPECT().Cursor(gomock.Any(), "notion").Return(nil, nil)

	for _, doc := range []lore.Document{ghDoc, noDoc} {
		m.store.EXPECT().UpsertDocuments(gomock.Any(), []lore.Document{doc}).Return(nil)
		m.chunker.EXPECT().Chunk(doc).Return(syncChunks(doc.ID, "body"))
		m.emb.EXPECT().Embed(gomock.Any(), []string{"body"}).Return([][]float32{{0.5}}, nil)
		m.store.EXPECT().ReplaceChunks(gomock.Any(), doc.ID,
			withSyncVectors(syncChunks(doc.ID, "body"), [][]float32{{0.5}})).Return(nil)
	}
	m.store.EXPECT().SetCursor(gomock.Any(), "github", ghCursor).Return(nil)
	m.store.EXPECT().SetCursor(gomock.Any(), "notion", noCursor).Return(nil)
	m.linkedBatches(2)
	m.linkedPending()

	if _, err := m.orchestrator(github, notion).Sync(context.Background(), services.SyncOptions{}); err != nil {
		t.Fatalf("Sync() = %v, want nil", err)
	}
}

func TestSyncClearsTheChunksOfADocumentThatChunksToNothing(t *testing.T) {
	t.Parallel()

	m := newSyncMocks(t)
	m.acquiredLease()
	m.matchingIdentity()

	doc := syncDoc("github:pr:1")
	doc.Body = ""
	cursor := lore.Cursor{"page": "1"}

	conn := m.connector("github")
	conn.EXPECT().Changes(gomock.Any(), nil).Return(newSyncStream(syncBatch(cursor, doc)).seq())

	m.store.EXPECT().Cursor(gomock.Any(), "github").Return(nil, nil)
	m.store.EXPECT().UpsertDocuments(gomock.Any(), []lore.Document{doc}).Return(nil)
	m.chunker.EXPECT().Chunk(doc).Return(nil)
	// No Embed is declared: an empty chunk set must not cost an embedding call.
	m.store.EXPECT().ReplaceChunks(gomock.Any(), doc.ID, nil).Return(nil)
	m.store.EXPECT().SetCursor(gomock.Any(), "github", cursor).Return(nil)
	m.linkedBatches(1)
	m.linkedPending()

	if _, err := m.orchestrator(conn).Sync(context.Background(), services.SyncOptions{}); err != nil {
		t.Fatalf("Sync() = %v, want nil", err)
	}
}

func TestSyncLeaseHolderNamesThisProcess(t *testing.T) {
	t.Parallel()

	want := syncHolder()

	m := newSyncMocks(t)
	m.matchingIdentity()
	m.freeLease()
	m.store.EXPECT().TryAcquireLease(gomock.Any(), want).Return(true, nil)
	m.store.EXPECT().ReleaseLease(gomock.Any(), want).Return(nil)

	conn := m.connector("github")
	conn.EXPECT().Changes(gomock.Any(), nil).Return(newSyncStream(syncBatch(lore.Cursor{"page": "1"})).seq())

	m.store.EXPECT().Cursor(gomock.Any(), "github").Return(nil, nil)
	m.store.EXPECT().UpsertDocuments(gomock.Any(), nil).Return(nil)
	m.store.EXPECT().SetCursor(gomock.Any(), "github", lore.Cursor{"page": "1"}).Return(nil)
	m.linkedBatches(1)
	m.linkedPending()

	if _, err := m.orchestrator(conn).Sync(context.Background(), services.SyncOptions{}); err != nil {
		t.Fatalf("Sync() = %v, want nil", err)
	}
}

func TestSyncWithoutConnectorsDoesNothing(t *testing.T) {
	t.Parallel()

	m := newSyncMocks(t)
	m.acquiredLease()
	m.matchingIdentity()
	m.linkedPending()

	if _, err := m.orchestrator().Sync(context.Background(), services.SyncOptions{}); err != nil {
		t.Fatalf("Sync() = %v, want nil", err)
	}
}

func TestSyncReleasesTheLeaseAfterContextCancellation(t *testing.T) {
	t.Parallel()

	m := newSyncMocks(t)
	m.matchingIdentity()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	m.freeLease()
	m.store.EXPECT().TryAcquireLease(gomock.Any(), gomock.Any()).Return(true, nil)
	m.store.EXPECT().ReleaseLease(gomock.Any(), gomock.Any()).DoAndReturn(
		func(released context.Context, _ string) error {
			if released.Err() != nil {
				t.Errorf("ReleaseLease context = %v, want one the cancellation did not reach", released.Err())
			}

			return nil
		})

	conn := m.connector("github")
	conn.EXPECT().Changes(gomock.Any(), nil).Return(newSyncStream(syncFailure(context.Canceled)).seq())
	m.store.EXPECT().Cursor(gomock.Any(), "github").Return(nil, nil)

	_, err := m.orchestrator(conn).Sync(ctx, services.SyncOptions{})
	assertSyncKind(t, err, internalerror.KindInternal)
}
