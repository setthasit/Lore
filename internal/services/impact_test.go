package services_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"go.uber.org/mock/gomock"

	"github.com/setthasit/Lore/internal/entities"
	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/internal/mocks/lore"
	mock_repositories "github.com/setthasit/Lore/internal/mocks/repositories"
	"github.com/setthasit/Lore/internal/services"
	"github.com/setthasit/Lore/sdk"
)

const (
	impactTopK      = 5
	impactWalkDepth = 2
)

// The service's excerpt budget, restated so a drift in either constant fails here.
const impactExcerptChars = 500

const impactDay = 24 * time.Hour

const (
	impactAnchorID   lore.DocID = "notion:page:decision"
	impactEarlierID  lore.DocID = "notion:page:discussion"
	impactLaterID    lore.DocID = "notion:page:postmortem"
	impactUnlinkedID lore.DocID = "notion:page:runbook"
	impactSameSecID  lore.DocID = "notion:page:memo"
)

const (
	impactAnchorTitle = "decision: adopt option B"
	impactAnchorBody  = "we chose option B over option A after the March outage"
	impactRef         = "https://notion.so/decision-option-b"
)

var errImpactStore = errors.New("index is on fire")

var impactVector = []float32{0.5, -0.25, 0.125}

var impactAt = time.Date(2025, 3, 12, 9, 0, 0, 0, time.UTC)

var (
	impactAnchorMeta   = impactMeta(impactAnchorID, impactAnchorTitle, impactAt)
	impactEarlierMeta  = impactMeta(impactEarlierID, "discussion: option A vs B", impactAt.Add(-2*impactDay))
	impactLaterMeta    = impactMeta(impactLaterID, "postmortem: option B fallout", impactAt.Add(7*impactDay))
	impactUnlinkedMeta = impactMeta(impactUnlinkedID, "runbook: rolling option B back", impactAt.Add(14*impactDay))
	impactSameSecMeta  = impactMeta(impactSameSecID, "memo: option B is live", impactAt)
)

var (
	impactLaterCitesAnchor   = impactEdge(impactLaterID, impactAnchorID)
	impactAnchorCitesEarlier = impactEdge(impactAnchorID, impactEarlierID)
)

func impactMeta(id lore.DocID, title string, createdAt time.Time) entities.DocumentMeta {
	return entities.DocumentMeta{
		ID:        id,
		Source:    "notion",
		Type:      lore.DocTypePage,
		Title:     title,
		Author:    "dev@example.test",
		URL:       "https://example.test/" + string(id),
		CreatedAt: createdAt,
		UpdatedAt: createdAt,
	}
}

func impactEdge(src, dst lore.DocID) entities.Edge {
	return entities.Edge{Src: src, Dst: dst, Kind: entities.EdgeKindReferencesDoc, Confidence: 1}
}

func impactHit(id lore.DocID) entities.ChunkHit {
	return entities.ChunkHit{
		Chunk: entities.Chunk{
			DocID:   id,
			Text:    string(id) + " excerpt",
			Source:  "notion",
			DocType: lore.DocTypePage,
		},
		Score: -2.5,
	}
}

func impactDefaultQuestion(title string) string {
	return "consequences, follow-ups, incidents related to " + title
}

func impactEmbedText(question, excerpt string) string {
	return question + "\n\n" + excerpt
}

func impactAfterAnchor() gomock.Matcher {
	return gomock.Eq(entities.Filters{CreatedFrom: impactAt})
}

type impactFixture struct {
	store *mock_repositories.MockIndexStore
	emb   *mock_lore.MockEmbedder
	svc   services.ImpactService
}

func newImpactFixture(t *testing.T) impactFixture {
	t.Helper()

	ctrl := gomock.NewController(t)
	store := mock_repositories.NewMockIndexStore(ctrl)
	emb := mock_lore.NewMockEmbedder(ctrl)
	cfg := services.QueryConfig{TopK: impactTopK, WalkDepth: impactWalkDepth}

	return impactFixture{store: store, emb: emb, svc: services.NewImpactService(store, emb, cfg)}
}

func (f impactFixture) expectResolve(ref string, candidates ...entities.DocumentMeta) *gomock.Call {
	return f.store.EXPECT().ResolveRef(gomock.Any(), ref).Return(candidates, nil)
}

func (f impactFixture) expectBody(id lore.DocID, body string) *gomock.Call {
	return f.store.EXPECT().DocumentsWithBody(gomock.Any(), []lore.DocID{id}).
		Return([]lore.Document{{ID: id, Body: body}}, nil)
}

func (f impactFixture) expectMetas(ids []lore.DocID, metas ...entities.DocumentMeta) *gomock.Call {
	return f.store.EXPECT().DocumentsByID(gomock.Any(), ids).Return(metas, nil)
}

func (f impactFixture) expectNeighbors(ids []lore.DocID, edges ...entities.Edge) *gomock.Call {
	return f.store.EXPECT().Neighbors(gomock.Any(), ids, nil, entities.DirBoth).Return(edges, nil)
}

func (f impactFixture) expectSearch(text string, filters gomock.Matcher, hits ...entities.ChunkHit) {
	f.emb.EXPECT().Embed(gomock.Any(), []string{text}).Return([][]float32{impactVector}, nil)
	f.store.EXPECT().SearchLexical(gomock.Any(), text, filters, impactTopK).Return(hits, nil)
	f.store.EXPECT().SearchVector(gomock.Any(), impactVector, filters, impactTopK).Return(nil, nil)
}

func (f impactFixture) expectAnchorByRef() {
	f.expectResolve(impactRef, impactAnchorMeta)
	f.expectBody(impactAnchorID, impactAnchorBody)
	f.expectMetas([]lore.DocID{impactAnchorID}, impactAnchorMeta)
}

func impactRoles(nodes []entities.EvidenceNode) []string {
	roles := make([]string, len(nodes))
	for i, node := range nodes {
		roles[i] = node.Role
	}

	return roles
}

func assertImpactRoles(t *testing.T, nodes []entities.EvidenceNode, want []string) {
	t.Helper()

	if got := impactRoles(nodes); !slices.Equal(got, want) {
		t.Errorf("roles = %q, want %q", got, want)
	}
}

func assertImpactNodes(t *testing.T, nodes []entities.EvidenceNode, want []lore.DocID) {
	t.Helper()

	if got := nodeIDs(nodes); !slices.Equal(got, want) {
		t.Fatalf("Nodes = %v, want %v", got, want)
	}
}

func assertImpactAnchorDoc(t *testing.T, got *entities.DocRef) {
	t.Helper()

	want := entities.DocRef{
		ID:        impactAnchorID,
		Title:     impactAnchorTitle,
		URL:       impactAnchorMeta.URL,
		CreatedAt: impactAt,
	}
	if got == nil || *got != want {
		t.Errorf("Anchor.Doc = %+v, want %+v", got, want)
	}
}

func TestImpactOfReturnsAChronologicalTimeline(t *testing.T) {
	t.Parallel()

	question := impactDefaultQuestion(impactAnchorTitle)

	f := newImpactFixture(t)
	f.expectAnchorByRef()

	// Both edges touch the anchor; the earlier document is pruned by the anchor time.
	f.expectNeighbors([]lore.DocID{impactAnchorID}, impactLaterCitesAnchor, impactAnchorCitesEarlier)
	f.expectMetas([]lore.DocID{impactLaterID, impactEarlierID}, impactLaterMeta, impactEarlierMeta)
	f.expectNeighbors([]lore.DocID{impactLaterID})

	f.expectSearch(impactEmbedText(question, impactAnchorBody), impactAfterAnchor(),
		impactHit(impactUnlinkedID), impactHit(impactSameSecID))
	f.expectMetas([]lore.DocID{impactUnlinkedID, impactSameSecID}, impactUnlinkedMeta, impactSameSecMeta)

	bundle, err := f.svc.ImpactOf(context.Background(), services.ImpactRequest{Ref: "  " + impactRef + "\n"})
	if err != nil {
		t.Fatalf("ImpactOf: %v", err)
	}

	assertImpactNodes(t, bundle.Nodes, []lore.DocID{impactAnchorID, impactLaterID, impactUnlinkedID})
	assertImpactRoles(t, bundle.Nodes, []string{
		entities.RoleSeed, entities.RoleFollowUp, entities.RoleSemanticMatch,
	})

	anchor, followUp, match := bundle.Nodes[0], bundle.Nodes[1], bundle.Nodes[2]
	if anchor.Doc != impactAnchorMeta || anchor.Excerpt != impactAnchorBody || anchor.Via != nil {
		t.Errorf("anchor node = %+v, want %+v with the body excerpt and no edges", anchor, impactAnchorMeta)
	}
	assertScore(t, "anchor", anchor.Score, 1)

	if followUp.Doc != impactLaterMeta || followUp.Excerpt != "" {
		t.Errorf("follow-up node = %+v, want %+v with no excerpt", followUp, impactLaterMeta)
	}
	if !slices.Equal(followUp.Via, []entities.Edge{impactLaterCitesAnchor}) {
		t.Errorf("follow-up Via = %+v, want the traversed edge %+v", followUp.Via, impactLaterCitesAnchor)
	}
	assertScore(t, "follow-up", followUp.Score, 0.6)

	if match.Doc != impactUnlinkedMeta || match.Excerpt != string(impactUnlinkedID)+" excerpt" {
		t.Errorf("semantic node = %+v, want %+v with its retrieval excerpt", match, impactUnlinkedMeta)
	}
	if match.Via != nil {
		t.Errorf("semantic node carries edges %+v, want none for a retrieval hit", match.Via)
	}
	assertScore(t, "semantic match", match.Score, 1)

	if bundle.Question != question {
		t.Errorf("Question = %q, want %q", bundle.Question, question)
	}
	if bundle.Anchor.Kind != entities.AnchorDocument {
		t.Errorf("Anchor.Kind = %d, want %d", bundle.Anchor.Kind, entities.AnchorDocument)
	}
	if bundle.Anchor.Query != "" || bundle.Anchor.Code != nil || bundle.Anchor.Window != nil {
		t.Errorf("Anchor carries a non-document grounding: %+v", bundle.Anchor)
	}
	assertImpactAnchorDoc(t, bundle.Anchor.Doc)
	assertChain(t, bundle.Chains, []lore.DocID{impactAnchorID, impactLaterID})
	assertGaps(t, bundle.Gaps, nil)
}

func TestImpactOfInterpretsFreeTextAsTheAnchor(t *testing.T) {
	t.Parallel()

	const query = "the option B decision"

	f := newImpactFixture(t)
	f.expectResolve(query)
	f.expectSearch(query, gomock.Eq(entities.Filters{}), impactHit(impactAnchorID), impactHit(impactUnlinkedID))
	f.expectMetas([]lore.DocID{impactAnchorID, impactUnlinkedID}, impactAnchorMeta, impactUnlinkedMeta)
	f.expectBody(impactAnchorID, impactAnchorBody)
	f.expectMetas([]lore.DocID{impactAnchorID}, impactAnchorMeta)
	f.expectNeighbors([]lore.DocID{impactAnchorID})
	f.expectSearch(
		impactEmbedText(impactDefaultQuestion(impactAnchorTitle), impactAnchorBody),
		impactAfterAnchor())

	bundle, err := f.svc.ImpactOf(context.Background(), services.ImpactRequest{Ref: "  " + query + "  "})
	if err != nil {
		t.Fatalf("ImpactOf: %v", err)
	}

	if want := entities.AnchorDocument | entities.AnchorQuery; bundle.Anchor.Kind != want {
		t.Errorf("Anchor.Kind = %d, want %d", bundle.Anchor.Kind, want)
	}
	if bundle.Anchor.Query != query {
		t.Errorf("Anchor.Query = %q, want the trimmed input %q", bundle.Anchor.Query, query)
	}
	assertImpactAnchorDoc(t, bundle.Anchor.Doc)
	assertImpactNodes(t, bundle.Nodes, []lore.DocID{impactAnchorID})
}

func TestImpactOfDefaultsTheQuestionToTheAnchorTitle(t *testing.T) {
	t.Parallel()

	want := impactDefaultQuestion(impactAnchorTitle)

	f := newImpactFixture(t)
	f.expectAnchorByRef()
	f.expectNeighbors([]lore.DocID{impactAnchorID})
	f.expectSearch(impactEmbedText(want, impactAnchorBody), impactAfterAnchor())

	bundle, err := f.svc.ImpactOf(context.Background(), services.ImpactRequest{Ref: impactRef})
	if err != nil {
		t.Fatalf("ImpactOf: %v", err)
	}
	if bundle.Question != want {
		t.Errorf("Question = %q, want %q", bundle.Question, want)
	}
}

func TestImpactOfHonoursAnExplicitQuestion(t *testing.T) {
	t.Parallel()

	const asked = "what incidents followed option B?"

	f := newImpactFixture(t)
	f.expectAnchorByRef()
	f.expectNeighbors([]lore.DocID{impactAnchorID})
	f.expectSearch(impactEmbedText(asked, impactAnchorBody), impactAfterAnchor())

	bundle, err := f.svc.ImpactOf(context.Background(), services.ImpactRequest{
		Ref:      impactRef,
		Question: "  " + asked + "\t",
	})
	if err != nil {
		t.Fatalf("ImpactOf: %v", err)
	}
	if bundle.Question != asked {
		t.Errorf("Question = %q, want the explicit question %q", bundle.Question, asked)
	}
}

func TestImpactOfCitesADoublyFoundDocumentOnce(t *testing.T) {
	t.Parallel()

	question := impactDefaultQuestion(impactAnchorTitle)

	f := newImpactFixture(t)
	f.expectAnchorByRef()
	f.expectNeighbors([]lore.DocID{impactAnchorID}, impactLaterCitesAnchor)
	// Once to admit the walk candidate, once to lift the same document from retrieval.
	f.expectMetas([]lore.DocID{impactLaterID}, impactLaterMeta).Times(2)
	f.expectNeighbors([]lore.DocID{impactLaterID})
	f.expectSearch(impactEmbedText(question, impactAnchorBody), impactAfterAnchor(), impactHit(impactLaterID))

	bundle, err := f.svc.ImpactOf(context.Background(), services.ImpactRequest{Ref: impactRef})
	if err != nil {
		t.Fatalf("ImpactOf: %v", err)
	}

	assertImpactNodes(t, bundle.Nodes, []lore.DocID{impactAnchorID, impactLaterID})
	assertImpactRoles(t, bundle.Nodes, []string{entities.RoleSeed, entities.RoleFollowUp})
	if !slices.Equal(bundle.Nodes[1].Via, []entities.Edge{impactLaterCitesAnchor}) {
		t.Errorf("Via = %+v, want the graph provenance kept over the retrieval hit", bundle.Nodes[1].Via)
	}
}

func TestImpactOfReportsAnEmptyWindowAsAGap(t *testing.T) {
	t.Parallel()

	question := impactDefaultQuestion(impactAnchorTitle)

	f := newImpactFixture(t)
	f.expectAnchorByRef()
	f.expectNeighbors([]lore.DocID{impactAnchorID})
	f.expectSearch(impactEmbedText(question, impactAnchorBody), impactAfterAnchor(), impactHit(impactSameSecID))
	f.expectMetas([]lore.DocID{impactSameSecID}, impactSameSecMeta)

	bundle, err := f.svc.ImpactOf(context.Background(), services.ImpactRequest{Ref: impactRef})
	if err != nil {
		t.Fatalf("ImpactOf: %v", err)
	}

	assertImpactNodes(t, bundle.Nodes, []lore.DocID{impactAnchorID})
	assertGaps(t, bundle.Gaps, []string{
		"no follow-up evidence after 2025-03-12",
		impactAnchorTitle + " (" + string(impactAnchorID) + ") stands alone; no linked discussion",
	})
}

func TestImpactOfRejectsAnAmbiguousRef(t *testing.T) {
	t.Parallel()

	const ref = "PROJ-4521"

	// No walk and no retrieval expectations: ambiguity stops the pipeline.
	f := newImpactFixture(t)
	f.expectResolve(ref, impactAnchorMeta, impactLaterMeta)

	_, err := f.svc.ImpactOf(context.Background(), services.ImpactRequest{Ref: ref})
	if !internalerror.IsBadRequest(err) {
		t.Fatalf("err = %v (%s), want bad request", err, internalerror.KindOf(err))
	}
	for _, want := range []string{
		ref,
		string(impactAnchorID), impactAnchorMeta.Title, impactAnchorMeta.URL,
		string(impactLaterID), impactLaterMeta.Title, impactLaterMeta.URL,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to name %q", err, want)
		}
	}
}

func TestImpactOfReportsUnknownFreeText(t *testing.T) {
	t.Parallel()

	const query = "the great sourdough migration"

	f := newImpactFixture(t)
	f.expectResolve(query)
	f.expectSearch(query, gomock.Eq(entities.Filters{}))

	_, err := f.svc.ImpactOf(context.Background(), services.ImpactRequest{Ref: query})
	if !internalerror.IsNotFound(err) {
		t.Fatalf("err = %v (%s), want not found", err, internalerror.KindOf(err))
	}
	if !strings.Contains(err.Error(), query) {
		t.Errorf("err = %v, want it to name the input %q", err, query)
	}
}

func TestImpactOfRejectsAnEmptyRef(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"empty":           "",
		"whitespace only": " \t\n ",
	}

	for name, ref := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			// No expectations: an unusable input must not reach the index.
			f := newImpactFixture(t)

			_, err := f.svc.ImpactOf(context.Background(), services.ImpactRequest{Ref: ref})
			if !internalerror.IsBadRequest(err) {
				t.Fatalf("err = %v (%s), want bad request", err, internalerror.KindOf(err))
			}
		})
	}
}

func TestImpactOfRefusesAnAnchorWithoutAURL(t *testing.T) {
	t.Parallel()

	urlless := impactAnchorMeta
	urlless.URL = ""

	f := newImpactFixture(t)
	f.expectResolve(impactRef, urlless)

	_, err := f.svc.ImpactOf(context.Background(), services.ImpactRequest{Ref: impactRef})
	if !internalerror.IsNotFound(err) {
		t.Fatalf("err = %v (%s), want not found", err, internalerror.KindOf(err))
	}
	for _, want := range []string{string(impactAnchorID), impactAnchorTitle, "URL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to name %q", err, want)
		}
	}
}

func TestImpactOfReportsAMissingAnchorBody(t *testing.T) {
	t.Parallel()

	f := newImpactFixture(t)
	f.expectResolve(impactRef, impactAnchorMeta)
	f.store.EXPECT().DocumentsWithBody(gomock.Any(), []lore.DocID{impactAnchorID}).Return(nil, nil)

	_, err := f.svc.ImpactOf(context.Background(), services.ImpactRequest{Ref: impactRef})
	if !internalerror.IsNotFound(err) {
		t.Fatalf("err = %v (%s), want not found", err, internalerror.KindOf(err))
	}
	if !strings.Contains(err.Error(), string(impactAnchorID)) {
		t.Errorf("err = %v, want it to name %s", err, impactAnchorID)
	}
}

func TestImpactOfTruncatesTheAnchorExcerptOnARuneBoundary(t *testing.T) {
	t.Parallel()

	// The budget lands two bytes into a four-byte rune, so a byte cut splits it.
	const splitRuneAt = impactExcerptChars - 2

	body := strings.Repeat("a", splitRuneAt) + strings.Repeat("🔥", 4)
	if utf8.ValidString(body[:impactExcerptChars]) {
		t.Fatalf("fixture misses the boundary: body[:%d] is already valid UTF-8", impactExcerptChars)
	}
	wantExcerpt := body[:splitRuneAt]

	f := newImpactFixture(t)
	f.expectResolve(impactRef, impactAnchorMeta)
	f.expectBody(impactAnchorID, body)
	f.expectMetas([]lore.DocID{impactAnchorID}, impactAnchorMeta)
	f.expectNeighbors([]lore.DocID{impactAnchorID})
	f.expectSearch(
		impactEmbedText(impactDefaultQuestion(impactAnchorTitle), wantExcerpt),
		impactAfterAnchor())

	bundle, err := f.svc.ImpactOf(context.Background(), services.ImpactRequest{Ref: impactRef})
	if err != nil {
		t.Fatalf("ImpactOf: %v", err)
	}

	excerpt := bundle.Nodes[0].Excerpt
	if excerpt != wantExcerpt {
		t.Errorf("excerpt = %d bytes, want the %d bytes before the split rune", len(excerpt), len(wantExcerpt))
	}
	if !utf8.ValidString(excerpt) {
		t.Error("excerpt is not valid UTF-8: the cut split a rune")
	}
	if len(excerpt) > impactExcerptChars || !strings.HasPrefix(body, excerpt) {
		t.Errorf("excerpt = %d bytes, want a body prefix within %d", len(excerpt), impactExcerptChars)
	}
}

func TestImpactOfClassifiesStoreFailures(t *testing.T) {
	t.Parallel()

	retrieval := impactEmbedText(impactDefaultQuestion(impactAnchorTitle), impactAnchorBody)

	tests := map[string]struct {
		expect func(f impactFixture)
		names  string
	}{
		"ref resolution": {
			expect: func(f impactFixture) {
				f.store.EXPECT().ResolveRef(gomock.Any(), impactRef).Return(nil, errImpactStore)
			},
			names: "ref",
		},
		"anchor body": {
			expect: func(f impactFixture) {
				f.expectResolve(impactRef, impactAnchorMeta)
				f.store.EXPECT().DocumentsWithBody(gomock.Any(), []lore.DocID{impactAnchorID}).
					Return(nil, errImpactStore)
			},
			names: "body",
		},
		"graph walk": {
			expect: func(f impactFixture) {
				f.expectAnchorByRef()
				f.store.EXPECT().
					Neighbors(gomock.Any(), []lore.DocID{impactAnchorID}, nil, entities.DirBoth).
					Return(nil, errImpactStore)
			},
			names: "graph",
		},
		"semantic search": {
			expect: func(f impactFixture) {
				f.expectAnchorByRef()
				f.expectNeighbors([]lore.DocID{impactAnchorID})
				f.emb.EXPECT().Embed(gomock.Any(), []string{retrieval}).
					Return([][]float32{impactVector}, nil)
				f.store.EXPECT().
					SearchLexical(gomock.Any(), retrieval, impactAfterAnchor(), impactTopK).
					Return(nil, errImpactStore)
			},
			names: "lexical",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			f := newImpactFixture(t)
			tc.expect(f)

			_, err := f.svc.ImpactOf(context.Background(), services.ImpactRequest{Ref: impactRef})
			if !internalerror.IsInternal(err) {
				t.Fatalf("err = %v (%s), want internal", err, internalerror.KindOf(err))
			}
			if !errors.Is(err, errImpactStore) {
				t.Errorf("err = %v, want the store's cause wrapped", err)
			}
			if !strings.Contains(err.Error(), tc.names) {
				t.Errorf("err = %v, want a message naming %q", err, tc.names)
			}
		})
	}
}
