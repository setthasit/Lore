package services

import (
	"fmt"
	"testing"

	"lore/internal/entities"
)

// The fusion constant and formula, restated independently of the
// implementation so these tests fail if either drifts: rank is 1-based and
// every list a chunk appears in contributes 1/(60+rank).
const fusionK = 60

func rrf(ranks ...int) float32 {
	var score float32
	for _, rank := range ranks {
		score += 1 / float32(fusionK+rank)
	}

	return score
}

const scoreEpsilon = 1e-6

// decoyScore is the backend-native score fusion must ignore. Every hit gets the
// same one, so a fusion that read scores instead of ranks would flatten every
// ordering these tests assert.
const decoyScore = -7.5

func hit(doc string, ordinal int) entities.ChunkHit {
	return entities.ChunkHit{
		Chunk: entities.Chunk{
			DocID:   entities.DocID(doc),
			Ordinal: ordinal,
			Text:    fmt.Sprintf("%s chunk %d", doc, ordinal),
		},
		Score: decoyScore,
	}
}

// fusedWant is one expected position in the fused order.
type fusedWant struct {
	doc     string
	ordinal int
	score   float32
}

func TestFuse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		lists [][]entities.ChunkHit
		want  []fusedWant
	}{
		{
			name:  "no lists at all fuses to nothing",
			lists: nil,
		},
		{
			name:  "empty lists fuse to nothing",
			lists: [][]entities.ChunkHit{{}, {}},
		},
		{
			name:  "a single list keeps its own ranking",
			lists: [][]entities.ChunkHit{{hit("a", 0), hit("b", 0), hit("c", 0)}},
			want: []fusedWant{
				{doc: "a", score: rrf(1)},
				{doc: "b", score: rrf(2)},
				{doc: "c", score: rrf(3)},
			},
		},
		{
			name:  "one empty list leaves the other's ranking untouched",
			lists: [][]entities.ChunkHit{{hit("a", 0), hit("b", 0)}, {}},
			want: []fusedWant{
				{doc: "a", score: rrf(1)},
				{doc: "b", score: rrf(2)},
			},
		},
		{
			name: "disjoint lists interleave by rank, list order breaking ties",
			lists: [][]entities.ChunkHit{
				{hit("a", 0), hit("b", 0)},
				{hit("c", 0), hit("d", 0)},
			},
			want: []fusedWant{
				{doc: "a", score: rrf(1)},
				{doc: "c", score: rrf(1)},
				{doc: "b", score: rrf(2)},
				{doc: "d", score: rrf(2)},
			},
		},
		{
			name: "identical lists double every score and keep the order",
			lists: [][]entities.ChunkHit{
				{hit("a", 0), hit("b", 0), hit("c", 0)},
				{hit("a", 0), hit("b", 0), hit("c", 0)},
			},
			want: []fusedWant{
				{doc: "a", score: rrf(1, 1)},
				{doc: "b", score: rrf(2, 2)},
				{doc: "c", score: rrf(3, 3)},
			},
		},
		{
			name: "agreement outranks a single list's confidence",
			lists: [][]entities.ChunkHit{
				{hit("solo", 0), hit("both", 0)},
				{hit("both", 0), hit("other", 0)},
			},
			want: []fusedWant{
				{doc: "both", score: rrf(2, 1)},
				{doc: "solo", score: rrf(1)},
				{doc: "other", score: rrf(2)},
			},
		},
		{
			name: "reversed lists tie and stay in first-seen order",
			lists: [][]entities.ChunkHit{
				{hit("a", 0), hit("b", 0)},
				{hit("b", 0), hit("a", 0)},
			},
			want: []fusedWant{
				{doc: "a", score: rrf(1, 2)},
				{doc: "b", score: rrf(2, 1)},
			},
		},
		{
			name: "chunks of one document fuse independently by ordinal",
			lists: [][]entities.ChunkHit{
				{hit("a", 0), hit("a", 3)},
				{hit("a", 3)},
			},
			want: []fusedWant{
				{doc: "a", ordinal: 3, score: rrf(2, 1)},
				{doc: "a", ordinal: 0, score: rrf(1)},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := fuse(tt.lists...)
			if len(got) != len(tt.want) {
				t.Fatalf("fused %d chunks, want %d: %+v", len(got), len(tt.want), got)
			}
			for i, want := range tt.want {
				if string(got[i].DocID) != want.doc || got[i].Ordinal != want.ordinal {
					t.Errorf("position %d = %s#%d, want %s#%d",
						i, got[i].DocID, got[i].Ordinal, want.doc, want.ordinal)

					continue
				}
				if diff := got[i].Score - want.score; diff > scoreEpsilon || diff < -scoreEpsilon {
					t.Errorf("%s#%d score = %v, want %v", want.doc, want.ordinal, got[i].Score, want.score)
				}
				if got[i].Text != fmt.Sprintf("%s chunk %d", want.doc, want.ordinal) {
					t.Errorf("%s#%d carries text %q", want.doc, want.ordinal, got[i].Text)
				}
			}
		})
	}
}

func TestFuseIsDescending(t *testing.T) {
	t.Parallel()

	lexical := []entities.ChunkHit{hit("a", 0), hit("b", 1), hit("c", 0), hit("d", 2)}
	semantic := []entities.ChunkHit{hit("d", 2), hit("e", 0), hit("a", 0)}

	fused := fuse(lexical, semantic)
	if len(fused) != 5 {
		t.Fatalf("fused %d chunks, want 5", len(fused))
	}
	for i := 1; i < len(fused); i++ {
		if fused[i-1].Score < fused[i].Score {
			t.Errorf("position %d (%v) outscores position %d (%v)",
				i, fused[i].Score, i-1, fused[i-1].Score)
		}
	}
}
