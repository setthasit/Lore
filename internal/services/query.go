package services

import (
	"context"
	"strings"
	"time"

	"github.com/setthasit/Lore/internal/connectors/embedder"
	"github.com/setthasit/Lore/internal/entities"
	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/internal/repositories"
)

type QueryService interface {
	// Nodes whose document metadata is missing from the index are dropped, not cited.
	FindDecision(ctx context.Context, req FindDecisionRequest) (*entities.EvidenceBundle, error)
}

type FindDecisionRequest struct {
	Question string
	Around   string
	Source   string
	Repo     string
	DocType  string
	Since    time.Time
	Until    time.Time
}

const (
	defaultTopK        = 12
	defaultEventWindow = 30 * 24 * time.Hour
)

type QueryConfig struct {
	TopK        int
	WalkDepth   int
	EventWindow time.Duration
}

type queryService struct {
	store repositories.IndexStore
	emb   embedder.Embedder
	cfg   QueryConfig
	now   func() time.Time
}

var _ QueryService = (*queryService)(nil)

func NewQueryService(store repositories.IndexStore, emb embedder.Embedder, cfg QueryConfig) QueryService {
	if cfg.TopK <= 0 {
		cfg.TopK = defaultTopK
	}
	if cfg.WalkDepth <= 0 {
		cfg.WalkDepth = defaultWalkDepth
	}
	if cfg.EventWindow <= 0 {
		cfg.EventWindow = defaultEventWindow
	}

	return &queryService{store: store, emb: emb, cfg: cfg, now: time.Now}
}

func (q *queryService) FindDecision(ctx context.Context, req FindDecisionRequest) (*entities.EvidenceBundle, error) {
	question := strings.TrimSpace(req.Question)
	if question == "" {
		return nil, internalerror.NewBadRequestError("question must not be empty", nil)
	}

	event, err := resolveEvent(ctx, q.store, q.emb, req.Around,
		eventOptions{Window: q.cfg.EventWindow, TopK: q.cfg.TopK})
	if err != nil {
		return nil, err
	}

	fused, err := hybridSearch(ctx, q.store, q.emb, question, filtersOf(req, event.Window), q.cfg.TopK)
	if err != nil {
		return nil, err
	}
	seeds, err := liftDocuments(ctx, q.store, fused)
	if err != nil {
		return nil, err
	}

	walked, err := walkGraph(ctx, q.store, seedIDs(seeds),
		walkOptions{Depth: q.cfg.WalkDepth, Direction: entities.DirBoth})
	if err != nil {
		return nil, internalerror.NewInternalError("walking the provenance graph failed", err)
	}

	found := rank(rankRequest{Seeds: seeds, Walk: walked, Window: event.Window, Now: q.now()})

	return &entities.EvidenceBundle{
		Question: question,
		Anchor:   anchorOf(question, event.Window),
		Nodes:    found.Nodes,
		Chains:   found.Chains,
		Gaps:     gapsOf(event.Gap, found.Gaps),
	}, nil
}

func filtersOf(req FindDecisionRequest, window *entities.TimeWindow) entities.Filters {
	from, to := intersected(req.Since, req.Until, window)

	return entities.Filters{
		Source:      req.Source,
		RepoRef:     req.Repo,
		DocType:     entities.DocType(req.DocType),
		CreatedFrom: from,
		CreatedTo:   to,
	}
}

// A zero bound is absent, not an instant in year 1, so it never narrows the window.
func intersected(since, until time.Time, window *entities.TimeWindow) (from, to time.Time) {
	if window == nil {
		return since, until
	}

	from, to = window.From, window.To
	if since.After(from) {
		from = since
	}
	if !until.IsZero() && until.Before(to) {
		to = until
	}

	return from, to
}

func seedIDs(seeds []seedHit) []entities.DocID {
	ids := make([]entities.DocID, len(seeds))
	for i, seed := range seeds {
		ids[i] = seed.Meta.ID
	}

	return ids
}

func anchorOf(question string, window *entities.TimeWindow) entities.Anchor {
	kind := entities.AnchorQuery
	if window != nil {
		kind |= entities.AnchorTimeWindow
	}

	return entities.Anchor{Kind: kind, Query: question, Window: window}
}

// An unresolved event comes first: it explains why the rest is unwindowed.
func gapsOf(event string, found []string) []string {
	if event == "" {
		return found
	}

	return append([]string{event}, found...)
}
