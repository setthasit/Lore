package services_test

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"lore/internal/entities"
	"lore/internal/errors/internalerror"
	mock_embedder "lore/internal/mocks/embedder"
	mock_repositories "lore/internal/mocks/repositories"
	"lore/internal/services"
)

const (
	queryTopK        = 5
	queryWalkDepth   = 2
	queryEventWindow = 7 * 24 * time.Hour
)

const (
	queryDefaultTopK        = 12
	queryDefaultWalkDepth   = 3
	queryDefaultEventWindow = 30 * 24 * time.Hour
)

const queryScoreEpsilon = 1e-6

// Fixtures predate the recency horizon, so the unanchored time prior clamps to
// the same factor for every document and ordering follows relevance alone.
const queryRecencyPrior = 0.8

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

	return newQueryFixtureWith(t, services.QueryConfig{
		TopK:        queryTopK,
		WalkDepth:   queryWalkDepth,
		EventWindow: queryEventWindow,
	})
}

func newQueryFixtureWith(t *testing.T, cfg services.QueryConfig) queryFixture {
	t.Helper()

	ctrl := gomock.NewController(t)
	store := mock_repositories.NewMockIndexStore(ctrl)
	emb := mock_embedder.NewMockEmbedder(ctrl)

	return queryFixture{store: store, emb: emb, svc: services.NewQueryService(store, emb, cfg)}
}

func (f queryFixture) expectEmbed(text string) *gomock.Call {
	return f.emb.EXPECT().Embed(gomock.Any(), []string{text}).Return([][]float32{queryVector}, nil)
}

func (f queryFixture) expectRetrieval(query string, filters any, lexical, semantic []entities.ChunkHit) {
	f.expectEmbed(query)
	f.store.EXPECT().SearchLexical(gomock.Any(), query, filters, queryTopK).Return(lexical, nil)
	f.store.EXPECT().SearchVector(gomock.Any(), queryVector, filters, queryTopK).Return(semantic, nil)
}

// Seed metadata is read twice: once to lift chunks to documents, once to open the walk.
func (f queryFixture) expectSeedLoad(ids []entities.DocID, metas ...entities.DocumentMeta) {
	f.store.EXPECT().DocumentsByID(gomock.Any(), ids).Return(metas, nil).Times(2)
}

func (f queryFixture) expectLoad(ids []entities.DocID, metas ...entities.DocumentMeta) *gomock.Call {
	return f.store.EXPECT().DocumentsByID(gomock.Any(), ids).Return(metas, nil)
}

func (f queryFixture) expectNeighbors(ids []entities.DocID, edges ...entities.Edge) *gomock.Call {
	return f.store.EXPECT().Neighbors(gomock.Any(), ids, nil, entities.DirBoth).Return(edges, nil)
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
		CreatedAt: time.Date(2020, 3, 12, 9, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2020, 3, 13, 9, 0, 0, 0, time.UTC),
	}
}

func queryEdge(src, dst entities.DocID) entities.Edge {
	return entities.Edge{Src: src, Dst: dst, Kind: entities.EdgeKindReferencesDoc, Confidence: 1}
}

func assertScore(t *testing.T, what string, got, want float32) {
	t.Helper()

	if diff := got - want; diff > queryScoreEpsilon || diff < -queryScoreEpsilon {
		t.Errorf("%s score = %v, want %v", what, got, want)
	}
}

func assertGaps(t *testing.T, got, want []string) {
	t.Helper()

	if !slices.Equal(got, want) {
		t.Errorf("Gaps = %q, want %q", got, want)
	}
}

func assertChain(t *testing.T, got [][]entities.DocID, want []entities.DocID) {
	t.Helper()

	if len(got) != 1 || !slices.Equal(got[0], want) {
		t.Errorf("Chains = %v, want exactly one chain %v", got, want)
	}
}

func nodeIDs(nodes []entities.EvidenceNode) []entities.DocID {
	ids := make([]entities.DocID, len(nodes))
	for i, node := range nodes {
		ids[i] = node.Doc.ID
	}

	return ids
}

func TestFindDecisionFusesAndLiftsToDocuments(t *testing.T) {
	t.Parallel()

	const question = "why did we choose option B instead of A?"

	f := newQueryFixture(t)
	lexical := []entities.ChunkHit{queryHit("docA", 0), queryHit("docB", 0), queryHit("docA", 3)}
	semantic := []entities.ChunkHit{queryHit("docA", 0), queryHit("docC", 0), queryHit("docB", 0)}
	f.expectRetrieval(question, gomock.Any(), lexical, semantic)

	lifted := []entities.DocID{"docA", "docB", "docC"}
	f.expectSeedLoad(lifted, queryMeta("docC"), queryMeta("docA"), queryMeta("docB"))
	f.expectNeighbors(lifted)

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
	if bundle.Chains != nil {
		t.Errorf("Chains = %v, want none: the walk found no edge", bundle.Chains)
	}
	assertGaps(t, bundle.Gaps, []string{
		"decision: docA (docA) stands alone; no linked discussion",
		"decision: docB (docB) stands alone; no linked discussion",
		"decision: docC (docC) stands alone; no linked discussion",
	})

	want := []struct {
		doc     entities.DocID
		excerpt string
		score   float32
	}{
		{doc: "docA", excerpt: "docA excerpt 0", score: queryRecencyPrior},              // (2/61)/(2/61)
		{doc: "docB", excerpt: "docB excerpt 0", score: 0.9760625 * queryRecencyPrior},  // (1/62+1/63)/(2/61)
		{doc: "docC", excerpt: "docC excerpt 0", score: 0.49193548 * queryRecencyPrior}, // (1/62)/(2/61)
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
	f.expectRetrieval(question, gomock.Eq(wantFilters), nil, nil)

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
	f.expectRetrieval(question, gomock.Any(), nil, []entities.ChunkHit{})

	bundle, err := f.svc.FindDecision(context.Background(), services.FindDecisionRequest{Question: question})
	if err != nil {
		t.Fatalf("FindDecision: %v", err)
	}
	if len(bundle.Nodes) != 0 || bundle.Chains != nil || bundle.Gaps != nil {
		t.Errorf("bundle is not empty: %+v", bundle)
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
	hits := []entities.ChunkHit{queryHit("docA", 0), queryHit("docB", 0), queryHit("docC", 0)}
	f.expectRetrieval(question, gomock.Any(), hits, nil)
	f.expectLoad([]entities.DocID{"docA", "docB", "docC"}, queryMeta("docA"), urlless)
	f.expectLoad([]entities.DocID{"docA"}, queryMeta("docA"))
	f.expectNeighbors([]entities.DocID{"docA"})

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
	assertScore(t, "docA", bundle.Nodes[0].Score, queryRecencyPrior)
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

func TestFindDecisionWindowsSeedRetrievalOnADate(t *testing.T) {
	t.Parallel()

	const question = "why did we roll back?"

	wantWindow := entities.TimeWindow{
		From:       time.Date(2025, 3, 5, 0, 0, 0, 0, time.UTC),
		To:         time.Date(2025, 3, 19, 0, 0, 0, 0, time.UTC),
		Derivation: "date 2025-03-12 ± 7d",
	}

	f := newQueryFixture(t)
	f.expectRetrieval(question, gomock.Eq(entities.Filters{
		CreatedFrom: wantWindow.From,
		CreatedTo:   wantWindow.To,
	}), nil, nil)

	bundle, err := f.svc.FindDecision(context.Background(), services.FindDecisionRequest{
		Question: question,
		Around:   "  2025-03-12  ",
	})
	if err != nil {
		t.Fatalf("FindDecision: %v", err)
	}

	if want := entities.AnchorQuery | entities.AnchorTimeWindow; bundle.Anchor.Kind != want {
		t.Errorf("Anchor.Kind = %d, want %d", bundle.Anchor.Kind, want)
	}
	if bundle.Anchor.Window == nil {
		t.Fatalf("Anchor.Window is nil, want %+v", wantWindow)
	}
	if *bundle.Anchor.Window != wantWindow {
		t.Errorf("Anchor.Window = %+v, want %+v", *bundle.Anchor.Window, wantWindow)
	}
	if bundle.Gaps != nil {
		t.Errorf("Gaps = %q, want none: the date resolved", bundle.Gaps)
	}
}

func TestFindDecisionResolvesFreeTextEventBeforeSeeding(t *testing.T) {
	t.Parallel()

	const (
		question = "why did we choose option B?"
		around   = "the march outage"
	)

	anchor := queryMeta("docEvent")
	wantWindow := entities.TimeWindow{
		From:       anchor.CreatedAt.Add(-queryEventWindow),
		To:         anchor.CreatedAt.Add(queryEventWindow),
		Derivation: `event "the march outage" dated 2020-03-12 via docEvent`,
		AnchoredBy: anchor.ID,
	}
	wantSeedFilters := entities.Filters{
		Source:      "github",
		CreatedFrom: wantWindow.From,
		CreatedTo:   wantWindow.To,
	}

	f := newQueryFixture(t)
	gomock.InOrder(
		f.expectEmbed(around),
		f.store.EXPECT().SearchLexical(gomock.Any(), around, gomock.Eq(entities.Filters{}), queryTopK).
			Return([]entities.ChunkHit{queryHit(anchor.ID, 0)}, nil),
		f.store.EXPECT().SearchVector(gomock.Any(), queryVector, gomock.Eq(entities.Filters{}), queryTopK).
			Return(nil, nil),
		f.expectLoad([]entities.DocID{anchor.ID}, anchor),
		f.expectEmbed(question),
		f.store.EXPECT().SearchLexical(gomock.Any(), question, gomock.Eq(wantSeedFilters), queryTopK).
			Return(nil, nil),
		f.store.EXPECT().SearchVector(gomock.Any(), queryVector, gomock.Eq(wantSeedFilters), queryTopK).
			Return(nil, nil),
	)

	bundle, err := f.svc.FindDecision(context.Background(), services.FindDecisionRequest{
		Question: question,
		Around:   around,
		Source:   "github",
	})
	if err != nil {
		t.Fatalf("FindDecision: %v", err)
	}

	if want := entities.AnchorQuery | entities.AnchorTimeWindow; bundle.Anchor.Kind != want {
		t.Errorf("Anchor.Kind = %d, want %d", bundle.Anchor.Kind, want)
	}
	if bundle.Anchor.Window == nil {
		t.Fatalf("Anchor.Window is nil, want %+v", wantWindow)
	}
	if *bundle.Anchor.Window != wantWindow {
		t.Errorf("Anchor.Window = %+v, want %+v", *bundle.Anchor.Window, wantWindow)
	}
}

func TestFindDecisionProceedsUnwindowedWhenEventUnresolved(t *testing.T) {
	t.Parallel()

	const (
		question = "why did we roll back?"
		around   = "incident X"
	)

	f := newQueryFixture(t)
	f.expectRetrieval(around, gomock.Eq(entities.Filters{}), nil, nil)
	f.expectRetrieval(question, gomock.Eq(entities.Filters{}), []entities.ChunkHit{queryHit("docA", 0)}, nil)
	f.expectSeedLoad([]entities.DocID{"docA"}, queryMeta("docA"))
	f.expectNeighbors([]entities.DocID{"docA"})

	bundle, err := f.svc.FindDecision(context.Background(), services.FindDecisionRequest{
		Question: question,
		Around:   around,
	})
	if err != nil {
		t.Fatalf("FindDecision: %v", err)
	}

	if bundle.Anchor.Kind != entities.AnchorQuery {
		t.Errorf("Anchor.Kind = %d, want %d", bundle.Anchor.Kind, entities.AnchorQuery)
	}
	if bundle.Anchor.Window != nil {
		t.Errorf("Anchor.Window = %+v, want none: the event never resolved", *bundle.Anchor.Window)
	}
	assertGaps(t, bundle.Gaps, []string{
		`could not resolve event "incident X" to a time — nothing in the index matched the event text`,
		"decision: docA (docA) stands alone; no linked discussion",
	})
	if len(bundle.Nodes) != 1 || bundle.Nodes[0].Doc.ID != "docA" {
		t.Errorf("Nodes = %v, want the unwindowed seed docA", nodeIDs(bundle.Nodes))
	}
}

func TestFindDecisionIntersectsWindowWithCallerBounds(t *testing.T) {
	t.Parallel()

	const question = "why did we roll back?"

	windowFrom := time.Date(2025, 3, 5, 0, 0, 0, 0, time.UTC)
	windowTo := time.Date(2025, 3, 19, 0, 0, 0, 0, time.UTC)

	tests := map[string]struct {
		since, until time.Time
		wantFrom     time.Time
		wantTo       time.Time
	}{
		"caller is wider on both sides": {
			since:    time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
			until:    time.Date(2025, 12, 31, 0, 0, 0, 0, time.UTC),
			wantFrom: windowFrom,
			wantTo:   windowTo,
		},
		"caller is narrower on both sides": {
			since:    time.Date(2025, 3, 8, 0, 0, 0, 0, time.UTC),
			until:    time.Date(2025, 3, 15, 0, 0, 0, 0, time.UTC),
			wantFrom: time.Date(2025, 3, 8, 0, 0, 0, 0, time.UTC),
			wantTo:   time.Date(2025, 3, 15, 0, 0, 0, 0, time.UTC),
		},
		"caller is narrower on one side": {
			since:    time.Date(2025, 3, 10, 0, 0, 0, 0, time.UTC),
			until:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			wantFrom: time.Date(2025, 3, 10, 0, 0, 0, 0, time.UTC),
			wantTo:   windowTo,
		},
		"caller bounds only one side": {
			since:    time.Date(2025, 3, 10, 0, 0, 0, 0, time.UTC),
			wantFrom: time.Date(2025, 3, 10, 0, 0, 0, 0, time.UTC),
			wantTo:   windowTo,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			f := newQueryFixture(t)
			f.expectRetrieval(question, gomock.Eq(entities.Filters{
				CreatedFrom: tc.wantFrom,
				CreatedTo:   tc.wantTo,
			}), nil, nil)

			bundle, err := f.svc.FindDecision(context.Background(), services.FindDecisionRequest{
				Question: question,
				Around:   "2025-03-12",
				Since:    tc.since,
				Until:    tc.until,
			})
			if err != nil {
				t.Fatalf("FindDecision: %v", err)
			}
			if bundle.Anchor.Window.From != windowFrom || bundle.Anchor.Window.To != windowTo {
				t.Errorf("reported window = %+v, want the resolved window %s..%s unclamped",
					*bundle.Anchor.Window, windowFrom, windowTo)
			}
		})
	}
}

func TestFindDecisionExpandsSeedThroughGraph(t *testing.T) {
	t.Parallel()

	const question = "why did we choose option B?"

	implementation := queryMeta("docPR")
	implementation.Type = entities.DocTypePR
	edge := queryEdge("docA", implementation.ID)

	f := newQueryFixture(t)
	f.expectRetrieval(question, gomock.Any(), []entities.ChunkHit{queryHit("docA", 0)}, nil)
	f.expectSeedLoad([]entities.DocID{"docA"}, queryMeta("docA"))
	f.expectNeighbors([]entities.DocID{"docA"}, edge)
	f.expectLoad([]entities.DocID{implementation.ID}, implementation)
	f.expectNeighbors([]entities.DocID{implementation.ID})

	bundle, err := f.svc.FindDecision(context.Background(), services.FindDecisionRequest{Question: question})
	if err != nil {
		t.Fatalf("FindDecision: %v", err)
	}

	if len(bundle.Nodes) != 2 {
		t.Fatalf("Nodes = %v, want the seed and its neighbour", nodeIDs(bundle.Nodes))
	}
	seed, reached := bundle.Nodes[0], bundle.Nodes[1]
	if seed.Doc.ID != "docA" || seed.Role != entities.RoleSeed {
		t.Errorf("first node = %s/%q, want docA/%q", seed.Doc.ID, seed.Role, entities.RoleSeed)
	}
	if reached.Doc != implementation {
		t.Errorf("second node = %+v, want %+v", reached.Doc, implementation)
	}
	if reached.Role != entities.RoleLinkedChange {
		t.Errorf("reached role = %q, want %q", reached.Role, entities.RoleLinkedChange)
	}
	if !slices.Equal(reached.Via, []entities.Edge{edge}) {
		t.Errorf("reached Via = %+v, want the traversed edge %+v", reached.Via, edge)
	}
	assertScore(t, "seed", seed.Score, queryRecencyPrior)
	assertScore(t, "reached", reached.Score, 0.6*queryRecencyPrior)
	assertChain(t, bundle.Chains, []entities.DocID{"docA", implementation.ID})
	if bundle.Gaps != nil {
		t.Errorf("Gaps = %q, want none: the seed opened a chain", bundle.Gaps)
	}
}

func TestFindDecisionStopsAtConfiguredWalkDepth(t *testing.T) {
	t.Parallel()

	const question = "why did we choose option B?"

	f := newQueryFixture(t)
	f.expectRetrieval(question, gomock.Any(), []entities.ChunkHit{queryHit("docA", 0)}, nil)
	f.expectSeedLoad([]entities.DocID{"docA"}, queryMeta("docA"))

	// A fourth reachable node stays uncited: queryWalkDepth allows two hops.
	for _, hop := range []struct{ from, to entities.DocID }{{"docA", "docB"}, {"docB", "docC"}, {"docC", "docD"}} {
		f.expectNeighbors([]entities.DocID{hop.from}, queryEdge(hop.from, hop.to)).MaxTimes(1)
		f.expectLoad([]entities.DocID{hop.to}, queryMeta(hop.to)).MaxTimes(1)
	}

	bundle, err := f.svc.FindDecision(context.Background(), services.FindDecisionRequest{Question: question})
	if err != nil {
		t.Fatalf("FindDecision: %v", err)
	}

	want := []entities.DocID{"docA", "docB", "docC"}
	if !slices.Equal(nodeIDs(bundle.Nodes), want) {
		t.Errorf("Nodes = %v, want %v", nodeIDs(bundle.Nodes), want)
	}
	assertChain(t, bundle.Chains, want)
}

func TestFindDecisionReportsSeedsTheWalkNeverLeaves(t *testing.T) {
	t.Parallel()

	const question = "why did we choose option B?"

	edge := queryEdge("docA", "docPR")

	f := newQueryFixture(t)
	hits := []entities.ChunkHit{queryHit("docA", 0), queryHit("docB", 0)}
	f.expectRetrieval(question, gomock.Any(), hits, nil)
	f.expectSeedLoad([]entities.DocID{"docA", "docB"}, queryMeta("docA"), queryMeta("docB"))
	f.expectNeighbors([]entities.DocID{"docA", "docB"}, edge)
	f.expectLoad([]entities.DocID{"docPR"}, queryMeta("docPR"))
	f.expectNeighbors([]entities.DocID{"docPR"})

	bundle, err := f.svc.FindDecision(context.Background(), services.FindDecisionRequest{Question: question})
	if err != nil {
		t.Fatalf("FindDecision: %v", err)
	}

	assertChain(t, bundle.Chains, []entities.DocID{"docA", "docPR"})
	assertGaps(t, bundle.Gaps, []string{"decision: docB (docB) stands alone; no linked discussion"})
}

func TestFindDecisionChainsTwoLinkedSeeds(t *testing.T) {
	t.Parallel()

	const question = "why did we choose option B?"

	f := newQueryFixture(t)
	hits := []entities.ChunkHit{queryHit("docA", 0), queryHit("docB", 0)}
	f.expectRetrieval(question, gomock.Any(), hits, nil)
	f.expectSeedLoad([]entities.DocID{"docA", "docB"}, queryMeta("docA"), queryMeta("docB"))
	f.expectNeighbors([]entities.DocID{"docA", "docB"}, queryEdge("docA", "docB"), queryEdge("docB", "docPR"))
	f.expectLoad([]entities.DocID{"docPR"}, queryMeta("docPR"))
	f.expectNeighbors([]entities.DocID{"docPR"})

	bundle, err := f.svc.FindDecision(context.Background(), services.FindDecisionRequest{Question: question})
	if err != nil {
		t.Fatalf("FindDecision: %v", err)
	}

	want := []entities.DocID{"docA", "docB", "docPR"}
	if !slices.Equal(nodeIDs(bundle.Nodes), want) {
		t.Errorf("Nodes = %v, want %v", nodeIDs(bundle.Nodes), want)
	}
	assertChain(t, bundle.Chains, want)
	assertGaps(t, bundle.Gaps, nil)
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

func TestFindDecisionClassifiesWalkFailure(t *testing.T) {
	t.Parallel()

	const question = "why did we choose option B?"

	f := newQueryFixture(t)
	f.expectRetrieval(question, gomock.Any(), []entities.ChunkHit{queryHit("docA", 0)}, nil)
	f.expectSeedLoad([]entities.DocID{"docA"}, queryMeta("docA"))
	f.store.EXPECT().Neighbors(gomock.Any(), []entities.DocID{"docA"}, nil, entities.DirBoth).
		Return(nil, errQueryStore)

	_, err := f.svc.FindDecision(context.Background(), services.FindDecisionRequest{Question: question})
	if !internalerror.IsInternal(err) {
		t.Fatalf("err = %v (%s), want internal", err, internalerror.KindOf(err))
	}
	if !errors.Is(err, errQueryStore) {
		t.Errorf("err = %v, want the store's cause wrapped", err)
	}
	if !strings.Contains(err.Error(), "graph") {
		t.Errorf("err = %v, want a message naming the graph walk", err)
	}
}

func TestNewQueryServiceAppliesDefaults(t *testing.T) {
	t.Parallel()

	const question = "what did we decide about retries?"

	for _, unset := range []int{0, -3} {
		t.Run(strconv.Itoa(unset), func(t *testing.T) {
			t.Parallel()

			cfg := services.QueryConfig{
				TopK:        unset,
				WalkDepth:   unset,
				EventWindow: time.Duration(unset),
			}

			t.Run("top k and walk depth", func(t *testing.T) {
				t.Parallel()

				f := newQueryFixtureWith(t, cfg)
				f.expectEmbed(question)
				hits := []entities.ChunkHit{queryHit("docA", 0)}
				f.store.EXPECT().
					SearchLexical(gomock.Any(), question, gomock.Any(), queryDefaultTopK).
					Return(hits, nil)
				f.store.EXPECT().
					SearchVector(gomock.Any(), queryVector, gomock.Any(), queryDefaultTopK).
					Return(nil, nil)
				f.expectSeedLoad([]entities.DocID{"docA"}, queryMeta("docA"))

				hops := []struct{ from, to entities.DocID }{
					{"docA", "docB"}, {"docB", "docC"}, {"docC", "docD"}, {"docD", "docE"},
				}
				for _, hop := range hops {
					f.expectNeighbors([]entities.DocID{hop.from}, queryEdge(hop.from, hop.to)).MaxTimes(1)
					f.expectLoad([]entities.DocID{hop.to}, queryMeta(hop.to)).MaxTimes(1)
				}

				bundle, err := f.svc.FindDecision(context.Background(),
					services.FindDecisionRequest{Question: question})
				if err != nil {
					t.Fatalf("FindDecision: %v", err)
				}
				if got := len(bundle.Nodes); got != queryDefaultWalkDepth+1 {
					t.Errorf("Nodes = %v, want the seed plus %d hops",
						nodeIDs(bundle.Nodes), queryDefaultWalkDepth)
				}
			})

			t.Run("event window", func(t *testing.T) {
				t.Parallel()

				at := time.Date(2025, 3, 12, 0, 0, 0, 0, time.UTC)
				wantFilters := entities.Filters{
					CreatedFrom: at.Add(-queryDefaultEventWindow),
					CreatedTo:   at.Add(queryDefaultEventWindow),
				}

				f := newQueryFixtureWith(t, cfg)
				f.expectEmbed(question)
				f.store.EXPECT().
					SearchLexical(gomock.Any(), question, gomock.Eq(wantFilters), queryDefaultTopK).
					Return(nil, nil)
				f.store.EXPECT().
					SearchVector(gomock.Any(), queryVector, gomock.Eq(wantFilters), queryDefaultTopK).
					Return(nil, nil)

				bundle, err := f.svc.FindDecision(context.Background(), services.FindDecisionRequest{
					Question: question,
					Around:   at.Format(time.DateOnly),
				})
				if err != nil {
					t.Fatalf("FindDecision: %v", err)
				}
				if got := bundle.Anchor.Window.Derivation; got != "date 2025-03-12 ± 30d" {
					t.Errorf("window derivation = %q, want the 30d default", got)
				}
			})
		})
	}
}
