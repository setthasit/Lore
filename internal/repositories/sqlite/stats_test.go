package sqlite

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/setthasit/Lore/internal/entities"
	"github.com/setthasit/Lore/sdk"
)

func TestHasEdges(t *testing.T) {
	ctx := context.Background()
	src := lore.NewDocID("github", lore.DocTypePR, "acme/lore/pull/42")
	dst := lore.NewDocID("github", lore.DocTypeIssue, "acme/lore/issues/7")
	docs := []lore.Document{
		{ID: src, Source: "github", Type: lore.DocTypePR},
		{ID: dst, Source: "github", Type: lore.DocTypeIssue},
	}
	edges := []entities.Edge{{Src: src, Dst: dst, Kind: entities.EdgeKindPRClosesIssue, Confidence: 1}}

	tests := []struct {
		name  string
		docs  []lore.Document
		edges []entities.Edge
		want  bool
	}{
		{name: "empty store", want: false},
		{name: "documents without edges", docs: docs, want: false},
		{name: "one edge", docs: docs, edges: edges, want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := openTestStore(t)
			if err := s.UpsertDocuments(ctx, tc.docs); err != nil {
				t.Fatalf("UpsertDocuments: %v", err)
			}
			if err := s.UpsertEdges(ctx, tc.edges); err != nil {
				t.Fatalf("UpsertEdges: %v", err)
			}
			got, err := s.HasEdges(ctx)
			if err != nil {
				t.Fatalf("HasEdges: %v", err)
			}
			if got != tc.want {
				t.Errorf("HasEdges = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHasEdgesCanceledContext(t *testing.T) {
	s := openTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got, err := s.HasEdges(ctx)
	if got {
		t.Error("HasEdges = true, want false on error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("HasEdges error = %v, want context.Canceled", err)
	}
	if !strings.HasPrefix(err.Error(), "sqlite: has edges: ") {
		t.Errorf("HasEdges error = %q, want sqlite: has edges prefix", err)
	}
}

func TestStatsEmptyStoreIsZeros(t *testing.T) {
	s := openTestStore(t)

	got, err := s.Stats(context.Background())
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if got.Documents != 0 || got.Chunks != 0 || got.Edges != 0 {
		t.Errorf("counts = %d documents, %d chunks, %d edges; want 0, 0, 0",
			got.Documents, got.Chunks, got.Edges)
	}
	if got.Cursors != nil {
		t.Errorf("Cursors = %v, want none", got.Cursors)
	}
	if got.Sources != nil {
		t.Errorf("Sources = %+v, want none", got.Sources)
	}
	if got.Lease != nil {
		t.Errorf("Lease = %+v, want nil", got.Lease)
	}
}

func TestStatsReportsCountsCursorsAndLease(t *testing.T) {
	stamp := time.Date(2025, time.March, 12, 9, 30, 0, 0, time.UTC)
	s := openTestStore(t, WithClock(func() time.Time { return stamp }))
	ctx := context.Background()

	seedSearchCorpus(t, s)

	// A second chunk under one document keeps the three counts distinct, so a transposed positional scan cannot pass.
	split := searchCorpus[0]
	first := entities.Chunk{
		DocID:     split.id,
		Ordinal:   0,
		Text:      split.text,
		Source:    split.source,
		RepoRef:   split.repoRef,
		DocType:   split.docType,
		Author:    "dev@example.test",
		CreatedAt: split.created,
		UpdatedAt: split.created,
		ThreadID:  "thread-split",
		Embedding: split.embedding,
	}
	second := first
	second.Ordinal = 1
	if err := s.ReplaceChunks(ctx, split.id, []entities.Chunk{first, second}); err != nil {
		t.Fatalf("ReplaceChunks(%q): %v", split.id, err)
	}

	edges := []entities.Edge{{
		Src:        searchCorpus[0].id,
		Dst:        searchCorpus[1].id,
		Kind:       entities.EdgeKindCommitInPR,
		Confidence: 1,
	}, {
		Src:        searchCorpus[1].id,
		Dst:        searchCorpus[3].id,
		Kind:       entities.EdgeKindPRClosesIssue,
		Confidence: 1,
	}}
	if err := s.UpsertEdges(ctx, edges); err != nil {
		t.Fatalf("UpsertEdges: %v", err)
	}

	if err := s.SetCursor(ctx, "notion", lore.Cursor{"page": "3"}); err != nil {
		t.Fatalf("SetCursor(notion): %v", err)
	}
	if err := s.SetCursor(ctx, "github", nil); err != nil {
		t.Fatalf("SetCursor(github): %v", err)
	}

	if ok, err := s.TryAcquireLease(ctx, "host-1/4242"); err != nil || !ok {
		t.Fatalf("TryAcquireLease = %v, %v; want true, nil", ok, err)
	}

	got, err := s.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}

	if got.Documents != int64(len(searchCorpus)) {
		t.Errorf("Documents = %d, want %d", got.Documents, len(searchCorpus))
	}
	if got.Chunks != int64(len(searchCorpus))+1 {
		t.Errorf("Chunks = %d, want %d", got.Chunks, len(searchCorpus)+1)
	}
	if got.Edges != int64(len(edges)) {
		t.Errorf("Edges = %d, want %d", got.Edges, len(edges))
	}

	wantSources := []entities.SourceState{
		{ID: "github", Documents: 3, LastCheckpoint: stamp},
		{ID: "jira", Documents: 1},
		{ID: "notion", Documents: 1, LastCheckpoint: stamp},
	}
	if !slices.Equal(got.Sources, wantSources) {
		t.Errorf("Sources = %+v, want %+v", got.Sources, wantSources)
	}
	wantCursors := []entities.CursorAge{
		{Connector: "github", UpdatedAt: stamp},
		{Connector: "notion", UpdatedAt: stamp},
	}
	if !slices.Equal(got.Cursors, wantCursors) {
		t.Errorf("Cursors = %+v, want %+v", got.Cursors, wantCursors)
	}

	if got.Lease == nil {
		t.Fatal("Lease = nil, want the held lease")
	}
	if got.Lease.Holder != "host-1/4242" {
		t.Errorf("Lease.Holder = %q, want %q", got.Lease.Holder, "host-1/4242")
	}
	if !got.Lease.AcquiredAt.Equal(stamp) || !got.Lease.HeartbeatAt.Equal(stamp) {
		t.Errorf("Lease times = %s, %s; want both %s",
			got.Lease.AcquiredAt, got.Lease.HeartbeatAt, stamp)
	}

	if err := s.ReleaseLease(ctx, "host-1/4242"); err != nil {
		t.Fatalf("ReleaseLease: %v", err)
	}
	if got, err = s.Stats(ctx); err != nil {
		t.Fatalf("Stats (after release): %v", err)
	}
	if got.Lease != nil {
		t.Errorf("Lease = %+v, want nil after release", got.Lease)
	}
	if !slices.Equal(got.Sources, wantSources) {
		t.Errorf("Sources after release = %+v, want %+v", got.Sources, wantSources)
	}
	if !slices.Equal(got.Cursors, wantCursors) {
		t.Errorf("Cursors after release = %+v, want %+v", got.Cursors, wantCursors)
	}
}

func TestStatsCursorAgeAdvancesWithEveryCheckpoint(t *testing.T) {
	first := time.Date(2025, time.March, 12, 9, 30, 0, 0, time.UTC)
	clock := first
	s := openTestStore(t, WithClock(func() time.Time { return clock }))
	ctx := context.Background()

	if err := s.SetCursor(ctx, "github", lore.Cursor{"since": "a"}); err != nil {
		t.Fatalf("SetCursor: %v", err)
	}

	second := first.Add(90 * time.Minute)
	clock = second
	if err := s.SetCursor(ctx, "github", lore.Cursor{"since": "b"}); err != nil {
		t.Fatalf("SetCursor (update): %v", err)
	}

	got, err := s.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	wantSources := []entities.SourceState{{ID: "github", LastCheckpoint: second}}
	if !slices.Equal(got.Sources, wantSources) {
		t.Errorf("Sources = %+v, want %+v", got.Sources, wantSources)
	}
	if len(got.Cursors) != 1 {
		t.Fatalf("Cursors = %+v, want 1 entry", got.Cursors)
	}
	if !got.Cursors[0].UpdatedAt.Equal(second) {
		t.Errorf("UpdatedAt = %s, want the later checkpoint %s", got.Cursors[0].UpdatedAt, second)
	}
}

func TestStatsCountsDocumentsPerSource(t *testing.T) {
	stamp := time.Date(2025, time.March, 12, 9, 30, 0, 0, time.UTC)
	s := openTestStore(t, WithClock(func() time.Time { return stamp }))
	ctx := context.Background()

	docs := []lore.Document{
		{ID: lore.NewDocID("notion", lore.DocTypePage, "one"), Source: "notion", Type: lore.DocTypePage},
		{ID: lore.NewDocID("github", lore.DocTypePR, "one"), Source: "github", Type: lore.DocTypePR},
		{ID: lore.NewDocID("github", lore.DocTypePR, "two"), Source: "github", Type: lore.DocTypePR},
		{ID: lore.NewDocID("github", lore.DocTypePR, "three"), Source: "github", Type: lore.DocTypePR},
	}
	if err := s.UpsertDocuments(ctx, docs); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}
	if err := s.SetCursor(ctx, "notion", lore.Cursor{"page": "one"}); err != nil {
		t.Fatalf("SetCursor: %v", err)
	}

	got, err := s.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	wantSources := []entities.SourceState{
		{ID: "github", Documents: 3},
		{ID: "notion", Documents: 1, LastCheckpoint: stamp},
	}
	if !slices.Equal(got.Sources, wantSources) {
		t.Errorf("Sources = %+v, want %+v", got.Sources, wantSources)
	}
	wantCursors := []entities.CursorAge{{Connector: "notion", UpdatedAt: stamp}}
	if !slices.Equal(got.Cursors, wantCursors) {
		t.Errorf("Cursors = %+v, want %+v", got.Cursors, wantCursors)
	}
	if got.Documents != 4 || got.Chunks != 0 || got.Edges != 0 || got.Lease != nil {
		t.Errorf("Stats = %+v, want 4 documents, 0 chunks, 0 edges, no lease", got)
	}
}

func TestStatsDocumentsWithoutCheckpoint(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	doc := lore.Document{ID: lore.NewDocID("jira", lore.DocTypeTicket, "PROJ-1"), Source: "jira", Type: lore.DocTypeTicket}
	if err := s.UpsertDocuments(ctx, []lore.Document{doc}); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}

	got, err := s.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	want := []entities.SourceState{{ID: "jira", Documents: 1}}
	if !slices.Equal(got.Sources, want) {
		t.Errorf("Sources = %+v, want %+v", got.Sources, want)
	}
	if got.Cursors != nil {
		t.Errorf("Cursors = %+v, want none", got.Cursors)
	}
}

func TestStatsCursorOnlySources(t *testing.T) {
	stamp := time.Date(2025, time.March, 12, 9, 30, 0, 0, time.UTC)
	clock := stamp
	s := openTestStore(t, WithClock(func() time.Time { return clock }))
	ctx := context.Background()
	if err := s.SetCursor(ctx, "notion", nil); err != nil {
		t.Fatalf("SetCursor(notion): %v", err)
	}
	later := stamp.Add(time.Hour)
	clock = later
	if err := s.SetCursor(ctx, "github", lore.Cursor{"since": "one"}); err != nil {
		t.Fatalf("SetCursor(github): %v", err)
	}

	got, err := s.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	wantSources := []entities.SourceState{
		{ID: "github", LastCheckpoint: later},
		{ID: "notion", LastCheckpoint: stamp},
	}
	if !slices.Equal(got.Sources, wantSources) {
		t.Errorf("Sources = %+v, want %+v", got.Sources, wantSources)
	}
	wantCursors := []entities.CursorAge{
		{Connector: "github", UpdatedAt: later},
		{Connector: "notion", UpdatedAt: stamp},
	}
	if !slices.Equal(got.Cursors, wantCursors) {
		t.Errorf("Cursors = %+v, want %+v", got.Cursors, wantCursors)
	}
}
