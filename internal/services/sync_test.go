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

	"lore/internal/entities"
	"lore/internal/errors/internalerror"
	mock_embedder "lore/internal/mocks/embedder"
	mock_entities "lore/internal/mocks/entities"
	mock_repositories "lore/internal/mocks/repositories"
	mock_services "lore/internal/mocks/services"
	"lore/internal/services"
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
	emb     *mock_embedder.MockEmbedder
	links   *mock_services.MockLinkResolver
}

func newSyncMocks(t *testing.T) syncMocks {
	t.Helper()

	ctrl := gomock.NewController(t)

	return syncMocks{
		ctrl:    ctrl,
		store:   mock_repositories.NewMockIndexStore(ctrl),
		chunker: mock_services.NewMockChunker(ctrl),
		emb:     mock_embedder.NewMockEmbedder(ctrl),
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
	m.emb.EXPECT().Identity().Return(currentIdentity).AnyTimes()
	m.store.EXPECT().Meta(gomock.Any(), metaKeyEmbedderIdentity).Return(currentIdentity, nil)
}

func (m syncMocks) connector(name string) *mock_entities.MockConnector {
	conn := mock_entities.NewMockConnector(m.ctrl)
	conn.EXPECT().Name().Return(name).AnyTimes()

	return conn
}

func (m syncMocks) linkedBatches(n int) {
	m.links.EXPECT().Link(gomock.Any(), gomock.Any()).Times(n).Return(nil)
}

func (m syncMocks) linkedPending() {
	m.links.EXPECT().LinkPending(gomock.Any()).Return(nil)
}

func (m syncMocks) orchestrator(connectors ...entities.Connector) services.SyncOrchestrator {
	return services.NewSyncOrchestrator(m.store, connectors, m.chunker, m.emb, m.links)
}

type syncStreamItem struct {
	batch entities.Batch
	err   error
}

func syncBatch(cursor entities.Cursor, docs ...entities.Document) syncStreamItem {
	return syncStreamItem{batch: entities.Batch{Docs: docs, Cursor: cursor}}
}

func syncFailure(err error) syncStreamItem { return syncStreamItem{err: err} }

type syncStream struct {
	items  []syncStreamItem
	yields int
}

func newSyncStream(items ...syncStreamItem) *syncStream { return &syncStream{items: items} }

func (s *syncStream) seq() iter.Seq2[entities.Batch, error] {
	return func(yield func(entities.Batch, error) bool) {
		for _, item := range s.items {
			s.yields++
			if !yield(item.batch, item.err) {
				return
			}
		}
	}
}

func syncDoc(id entities.DocID) entities.Document {
	return entities.Document{
		ID:      id,
		Source:  "github",
		Type:    entities.DocTypePR,
		RepoRef: "github:acme/lore",
		Title:   "Checkpoint per batch",
		Body:    "the sync round commits, then checkpoints",
	}
}

func syncChunks(id entities.DocID, texts ...string) []entities.Chunk {
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

func TestSyncCheckpointsOnlyAfterTheBatchIsCommitted(t *testing.T) {
	t.Parallel()

	m := newSyncMocks(t)
	m.acquiredLease()
	m.matchingIdentity()

	doc := syncDoc("github:pr:1")
	next := entities.Cursor{"updated_at": "2024-03-02T09:30:00Z"}
	vectors := [][]float32{{0.1, 0.2}, {0.3, 0.4}}
	texts := []string{"first chunk", "second chunk"}
	stored := withSyncVectors(syncChunks(doc.ID, texts...), vectors)

	conn := m.connector("github")
	conn.EXPECT().Changes(gomock.Any(), entities.Cursor{"updated_at": "2024-03-01T00:00:00Z"}).
		Return(newSyncStream(syncBatch(next, doc)).seq())

	gomock.InOrder(
		m.store.EXPECT().Cursor(gomock.Any(), "github").
			Return(entities.Cursor{"updated_at": "2024-03-01T00:00:00Z"}, nil),
		m.store.EXPECT().UpsertDocuments(gomock.Any(), []entities.Document{doc}).Return(nil),
		m.chunker.EXPECT().Chunk(doc).Return(syncChunks(doc.ID, texts...)),
		m.emb.EXPECT().Embed(gomock.Any(), texts).Return(vectors, nil),
		m.store.EXPECT().ReplaceChunks(gomock.Any(), doc.ID, stored).Return(nil),
		m.links.EXPECT().Link(gomock.Any(), []entities.Document{doc}).Return(nil),
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
	committed := entities.Cursor{"updated_at": "1"}

	tests := []struct {
		name      string
		setup     func(m syncMocks, conn *mock_entities.MockConnector)
		want      internalerror.Kind
		wantCause error
	}{
		{
			name: "cursor unreadable, so the connector is never asked for changes",
			setup: func(m syncMocks, _ *mock_entities.MockConnector) {
				m.store.EXPECT().Cursor(gomock.Any(), "github").Return(nil, errSyncStore)
			},
			want:      internalerror.KindInternal,
			wantCause: errSyncStore,
		},
		{
			name: "connector fails on its first batch",
			setup: func(m syncMocks, conn *mock_entities.MockConnector) {
				m.store.EXPECT().Cursor(gomock.Any(), "github").Return(nil, nil)
				conn.EXPECT().Changes(gomock.Any(), nil).Return(newSyncStream(syncFailure(errSyncStore)).seq())
			},
			want:      internalerror.KindInternal,
			wantCause: errSyncStore,
		},
		{
			name: "documents cannot be stored",
			setup: func(m syncMocks, conn *mock_entities.MockConnector) {
				m.store.EXPECT().Cursor(gomock.Any(), "github").Return(nil, nil)
				conn.EXPECT().Changes(gomock.Any(), nil).Return(newSyncStream(syncBatch(committed, doc)).seq())
				m.store.EXPECT().UpsertDocuments(gomock.Any(), []entities.Document{doc}).Return(errSyncStore)
			},
			want:      internalerror.KindInternal,
			wantCause: errSyncStore,
		},
		{
			name: "embedder fails",
			setup: func(m syncMocks, conn *mock_entities.MockConnector) {
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
			setup: func(m syncMocks, conn *mock_entities.MockConnector) {
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
			setup: func(m syncMocks, conn *mock_entities.MockConnector) {
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

			conn := m.connector("github")
			// No SetCursor is declared: the strict controller fails any checkpoint of an uncommitted batch.
			tt.setup(m, conn)

			_, err := m.orchestrator(conn).Sync(context.Background(), services.SyncOptions{})
			assertSyncKind(t, err, tt.want)
			if tt.wantCause != nil && !errors.Is(err, tt.wantCause) {
				t.Errorf("Sync() error = %v, want it to wrap %v", err, tt.wantCause)
			}
		})
	}
}

func TestSyncKeepsEarlierCheckpointsWhenAConnectorDiesMidStream(t *testing.T) {
	t.Parallel()

	m := newSyncMocks(t)
	m.acquiredLease()
	m.matchingIdentity()

	first := entities.Cursor{"page": "1"}
	stream := newSyncStream(
		syncBatch(first),
		syncFailure(errSyncStore),
		syncBatch(entities.Cursor{"page": "3"}),
	)

	conn := m.connector("github")
	conn.EXPECT().Changes(gomock.Any(), nil).Return(stream.seq())

	m.store.EXPECT().Cursor(gomock.Any(), "github").Return(nil, nil)
	m.store.EXPECT().UpsertDocuments(gomock.Any(), nil).Return(nil)
	m.store.EXPECT().SetCursor(gomock.Any(), "github", first).Return(nil)
	m.linkedBatches(1)

	_, err := m.orchestrator(conn).Sync(context.Background(), services.SyncOptions{})
	assertSyncKind(t, err, internalerror.KindInternal)

	if stream.yields != 2 {
		t.Errorf("connector yielded %d items, want 2 — the round must abandon the stream, not drain it", stream.yields)
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
			wantExact: "cannot run a sync round: sync lease held — another process is already writing this index; " +
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
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Sync() error = %q, want it to name %q", err, tt.want)
			}
			if tt.wantExact != "" && err.Error() != tt.wantExact {
				t.Errorf("Sync() error = %q, want exactly %q", err, tt.wantExact)
			}
		})
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
	m.emb.EXPECT().Identity().Return(currentIdentity).AnyTimes()
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
	m.emb.EXPECT().Identity().Return(currentIdentity).AnyTimes()

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
			m.emb.EXPECT().Identity().Return(currentIdentity).AnyTimes()
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
			m.emb.EXPECT().Identity().Return(currentIdentity).AnyTimes()
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
	noDoc := entities.Document{ID: "notion:page:9", Source: "notion", Type: entities.DocTypePage}
	ghCursor := entities.Cursor{"updated_at": "gh-1"}
	noCursor := entities.Cursor{"last_edited_time": "no-1"}

	github, notion := m.connector("github"), m.connector("notion")
	github.EXPECT().Changes(gomock.Any(), entities.Cursor{"updated_at": "gh-0"}).
		Return(newSyncStream(syncBatch(ghCursor, ghDoc)).seq())
	notion.EXPECT().Changes(gomock.Any(), nil).
		Return(newSyncStream(syncBatch(noCursor, noDoc)).seq())

	m.store.EXPECT().Cursor(gomock.Any(), "github").Return(entities.Cursor{"updated_at": "gh-0"}, nil)
	m.store.EXPECT().Cursor(gomock.Any(), "notion").Return(nil, nil)

	for _, doc := range []entities.Document{ghDoc, noDoc} {
		m.store.EXPECT().UpsertDocuments(gomock.Any(), []entities.Document{doc}).Return(nil)
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
	cursor := entities.Cursor{"page": "1"}

	conn := m.connector("github")
	conn.EXPECT().Changes(gomock.Any(), nil).Return(newSyncStream(syncBatch(cursor, doc)).seq())

	m.store.EXPECT().Cursor(gomock.Any(), "github").Return(nil, nil)
	m.store.EXPECT().UpsertDocuments(gomock.Any(), []entities.Document{doc}).Return(nil)
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
	conn.EXPECT().Changes(gomock.Any(), nil).Return(newSyncStream(syncBatch(entities.Cursor{"page": "1"})).seq())

	m.store.EXPECT().Cursor(gomock.Any(), "github").Return(nil, nil)
	m.store.EXPECT().UpsertDocuments(gomock.Any(), nil).Return(nil)
	m.store.EXPECT().SetCursor(gomock.Any(), "github", entities.Cursor{"page": "1"}).Return(nil)
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
