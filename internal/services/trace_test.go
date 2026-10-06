package services_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/setthasit/Lore/internal/entities"
	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/internal/mocks/lore"
	mock_repositories "github.com/setthasit/Lore/internal/mocks/repositories"
	"github.com/setthasit/Lore/internal/services"
	"github.com/setthasit/Lore/sdk"
)

const (
	traceRef  = "PROJ-4521"
	traceBody = "we chose option B; option A lost on operational cost. Both paragraphs, verbatim."

	traceExcerptCap    = 8000
	traceFocus         = "how fast forward uses a closed form"
	traceFocusTopK     = 3
	traceFocusLimit    = 1000
	traceStandaloneGap = "decision: docA (docA) stands alone; no linked discussion"
)

var (
	errTraceStore = errors.New("index is unavailable")
	errTraceEmbed = errors.New("provider refused the request")
)

var traceVector = []float32{0.5, -0.25, 0.125}

var (
	traceOversizeBody    = strings.Repeat("a", traceExcerptCap) + strings.Repeat("b", 70_000-traceExcerptCap)
	traceOversizeExcerpt = strings.Repeat("a", traceExcerptCap) + "\n\n" +
		"[truncated: 8,000 of 70,000 characters shown. Pass focus with a question to get the passages that match it.]"
)

type traceFixture struct {
	store *mock_repositories.MockIndexStore
	emb   *mock_lore.MockEmbedder
	svc   services.TraceService
}

func newTraceFixture(t *testing.T) traceFixture {
	t.Helper()

	ctrl := gomock.NewController(t)
	store := mock_repositories.NewMockIndexStore(ctrl)
	emb := mock_lore.NewMockEmbedder(ctrl)

	return traceFixture{store: store, emb: emb, svc: services.NewTraceService(store, emb)}
}

func (f traceFixture) expectResolve(candidates ...entities.DocumentMeta) *gomock.Call {
	return f.store.EXPECT().ResolveRef(gomock.Any(), traceRef).Return(candidates, nil)
}

func (f traceFixture) expectBody(id lore.DocID, body string) *gomock.Call {
	return f.store.EXPECT().DocumentsWithBody(gomock.Any(), []lore.DocID{id}).
		Return([]lore.Document{{ID: id, Body: body}}, nil)
}

func (f traceFixture) expectMetas(ids []lore.DocID, metas ...entities.DocumentMeta) *gomock.Call {
	return f.store.EXPECT().DocumentsByID(gomock.Any(), ids).Return(metas, nil)
}

func (f traceFixture) expectNeighbors(
	dir entities.Direction,
	ids []lore.DocID,
	edges ...entities.Edge,
) *gomock.Call {
	return f.store.EXPECT().Neighbors(gomock.Any(), ids, nil, dir).Return(edges, nil)
}

func (f traceFixture) expectAnchor(anchor entities.DocumentMeta) {
	f.expectAnchorWithBody(anchor, traceBody)
}

func (f traceFixture) expectAnchorWithBody(anchor entities.DocumentMeta, body string) {
	f.expectResolve(anchor)
	f.expectBody(anchor.ID, body)
	f.expectMetas([]lore.DocID{anchor.ID}, anchor)
}

func (f traceFixture) expectStandaloneAnchor(anchor entities.DocumentMeta, body string) {
	f.expectAnchorWithBody(anchor, body)
	f.expectNeighbors(entities.DirBoth, []lore.DocID{anchor.ID})
	f.store.EXPECT().HasEdges(gomock.Any()).Return(true, nil)
}

func (f traceFixture) expectFocusSearch(focus string, anchor lore.DocID, lexical, semantic []entities.ChunkHit) {
	withinAnchor := entities.Filters{DocID: anchor}
	f.emb.EXPECT().Embed(gomock.Any(), []string{focus}).Return([][]float32{traceVector}, nil)
	f.store.EXPECT().SearchLexical(gomock.Any(), focus, withinAnchor, traceFocusTopK).Return(lexical, nil)
	f.store.EXPECT().SearchVector(gomock.Any(), traceVector, withinAnchor, traceFocusTopK).Return(semantic, nil)
}

func (f traceFixture) mustTrace(t *testing.T, req services.TraceRequest) *entities.EvidenceBundle {
	t.Helper()

	bundle, err := f.svc.Trace(context.Background(), req)
	if err != nil {
		t.Fatalf("Trace: %v", err)
	}

	return bundle
}

func traceMeta(doc lore.DocID, createdAt time.Time) entities.DocumentMeta {
	return entities.DocumentMeta{
		ID:        doc,
		Source:    "github",
		Type:      lore.DocTypePage,
		Title:     "decision: " + string(doc),
		Author:    "dev@example.test",
		URL:       "https://example.test/" + string(doc),
		CreatedAt: createdAt,
		UpdatedAt: createdAt,
	}
}

func traceDate(year int, month time.Month, day int) time.Time {
	return time.Date(year, month, day, 9, 0, 0, 0, time.UTC)
}

func traceEdge(src, dst lore.DocID) entities.Edge {
	return entities.Edge{Src: src, Dst: dst, Kind: entities.EdgeKindReferencesDoc, Confidence: 1}
}

func traceHits(doc lore.DocID, textAt map[int]string, ordinals []int) []entities.ChunkHit {
	hits := make([]entities.ChunkHit, len(ordinals))
	for i, ordinal := range ordinals {
		hits[i] = queryHit(doc, ordinal)
		hits[i].Text = textAt[ordinal]
	}

	return hits
}

func assertStandaloneExcerpt(t *testing.T, bundle *entities.EvidenceBundle, want string) {
	t.Helper()

	if len(bundle.Nodes) != 1 {
		t.Fatalf("Nodes = %v, want the anchor alone", nodeIDs(bundle.Nodes))
	}
	assertExcerptOf(t, bundle.Nodes[0].Doc.ID, bundle.Nodes, want)
}

func assertExcerptOf(t *testing.T, doc lore.DocID, nodes []entities.EvidenceNode, want string) {
	t.Helper()

	at := slices.IndexFunc(nodes, func(node entities.EvidenceNode) bool { return node.Doc.ID == doc })
	if at < 0 {
		t.Fatalf("Nodes = %v, want %s among them", nodeIDs(nodes), doc)
	}
	if got := nodes[at].Excerpt; got != want {
		t.Errorf("%s Excerpt = %s, want %s", doc, excerptSummary(got), excerptSummary(want))
	}
}

func excerptSummary(excerpt string) string {
	const tailRunes = 160

	runes := []rune(excerpt)

	return fmt.Sprintf("%d characters ending %q", len(runes), string(runes[max(0, len(runes)-tailRunes):]))
}

func withoutExcerptOf(anchor lore.DocID, nodes []entities.EvidenceNode) []entities.EvidenceNode {
	stripped := slices.Clone(nodes)
	for i := range stripped {
		if stripped[i].Doc.ID == anchor {
			stripped[i].Excerpt = ""
		}
	}

	return stripped
}

func TestTraceOrdersTheNeighbourhoodChronologically(t *testing.T) {
	t.Parallel()

	// Chronology is docB, docA, docC while both discovery and score order docA, docB, docC.
	anchor := traceMeta("docA", traceDate(2021, time.June, 1))
	design := traceMeta("docB", traceDate(2020, time.January, 15))
	change := traceMeta("docC", traceDate(2022, time.March, 9))
	change.Type = lore.DocTypePR

	debated := traceEdge(anchor.ID, design.ID)
	implemented := traceEdge(design.ID, change.ID)

	f := newTraceFixture(t)
	f.expectAnchor(anchor)
	f.expectNeighbors(entities.DirBoth, []lore.DocID{anchor.ID}, debated)
	f.expectMetas([]lore.DocID{design.ID}, design)
	f.expectNeighbors(entities.DirBoth, []lore.DocID{design.ID}, implemented)
	f.expectMetas([]lore.DocID{change.ID}, change)

	bundle, err := f.svc.Trace(context.Background(), services.TraceRequest{Ref: traceRef})
	if err != nil {
		t.Fatalf("Trace: %v", err)
	}

	want := []lore.DocID{design.ID, anchor.ID, change.ID}
	if !slices.Equal(nodeIDs(bundle.Nodes), want) {
		t.Fatalf("Nodes = %v, want chronological order %v", nodeIDs(bundle.Nodes), want)
	}
	oldest, resolved, newest := bundle.Nodes[0], bundle.Nodes[1], bundle.Nodes[2]

	if resolved.Excerpt != traceBody {
		t.Errorf("anchor Excerpt = %q, want the whole body %q", resolved.Excerpt, traceBody)
	}
	if resolved.Role != entities.RoleSeed {
		t.Errorf("anchor Role = %q, want %q", resolved.Role, entities.RoleSeed)
	}
	if resolved.Via != nil {
		t.Errorf("anchor Via = %+v, want none: the anchor was not reached", resolved.Via)
	}
	assertScore(t, "anchor", resolved.Score, 1)

	if oldest.Role != entities.RoleDesignDoc {
		t.Errorf("%s Role = %q, want %q", oldest.Doc.ID, oldest.Role, entities.RoleDesignDoc)
	}
	if !slices.Equal(oldest.Via, []entities.Edge{debated}) {
		t.Errorf("%s Via = %+v, want the traversed edge %+v", oldest.Doc.ID, oldest.Via, debated)
	}
	if oldest.Excerpt != "" {
		t.Errorf("%s Excerpt = %q, want empty: neighbour bodies are not loaded", oldest.Doc.ID, oldest.Excerpt)
	}
	assertScore(t, string(oldest.Doc.ID), oldest.Score, 0.6)

	if newest.Role != entities.RoleLinkedChange {
		t.Errorf("%s Role = %q, want %q", newest.Doc.ID, newest.Role, entities.RoleLinkedChange)
	}
	if !slices.Equal(newest.Via, []entities.Edge{debated, implemented}) {
		t.Errorf("%s Via = %+v, want both traversed edges", newest.Doc.ID, newest.Via)
	}
	assertScore(t, string(newest.Doc.ID), newest.Score, 0.36)

	if bundle.Anchor.Kind != entities.AnchorDocument {
		t.Errorf("Anchor.Kind = %d, want %d", bundle.Anchor.Kind, entities.AnchorDocument)
	}
	wantDoc := entities.DocRef{ID: anchor.ID, Title: anchor.Title, URL: anchor.URL, CreatedAt: anchor.CreatedAt}
	if bundle.Anchor.Doc == nil || *bundle.Anchor.Doc != wantDoc {
		t.Errorf("Anchor.Doc = %+v, want %+v", bundle.Anchor.Doc, wantDoc)
	}
	if bundle.Question != "provenance of "+anchor.Title {
		t.Errorf("Question = %q, want it to name %q", bundle.Question, anchor.Title)
	}
	assertChain(t, bundle.Chains, []lore.DocID{anchor.ID, design.ID, change.ID})
	assertGaps(t, bundle.Gaps, nil)
}

func TestTraceBreaksChronologyTiesByID(t *testing.T) {
	t.Parallel()

	sameInstant := traceDate(2020, time.January, 15)
	anchor := traceMeta("docA", traceDate(2021, time.June, 1))
	later := traceMeta("docC", sameInstant)
	earlier := traceMeta("docB", sameInstant)

	f := newTraceFixture(t)
	f.expectAnchor(anchor)
	f.expectNeighbors(entities.DirBoth, []lore.DocID{anchor.ID},
		traceEdge(anchor.ID, later.ID), traceEdge(anchor.ID, earlier.ID))
	f.expectMetas([]lore.DocID{later.ID, earlier.ID}, later, earlier)

	bundle, err := f.svc.Trace(context.Background(), services.TraceRequest{Ref: traceRef, Depth: 1})
	if err != nil {
		t.Fatalf("Trace: %v", err)
	}

	want := []lore.DocID{earlier.ID, later.ID, anchor.ID}
	if !slices.Equal(nodeIDs(bundle.Nodes), want) {
		t.Errorf("Nodes = %v, want equal timestamps ordered by id: %v", nodeIDs(bundle.Nodes), want)
	}
}

func TestTraceWalksTheRequestedDirection(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		direction string
		want      entities.Direction
	}{
		{"out", "out", entities.DirOut},
		{"in", "in", entities.DirIn},
		{"both", "both", entities.DirBoth},
		{"unset", "", entities.DirBoth},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			anchor := traceMeta("docA", traceDate(2021, time.June, 1))

			f := newTraceFixture(t)
			f.expectAnchor(anchor)
			f.expectNeighbors(tc.want, []lore.DocID{anchor.ID})
			f.store.EXPECT().HasEdges(gomock.Any()).Return(true, nil)

			_, err := f.svc.Trace(context.Background(),
				services.TraceRequest{Ref: traceRef, Direction: tc.direction})
			if err != nil {
				t.Fatalf("Trace(direction %q): %v", tc.direction, err)
			}
		})
	}
}

func TestTraceRejectsUnknownDirection(t *testing.T) {
	t.Parallel()

	// No expectations: an unusable direction must not reach the index.
	f := newTraceFixture(t)

	_, err := f.svc.Trace(context.Background(),
		services.TraceRequest{Ref: traceRef, Direction: "sideways"})
	if !internalerror.IsBadRequest(err) {
		t.Fatalf("err = %v (%s), want bad request", err, internalerror.KindOf(err))
	}
	for _, accepted := range []string{"in", "out", "both"} {
		if !strings.Contains(err.Error(), accepted) {
			t.Errorf("err = %v, want a message naming the accepted value %q", err, accepted)
		}
	}
}

func TestTraceCapsWalkDepth(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		depth int
		hops  int
	}{
		{"unset", 0, 2},
		{"negative", -3, 2},
		{"one hop", 1, 1},
		{"above the cap", 5, 2},
	}

	layers := []lore.DocID{"docA", "docB", "docC"}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			anchor := traceMeta(layers[0], traceDate(2021, time.June, 1))

			f := newTraceFixture(t)
			f.expectAnchor(anchor)
			// A Neighbors call beyond tc.hops is unexpected and fails the test.
			for hop := range tc.hops {
				from, to := layers[hop], layers[hop+1]
				f.expectNeighbors(entities.DirBoth, []lore.DocID{from}, traceEdge(from, to))
				f.expectMetas([]lore.DocID{to}, traceMeta(to, traceDate(2021, time.July, hop+1)))
			}

			bundle, err := f.svc.Trace(context.Background(),
				services.TraceRequest{Ref: traceRef, Depth: tc.depth})
			if err != nil {
				t.Fatalf("Trace: %v", err)
			}
			if len(bundle.Nodes) != tc.hops+1 {
				t.Errorf("Nodes = %v, want the anchor plus %d reached layers", nodeIDs(bundle.Nodes), tc.hops)
			}
		})
	}
}

func TestTraceRejectsEmptyRef(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"empty":           "",
		"whitespace only": " \t\n ",
	}

	for name, ref := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			// No expectations: an unusable ref must not reach the index.
			f := newTraceFixture(t)

			_, err := f.svc.Trace(context.Background(), services.TraceRequest{Ref: ref})
			if !internalerror.IsBadRequest(err) {
				t.Fatalf("err = %v (%s), want bad request", err, internalerror.KindOf(err))
			}
		})
	}
}

func TestTraceReportsUnknownRef(t *testing.T) {
	t.Parallel()

	// Only ResolveRef is expected: an unresolved ref is never walked.
	f := newTraceFixture(t)
	f.expectResolve()

	_, err := f.svc.Trace(context.Background(), services.TraceRequest{Ref: traceRef})
	if !internalerror.IsNotFound(err) {
		t.Fatalf("err = %v (%s), want not found", err, internalerror.KindOf(err))
	}
	if !strings.Contains(err.Error(), traceRef) {
		t.Errorf("err = %v, want a message naming ref %q", err, traceRef)
	}
}

func TestTraceRejectsAmbiguousRef(t *testing.T) {
	t.Parallel()

	first := traceMeta("docA", traceDate(2021, time.June, 1))
	second := traceMeta("docB", traceDate(2021, time.June, 2))

	// Only ResolveRef is expected: an ambiguous ref is never walked.
	f := newTraceFixture(t)
	f.expectResolve(first, second)

	_, err := f.svc.Trace(context.Background(), services.TraceRequest{Ref: traceRef})
	if !internalerror.IsBadRequest(err) {
		t.Fatalf("err = %v (%s), want bad request", err, internalerror.KindOf(err))
	}
	if !strings.Contains(err.Error(), traceRef) {
		t.Errorf("err = %v, want a message naming ref %q", err, traceRef)
	}
	for _, candidate := range []entities.DocumentMeta{first, second} {
		for _, part := range []string{string(candidate.ID), candidate.Title, candidate.URL} {
			if !strings.Contains(err.Error(), part) {
				t.Errorf("err = %v, want candidate %s identified by %q", err, candidate.ID, part)
			}
		}
	}
}

func TestTraceReportsAnchorMissingFromTheIndex(t *testing.T) {
	t.Parallel()

	anchor := traceMeta("docA", traceDate(2021, time.June, 1))

	f := newTraceFixture(t)
	f.expectResolve(anchor)
	f.store.EXPECT().DocumentsWithBody(gomock.Any(), []lore.DocID{anchor.ID}).Return(nil, nil)

	_, err := f.svc.Trace(context.Background(), services.TraceRequest{Ref: traceRef})
	if !internalerror.IsNotFound(err) {
		t.Fatalf("err = %v (%s), want not found", err, internalerror.KindOf(err))
	}
	if !strings.Contains(err.Error(), string(anchor.ID)) {
		t.Errorf("err = %v, want a message naming document %q", err, anchor.ID)
	}
}

func TestTraceRejectsAnchorWithoutACitableURL(t *testing.T) {
	t.Parallel()

	anchor := traceMeta("docA", traceDate(2021, time.June, 1))
	anchor.URL = ""

	f := newTraceFixture(t)
	f.expectResolve(anchor)

	_, err := f.svc.Trace(context.Background(), services.TraceRequest{Ref: traceRef})
	if !internalerror.IsNotFound(err) {
		t.Fatalf("err = %v (%s), want not found", err, internalerror.KindOf(err))
	}
	for _, part := range []string{traceRef, "URL"} {
		if !strings.Contains(err.Error(), part) {
			t.Errorf("err = %v, want a message naming %q", err, part)
		}
	}
}

func TestTraceDropsNeighbourWithoutURL(t *testing.T) {
	t.Parallel()

	anchor := traceMeta("docA", traceDate(2021, time.June, 1))
	uncitable := traceMeta("docB", traceDate(2020, time.January, 15))
	uncitable.URL = ""

	f := newTraceFixture(t)
	f.expectAnchor(anchor)
	f.expectNeighbors(entities.DirBoth, []lore.DocID{anchor.ID}, traceEdge(anchor.ID, uncitable.ID))
	f.expectMetas([]lore.DocID{uncitable.ID}, uncitable)
	f.expectNeighbors(entities.DirBoth, []lore.DocID{uncitable.ID})
	f.store.EXPECT().HasEdges(gomock.Any()).Return(true, nil)

	bundle, err := f.svc.Trace(context.Background(), services.TraceRequest{Ref: traceRef})
	if err != nil {
		t.Fatalf("Trace: %v", err)
	}

	if !slices.Equal(nodeIDs(bundle.Nodes), []lore.DocID{anchor.ID}) {
		t.Errorf("Nodes = %v, want the anchor alone: %s carries no URL", nodeIDs(bundle.Nodes), uncitable.ID)
	}
}

func TestTraceReportsAStandaloneAnchor(t *testing.T) {
	t.Parallel()

	anchor := traceMeta("docA", traceDate(2021, time.June, 1))

	f := newTraceFixture(t)
	f.expectAnchor(anchor)
	f.expectNeighbors(entities.DirBoth, []lore.DocID{anchor.ID})
	f.store.EXPECT().HasEdges(gomock.Any()).Return(true, nil)

	bundle, err := f.svc.Trace(context.Background(), services.TraceRequest{Ref: traceRef})
	if err != nil {
		t.Fatalf("Trace: %v", err)
	}

	assertGaps(t, bundle.Gaps, []string{traceStandaloneGap})
	if len(bundle.Chains) != 0 {
		t.Errorf("Chains = %v, want none: the anchor has no neighbourhood", bundle.Chains)
	}
}

func TestTraceReportsOneIndexLevelGapWhenNothingIsLinked(t *testing.T) {
	t.Parallel()

	anchor := traceMeta("docA", traceDate(2021, time.June, 1))
	f := newTraceFixture(t)
	f.expectAnchorWithBody(anchor, traceOversizeBody)
	f.expectNeighbors(entities.DirBoth, []lore.DocID{anchor.ID})
	f.expectFocusSearch(traceFocus, anchor.ID, nil, nil)
	f.store.EXPECT().HasEdges(gomock.Any()).Return(false, nil)

	bundle := f.mustTrace(t, services.TraceRequest{Ref: traceRef, Focus: traceFocus})
	assertStandaloneExcerpt(t, bundle, traceOversizeExcerpt)
	assertGaps(t, bundle.Gaps, []string{
		indexUnlinkedGap,
		`focus "how fast forward uses a closed form" matched no passage of decision: docA (docA)`,
	})
}

func TestTraceCapsTheAnchorBody(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		body string
		want string
	}{
		"empty body": {
			body: "",
			want: "",
		},
		"one character under the cap": {
			body: strings.Repeat("a", traceExcerptCap-1),
			want: strings.Repeat("a", traceExcerptCap-1),
		},
		"exactly at the cap": {
			body: strings.Repeat("a", traceExcerptCap),
			want: strings.Repeat("a", traceExcerptCap),
		},
		"multibyte at the cap": {
			body: strings.Repeat("界", traceExcerptCap),
			want: strings.Repeat("界", traceExcerptCap),
		},
		"one character over the cap": {
			body: strings.Repeat("a", traceExcerptCap+1),
			want: strings.Repeat("a", traceExcerptCap) + "\n\n" +
				"[truncated: 8,000 of 8,001 characters shown. Pass focus with a question to get the passages that match it.]",
		},
		"over the cap": {
			body: traceOversizeBody,
			want: traceOversizeExcerpt,
		},
		"multibyte over the cap": {
			body: strings.Repeat("界", traceExcerptCap) + strings.Repeat("語", 4000),
			want: strings.Repeat("界", traceExcerptCap) + "\n\n" +
				"[truncated: 8,000 of 12,000 characters shown. Pass focus with a question to get the passages that match it.]",
		},
		"a hundred thousand characters": {
			body: strings.Repeat("a", traceExcerptCap) + strings.Repeat("b", 100_000-traceExcerptCap),
			want: strings.Repeat("a", traceExcerptCap) + "\n\n" +
				"[truncated: 8,000 of 100,000 characters shown. Pass focus with a question to get the passages that match it.]",
		},
		"a million characters": {
			body: strings.Repeat("a", traceExcerptCap) + strings.Repeat("b", 1_000_000-traceExcerptCap),
			want: strings.Repeat("a", traceExcerptCap) + "\n\n" +
				"[truncated: 8,000 of 1,000,000 characters shown. Pass focus with a question to get the passages that match it.]",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			f := newTraceFixture(t)
			f.expectStandaloneAnchor(traceMeta("docA", traceDate(2021, time.June, 1)), tc.body)

			bundle := f.mustTrace(t, services.TraceRequest{Ref: traceRef})

			assertStandaloneExcerpt(t, bundle, tc.want)
			assertGaps(t, bundle.Gaps, []string{traceStandaloneGap})
		})
	}
}

func TestTraceIgnoresBlankFocus(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"a few characters":      " \t\n ",
		"longer than the limit": strings.Repeat(" \t\n", traceFocusLimit),
	}

	for name, focus := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			f := newTraceFixture(t)
			f.expectStandaloneAnchor(traceMeta("docA", traceDate(2021, time.June, 1)), traceOversizeBody)

			bundle := f.mustTrace(t, services.TraceRequest{Ref: traceRef, Focus: focus})

			assertStandaloneExcerpt(t, bundle, traceOversizeExcerpt)
			assertGaps(t, bundle.Gaps, []string{traceStandaloneGap})
		})
	}
}

func TestTraceAcceptsFocusAtTheLimit(t *testing.T) {
	t.Parallel()

	atLimit := strings.Repeat("a", traceFocusLimit)
	multibyteAtLimit := strings.Repeat("𝄞", traceFocusLimit)

	tests := map[string]struct {
		focus    string
		searched string
	}{
		"exactly at the limit":      {focus: atLimit, searched: atLimit},
		"multibyte at the limit":    {focus: multibyteAtLimit, searched: multibyteAtLimit},
		"at the limit once trimmed": {focus: " \t" + atLimit + "\n ", searched: atLimit},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			anchor := traceMeta("docA", traceDate(2021, time.June, 1))

			f := newTraceFixture(t)
			f.expectStandaloneAnchor(anchor, traceBody)
			f.expectFocusSearch(tc.searched, anchor.ID, []entities.ChunkHit{queryHit(anchor.ID, 0)}, nil)

			bundle := f.mustTrace(t, services.TraceRequest{Ref: traceRef, Focus: tc.focus})

			assertStandaloneExcerpt(t, bundle, "docA excerpt 0")
		})
	}
}

func TestTraceRejectsFocusOverTheLimitBeforeAnyLookup(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"one character over the limit":           strings.Repeat("a", traceFocusLimit+1),
		"multibyte one character over the limit": strings.Repeat("𝄞", traceFocusLimit+1),
		"invalid bytes one over the limit":       strings.Repeat("\xff", traceFocusLimit+1),
		"over the limit once trimmed":            " \t" + strings.Repeat("a", traceFocusLimit+1) + "\n ",
		"a megabyte":                             strings.Repeat("a", 1<<20),
	}

	for name, focus := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			f := newTraceFixture(t)

			_, err := f.svc.Trace(context.Background(), services.TraceRequest{Ref: traceRef, Focus: focus})
			if !internalerror.IsBadRequest(err) {
				t.Fatalf("err = %v (%s), want bad request", err, internalerror.KindOf(err))
			}
			if want := "focus must be at most 1,000 characters"; err.Error() != want {
				t.Errorf("err = %q, want exactly %q: the limit named and no caller text", err, want)
			}
		})
	}
}

func TestTraceFocusExcerptsTheMatchingPassages(t *testing.T) {
	t.Parallel()

	// Fused rank is 2, 5, 3, 0, 7, so passages 0 and 7 fall outside the top three.
	lexicalRank, vectorRank := []int{5, 2, 0}, []int{2, 3, 7}

	prose := map[int]string{
		0: "an unrelated preamble",
		2: "fast forward skips whole periods",
		3: "it sums them with a closed form",
		4: "every period adds the same amount",
		5: "the closed form is exact",
		7: "an appendix lists the edge cases",
	}

	tests := map[string]struct {
		body     string
		textAt   map[int]string
		lexical  []int
		semantic []int
		want     string
	}{
		"five ranked, an adjacent pair then a gap": {
			body:     traceOversizeBody,
			textAt:   prose,
			lexical:  lexicalRank,
			semantic: vectorRank,
			want:     "fast forward skips whole periods\n\nit sums them with a closed form\n\n…\n\nthe closed form is exact",
		},
		"a gap then an adjacent pair, body within the cap": {
			body:    traceBody,
			textAt:  prose,
			lexical: []int{5, 2, 4},
			want:    "fast forward skips whole periods\n\n…\n\nevery period adds the same amount\n\nthe closed form is exact",
		},
		"three adjacent passages": {
			body:    traceOversizeBody,
			textAt:  prose,
			lexical: []int{4, 2, 3},
			want:    "fast forward skips whole periods\n\nit sums them with a closed form\n\nevery period adds the same amount",
		},
		"three passages apart": {
			body:    traceOversizeBody,
			textAt:  prose,
			lexical: []int{7, 2, 4},
			want: "fast forward skips whole periods\n\n…\n\nevery period adds the same amount\n\n…\n\n" +
				"an appendix lists the edge cases",
		},
		"two passages apart, ranked in reverse": {
			body:    traceOversizeBody,
			textAt:  prose,
			lexical: []int{5, 2},
			want:    "fast forward skips whole periods\n\n…\n\nthe closed form is exact",
		},
		"two adjacent passages": {
			body:    traceOversizeBody,
			textAt:  prose,
			lexical: []int{2, 3},
			want:    "fast forward skips whole periods\n\nit sums them with a closed form",
		},
		"a single passage": {
			body:    traceOversizeBody,
			textAt:  prose,
			lexical: []int{3},
			want:    "it sums them with a closed form",
		},
		"passages over the cap": {
			body: traceOversizeBody,
			textAt: map[int]string{
				0: "an unrelated preamble",
				2: strings.Repeat("界", 4000),
				3: strings.Repeat("語", 4000),
				5: strings.Repeat("字", 4000),
				7: "an appendix lists the edge cases",
			},
			lexical:  lexicalRank,
			semantic: vectorRank,
			want: strings.Repeat("界", 4000) + "\n\n" + strings.Repeat("語", traceExcerptCap-4000-len("\n\n")) + "\n\n" +
				"[truncated: 8,000 of 12,007 characters shown.]",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			anchor := traceMeta("docA", traceDate(2021, time.June, 1))

			f := newTraceFixture(t)
			f.expectStandaloneAnchor(anchor, tc.body)
			f.expectFocusSearch(traceFocus, anchor.ID,
				traceHits(anchor.ID, tc.textAt, tc.lexical), traceHits(anchor.ID, tc.textAt, tc.semantic))

			bundle := f.mustTrace(t, services.TraceRequest{Ref: traceRef, Focus: "  " + traceFocus + "\n"})

			assertStandaloneExcerpt(t, bundle, tc.want)
			assertGaps(t, bundle.Gaps, []string{traceStandaloneGap})
		})
	}
}

func TestTraceFocusMatchingNothingFallsBackToTheBody(t *testing.T) {
	t.Parallel()

	const focus = `why "option B" won`

	tests := map[string]struct {
		body string
		want string
	}{
		"body within the cap": {body: traceBody, want: traceBody},
		"body over the cap":   {body: traceOversizeBody, want: traceOversizeExcerpt},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			anchor := traceMeta("docA", traceDate(2021, time.June, 1))

			f := newTraceFixture(t)
			f.expectStandaloneAnchor(anchor, tc.body)
			f.expectFocusSearch(focus, anchor.ID, nil, nil)

			bundle := f.mustTrace(t, services.TraceRequest{Ref: traceRef, Focus: "  " + focus + "\n"})

			assertStandaloneExcerpt(t, bundle, tc.want)
			assertGaps(t, bundle.Gaps, []string{
				traceStandaloneGap,
				`focus "why \"option B\" won" matched no passage of decision: docA (docA)`,
			})
		})
	}
}

func TestTraceFocusLeavesTheNeighbourhoodUnchanged(t *testing.T) {
	t.Parallel()

	anchor := traceMeta("docA", traceDate(2021, time.June, 1))
	design := traceMeta("docB", traceDate(2020, time.January, 15))
	change := traceMeta("docC", traceDate(2022, time.March, 9))

	expectLinked := func(f traceFixture) {
		f.expectAnchor(anchor)
		f.expectNeighbors(entities.DirBoth, []lore.DocID{anchor.ID}, traceEdge(anchor.ID, design.ID))
		f.expectMetas([]lore.DocID{design.ID}, design)
		f.expectNeighbors(entities.DirBoth, []lore.DocID{design.ID}, traceEdge(design.ID, change.ID))
		f.expectMetas([]lore.DocID{change.ID}, change)
	}
	expectStandalone := func(f traceFixture) {
		f.expectStandaloneAnchor(anchor, traceBody)
	}
	matching := []entities.ChunkHit{queryHit(anchor.ID, 0)}
	const noMatchGap = `focus "how fast forward uses a closed form" matched no passage of decision: docA (docA)`

	tests := map[string]struct {
		expectGraph func(f traceFixture)
		hits        []entities.ChunkHit
		wantExcerpt string
		wantGaps    []string
	}{
		"linked, focus matching": {
			expectGraph: expectLinked,
			hits:        matching,
			wantExcerpt: "docA excerpt 0",
		},
		"linked, focus matching nothing": {
			expectGraph: expectLinked,
			wantExcerpt: traceBody,
			wantGaps:    []string{noMatchGap},
		},
		"standalone, focus matching": {
			expectGraph: expectStandalone,
			hits:        matching,
			wantExcerpt: "docA excerpt 0",
			wantGaps:    []string{traceStandaloneGap},
		},
		"standalone, focus matching nothing": {
			expectGraph: expectStandalone,
			wantExcerpt: traceBody,
			wantGaps:    []string{traceStandaloneGap, noMatchGap},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			unfocused := newTraceFixture(t)
			tc.expectGraph(unfocused)
			want := unfocused.mustTrace(t, services.TraceRequest{Ref: traceRef})

			focused := newTraceFixture(t)
			tc.expectGraph(focused)
			focused.expectFocusSearch(traceFocus, anchor.ID, tc.hits, nil)
			got := focused.mustTrace(t, services.TraceRequest{Ref: traceRef, Focus: traceFocus})

			assertExcerptOf(t, anchor.ID, got.Nodes, tc.wantExcerpt)
			gotNodes, wantNodes := withoutExcerptOf(anchor.ID, got.Nodes), withoutExcerptOf(anchor.ID, want.Nodes)
			if !reflect.DeepEqual(gotNodes, wantNodes) {
				t.Errorf("Nodes = %+v, want the unfocused nodes apart from the anchor excerpt: %+v",
					gotNodes, wantNodes)
			}
			if !reflect.DeepEqual(got.Chains, want.Chains) {
				t.Errorf("Chains = %v, want the unfocused chains %v", got.Chains, want.Chains)
			}
			assertGaps(t, got.Gaps, tc.wantGaps)
		})
	}
}

func TestTraceClassifiesFocusEmbedderFailure(t *testing.T) {
	t.Parallel()

	anchor := traceMeta("docA", traceDate(2021, time.June, 1))

	f := newTraceFixture(t)
	f.expectResolve(anchor)
	f.expectBody(anchor.ID, traceBody)
	f.emb.EXPECT().Embed(gomock.Any(), []string{traceFocus}).Return(nil, errTraceEmbed)

	_, err := f.svc.Trace(context.Background(), services.TraceRequest{Ref: traceRef, Focus: traceFocus})
	if !internalerror.IsInternal(err) {
		t.Fatalf("err = %v (%s), want internal", err, internalerror.KindOf(err))
	}
	if !errors.Is(err, errTraceEmbed) {
		t.Errorf("err = %v, want the embedder's cause wrapped", err)
	}
}

func TestTraceClassifiesStoreFailures(t *testing.T) {
	t.Parallel()

	anchor := traceMeta("docA", traceDate(2021, time.June, 1))

	tests := map[string]struct {
		expect func(f traceFixture)
		named  string
	}{
		"ref resolution": {
			expect: func(f traceFixture) {
				f.store.EXPECT().ResolveRef(gomock.Any(), traceRef).Return(nil, errTraceStore)
			},
			named: "ref",
		},
		"body load": {
			expect: func(f traceFixture) {
				f.expectResolve(anchor)
				f.store.EXPECT().DocumentsWithBody(gomock.Any(), []lore.DocID{anchor.ID}).
					Return(nil, errTraceStore)
			},
			named: "body",
		},
		"graph walk": {
			expect: func(f traceFixture) {
				f.expectAnchor(anchor)
				f.store.EXPECT().Neighbors(gomock.Any(), []lore.DocID{anchor.ID}, nil, entities.DirBoth).
					Return(nil, errTraceStore)
			},
			named: "graph",
		},
		"index links": {
			expect: func(f traceFixture) {
				f.expectAnchor(anchor)
				f.expectNeighbors(entities.DirBoth, []lore.DocID{anchor.ID})
				f.store.EXPECT().HasEdges(gomock.Any()).Return(false, errTraceStore)
			},
			named: "index holds links",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			f := newTraceFixture(t)
			tc.expect(f)

			_, err := f.svc.Trace(context.Background(), services.TraceRequest{Ref: traceRef})
			if !internalerror.IsInternal(err) {
				t.Fatalf("err = %v (%s), want internal", err, internalerror.KindOf(err))
			}
			if !errors.Is(err, errTraceStore) {
				t.Errorf("err = %v, want the store's cause wrapped", err)
			}
			if !strings.Contains(err.Error(), tc.named) {
				t.Errorf("err = %v, want a message naming %q", err, tc.named)
			}
		})
	}
}
