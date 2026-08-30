package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"lore/internal/entities"
	"lore/internal/errors/internalerror"
	mock_embedder "lore/internal/mocks/embedder"
	mock_repositories "lore/internal/mocks/repositories"
)

const (
	retrieveQuery = "why did we roll back the scheduler?"
	retrieveK     = 4
)

var retrieveVector = []float32{0.5, -0.25, 0.125}

var (
	errRetrieveEmbed = errors.New("provider refused the request")
	errRetrieveStore = errors.New("index is unreachable")
)

type retrieveMocks struct {
	store *mock_repositories.MockIndexStore
	emb   *mock_embedder.MockEmbedder
}

func newRetrieveMocks(t *testing.T) retrieveMocks {
	t.Helper()

	ctrl := gomock.NewController(t)

	return retrieveMocks{
		store: mock_repositories.NewMockIndexStore(ctrl),
		emb:   mock_embedder.NewMockEmbedder(ctrl),
	}
}

func retrieveMeta(id string) entities.DocumentMeta {
	return entities.DocumentMeta{
		ID:        entities.DocID(id),
		Source:    "github",
		Type:      entities.DocTypeIssue,
		Title:     "decision " + id,
		URL:       "https://example.test/" + id,
		CreatedAt: time.Date(2025, time.March, 12, 9, 0, 0, 0, time.UTC),
	}
}

func retrieveFused(doc string, ordinal int, score float32) fusedChunk {
	return fusedChunk{Chunk: hit(doc, ordinal).Chunk, Score: score}
}

func assertInternal(t *testing.T, err error, message string, cause error) {
	t.Helper()

	var classified *internalerror.Error
	if !errors.As(err, &classified) {
		t.Fatalf("err = %v, want a classified error", err)
	}
	if classified.Kind != internalerror.KindInternal {
		t.Errorf("kind = %s, want internal", classified.Kind)
	}
	if classified.Message != message {
		t.Errorf("message = %q, want %q", classified.Message, message)
	}
	if cause != nil && !errors.Is(err, cause) {
		t.Errorf("err = %v, want %v wrapped", err, cause)
	}
}

func assertClose(t *testing.T, what string, got, want float32) {
	t.Helper()

	if diff := got - want; diff > scoreEpsilon || diff < -scoreEpsilon {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

func TestHybridSearchFusesBothSearchesRunWithTheSameFiltersAndK(t *testing.T) {
	t.Parallel()

	filters := entities.Filters{Source: "github", RepoRef: "acme/lore", DocType: entities.DocTypePR}

	m := newRetrieveMocks(t)
	m.emb.EXPECT().Embed(gomock.Any(), []string{retrieveQuery}).Return([][]float32{retrieveVector}, nil)
	m.store.EXPECT().SearchLexical(gomock.Any(), retrieveQuery, gomock.Eq(filters), retrieveK).
		Return([]entities.ChunkHit{hit("docA", 0), hit("docB", 0)}, nil)
	m.store.EXPECT().SearchVector(gomock.Any(), retrieveVector, gomock.Eq(filters), retrieveK).
		Return([]entities.ChunkHit{hit("docB", 0), hit("docC", 0)}, nil)

	fused, err := hybridSearch(context.Background(), m.store, m.emb, retrieveQuery, filters, retrieveK)
	if err != nil {
		t.Fatalf("hybridSearch: %v", err)
	}

	want := []struct {
		doc   string
		score float32
	}{
		{doc: "docB", score: rrf(2, 1)},
		{doc: "docA", score: rrf(1)},
		{doc: "docC", score: rrf(2)},
	}
	if len(fused) != len(want) {
		t.Fatalf("got %d fused chunks, want %d: %+v", len(fused), len(want), fused)
	}
	for i, w := range want {
		if string(fused[i].DocID) != w.doc {
			t.Errorf("chunk %d = %s, want %s", i, fused[i].DocID, w.doc)

			continue
		}
		assertClose(t, "chunk "+w.doc+" score", fused[i].Score, w.score)
	}
}

func TestHybridSearchClassifiesFailuresAndStopsThere(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		expect  func(m retrieveMocks)
		message string
		cause   error
	}{
		"embedder": {
			expect: func(m retrieveMocks) {
				m.emb.EXPECT().Embed(gomock.Any(), []string{retrieveQuery}).Return(nil, errRetrieveEmbed)
			},
			message: "embedding the question failed",
			cause:   errRetrieveEmbed,
		},
		"misaligned embeddings": {
			expect: func(m retrieveMocks) {
				m.emb.EXPECT().Embed(gomock.Any(), []string{retrieveQuery}).
					Return([][]float32{retrieveVector, retrieveVector}, nil)
			},
			message: "embedder returned 2 vectors for one text",
		},
		"lexical search": {
			expect: func(m retrieveMocks) {
				m.emb.EXPECT().Embed(gomock.Any(), []string{retrieveQuery}).
					Return([][]float32{retrieveVector}, nil)
				m.store.EXPECT().SearchLexical(gomock.Any(), retrieveQuery, gomock.Any(), retrieveK).
					Return(nil, errRetrieveStore)
			},
			message: "lexical search failed",
			cause:   errRetrieveStore,
		},
		"vector search": {
			expect: func(m retrieveMocks) {
				m.emb.EXPECT().Embed(gomock.Any(), []string{retrieveQuery}).
					Return([][]float32{retrieveVector}, nil)
				m.store.EXPECT().SearchLexical(gomock.Any(), retrieveQuery, gomock.Any(), retrieveK).
					Return([]entities.ChunkHit{hit("docA", 0)}, nil)
				m.store.EXPECT().SearchVector(gomock.Any(), retrieveVector, gomock.Any(), retrieveK).
					Return(nil, errRetrieveStore)
			},
			message: "vector search failed",
			cause:   errRetrieveStore,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			m := newRetrieveMocks(t)
			tc.expect(m)

			fused, err := hybridSearch(context.Background(), m.store, m.emb,
				retrieveQuery, entities.Filters{}, retrieveK)
			if fused != nil {
				t.Errorf("fused = %+v, want none", fused)
			}
			assertInternal(t, err, tc.message, tc.cause)
		})
	}
}

func TestLiftDocumentsKeepsOneCitableSeedPerDocumentInFusionOrder(t *testing.T) {
	t.Parallel()

	urlless := retrieveMeta("docC")
	urlless.URL = ""

	m := newRetrieveMocks(t)
	m.store.EXPECT().
		DocumentsByID(gomock.Any(), []entities.DocID{"docA", "docB", "docC", "docD"}).
		Return([]entities.DocumentMeta{retrieveMeta("docB"), urlless, retrieveMeta("docA")}, nil).
		Times(1)

	seeds, err := liftDocuments(context.Background(), m.store, []fusedChunk{
		retrieveFused("docA", 0, 0.9),
		retrieveFused("docB", 0, 0.8),
		retrieveFused("docA", 3, 0.7),
		retrieveFused("docC", 0, 0.6),
		retrieveFused("docD", 0, 0.5),
	})
	if err != nil {
		t.Fatalf("liftDocuments: %v", err)
	}

	want := []struct {
		doc       string
		excerpt   string
		relevance float32
	}{
		{doc: "docA", excerpt: "docA chunk 0", relevance: 0.9},
		{doc: "docB", excerpt: "docB chunk 0", relevance: 0.8},
	}
	if len(seeds) != len(want) {
		t.Fatalf("got %d seeds, want %d: %+v", len(seeds), len(want), seeds)
	}
	for i, w := range want {
		if seeds[i].Meta != retrieveMeta(w.doc) {
			t.Errorf("seed %d = %+v, want %s", i, seeds[i].Meta, w.doc)

			continue
		}
		if seeds[i].Excerpt != w.excerpt {
			t.Errorf("seed %s excerpt = %q, want %q", w.doc, seeds[i].Excerpt, w.excerpt)
		}
		assertClose(t, "seed "+w.doc+" relevance", seeds[i].Relevance, w.relevance)
	}
}

func TestLiftDocumentsWithoutChunksAsksTheStoreNothing(t *testing.T) {
	t.Parallel()

	m := newRetrieveMocks(t)

	seeds, err := liftDocuments(context.Background(), m.store, nil)
	if err != nil {
		t.Fatalf("liftDocuments: %v", err)
	}
	if seeds != nil {
		t.Errorf("seeds = %+v, want none", seeds)
	}
}

func TestLiftDocumentsClassifiesHydrationFailure(t *testing.T) {
	t.Parallel()

	m := newRetrieveMocks(t)
	m.store.EXPECT().DocumentsByID(gomock.Any(), []entities.DocID{"docA"}).Return(nil, errRetrieveStore)

	seeds, err := liftDocuments(context.Background(), m.store, []fusedChunk{retrieveFused("docA", 0, 0.9)})
	if seeds != nil {
		t.Errorf("seeds = %+v, want none", seeds)
	}
	assertInternal(t, err, "loading document metadata failed", errRetrieveStore)
}
