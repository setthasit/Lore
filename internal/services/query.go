package services

import (
	"context"
	"fmt"
	"strings"
	"time"

	"lore/internal/connectors/embedder"
	"lore/internal/entities"
	"lore/internal/errors/internalerror"
	"lore/internal/repositories"
)

type QueryService interface {
	// Nodes whose document metadata is missing from the index are dropped, not cited.
	FindDecision(ctx context.Context, req FindDecisionRequest) (*entities.EvidenceBundle, error)
}

// Around is refused, not ignored: event anchoring is unsupported.
type FindDecisionRequest struct {
	Question string
	Around   string
	Source   string
	Repo     string
	DocType  string
	Since    time.Time
	Until    time.Time
}

const defaultTopK = 12

type queryService struct {
	store repositories.IndexStore
	emb   embedder.Embedder
	topK  int
}

var _ QueryService = (*queryService)(nil)

func NewQueryService(store repositories.IndexStore, emb embedder.Embedder, topK int) QueryService {
	if topK <= 0 {
		topK = defaultTopK
	}

	return &queryService{store: store, emb: emb, topK: topK}
}

func (q *queryService) FindDecision(ctx context.Context, req FindDecisionRequest) (*entities.EvidenceBundle, error) {
	question := strings.TrimSpace(req.Question)
	if question == "" {
		return nil, internalerror.NewBadRequestError("question must not be empty", nil)
	}
	if strings.TrimSpace(req.Around) != "" {
		return nil, internalerror.NewPreconditionError("event anchoring not yet supported", nil)
	}

	filters := filtersOf(req)

	vectors, err := q.emb.Embed(ctx, []string{question})
	if err != nil {
		return nil, internalerror.NewInternalError("embedding the question failed", err)
	}
	if len(vectors) != 1 {
		return nil, internalerror.NewInternalError(
			fmt.Sprintf("embedder returned %d vectors for one text", len(vectors)), nil)
	}

	lexical, err := q.store.SearchLexical(ctx, question, filters, q.topK)
	if err != nil {
		return nil, internalerror.NewInternalError("lexical search failed", err)
	}
	semantic, err := q.store.SearchVector(ctx, vectors[0], filters, q.topK)
	if err != nil {
		return nil, internalerror.NewInternalError("vector search failed", err)
	}

	nodes, err := q.lift(ctx, fuse(lexical, semantic))
	if err != nil {
		return nil, err
	}

	return &entities.EvidenceBundle{
		Question: question,
		Anchor:   entities.Anchor{Kind: entities.AnchorQuery, Query: question},
		Nodes:    nodes,
	}, nil
}

func filtersOf(req FindDecisionRequest) entities.Filters {
	return entities.Filters{
		Source:      req.Source,
		RepoRef:     req.Repo,
		DocType:     entities.DocType(req.DocType),
		CreatedFrom: req.Since,
		CreatedTo:   req.Until,
	}
}

func (q *queryService) lift(ctx context.Context, fused []fusedChunk) ([]entities.EvidenceNode, error) {
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

	metas, err := q.store.DocumentsByID(ctx, ids)
	if err != nil {
		return nil, internalerror.NewInternalError("loading document metadata failed", err)
	}
	byID := make(map[entities.DocID]entities.DocumentMeta, len(metas))
	for _, meta := range metas {
		byID[meta.ID] = meta
	}

	nodes := make([]entities.EvidenceNode, 0, len(ids))
	for _, id := range ids {
		meta, held := byID[id]
		if !held || meta.URL == "" {
			continue
		}
		nodes = append(nodes, entities.EvidenceNode{
			Doc:     meta,
			Excerpt: best[id].Text,
			Role:    entities.RoleSeed,
			Score:   best[id].Score,
		})
	}

	return nodes, nil
}
