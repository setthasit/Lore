package sqlite

import (
	"context"
	"slices"
	"testing"

	"lore/internal/entities"
)

func TestNeighborsHonorsDirectionAndKind(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	commit := entities.NewDocID("github", entities.DocTypeCommit, "acme/lore/commit/aaaaaaaaaaaa")
	pr := entities.NewDocID("github", entities.DocTypePR, "acme/lore/pull/42")
	issue := entities.NewDocID("github", entities.DocTypeIssue, "acme/lore/issues/7")

	commitInPR := entities.Edge{Src: commit, Dst: pr, Kind: entities.EdgeKindCommitInPR, Confidence: 1}
	prClosesIssue := entities.Edge{Src: pr, Dst: issue, Kind: entities.EdgeKindPRClosesIssue, Confidence: 1}
	commitMentions := entities.Edge{Src: commit, Dst: issue, Kind: entities.EdgeKindReferencesDoc, Confidence: 0.9}

	if err := s.UpsertEdges(ctx, []entities.Edge{commitInPR, prClosesIssue, commitMentions}); err != nil {
		t.Fatalf("UpsertEdges: %v", err)
	}

	tests := []struct {
		name  string
		ids   []entities.DocID
		kinds []entities.EdgeKind
		dir   entities.Direction
		want  []entities.Edge
	}{
		{
			name: "outgoing from a mid-graph node",
			ids:  []entities.DocID{pr},
			dir:  entities.DirOut,
			want: []entities.Edge{prClosesIssue},
		},
		{
			name: "incoming to a mid-graph node",
			ids:  []entities.DocID{pr},
			dir:  entities.DirIn,
			want: []entities.Edge{commitInPR},
		},
		{
			name: "both directions union the two sides",
			ids:  []entities.DocID{pr},
			dir:  entities.DirBoth,
			want: []entities.Edge{commitInPR, prClosesIssue},
		},
		{
			name: "both endpoints of an edge yield it once",
			ids:  []entities.DocID{commit, pr},
			dir:  entities.DirBoth,
			want: []entities.Edge{commitInPR, prClosesIssue, commitMentions},
		},
		{
			name:  "kind filter excludes every other kind",
			ids:   []entities.DocID{commit},
			kinds: []entities.EdgeKind{entities.EdgeKindReferencesDoc},
			dir:   entities.DirOut,
			want:  []entities.Edge{commitMentions},
		},
		{
			name:  "kind nobody wrote matches nothing",
			ids:   []entities.DocID{commit},
			kinds: []entities.EdgeKind{entities.EdgeKindSupersedes},
			dir:   entities.DirBoth,
			want:  nil,
		},
		{
			name: "no ids",
			dir:  entities.DirBoth,
			want: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.Neighbors(ctx, tc.ids, tc.kinds, tc.dir)
			if err != nil {
				t.Fatalf("Neighbors: %v", err)
			}
			again, err := s.Neighbors(ctx, tc.ids, tc.kinds, tc.dir)
			if err != nil {
				t.Fatalf("Neighbors (repeat): %v", err)
			}
			if !slices.Equal(got, again) {
				t.Errorf("Neighbors order is unstable: %+v then %+v", got, again)
			}
			assertEdges(t, got, tc.want)
		})
	}
}

func TestUpsertEdgesKeepsTheHighestConfidence(t *testing.T) {
	ctx := context.Background()

	strong := entities.Edge{
		Src:        entities.NewDocID("notion", entities.DocTypePage, "design/retrieval"),
		Dst:        entities.NewDocID("github", entities.DocTypeCommit, "acme/lore/commit/bbbbbbbbbbbb"),
		Kind:       entities.EdgeKindMentionsCommit,
		Confidence: 0.9,
	}
	weak := strong
	weak.Confidence = 0.5

	orders := map[string][]entities.Edge{
		"strong first": {strong, weak},
		"weak first":   {weak, strong},
	}
	for name, order := range orders {
		t.Run(name, func(t *testing.T) {
			s := openTestStore(t)
			for _, e := range order {
				if err := s.UpsertEdges(ctx, []entities.Edge{e}); err != nil {
					t.Fatalf("UpsertEdges: %v", err)
				}
			}

			got, err := s.Neighbors(ctx, []entities.DocID{strong.Src}, nil, entities.DirOut)
			if err != nil {
				t.Fatalf("Neighbors: %v", err)
			}
			assertEdges(t, got, []entities.Edge{strong})
		})
	}
}

func TestPendingRefsSurviveUpsertAndTargetedDelete(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	src := entities.NewDocID("github", entities.DocTypePR, "acme/lore/pull/42")
	page := entities.PendingRef{
		SourceDoc: src,
		Ref:       entities.RawRef{Kind: entities.RefKindURL, Value: "https://notion.so/design/retrieval"},
	}
	ticket := entities.PendingRef{
		SourceDoc: src,
		Ref:       entities.RawRef{Kind: entities.RefKindTicketKey, Value: "PROJ-123"},
	}

	if err := s.UpsertPendingRefs(ctx, []entities.PendingRef{page, ticket, page}); err != nil {
		t.Fatalf("UpsertPendingRefs: %v", err)
	}
	if err := s.UpsertPendingRefs(ctx, []entities.PendingRef{ticket}); err != nil {
		t.Fatalf("UpsertPendingRefs (again): %v", err)
	}

	got, err := s.PendingRefs(ctx)
	if err != nil {
		t.Fatalf("PendingRefs: %v", err)
	}
	if want := []entities.PendingRef{ticket, page}; !slices.Equal(got, want) {
		t.Fatalf("PendingRefs = %+v, want %+v", got, want)
	}

	absent := entities.PendingRef{
		SourceDoc: src,
		Ref:       entities.RawRef{Kind: entities.RefKindCommitSHA, Value: "deadbeef"},
	}
	if err := s.DeletePendingRefs(ctx, []entities.PendingRef{page, absent}); err != nil {
		t.Fatalf("DeletePendingRefs: %v", err)
	}

	got, err = s.PendingRefs(ctx)
	if err != nil {
		t.Fatalf("PendingRefs (after delete): %v", err)
	}
	if want := []entities.PendingRef{ticket}; !slices.Equal(got, want) {
		t.Errorf("PendingRefs (after delete) = %+v, want %+v", got, want)
	}
}

func TestGraphWritesIgnoreEmptyInput(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.UpsertEdges(ctx, nil); err != nil {
		t.Fatalf("UpsertEdges(nil): %v", err)
	}
	if err := s.UpsertPendingRefs(ctx, nil); err != nil {
		t.Fatalf("UpsertPendingRefs(nil): %v", err)
	}
	if err := s.DeletePendingRefs(ctx, nil); err != nil {
		t.Fatalf("DeletePendingRefs(nil): %v", err)
	}

	edges, err := s.Neighbors(ctx, nil, nil, entities.DirBoth)
	if err != nil {
		t.Fatalf("Neighbors(nil): %v", err)
	}
	if edges != nil {
		t.Errorf("Neighbors(nil) = %+v, want nil", edges)
	}

	refs, err := s.PendingRefs(ctx)
	if err != nil {
		t.Fatalf("PendingRefs: %v", err)
	}
	if refs != nil {
		t.Errorf("PendingRefs = %+v, want nil", refs)
	}

	var edgeRows, refRows int
	err = s.db.QueryRowContext(ctx,
		`SELECT (SELECT count(*) FROM edges), (SELECT count(*) FROM pending_refs)`).
		Scan(&edgeRows, &refRows)
	if err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if edgeRows != 0 || refRows != 0 {
		t.Errorf("edges = %d rows, pending_refs = %d rows, want both empty", edgeRows, refRows)
	}
}

func assertEdges(t *testing.T, got, want []entities.Edge) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("Neighbors returned %d edges %+v, want %d %+v", len(got), got, len(want), want)
	}

	counts := make(map[entities.Edge]int, len(got))
	for _, e := range got {
		counts[e]++
	}
	for _, e := range want {
		if counts[e] == 0 {
			t.Errorf("Neighbors is missing edge %+v, returned %+v", e, got)
			continue
		}
		counts[e]--
	}
}
