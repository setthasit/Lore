package services_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/setthasit/Lore/internal/entities"
	"github.com/setthasit/Lore/internal/errors/internalerror"
	mock_repositories "github.com/setthasit/Lore/internal/mocks/repositories"
	"github.com/setthasit/Lore/internal/services"
)

const (
	traceRef  = "PROJ-4521"
	traceBody = "we chose option B; option A lost on operational cost. Both paragraphs, verbatim."
)

var errTraceStore = errors.New("index is unavailable")

type traceFixture struct {
	store *mock_repositories.MockIndexStore
	svc   services.TraceService
}

func newTraceFixture(t *testing.T) traceFixture {
	t.Helper()

	store := mock_repositories.NewMockIndexStore(gomock.NewController(t))

	return traceFixture{store: store, svc: services.NewTraceService(store)}
}

func (f traceFixture) expectResolve(candidates ...entities.DocumentMeta) *gomock.Call {
	return f.store.EXPECT().ResolveRef(gomock.Any(), traceRef).Return(candidates, nil)
}

func (f traceFixture) expectBody(id entities.DocID, body string) *gomock.Call {
	return f.store.EXPECT().DocumentsWithBody(gomock.Any(), []entities.DocID{id}).
		Return([]entities.Document{{ID: id, Body: body}}, nil)
}

func (f traceFixture) expectMetas(ids []entities.DocID, metas ...entities.DocumentMeta) *gomock.Call {
	return f.store.EXPECT().DocumentsByID(gomock.Any(), ids).Return(metas, nil)
}

func (f traceFixture) expectNeighbors(
	dir entities.Direction,
	ids []entities.DocID,
	edges ...entities.Edge,
) *gomock.Call {
	return f.store.EXPECT().Neighbors(gomock.Any(), ids, nil, dir).Return(edges, nil)
}

func (f traceFixture) expectAnchor(anchor entities.DocumentMeta) {
	f.expectResolve(anchor)
	f.expectBody(anchor.ID, traceBody)
	f.expectMetas([]entities.DocID{anchor.ID}, anchor)
}

func traceMeta(doc entities.DocID, createdAt time.Time) entities.DocumentMeta {
	return entities.DocumentMeta{
		ID:        doc,
		Source:    "github",
		Type:      entities.DocTypePage,
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

func traceEdge(src, dst entities.DocID) entities.Edge {
	return entities.Edge{Src: src, Dst: dst, Kind: entities.EdgeKindReferencesDoc, Confidence: 1}
}

func TestTraceOrdersTheNeighbourhoodChronologically(t *testing.T) {
	t.Parallel()

	// Chronology is docB, docA, docC while both discovery and score order docA, docB, docC.
	anchor := traceMeta("docA", traceDate(2021, time.June, 1))
	design := traceMeta("docB", traceDate(2020, time.January, 15))
	change := traceMeta("docC", traceDate(2022, time.March, 9))
	change.Type = entities.DocTypePR

	debated := traceEdge(anchor.ID, design.ID)
	implemented := traceEdge(design.ID, change.ID)

	f := newTraceFixture(t)
	f.expectAnchor(anchor)
	f.expectNeighbors(entities.DirBoth, []entities.DocID{anchor.ID}, debated)
	f.expectMetas([]entities.DocID{design.ID}, design)
	f.expectNeighbors(entities.DirBoth, []entities.DocID{design.ID}, implemented)
	f.expectMetas([]entities.DocID{change.ID}, change)

	bundle, err := f.svc.Trace(context.Background(), services.TraceRequest{Ref: traceRef})
	if err != nil {
		t.Fatalf("Trace: %v", err)
	}

	want := []entities.DocID{design.ID, anchor.ID, change.ID}
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
	assertChain(t, bundle.Chains, []entities.DocID{anchor.ID, design.ID, change.ID})
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
	f.expectNeighbors(entities.DirBoth, []entities.DocID{anchor.ID},
		traceEdge(anchor.ID, later.ID), traceEdge(anchor.ID, earlier.ID))
	f.expectMetas([]entities.DocID{later.ID, earlier.ID}, later, earlier)

	bundle, err := f.svc.Trace(context.Background(), services.TraceRequest{Ref: traceRef, Depth: 1})
	if err != nil {
		t.Fatalf("Trace: %v", err)
	}

	want := []entities.DocID{earlier.ID, later.ID, anchor.ID}
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
			f.expectNeighbors(tc.want, []entities.DocID{anchor.ID})

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

	layers := []entities.DocID{"docA", "docB", "docC"}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			anchor := traceMeta(layers[0], traceDate(2021, time.June, 1))

			f := newTraceFixture(t)
			f.expectAnchor(anchor)
			// A Neighbors call beyond tc.hops is unexpected and fails the test.
			for hop := range tc.hops {
				from, to := layers[hop], layers[hop+1]
				f.expectNeighbors(entities.DirBoth, []entities.DocID{from}, traceEdge(from, to))
				f.expectMetas([]entities.DocID{to}, traceMeta(to, traceDate(2021, time.July, hop+1)))
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
	f.store.EXPECT().DocumentsWithBody(gomock.Any(), []entities.DocID{anchor.ID}).Return(nil, nil)

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
	f.expectNeighbors(entities.DirBoth, []entities.DocID{anchor.ID}, traceEdge(anchor.ID, uncitable.ID))
	f.expectMetas([]entities.DocID{uncitable.ID}, uncitable)
	f.expectNeighbors(entities.DirBoth, []entities.DocID{uncitable.ID})

	bundle, err := f.svc.Trace(context.Background(), services.TraceRequest{Ref: traceRef})
	if err != nil {
		t.Fatalf("Trace: %v", err)
	}

	if !slices.Equal(nodeIDs(bundle.Nodes), []entities.DocID{anchor.ID}) {
		t.Errorf("Nodes = %v, want the anchor alone: %s carries no URL", nodeIDs(bundle.Nodes), uncitable.ID)
	}
}

func TestTraceReportsAStandaloneAnchor(t *testing.T) {
	t.Parallel()

	anchor := traceMeta("docA", traceDate(2021, time.June, 1))

	f := newTraceFixture(t)
	f.expectAnchor(anchor)
	f.expectNeighbors(entities.DirBoth, []entities.DocID{anchor.ID})

	bundle, err := f.svc.Trace(context.Background(), services.TraceRequest{Ref: traceRef})
	if err != nil {
		t.Fatalf("Trace: %v", err)
	}

	assertGaps(t, bundle.Gaps, []string{"decision: docA (docA) stands alone; no linked discussion"})
	if len(bundle.Chains) != 0 {
		t.Errorf("Chains = %v, want none: the anchor has no neighbourhood", bundle.Chains)
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
				f.store.EXPECT().DocumentsWithBody(gomock.Any(), []entities.DocID{anchor.ID}).
					Return(nil, errTraceStore)
			},
			named: "body",
		},
		"graph walk": {
			expect: func(f traceFixture) {
				f.expectAnchor(anchor)
				f.store.EXPECT().Neighbors(gomock.Any(), []entities.DocID{anchor.ID}, nil, entities.DirBoth).
					Return(nil, errTraceStore)
			},
			named: "graph",
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
