package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/setthasit/Lore/internal/entities"
	"github.com/setthasit/Lore/sdk"
)

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
	if got.Lease != nil {
		t.Errorf("Lease = %+v, want nil", got.Lease)
	}
}

func TestStatsReportsCountsCursorsAndLease(t *testing.T) {
	// A fixed clock makes the cursor stamp and the lease timestamps assertable.
	stamp := time.Date(2025, time.March, 12, 9, 30, 0, 0, time.UTC)
	s := openTestStore(t, WithClock(func() time.Time { return stamp }))
	ctx := context.Background()

	seedSearchCorpus(t, s)

	// A second chunk under one document keeps the three counts distinct, so a
	// transposed positional scan in Stats cannot pass.
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

	if len(got.Cursors) != 2 {
		t.Fatalf("Cursors = %+v, want 2 entries", got.Cursors)
	}
	if got.Cursors[0].Connector != "github" || got.Cursors[1].Connector != "notion" {
		t.Errorf("Cursors order = %q, %q; want github, notion",
			got.Cursors[0].Connector, got.Cursors[1].Connector)
	}
	for _, age := range got.Cursors {
		if !age.UpdatedAt.Equal(stamp) {
			t.Errorf("%s UpdatedAt = %s, want %s", age.Connector, age.UpdatedAt, stamp)
		}
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
	if len(got.Cursors) != 2 {
		t.Errorf("Cursors = %+v, want 2 entries after release", got.Cursors)
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
	if len(got.Cursors) != 1 {
		t.Fatalf("Cursors = %+v, want 1 entry", got.Cursors)
	}
	if !got.Cursors[0].UpdatedAt.Equal(second) {
		t.Errorf("UpdatedAt = %s, want the later checkpoint %s", got.Cursors[0].UpdatedAt, second)
	}
}
