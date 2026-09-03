package services

import (
	"context"
	"fmt"
	"unicode/utf8"

	"github.com/setthasit/Lore/internal/connectors/embedder"
	"github.com/setthasit/Lore/internal/entities"
	"github.com/setthasit/Lore/internal/errors/internalerror"
)

const anchorExcerptChars = 500

type searchSource interface {
	SearchLexical(ctx context.Context, query string, f entities.Filters, k int) ([]entities.ChunkHit, error)
	SearchVector(ctx context.Context, embedding []float32, f entities.Filters, k int) ([]entities.ChunkHit, error)
}

type documentSource interface {
	DocumentsByID(ctx context.Context, ids []entities.DocID) ([]entities.DocumentMeta, error)
}

func hybridSearch(
	ctx context.Context,
	s searchSource,
	emb embedder.Embedder,
	query string,
	f entities.Filters,
	k int,
) ([]fusedChunk, error) {
	vectors, err := emb.Embed(ctx, []string{query})
	if err != nil {
		return nil, internalerror.NewInternalError("embedding the question failed", err)
	}
	if len(vectors) != 1 {
		return nil, internalerror.NewInternalError(
			fmt.Sprintf("embedder returned %d vectors for one text", len(vectors)), nil)
	}

	lexical, err := s.SearchLexical(ctx, query, f, k)
	if err != nil {
		return nil, internalerror.NewInternalError("lexical search failed", err)
	}
	semantic, err := s.SearchVector(ctx, vectors[0], f, k)
	if err != nil {
		return nil, internalerror.NewInternalError("vector search failed", err)
	}

	return fuse(lexical, semantic), nil
}

// One seed per document, in fusion order.
func liftDocuments(ctx context.Context, s documentSource, fused []fusedChunk) ([]seedHit, error) {
	if len(fused) == 0 {
		return nil, nil
	}

	ids := make([]entities.DocID, 0, len(fused))
	best := make(map[entities.DocID]fusedChunk, len(fused))
	for _, chunk := range fused {
		if _, seen := best[chunk.DocID]; seen {
			continue
		}
		best[chunk.DocID] = chunk
		ids = append(ids, chunk.DocID)
	}

	metas, err := s.DocumentsByID(ctx, ids)
	if err != nil {
		return nil, internalerror.NewInternalError("loading document metadata failed", err)
	}
	byID := make(map[entities.DocID]entities.DocumentMeta, len(metas))
	for _, meta := range metas {
		byID[meta.ID] = meta
	}

	seeds := make([]seedHit, 0, len(ids))
	for _, id := range ids {
		meta, held := byID[id]
		if !held || meta.URL == "" {
			continue
		}
		seeds = append(seeds, seedHit{Meta: meta, Excerpt: best[id].Text, Relevance: best[id].Score})
	}

	return seeds, nil
}

func anchorExcerpt(body string) string {
	if len(body) <= anchorExcerptChars {
		return body
	}

	cut := anchorExcerptChars
	for cut > 0 && !utf8.RuneStart(body[cut]) {
		cut--
	}

	return body[:cut]
}
