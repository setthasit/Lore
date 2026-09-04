package services

import (
	"cmp"
	"fmt"
	"slices"
	"time"

	"github.com/setthasit/Lore/internal/entities"
	"github.com/setthasit/Lore/sdk"
)

type rankWeights struct {
	ProximityDecay float32
	RecencyPenalty float32
	RecencyHorizon time.Duration
	WindowFalloff  float32
	MinWindowSpan  time.Duration
}

var defaultRankWeights = rankWeights{
	ProximityDecay: 0.6,
	RecencyPenalty: 0.2,
	RecencyHorizon: 365 * 24 * time.Hour,
	WindowFalloff:  1,
	MinWindowSpan:  time.Second,
}

type seedHit struct {
	Meta      entities.DocumentMeta
	Excerpt   string
	Relevance float32
}

type rankRequest struct {
	Seeds  []seedHit
	Walk   walkResult
	Window *entities.TimeWindow
	Now    time.Time
}

type ranked struct {
	Nodes  []entities.EvidenceNode
	Chains [][]lore.DocID
	Gaps   []string
}

func rank(req rankRequest) ranked {
	if len(req.Seeds) == 0 && len(req.Walk.Paths) == 0 {
		return ranked{}
	}

	weights := defaultRankWeights
	prior := newTimePrior(weights, req.Window, req.Now)
	relevance := scaledRelevance(req.Seeds)
	collected := newNodeSet(len(req.Seeds) + len(req.Walk.Paths))

	for _, seed := range req.Seeds {
		collected.add(entities.EvidenceNode{
			Doc:     seed.Meta,
			Excerpt: seed.Excerpt,
			Role:    entities.RoleSeed,
			Score:   relevance.of(seed.Meta.ID) * prior.of(seed.Meta.CreatedAt),
		})
	}
	for _, path := range req.Walk.Paths {
		meta, indexed := req.Walk.Metas[lastNode(path)]
		if !indexed {
			continue
		}
		reach := weights.proximity(len(path.Edges)) * path.Confidence
		collected.add(entities.EvidenceNode{
			Doc:   meta,
			Role:  graphRole(meta.Type),
			Score: reach * relevance.of(path.Nodes[0]) * prior.of(meta.CreatedAt),
			Via:   path.Edges,
		})
	}
	slices.SortFunc(collected.nodes, byRank)

	chains := assembleChains(req.Walk.Paths, req.Walk.SeedLinks, collected.nodes)

	return ranked{
		Nodes:  collected.nodes,
		Chains: chains,
		Gaps:   standaloneSeedGaps(collected.nodes, chains),
	}
}

func byRank(a, b entities.EvidenceNode) int {
	return cmp.Or(
		cmp.Compare(b.Score, a.Score),
		b.Doc.CreatedAt.Compare(a.Doc.CreatedAt),
		cmp.Compare(a.Doc.ID, b.Doc.ID),
	)
}

func byChronology(a, b entities.EvidenceNode) int {
	return cmp.Or(
		a.Doc.CreatedAt.Compare(b.Doc.CreatedAt),
		cmp.Compare(a.Doc.ID, b.Doc.ID),
	)
}

type nodeSet struct {
	nodes []entities.EvidenceNode
	seen  map[lore.DocID]struct{}
}

func newNodeSet(size int) *nodeSet {
	return &nodeSet{
		nodes: make([]entities.EvidenceNode, 0, size),
		seen:  make(map[lore.DocID]struct{}, size),
	}
}

func (s *nodeSet) add(node entities.EvidenceNode) {
	if _, held := s.seen[node.Doc.ID]; held {
		return
	}
	s.seen[node.Doc.ID] = struct{}{}
	if node.Doc.URL == "" {
		return
	}
	s.nodes = append(s.nodes, node)
}

// A path whose end document is not indexed yields no node.
func (s *nodeSet) addWalked(walked walkResult, role func(lore.DocType) string) {
	for _, path := range walked.Paths {
		meta, indexed := walked.Metas[lastNode(path)]
		if !indexed {
			continue
		}
		s.add(entities.EvidenceNode{
			Doc:   meta,
			Role:  role(meta.Type),
			Score: defaultRankWeights.proximity(len(path.Edges)) * path.Confidence,
			Via:   path.Edges,
		})
	}
}

func (s *nodeSet) addMatches(matches []seedHit) {
	relevance := scaledRelevance(matches)
	for _, match := range matches {
		s.add(entities.EvidenceNode{
			Doc:     match.Meta,
			Excerpt: match.Excerpt,
			Role:    entities.RoleSemanticMatch,
			Score:   relevance.of(match.Meta.ID),
		})
	}
}

func (w rankWeights) proximity(hops int) float32 {
	decay := float32(1)
	for range hops {
		decay *= w.ProximityDecay
	}

	return decay
}

type timePrior struct {
	weights  rankWeights
	now      time.Time
	centre   time.Time
	halfSpan time.Duration
	windowed bool
}

func newTimePrior(weights rankWeights, window *entities.TimeWindow, now time.Time) timePrior {
	if window == nil {
		return timePrior{weights: weights, now: now}
	}
	span := window.To.Sub(window.From)

	return timePrior{
		weights:  weights,
		centre:   window.From.Add(span / 2),
		halfSpan: max(span/2, weights.MinWindowSpan),
		windowed: true,
	}
}

func (p timePrior) of(createdAt time.Time) float32 {
	if p.windowed {
		spans := float32(createdAt.Sub(p.centre).Abs()) / float32(p.halfSpan)

		return 1 / (1 + p.weights.WindowFalloff*spans)
	}
	aged := float32(max(p.now.Sub(createdAt), 0)) / float32(p.weights.RecencyHorizon)

	return 1 - p.weights.RecencyPenalty*min(aged, 1)
}

type seedRelevance map[lore.DocID]float32

func scaledRelevance(seeds []seedHit) seedRelevance {
	var top float32
	for _, seed := range seeds {
		top = max(top, seed.Relevance)
	}
	if top <= 0 {
		return nil
	}

	scaled := make(seedRelevance, len(seeds))
	for _, seed := range seeds {
		scaled[seed.Meta.ID] = seed.Relevance / top
	}

	return scaled
}

// An unscaled seed carries no retrieval signal, which is neutral, never zero.
func (r seedRelevance) of(seed lore.DocID) float32 {
	if scaled, held := r[seed]; held {
		return scaled
	}

	return 1
}

var rolesByDocType = map[lore.DocType]string{
	lore.DocTypePage:          entities.RoleDesignDoc,
	lore.DocTypeIssue:         entities.RoleLinkedTicket,
	lore.DocTypeTicket:        entities.RoleLinkedTicket,
	lore.DocTypePRReview:      entities.RoleReviewThread,
	lore.DocTypeReviewComment: entities.RoleReviewThread,
	lore.DocTypeIssueComment:  entities.RoleReviewThread,
	lore.DocTypeTicketComment: entities.RoleReviewThread,
	lore.DocTypePR:            entities.RoleLinkedChange,
	lore.DocTypeCommit:        entities.RoleLinkedChange,
}

func graphRole(t lore.DocType) string {
	if role, known := rolesByDocType[t]; known {
		return role
	}

	return entities.RoleLinkedChange
}

func assembleChains(paths []walkPath, links []entities.Edge, nodes []entities.EvidenceNode) [][]lore.DocID {
	if len(paths) == 0 && len(links) == 0 {
		return nil
	}

	scores := make(map[lore.DocID]float32, len(nodes))
	for _, node := range nodes {
		scores[node.Doc.ID] = node.Score
	}

	cited := make([][]lore.DocID, 0, len(paths)+len(links))
	for _, path := range paths {
		if chain := citedChain(path.Nodes, scores); len(chain) > 1 {
			cited = append(cited, chain)
		}
	}
	joins := citedLinks(links, scores)
	cited = joinedChains(append(cited, joins...), joins)

	chains := make([][]lore.DocID, 0, len(cited))
	for _, chain := range cited {
		if chainKept(chains, chain) || chainExtended(cited, chain) {
			continue
		}
		chains = append(chains, chain)
	}
	if len(chains) == 0 {
		return nil
	}
	slices.SortFunc(chains, chainOrder(scores))

	return chains
}

// A chain naming a document the bundle does not carry cannot be resolved by its
// consumer, so it ends at its last cited node.
func citedChain(nodes []lore.DocID, scores map[lore.DocID]float32) []lore.DocID {
	for i, id := range nodes {
		if _, ok := scores[id]; !ok {
			return slices.Clone(nodes[:i])
		}
	}

	return slices.Clone(nodes)
}

func citedLinks(links []entities.Edge, scores map[lore.DocID]float32) [][]lore.DocID {
	cited := make([][]lore.DocID, 0, len(links))
	for _, link := range links {
		if chain := citedChain([]lore.DocID{link.Src, link.Dst}, scores); len(chain) > 1 {
			cited = append(cited, chain)
		}
	}

	return cited
}

// A cluster of seeds that all cite each other joins into more chains than any
// consumer reads, so the chain set is capped the way walk depth caps traversal.
const maxChains = 64

// A link between two seeds and the path leaving its far end are one chain: the
// ticket, the decision page it debates and the pull request implementing it.
func joinedChains(chains, links [][]lore.DocID) [][]lore.DocID {
	for grew := true; grew && len(chains) < maxChains; {
		grew = false
		for _, chain := range chains {
			for _, link := range links {
				if link[1] != chain[0] {
					continue
				}
				joined := prepended(link[0], chain)
				if joined == nil || chainKept(chains, joined) {
					continue
				}
				chains = append(chains, joined)
				grew = true
				if len(chains) >= maxChains {
					return chains
				}
			}
		}
	}

	return chains
}

// Refusing a repeat both keeps chains readable and terminates the joining.
func prepended(head lore.DocID, chain []lore.DocID) []lore.DocID {
	if slices.Contains(chain, head) {
		return nil
	}
	joined := make([]lore.DocID, 0, len(chain)+1)

	return append(append(joined, head), chain...)
}

func chainKept(chains [][]lore.DocID, nodes []lore.DocID) bool {
	return slices.ContainsFunc(chains, func(kept []lore.DocID) bool { return slices.Equal(kept, nodes) })
}

// The walk ends a path on every reached node and joining prepends to a chain, so
// [b c] arrives beside [a b c].
func chainExtended(chains [][]lore.DocID, nodes []lore.DocID) bool {
	return slices.ContainsFunc(chains, func(other []lore.DocID) bool {
		return len(nodes) < len(other) && chainContains(other, nodes)
	})
}

func chainContains(chain, part []lore.DocID) bool {
	for i := range len(chain) - len(part) + 1 {
		if slices.Equal(chain[i:i+len(part)], part) {
			return true
		}
	}

	return false
}

func chainOrder(scores map[lore.DocID]float32) func(a, b []lore.DocID) int {
	return func(a, b []lore.DocID) int {
		return cmp.Or(
			cmp.Compare(chainScore(b, scores), chainScore(a, scores)),
			cmp.Compare(len(b), len(a)),
			slices.Compare(a, b),
		)
	}
}

func chainScore(chain []lore.DocID, scores map[lore.DocID]float32) float32 {
	var best float32
	for _, id := range chain {
		best = max(best, scores[id])
	}

	return best
}

func standaloneSeedGaps(nodes []entities.EvidenceNode, chains [][]lore.DocID) []string {
	linked := make(map[lore.DocID]bool, len(nodes))
	for _, chain := range chains {
		for _, id := range chain {
			linked[id] = true
		}
	}

	var gaps []string
	for _, node := range nodes {
		if !seedRole(node.Role) || linked[node.Doc.ID] {
			continue
		}
		gaps = append(gaps, fmt.Sprintf("%s (%s) stands alone; no linked discussion", node.Doc.Title, node.Doc.ID))
	}

	return gaps
}

func seedRole(role string) bool {
	return role == entities.RoleSeed || role == entities.RoleBlamedCommit
}
