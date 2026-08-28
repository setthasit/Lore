package services

import (
	"sort"

	"lore/internal/entities"
)

// rrfK damps the head of every ranked list in Reciprocal Rank Fusion
// (03-data-model.md, "Hybrid retrieval"): score(d) = Σ 1/(k + rank_i(d)),
// k = 60. It is the constant the original RRF paper reports as robust across
// collections, and it is what keeps a single list's top hit from dominating the
// merge: at k = 60 the gap between rank 1 and rank 2 is ~2% of the score, so
// agreement between the two strategies outweighs one strategy's confidence.
const rrfK = 60

// chunkKey is a chunk's identity across ranked lists. Chunk carries no id of its
// own — a chunk is a position within a document body — so the parent document
// plus the ordinal is what makes the same chunk recognisable in both lists.
type chunkKey struct {
	doc     entities.DocID
	ordinal int
}

// fusedChunk is a chunk with its fused relevance. Unlike ChunkHit.Score, which
// is backend-native and comparable inside one result list only, Score here is
// derived purely from ranks and is therefore comparable across every strategy
// that contributed to the fusion.
type fusedChunk struct {
	entities.Chunk
	Score float32
}

// fuse merges independently ranked hit lists by Reciprocal Rank Fusion and
// returns every distinct chunk once, best first.
//
// Only ranks are read, never the incoming scores: the store's contract makes
// lexical relevance and vector distance incomparable quantities, which is
// exactly the problem rank fusion solves. A chunk retrieved by both strategies
// accumulates both reciprocals, so cross-strategy agreement outranks a single
// strategy's top hit.
//
// Ties keep the order in which the chunks were first seen — list order, then
// rank within the list — so fusion is deterministic and a chunk that led the
// first list stays ahead of an equally scored latecomer. Empty input, and input
// whose lists are all empty, yields no hits and allocates nothing.
func fuse(lists ...[]entities.ChunkHit) []fusedChunk {
	total := 0
	for _, list := range lists {
		total += len(list)
	}
	if total == 0 {
		return nil
	}

	fused := make([]fusedChunk, 0, total)
	at := make(map[chunkKey]int, total)
	for _, list := range lists {
		for rank, hit := range list {
			score := 1 / float32(rrfK+rank+1) // ranks are 1-based
			key := chunkKey{doc: hit.DocID, ordinal: hit.Ordinal}
			if i, seen := at[key]; seen {
				fused[i].Score += score
				continue
			}
			at[key] = len(fused)
			fused = append(fused, fusedChunk{Chunk: hit.Chunk, Score: score})
		}
	}
	sort.SliceStable(fused, func(i, j int) bool { return fused[i].Score > fused[j].Score })

	return fused
}
