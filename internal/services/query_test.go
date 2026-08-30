package services_test

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"lore/internal/entities"
	"lore/internal/errors/internalerror"
	mock_embedder "lore/internal/mocks/embedder"
	mock_repositories "lore/internal/mocks/repositories"
	"lore/internal/services"
)

const queryTopK = 5

const queryScoreEpsilon = 1e-6

var (
	errQueryStore = errors.New("index is on fire")
	errQueryEmbed = errors.New("provider refused the request")
)

var queryVector = []float32{0.25, -0.5, 0.75}

type queryFixture struct {
	store *mock_repositories.MockIndexStore
	emb   *mock_embedder.MockEmbedder
	svc   services.QueryService
}

func newQueryFixture(t *testing.T) queryFixture {
	t.Helper()

	ctrl := gomock.NewController(t)
	store := mock_repositories.NewMockIndexStore(ctrl)
	emb := mock_embedder.NewMockEmbedder(ctrl)

	return queryFixture{store: store, emb: emb, svc: services.NewQueryService(store, emb, queryTopK)}
}

func (f queryFixture) expectEmbed(question string) {
	f.emb.EXPECT().Embed(gomock.Any(), []string{question}).Return([][]float32{queryVector}, nil)
}

func queryHit(doc entities.DocID, ordinal int) entities.ChunkHit {
	return entities.ChunkHit{
		Chunk: entities.Chunk{
			DocID:   doc,
			Ordinal: ordinal,
			Text:    string(doc) + " excerpt " + strconv.Itoa(ordinal),
			Source:  "github",
			DocType: entities.DocTypePage,
		},
		Score: -3.25,
	}
}

func queryMeta(doc entities.DocID) entities.DocumentMeta {
	return entities.DocumentMeta{
		ID:        doc,
		Source:    "github",
		Type:      entities.DocTypePage,
		Title:     "decision: " + string(doc),
		Author:    "dev@example.test",
		URL:       "https://example.test/" + string(doc),
		CreatedAt: time.Date(2025, 3, 12, 9, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2025, 3, 13, 9, 0, 0, 0, time.UTC),
	}
}

func assertScore(t *testing.T, what string, got, want float32) {
	t.Helper()

	if diff := got - want; diff > queryScoreEpsilon || diff < -queryScoreEpsilon {
		t.Errorf("%s score = %v, want %v", what, got, want)
	}
}

func TestFindDecisionFusesAndLiftsToDocuments(t *testing.T) {
	t.Parallel()

	const question = "why did we choose option B instead of A?"

	f := newQueryFixture(t)
	f.expectEmbed(question)

	lexical := []entities.ChunkHit{queryHit("docA", 0), queryHit("docB", 0), queryHit("docA", 3)}
	semantic := []entities.ChunkHit{queryHit("docA", 0), queryHit("docC", 0), queryHit("docB", 0)}
	f.store.EXPECT().SearchLexical(gomock.Any(), question, gomock.Any(), queryTopK).Return(lexical, nil)
	f.store.EXPECT().SearchVector(gomock.Any(), queryVector, gomock.Any(), queryTopK).Return(semantic, nil)
	f.store.EXPECT().
		DocumentsByID(gomock.Any(), []entities.DocID{"docA", "docB", "docC"}).
		Return([]entities.DocumentMeta{queryMeta("docC"), queryMeta("docA"), queryMeta("docB")}, nil)

	bundle, err := f.svc.FindDecision(context.Background(), services.FindDecisionRequest{
		Question: "  " + question + "\n",
	})
	if err != nil {
		t.Fatalf("FindDecision: %v", err)
	}

	if bundle.Question != question {
		t.Errorf("Question = %q, want %q", bundle.Question, question)
	}
	if bundle.Anchor.Kind != entities.AnchorQuery || bundle.Anchor.Query != question {
		t.Errorf("Anchor = %+v, want query anchor for %q", bundle.Anchor, question)
	}
	if bundle.Anchor.Code != nil || bundle.Anchor.Doc != nil || bundle.Anchor.Window != nil {
		t.Errorf("Anchor carries a non-query grounding: %+v", bundle.Anchor)
	}
	if bundle.Chains != nil || bundle.Gaps != nil {
		t.Errorf("Chains/Gaps = %v/%v, want nil until the walk lands", bundle.Chains, bundle.Gaps)
	}

	want := []struct {
		doc     entities.DocID
		excerpt string
		score   float32
	}{
		{doc: "docA", excerpt: "docA excerpt 0", score: 0.032786885}, // 1/61 + 1/61
		{doc: "docB", excerpt: "docB excerpt 0", score: 0.032002048}, // 1/62 + 1/63
		{doc: "docC", excerpt: "docC excerpt 0", score: 0.016129032}, // 1/62
	}
	if len(bundle.Nodes) != len(want) {
		t.Fatalf("got %d nodes, want %d: %+v", len(bundle.Nodes), len(want), bundle.Nodes)
	}
	for i, w := range want {
		node := bundle.Nodes[i]
		if node.Doc.ID != w.doc {
			t.Errorf("node %d = %s, want %s", i, node.Doc.ID, w.doc)

			continue
		}
		if node.Doc != queryMeta(w.doc) {
			t.Errorf("node %s metadata = %+v", w.doc, node.Doc)
		}
		if node.Excerpt != w.excerpt {
			t.Errorf("node %s excerpt = %q, want %q", w.doc, node.Excerpt, w.excerpt)
		}
		if node.Role != entities.RoleSeed {
			t.Errorf("node %s role = %q, want %q", w.doc, node.Role, entities.RoleSeed)
		}
		if node.Via != nil {
			t.Errorf("node %s carries edges %+v, want none for a retrieval hit", w.doc, node.Via)
		}
		assertScore(t, "node "+string(w.doc), node.Score, w.score)
	}
	for i := 1; i < len(bundle.Nodes); i++ {
		if bundle.Nodes[i-1].Score < bundle.Nodes[i].Score {
			t.Errorf("nodes are not descending: %v then %v", bundle.Nodes[i-1].Score, bundle.Nodes[i].Score)
		}
	}
}

func TestFindDecisionPushesFiltersIntoBothSearches(t *testing.T) {
	t.Parallel()

	const question = "which database did we pick?"

	since := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2025, 6, 30, 0, 0, 0, 0, time.UTC)
	wantFilters := entities.Filters{
		Source:      "github",
		RepoRef:     "github:acme/lore",
		DocType:     entities.DocTypePR,
		CreatedFrom: since,
		CreatedTo:   until,
	}

	f := newQueryFixture(t)
	f.expectEmbed(question)
	f.store.EXPECT().
		SearchLexical(gomock.Any(), question, gomock.Eq(wantFilters), queryTopK).
		Return(nil, nil)
	f.store.EXPECT().
		SearchVector(gomock.Any(), queryVector, gomock.Eq(wantFilters), queryTopK).
		Return(nil, nil)

	if _, err := f.svc.FindDecision(context.Background(), services.FindDecisionRequest{
		Question: question,
		Source:   "github",
		Repo:     "github:acme/lore",
		DocType:  string(entities.DocTypePR),
		Since:    since,
		Until:    until,
	}); err != nil {
		t.Fatalf("FindDecision: %v", err)
	}
}

func TestFindDecisionZeroHits(t *testing.T) {
	t.Parallel()

	const question = "was anything ever decided about billing?"

	f := newQueryFixture(t)
	f.expectEmbed(question)
	f.store.EXPECT().SearchLexical(gomock.Any(), question, gomock.Any(), queryTopK).Return(nil, nil)
	f.store.EXPECT().SearchVector(gomock.Any(), queryVector, gomock.Any(), queryTopK).
		Return([]entities.ChunkHit{}, nil)

	bundle, err := f.svc.FindDecision(context.Background(), services.FindDecisionRequest{Question: question})
	if err != nil {
		t.Fatalf("FindDecision: %v", err)
	}
	if len(bundle.Nodes) != 0 {
		t.Errorf("got %d nodes, want none: %+v", len(bundle.Nodes), bundle.Nodes)
	}
	if bundle.Question != question || bundle.Anchor.Kind != entities.AnchorQuery {
		t.Errorf("empty bundle is not self-describing: %+v", bundle)
	}
}

func TestFindDecisionDropsNodesWithoutURL(t *testing.T) {
	t.Parallel()

	const question = "what happened to the old scheduler?"

	urlless := queryMeta("docC")
	urlless.URL = ""

	f := newQueryFixture(t)
	f.expectEmbed(question)
	hits := []entities.ChunkHit{queryHit("docA", 0), queryHit("docB", 0), queryHit("docC", 0)}
	f.store.EXPECT().SearchLexical(gomock.Any(), question, gomock.Any(), queryTopK).Return(hits, nil)
	f.store.EXPECT().SearchVector(gomock.Any(), queryVector, gomock.Any(), queryTopK).Return(nil, nil)
	f.store.EXPECT().
		DocumentsByID(gomock.Any(), []entities.DocID{"docA", "docB", "docC"}).
		Return([]entities.DocumentMeta{queryMeta("docA"), urlless}, nil)

	bundle, err := f.svc.FindDecision(context.Background(), services.FindDecisionRequest{Question: question})
	if err != nil {
		t.Fatalf("FindDecision: %v", err)
	}
	if len(bundle.Nodes) != 1 {
		t.Fatalf("got %d nodes, want only docA: %+v", len(bundle.Nodes), bundle.Nodes)
	}
	if bundle.Nodes[0].Doc.ID != "docA" {
		t.Errorf("surviving node = %s, want docA", bundle.Nodes[0].Doc.ID)
	}
	assertScore(t, "docA", bundle.Nodes[0].Score, 0.016393442) // 1/61
}

func TestFindDecisionRejectsEmptyQuestion(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"empty":           "",
		"whitespace only": " \t\n ",
	}

	for name, question := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			// No expectations: an unusable question must not reach the index.
			f := newQueryFixture(t)

			_, err := f.svc.FindDecision(context.Background(),
				services.FindDecisionRequest{Question: question})
			if !internalerror.IsBadRequest(err) {
				t.Fatalf("err = %v (%s), want bad request", err, internalerror.KindOf(err))
			}
		})
	}
}

func TestFindDecisionRefusesAround(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"free text": "incident X",
		"iso date":  "  2025-03-12  ",
	}

	for name, around := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			f := newQueryFixture(t)

			_, err := f.svc.FindDecision(context.Background(), services.FindDecisionRequest{
				Question: "why did we roll back?",
				Around:   around,
			})
			if !internalerror.IsPrecondition(err) {
				t.Fatalf("err = %v (%s), want precondition", err, internalerror.KindOf(err))
			}
		})
	}
}

func TestFindDecisionClassifiesEmbedderFailure(t *testing.T) {
	t.Parallel()

	const question = "why did we choose option B?"

	f := newQueryFixture(t)
	f.emb.EXPECT().Embed(gomock.Any(), []string{question}).Return(nil, errQueryEmbed)

	_, err := f.svc.FindDecision(context.Background(), services.FindDecisionRequest{Question: question})
	if !internalerror.IsInternal(err) {
		t.Fatalf("err = %v (%s), want internal", err, internalerror.KindOf(err))
	}
	if !errors.Is(err, errQueryEmbed) {
		t.Errorf("err = %v, want the embedder's cause wrapped", err)
	}
}

func TestFindDecisionRejectsMisalignedEmbeddings(t *testing.T) {
	t.Parallel()

	const question = "why did we choose option B?"

	tests := map[string][][]float32{
		"no vectors":  {},
		"two vectors": {queryVector, queryVector},
	}

	for name, vectors := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			f := newQueryFixture(t)
			f.emb.EXPECT().Embed(gomock.Any(), []string{question}).Return(vectors, nil)

			_, err := f.svc.FindDecision(context.Background(),
				services.FindDecisionRequest{Question: question})
			if !internalerror.IsInternal(err) {
				t.Fatalf("err = %v (%s), want internal", err, internalerror.KindOf(err))
			}
		})
	}
}

func TestFindDecisionClassifiesStoreFailures(t *testing.T) {
	t.Parallel()

	const question = "why did we choose option B?"

	tests := map[string]func(f queryFixture){
		"lexical search": func(f queryFixture) {
			f.store.EXPECT().SearchLexical(gomock.Any(), question, gomock.Any(), queryTopK).
				Return(nil, errQueryStore)
		},
		"vector search": func(f queryFixture) {
			f.store.EXPECT().SearchLexical(gomock.Any(), question, gomock.Any(), queryTopK).
				Return(nil, nil)
			f.store.EXPECT().SearchVector(gomock.Any(), queryVector, gomock.Any(), queryTopK).
				Return(nil, errQueryStore)
		},
		"document hydration": func(f queryFixture) {
			f.store.EXPECT().SearchLexical(gomock.Any(), question, gomock.Any(), queryTopK).
				Return([]entities.ChunkHit{queryHit("docA", 0)}, nil)
			f.store.EXPECT().SearchVector(gomock.Any(), queryVector, gomock.Any(), queryTopK).
				Return(nil, nil)
			f.store.EXPECT().DocumentsByID(gomock.Any(), []entities.DocID{"docA"}).
				Return(nil, errQueryStore)
		},
	}

	for name, expect := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			f := newQueryFixture(t)
			f.expectEmbed(question)
			expect(f)

			_, err := f.svc.FindDecision(context.Background(),
				services.FindDecisionRequest{Question: question})
			if !internalerror.IsInternal(err) {
				t.Fatalf("err = %v (%s), want internal", err, internalerror.KindOf(err))
			}
			if !errors.Is(err, errQueryStore) {
				t.Errorf("err = %v, want the store's cause wrapped", err)
			}
		})
	}
}

func TestNewQueryServiceDefaultsTopK(t *testing.T) {
	t.Parallel()

	const question = "what did we decide about retries?"

	for _, topK := range []int{0, -3} {
		t.Run(strconv.Itoa(topK), func(t *testing.T) {
			t.Parallel()

			ctrl := gomock.NewController(t)
			store := mock_repositories.NewMockIndexStore(ctrl)
			emb := mock_embedder.NewMockEmbedder(ctrl)
			emb.EXPECT().Embed(gomock.Any(), []string{question}).
				Return([][]float32{queryVector}, nil)
			store.EXPECT().SearchLexical(gomock.Any(), question, gomock.Any(), gomock.Not(gomock.Eq(topK))).
				Return(nil, nil)
			store.EXPECT().SearchVector(gomock.Any(), queryVector, gomock.Any(), gomock.Not(gomock.Eq(topK))).
				Return(nil, nil)

			svc := services.NewQueryService(store, emb, topK)
			if _, err := svc.FindDecision(context.Background(),
				services.FindDecisionRequest{Question: question}); err != nil {
				t.Fatalf("FindDecision: %v", err)
			}
		})
	}
}
