package services

import (
	"sort"

	"github.com/setthasit/Lore/internal/entities"
	"github.com/setthasit/Lore/sdk"
)

// k = 60 is the constant the original RRF paper reports as robust across collections.
const rrfK = 60

type chunkKey struct {
	doc     lore.DocID
	ordinal int
}

type fusedChunk struct {
	entities.Chunk
	Score float32
}

// Ties keep first-seen order, so fusion is deterministic.
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
