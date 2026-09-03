package services

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/setthasit/Lore/internal/connectors/embedder"
	"github.com/setthasit/Lore/internal/entities"
	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/internal/repositories"
)

type ImpactService interface {
	ImpactOf(ctx context.Context, req ImpactRequest) (*entities.EvidenceBundle, error)
}

type ImpactRequest struct {
	Ref      string // a resolvable ref, or free text interpreted by retrieval
	Question string
}

type impactService struct {
	store repositories.IndexStore
	emb   embedder.Embedder
	cfg   QueryConfig
}

var _ ImpactService = (*impactService)(nil)

func NewImpactService(store repositories.IndexStore, emb embedder.Embedder, cfg QueryConfig) ImpactService {
	if cfg.TopK <= 0 {
		cfg.TopK = defaultTopK
	}
	if cfg.WalkDepth <= 0 {
		cfg.WalkDepth = defaultWalkDepth
	}

	return &impactService{store: store, emb: emb, cfg: cfg}
}

func (s *impactService) ImpactOf(ctx context.Context, req ImpactRequest) (*entities.EvidenceBundle, error) {
	input := strings.TrimSpace(req.Ref)
	if input == "" {
		return nil, internalerror.NewBadRequestError("ref or query must not be empty", nil)
	}

	anchor, err := s.impactAnchorOf(ctx, input)
	if err != nil {
		return nil, err
	}

	at := anchor.meta.CreatedAt
	walked, err := walkGraph(ctx, s.store, []entities.DocID{anchor.meta.ID},
		walkOptions{Depth: s.cfg.WalkDepth, Direction: entities.DirBoth, TimeAfter: &at})
	if err != nil {
		return nil, internalerror.NewInternalError("walking the provenance graph failed", err)
	}

	question := impactQuestionOf(req.Question, anchor.meta.Title)
	excerpt := anchorExcerpt(anchor.body)
	matches, err := s.impactMatches(ctx, impactRetrievalText(question, excerpt), at)
	if err != nil {
		return nil, err
	}

	nodes := impactNodes(anchor.meta, excerpt, walked, matches)
	chains := assembleChains(walked.Paths, walked.SeedLinks, nodes)

	return &entities.EvidenceBundle{
		Question: question,
		Anchor:   anchor.evidenceAnchor(),
		Nodes:    nodes,
		Chains:   chains,
		Gaps:     impactGaps(nodes, chains, at),
	}, nil
}

type impactAnchor struct {
	meta      entities.DocumentMeta
	body      string
	fromQuery string
}

func (a impactAnchor) evidenceAnchor() entities.Anchor {
	kind := entities.AnchorDocument
	if a.fromQuery != "" {
		kind |= entities.AnchorQuery
	}

	return entities.Anchor{
		Kind:  kind,
		Query: a.fromQuery,
		Doc: &entities.DocRef{
			ID:        a.meta.ID,
			Title:     a.meta.Title,
			URL:       a.meta.URL,
			CreatedAt: a.meta.CreatedAt,
		},
	}
}

func (s *impactService) impactAnchorOf(ctx context.Context, input string) (impactAnchor, error) {
	anchor, err := s.impactResolved(ctx, input)
	if err != nil {
		return impactAnchor{}, err
	}

	body, err := documentBody(ctx, s.store, anchor.meta.ID)
	if err != nil {
		return impactAnchor{}, err
	}
	anchor.body = body

	return anchor, nil
}

func (s *impactService) impactResolved(ctx context.Context, input string) (impactAnchor, error) {
	anchor, found, err := resolveOneRef(ctx, s.store, input)
	if err != nil {
		return impactAnchor{}, err
	}
	if !found {
		return s.impactInterpreted(ctx, input)
	}

	return impactAnchor{meta: anchor}, nil
}

func (s *impactService) impactInterpreted(ctx context.Context, input string) (impactAnchor, error) {
	fused, err := hybridSearch(ctx, s.store, s.emb, input, entities.Filters{}, s.cfg.TopK)
	if err != nil {
		return impactAnchor{}, err
	}
	seeds, err := liftDocuments(ctx, s.store, fused)
	if err != nil {
		return impactAnchor{}, err
	}
	if len(seeds) == 0 {
		return impactAnchor{}, internalerror.NewNotFoundError(fmt.Sprintf(
			"nothing in the index matches %q", input), nil)
	}

	return impactAnchor{meta: seeds[0].Meta, fromQuery: input}, nil
}

func (s *impactService) impactMatches(ctx context.Context, text string, at time.Time) ([]seedHit, error) {
	fused, err := hybridSearch(ctx, s.store, s.emb, text, entities.Filters{CreatedFrom: at}, s.cfg.TopK)
	if err != nil {
		return nil, err
	}
	seeds, err := liftDocuments(ctx, s.store, fused)
	if err != nil {
		return nil, err
	}

	// Timestamps persist at second precision, so CreatedFrom can admit the anchor's own second.
	after := make([]seedHit, 0, len(seeds))
	for _, seed := range seeds {
		if seed.Meta.CreatedAt.After(at) {
			after = append(after, seed)
		}
	}

	return after, nil
}

func impactQuestionOf(question, anchorTitle string) string {
	if asked := strings.TrimSpace(question); asked != "" {
		return asked
	}

	return "consequences, follow-ups, incidents related to " + anchorTitle
}

func impactRetrievalText(question, excerpt string) string {
	if excerpt == "" {
		return question
	}

	return question + "\n\n" + excerpt
}

func impactNodes(
	anchor entities.DocumentMeta,
	excerpt string,
	walked walkResult,
	matches []seedHit,
) []entities.EvidenceNode {
	collected := newNodeSet(1 + len(walked.Paths) + len(matches))
	collected.add(entities.EvidenceNode{Doc: anchor, Excerpt: excerpt, Role: entities.RoleSeed, Score: 1})

	collected.addWalked(walked, followUpRole)
	collected.addMatches(matches)
	slices.SortFunc(collected.nodes, byChronology)

	return collected.nodes
}

// impact_of reports every reached document as a consequence, whatever its type.
func followUpRole(entities.DocType) string {
	return entities.RoleFollowUp
}

func impactGaps(nodes []entities.EvidenceNode, chains [][]entities.DocID, at time.Time) []string {
	standalone := standaloneSeedGaps(nodes, chains)
	if len(nodes) > 1 {
		return standalone
	}

	return append([]string{"no follow-up evidence after " + at.Format(time.DateOnly)}, standalone...)
}
