package services

import (
	"context"
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

	fused, err := hybridSearch(ctx, q.store, q.emb, question, filtersOf(req), q.topK)
	if err != nil {
		return nil, err
	}
	seeds, err := liftDocuments(ctx, q.store, fused)
	if err != nil {
		return nil, err
	}

	return &entities.EvidenceBundle{
		Question: question,
		Anchor:   entities.Anchor{Kind: entities.AnchorQuery, Query: question},
		Nodes:    seedNodes(seeds),
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

func seedNodes(seeds []seedHit) []entities.EvidenceNode {
	nodes := make([]entities.EvidenceNode, 0, len(seeds))
	for _, seed := range seeds {
		nodes = append(nodes, entities.EvidenceNode{
			Doc:     seed.Meta,
			Excerpt: seed.Excerpt,
			Role:    entities.RoleSeed,
			Score:   seed.Relevance,
		})
	}

	return nodes
}
