package services

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/setthasit/Lore/internal/entities"
	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/internal/repositories"
	"github.com/setthasit/Lore/sdk"
)

type TraceService interface {
	// The anchor node's Excerpt is its body, cut with a trailing notice when over the cap, or the passages matching Focus when any match.
	Trace(ctx context.Context, req TraceRequest) (*entities.EvidenceBundle, error)
}

type TraceRequest struct {
	Ref       string
	Direction string
	Depth     int
	Focus     string
}

const (
	defaultTraceDepth = 2
	maxTraceDepth     = 2

	maxTraceExcerptRunes = 8000
	maxFocusPassages     = 3
	maxFocusRunes        = 1000
	skippedPassageMark   = "…"
	focusHint            = " Pass focus with a question to get the passages that match it."
)

type traceService struct {
	store repositories.IndexStore
	emb   lore.Embedder
}

var _ TraceService = (*traceService)(nil)

func NewTraceService(store repositories.IndexStore, emb lore.Embedder) TraceService {
	return &traceService{store: store, emb: emb}
}

func (t *traceService) Trace(ctx context.Context, req TraceRequest) (*entities.EvidenceBundle, error) {
	ref := strings.TrimSpace(req.Ref)
	if ref == "" {
		return nil, internalerror.NewBadRequestError("ref must not be empty", nil)
	}
	focus := strings.TrimSpace(req.Focus)
	if utf8.RuneCountInString(focus) > maxFocusRunes {
		return nil, internalerror.NewBadRequestError(
			fmt.Sprintf("focus must be at most %s characters", groupThousands(maxFocusRunes)), nil)
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
	excerpt, focusGaps, err := t.traceExcerpt(ctx, anchor, body, focus)
	if err != nil {
		return nil, err
	}

	walked, err := walkGraph(ctx, t.store, []lore.DocID{anchor.ID},
		walkOptions{Depth: traceDepth(req.Depth), Direction: direction})
	if err != nil {
		return nil, internalerror.NewInternalError("walking the provenance graph failed", err)
	}

	nodes := traceNodes(anchor, excerpt, walked)
	chains := assembleChains(walked.Paths, walked.SeedLinks, nodes)
	standalone, err := collapseUnlinked(ctx, t.store, standaloneSeedGaps(nodes, chains))
	if err != nil {
		return nil, err
	}

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
		Gaps:   append(standalone, focusGaps...),
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

func (t *traceService) traceExcerpt(
	ctx context.Context,
	anchor entities.DocumentMeta,
	body string,
	focus string,
) (string, []string, error) {
	if focus == "" {
		return capExcerpt(body, focusHint), nil, nil
	}

	ranked, err := hybridSearch(ctx, t.store, t.emb, focus, entities.Filters{DocID: anchor.ID}, maxFocusPassages)
	if err != nil {
		return "", nil, err
	}
	if len(ranked) == 0 {
		gap := fmt.Sprintf("focus %q matched no passage of %s (%s)", focus, anchor.Title, anchor.ID)
		return capExcerpt(body, focusHint), []string{gap}, nil
	}

	passages := ranked[:min(len(ranked), maxFocusPassages)]
	slices.SortFunc(passages, func(a, b fusedChunk) int { return cmp.Compare(a.Ordinal, b.Ordinal) })

	return capExcerpt(joinPassages(passages), ""), nil, nil
}

func joinPassages(inPageOrder []fusedChunk) string {
	var joined strings.Builder
	for i, passage := range inPageOrder {
		if i > 0 {
			joined.WriteString(blockSeparator)
			if passage.Ordinal != inPageOrder[i-1].Ordinal+1 {
				joined.WriteString(skippedPassageMark + blockSeparator)
			}
		}
		joined.WriteString(passage.Text)
	}

	return joined.String()
}

func capExcerpt(text, hint string) string {
	shown := runePrefix(text, maxTraceExcerptRunes)
	if len(shown) == len(text) {
		return text
	}

	return shown + blockSeparator + fmt.Sprintf("[truncated: %s of %s characters shown.%s]",
		groupThousands(maxTraceExcerptRunes), groupThousands(utf8.RuneCountInString(text)), hint)
}

func runePrefix(text string, limit int) string {
	seen := 0
	for start := range text {
		if seen == limit {
			return text[:start]
		}
		seen++
	}

	return text
}

func groupThousands(n int) string {
	digits := strconv.Itoa(n)
	for at := len(digits) - 3; at > 0; at -= 3 {
		digits = digits[:at] + "," + digits[at:]
	}

	return digits
}

func traceNodes(anchor entities.DocumentMeta, excerpt string, walked walkResult) []entities.EvidenceNode {
	collected := newNodeSet(len(walked.Paths) + 1)
	collected.add(entities.EvidenceNode{Doc: anchor, Excerpt: excerpt, Role: entities.RoleSeed, Score: 1})

	collected.addWalked(walked, graphRole)
	slices.SortFunc(collected.nodes, byChronology)

	return collected.nodes
}
