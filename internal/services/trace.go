package services

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/setthasit/Lore/internal/entities"
	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/internal/repositories"
)

type TraceService interface {
	// The anchor node's Excerpt is the document's whole body, not a span.
	Trace(ctx context.Context, req TraceRequest) (*entities.EvidenceBundle, error)
}

type TraceRequest struct {
	Ref       string
	Direction string
	Depth     int
}

const (
	defaultTraceDepth = 2
	maxTraceDepth     = 2
)

type traceService struct {
	store repositories.IndexStore
}

var _ TraceService = (*traceService)(nil)

func NewTraceService(store repositories.IndexStore) TraceService {
	return &traceService{store: store}
}

func (t *traceService) Trace(ctx context.Context, req TraceRequest) (*entities.EvidenceBundle, error) {
	ref := strings.TrimSpace(req.Ref)
	if ref == "" {
		return nil, internalerror.NewBadRequestError("ref must not be empty", nil)
	}
	direction, err := traceDirection(req.Direction)
	if err != nil {
		return nil, err
	}

	anchor, err := t.traceAnchor(ctx, ref)
	if err != nil {
		return nil, err
	}
	body, err := documentBody(ctx, t.store, anchor.ID)
	if err != nil {
		return nil, err
	}

	walked, err := walkGraph(ctx, t.store, []entities.DocID{anchor.ID},
		walkOptions{Depth: traceDepth(req.Depth), Direction: direction})
	if err != nil {
		return nil, internalerror.NewInternalError("walking the provenance graph failed", err)
	}

	nodes := traceNodes(anchor, body, walked)
	chains := assembleChains(walked.Paths, walked.SeedLinks, nodes)

	return &entities.EvidenceBundle{
		Question: "provenance of " + anchor.Title,
		Anchor: entities.Anchor{
			Kind: entities.AnchorDocument,
			Doc: &entities.DocRef{
				ID:        anchor.ID,
				Title:     anchor.Title,
				URL:       anchor.URL,
				CreatedAt: anchor.CreatedAt,
			},
		},
		Nodes:  nodes,
		Chains: chains,
		Gaps:   standaloneSeedGaps(nodes, chains),
	}, nil
}

func traceDirection(direction string) (entities.Direction, error) {
	switch direction {
	case "out":
		return entities.DirOut, nil
	case "in":
		return entities.DirIn, nil
	case "both", "":
		return entities.DirBoth, nil
	default:
		return entities.DirBoth, internalerror.NewBadRequestError(
			fmt.Sprintf(`direction %q must be one of "in", "out", "both"`, direction), nil)
	}
}

func traceDepth(depth int) int {
	if depth <= 0 {
		return defaultTraceDepth
	}

	return min(depth, maxTraceDepth)
}

func (t *traceService) traceAnchor(ctx context.Context, ref string) (entities.DocumentMeta, error) {
	anchor, found, err := resolveOneRef(ctx, t.store, ref)
	if err != nil {
		return entities.DocumentMeta{}, err
	}
	if !found {
		return entities.DocumentMeta{}, internalerror.NewNotFoundError(
			fmt.Sprintf("ref %q matches no document", ref), nil)
	}

	return anchor, nil
}

func traceNodes(anchor entities.DocumentMeta, body string, walked walkResult) []entities.EvidenceNode {
	collected := newNodeSet(len(walked.Paths) + 1)
	collected.add(entities.EvidenceNode{Doc: anchor, Excerpt: body, Role: entities.RoleSeed, Score: 1})

	collected.addWalked(walked, graphRole)
	slices.SortFunc(collected.nodes, byChronology)

	return collected.nodes
}
