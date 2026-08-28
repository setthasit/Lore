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

// QueryService answers questions about a workspace's history with cited
// evidence. It is the read side of the index: every tool in 05-query-engine.md
// runs the same pipeline and differs only in how the question is seeded.
//
// This wave implements the retrieval seed of that pipeline — hybrid retrieval
// (BM25 + vectors + RRF) lifted to parent documents. The graph walk, chains and
// gaps land with the edges wave, so bundles currently carry seed nodes only;
// Chains and Gaps are nil rather than empty-but-meaningful.
//
// Errors come back classified: caller mistakes as bad request, unsupported
// requests as precondition, and store or embedder failures wrapped as internal.
type QueryService interface {
	// FindDecision retrieves the documents that explain a decision and returns
	// them as an EvidenceBundle whose nodes are ordered most relevant first.
	// A question that matches nothing is an empty bundle, not an error: "the
	// index holds no evidence" is an answer, and the caller can widen its
	// filters. Every returned node carries a real URL, so nodes whose document
	// metadata is missing from the index are dropped instead of cited.
	FindDecision(ctx context.Context, req FindDecisionRequest) (*entities.EvidenceBundle, error)
}

// FindDecisionRequest is the find_decision tool's input. Question is required;
// everything else narrows retrieval and zero values do not constrain.
//
// Around is the free-text event or ISO date the question is anchored to
// ("incident X", "2025-03-12"). Event resolution is not implemented yet and the
// field is refused rather than dropped: silently answering an unanchored
// question would look like a working time filter.
type FindDecisionRequest struct {
	Question string
	Around   string
	Source   string
	Repo     string
	DocType  string
	Since    time.Time
	Until    time.Time
}

// defaultTopK is the retrieval width used when the caller passes a
// non-positive one. It mirrors the configuration default (config.DefaultTopK)
// that the wiring layer normally resolves; a zero here would ask the store for
// zero hits and turn a wiring slip into an index that answers nothing.
const defaultTopK = 12

type queryService struct {
	store repositories.IndexStore
	emb   embedder.Embedder
	topK  int
}

var _ QueryService = (*queryService)(nil)

// NewQueryService returns the retrieval-backed QueryService. topK is the number
// of chunks each retrieval strategy contributes to the fusion, per strategy and
// not in total: both lists are ranked independently and RRF merges them.
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

	// Sequential on purpose: two SQLite reads on one connection, one of them
	// preceded by a network round-trip to the embedder, do not get faster for
	// being raced, and a goroutine pair here would buy nothing but a second
	// error path.
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

// filtersOf compiles the request's metadata narrowing into the store's filter
// shape. Since/Until are the caller's explicit window; the window an Around
// event resolves to will land in the same two fields.
func filtersOf(req FindDecisionRequest) entities.Filters {
	return entities.Filters{
		Source:      req.Source,
		RepoRef:     req.Repo,
		DocType:     entities.DocType(req.DocType),
		CreatedFrom: req.Since,
		CreatedTo:   req.Until,
	}
}

// lift turns fused chunks into document-level evidence: retrieval scores
// chunks, but evidence is cited by document, so several chunks of one document
// collapse into a single node.
//
// fused is score-descending, so the first chunk seen for a document is both its
// best-scoring chunk and its most relevant excerpt. Keeping that one and
// discarding the rest therefore yields nodes that are already ordered
// descending, with no second sort needed.
//
// Documents the index does not hold metadata for are dropped rather than cited:
// DocumentsByID omits unknown ids by contract, and a node without a URL is not
// evidence (05-query-engine.md, "Every node carries a real URL"). Chunks can
// outlive their parent document row only through an inconsistent index, so this
// is a safety net rather than an expected path — hence a silent drop and not a
// gap the caller must read.
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
