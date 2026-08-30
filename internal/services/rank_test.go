package services

import (
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"lore/internal/entities"
)

const rankDay = 24 * time.Hour

func rankMeta(id string, createdAt time.Time) entities.DocumentMeta {
	return walkMeta(entities.DocID(id), createdAt)
}

func rankMetas(metas ...entities.DocumentMeta) map[entities.DocID]entities.DocumentMeta {
	byID := make(map[entities.DocID]entities.DocumentMeta, len(metas))
	for _, meta := range metas {
		byID[meta.ID] = meta
	}

	return byID
}

func rankSeed(meta entities.DocumentMeta, relevance float32) seedHit {
	return seedHit{Meta: meta, Excerpt: "excerpt of " + string(meta.ID), Relevance: relevance}
}

func rankPath(confidence float32, names ...string) walkPath {
	edges := make([]entities.Edge, 0, len(names)-1)
	for i := 1; i < len(names); i++ {
		edges = append(edges, walkEdge(names[i-1], names[i], confidence))
	}

	return walkPath{Nodes: walkIDs(names...), Edges: edges, Confidence: confidence}
}

func rankedIDs(nodes []entities.EvidenceNode) []string {
	ids := make([]string, len(nodes))
	for i, node := range nodes {
		ids[i] = string(node.Doc.ID)
	}

	return ids
}

func assertRankOrder(t *testing.T, nodes []entities.EvidenceNode, want ...string) {
	t.Helper()

	at := make(map[string]int, len(nodes))
	for i, node := range nodes {
		at[string(node.Doc.ID)] = i
	}
	for i, id := range want {
		pos, cited := at[id]
		if !cited {
			t.Fatalf("%q is not cited in %v", id, rankedIDs(nodes))
		}
		if i > 0 && pos < at[want[i-1]] {
			t.Errorf("%q at %d outranks %q at %d, want the reverse: %v",
				id, pos, want[i-1], at[want[i-1]], rankedIDs(nodes))
		}
	}
}

func nodeOf(t *testing.T, nodes []entities.EvidenceNode, id string) entities.EvidenceNode {
	t.Helper()

	for _, node := range nodes {
		if node.Doc.ID == entities.DocID(id) {
			return node
		}
	}
	t.Fatalf("%q is not cited in %v", id, rankedIDs(nodes))

	return entities.EvidenceNode{}
}

func TestRankPrefersFewerHops(t *testing.T) {
	t.Parallel()

	const confidence = 0.9

	got := rank(rankRequest{
		Seeds: []seedHit{rankSeed(rankMeta("seed", walkEpoch), rrf(1))},
		Walk: walkResult{
			Paths: []walkPath{
				rankPath(confidence, "seed", "near"),
				rankPath(confidence, "seed", "one", "two", "far"),
			},
			Metas: rankMetas(rankMeta("near", walkEpoch), rankMeta("far", walkEpoch)),
		},
		Now: walkEpoch,
	})

	assertRankOrder(t, got.Nodes, "seed", "near", "far")
}

func TestRankBreaksProximityTiesOnPathConfidence(t *testing.T) {
	t.Parallel()

	got := rank(rankRequest{
		Seeds: []seedHit{rankSeed(rankMeta("seed", walkEpoch), rrf(1))},
		Walk: walkResult{
			Paths: []walkPath{rankPath(0.4, "seed", "loosely-linked"), rankPath(0.9, "seed", "tightly-linked")},
			Metas: rankMetas(rankMeta("loosely-linked", walkEpoch), rankMeta("tightly-linked", walkEpoch)),
		},
		Now: walkEpoch,
	})

	assertRankOrder(t, got.Nodes, "tightly-linked", "loosely-linked")
}

func TestRankFollowsSeedRelevance(t *testing.T) {
	t.Parallel()

	const confidence = 0.8

	got := rank(rankRequest{
		Seeds: []seedHit{
			rankSeed(rankMeta("faint-seed", walkEpoch), rrf(9)),
			rankSeed(rankMeta("top-seed", walkEpoch), rrf(1)),
		},
		Walk: walkResult{
			Paths: []walkPath{
				rankPath(confidence, "faint-seed", "from-faint"),
				rankPath(confidence, "top-seed", "from-top"),
			},
			Metas: rankMetas(rankMeta("from-faint", walkEpoch), rankMeta("from-top", walkEpoch)),
		},
		Now: walkEpoch,
	})

	assertRankOrder(t, got.Nodes, "top-seed", "faint-seed", "from-top", "from-faint")
}

func TestRankWithoutRetrievalSignalKeepsScoresPositive(t *testing.T) {
	t.Parallel()

	got := rank(rankRequest{
		Seeds: []seedHit{rankSeed(rankMeta("anchor", walkEpoch), 0)},
		Walk: walkResult{
			Paths: []walkPath{rankPath(0.9, "anchor", "reached")},
			Metas: rankMetas(rankMeta("reached", walkEpoch)),
		},
		Now: walkEpoch,
	})

	assertRankOrder(t, got.Nodes, "anchor", "reached")
	for _, node := range got.Nodes {
		if node.Score <= 0 {
			t.Errorf("%q scores %v, want a positive score", node.Doc.ID, node.Score)
		}
	}
}

func TestRankTimePriorPrefersTheWindowOverRecency(t *testing.T) {
	t.Parallel()

	seeds := []seedHit{
		rankSeed(rankMeta("in-window", walkEpoch), rrf(1)),
		rankSeed(rankMeta("newer-outside", walkEpoch.Add(90*rankDay)), rrf(1)),
	}
	now := walkEpoch.Add(120 * rankDay)
	window := &entities.TimeWindow{From: walkEpoch.Add(-30 * rankDay), To: walkEpoch.Add(30 * rankDay)}

	windowed := rank(rankRequest{Seeds: seeds, Window: window, Now: now})
	assertRankOrder(t, windowed.Nodes, "in-window", "newer-outside")

	unwindowed := rank(rankRequest{Seeds: seeds, Now: now})
	assertRankOrder(t, unwindowed.Nodes, "newer-outside", "in-window")
}

func TestRankWindowPriorFallsOffFromTheCentre(t *testing.T) {
	t.Parallel()

	got := rank(rankRequest{
		Seeds: []seedHit{
			rankSeed(rankMeta("far-outside", walkEpoch.Add(400*rankDay)), rrf(1)),
			rankSeed(rankMeta("just-outside", walkEpoch.Add(45*rankDay)), rrf(1)),
			rankSeed(rankMeta("edge", walkEpoch.Add(30*rankDay)), rrf(1)),
			rankSeed(rankMeta("centre", walkEpoch), rrf(1)),
		},
		Window: &entities.TimeWindow{From: walkEpoch.Add(-30 * rankDay), To: walkEpoch.Add(30 * rankDay)},
		Now:    walkEpoch,
	})

	assertRankOrder(t, got.Nodes, "centre", "edge", "just-outside", "far-outside")
}

func TestRankSurvivesAZeroWidthWindow(t *testing.T) {
	t.Parallel()

	got := rank(rankRequest{
		Seeds: []seedHit{
			rankSeed(rankMeta("later", walkEpoch.Add(rankDay)), rrf(1)),
			rankSeed(rankMeta("at-centre", walkEpoch), rrf(1)),
		},
		Window: &entities.TimeWindow{From: walkEpoch, To: walkEpoch},
		Now:    walkEpoch,
	})

	assertRankOrder(t, got.Nodes, "at-centre", "later")
}

func TestGraphRole(t *testing.T) {
	t.Parallel()

	tests := []struct {
		docType entities.DocType
		want    string
	}{
		{entities.DocTypePage, entities.RoleDesignDoc},
		{entities.DocTypeIssue, entities.RoleLinkedTicket},
		{entities.DocTypeTicket, entities.RoleLinkedTicket},
		{entities.DocTypePRReview, entities.RoleReviewThread},
		{entities.DocTypeReviewComment, entities.RoleReviewThread},
		{entities.DocTypeIssueComment, entities.RoleReviewThread},
		{entities.DocTypeTicketComment, entities.RoleReviewThread},
		{entities.DocTypePR, entities.RoleLinkedChange},
		{entities.DocTypeCommit, entities.RoleLinkedChange},
		{entities.DocType("message"), entities.RoleLinkedChange},
	}

	for _, tt := range tests {
		t.Run(string(tt.docType), func(t *testing.T) {
			t.Parallel()

			if got := graphRole(tt.docType); got != tt.want {
				t.Errorf("graphRole(%q) = %q, want %q", tt.docType, got, tt.want)
			}
		})
	}
}

func TestRankCitesSeedsAndReachedDocumentsDifferently(t *testing.T) {
	t.Parallel()

	commit := rankMeta("github:commit:abc", walkEpoch)
	commit.Type = entities.DocTypeCommit
	path := rankPath(0.9, "seed", "github:commit:abc")

	got := rank(rankRequest{
		Seeds: []seedHit{rankSeed(rankMeta("seed", walkEpoch), rrf(1))},
		Walk:  walkResult{Paths: []walkPath{path}, Metas: rankMetas(commit)},
		Now:   walkEpoch,
	})

	seed := nodeOf(t, got.Nodes, "seed")
	if seed.Role != entities.RoleSeed || seed.Via != nil {
		t.Errorf("seed = role %q via %+v, want role %q and no edges", seed.Role, seed.Via, entities.RoleSeed)
	}
	if seed.Excerpt != "excerpt of seed" {
		t.Errorf("seed excerpt = %q, want the retrieval excerpt", seed.Excerpt)
	}

	reached := nodeOf(t, got.Nodes, "github:commit:abc")
	if reached.Role != entities.RoleLinkedChange {
		t.Errorf("reached role = %q, want %q", reached.Role, entities.RoleLinkedChange)
	}
	if !reflect.DeepEqual(reached.Via, path.Edges) {
		t.Errorf("reached via = %+v, want %+v", reached.Via, path.Edges)
	}
	if reached.Excerpt != "" {
		t.Errorf("reached excerpt = %q, want empty", reached.Excerpt)
	}
}

func TestRankDropsDocumentsWithoutAURL(t *testing.T) {
	t.Parallel()

	mute := rankMeta("mute-seed", walkEpoch)
	mute.URL = ""
	hidden := rankMeta("hidden", walkEpoch)
	hidden.URL = ""

	got := rank(rankRequest{
		Seeds: []seedHit{rankSeed(mute, rrf(1)), rankSeed(rankMeta("loud-seed", walkEpoch), rrf(2))},
		Walk: walkResult{
			Paths: []walkPath{rankPath(0.9, "loud-seed", "hidden")},
			Metas: rankMetas(hidden),
		},
		Now: walkEpoch,
	})

	if ids := rankedIDs(got.Nodes); !slices.Equal(ids, []string{"loud-seed"}) {
		t.Errorf("cited %v, want only loud-seed", ids)
	}
	if got.Chains != nil {
		t.Errorf("chains = %v, want none through a document that is not evidence", got.Chains)
	}
	want := []string{"loud-seed (loud-seed) stands alone; no linked discussion"}
	if !slices.Equal(got.Gaps, want) {
		t.Errorf("gaps = %v, want %v", got.Gaps, want)
	}
}

func TestRankDropsDocumentsMissingFromTheIndex(t *testing.T) {
	t.Parallel()

	got := rank(rankRequest{
		Seeds: []seedHit{rankSeed(rankMeta("seed", walkEpoch), rrf(1))},
		Walk:  walkResult{Paths: []walkPath{rankPath(0.9, "seed", "ghost")}},
		Now:   walkEpoch,
	})

	if ids := rankedIDs(got.Nodes); !slices.Equal(ids, []string{"seed"}) {
		t.Errorf("cited %v, want only seed", ids)
	}
	if got.Chains != nil {
		t.Errorf("chains = %v, want none through an unindexed document", got.Chains)
	}
	want := []string{"seed (seed) stands alone; no linked discussion"}
	if !slices.Equal(got.Gaps, want) {
		t.Errorf("gaps = %v, want %v", got.Gaps, want)
	}
}

func TestRankCitesEachDocumentOnce(t *testing.T) {
	t.Parallel()

	seed := rankMeta("seed", walkEpoch)
	got := rank(rankRequest{
		Seeds: []seedHit{rankSeed(seed, rrf(1)), rankSeed(seed, rrf(4))},
		Walk: walkResult{
			Paths: []walkPath{rankPath(0.9, "seed", "reached"), rankPath(0.5, "seed", "reached")},
			Metas: rankMetas(rankMeta("reached", walkEpoch)),
		},
		Now: walkEpoch,
	})

	if ids := rankedIDs(got.Nodes); !slices.Equal(ids, []string{"seed", "reached"}) {
		t.Errorf("cited %v, want each document once", ids)
	}
	if chains := got.Chains; len(chains) != 1 {
		t.Errorf("chains = %v, want the repeated path once", chains)
	}
}

func TestAssembleChains(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		paths []walkPath
		links []entities.Edge
		cited map[entities.DocID]float32
		want  [][]entities.DocID
	}{
		{name: "nothing walked assembles no chains"},
		{
			name:  "a chain that only prefixes another is dropped",
			paths: []walkPath{rankPath(1, "a", "b"), rankPath(1, "a", "b", "c")},
			cited: map[entities.DocID]float32{"a": 0.3, "b": 0.2, "c": 0.1},
			want:  [][]entities.DocID{walkIDs("a", "b", "c")},
		},
		{
			name:  "a chain that only suffixes another is dropped",
			paths: []walkPath{rankPath(1, "b", "c"), rankPath(1, "a", "b", "c")},
			cited: map[entities.DocID]float32{"a": 0.3, "b": 0.2, "c": 0.1},
			want:  [][]entities.DocID{walkIDs("a", "b", "c")},
		},
		{
			name:  "identical chains collapse",
			paths: []walkPath{rankPath(1, "a", "b"), rankPath(0.5, "a", "b")},
			cited: map[entities.DocID]float32{"a": 0.3, "b": 0.2},
			want:  [][]entities.DocID{walkIDs("a", "b")},
		},
		{
			name:  "chains order by their best node score",
			paths: []walkPath{rankPath(1, "a", "b"), rankPath(1, "c", "d")},
			cited: map[entities.DocID]float32{"a": 0.5, "b": 0.2, "c": 0.1, "d": 0.9},
			want:  [][]entities.DocID{walkIDs("c", "d"), walkIDs("a", "b")},
		},
		{
			name:  "equally scored chains order by length",
			paths: []walkPath{rankPath(1, "a", "c"), rankPath(1, "a", "b"), rankPath(1, "a", "b", "d")},
			cited: map[entities.DocID]float32{"a": 0.5, "b": 0.5, "c": 0.5, "d": 0.5},
			want:  [][]entities.DocID{walkIDs("a", "b", "d"), walkIDs("a", "c")},
		},
		{
			name:  "chains of equal score and length order by id",
			paths: []walkPath{rankPath(1, "a", "c"), rankPath(1, "a", "b")},
			cited: map[entities.DocID]float32{"a": 0.5, "b": 0.5, "c": 0.5},
			want:  [][]entities.DocID{walkIDs("a", "b"), walkIDs("a", "c")},
		},
		{
			name:  "a chain ends at the last document the bundle carries",
			paths: []walkPath{rankPath(1, "a", "b", "c")},
			cited: map[entities.DocID]float32{"a": 0.5, "b": 0.4},
			want:  [][]entities.DocID{walkIDs("a", "b")},
		},
		{
			name:  "a chain whose only neighbour is not evidence is dropped",
			paths: []walkPath{rankPath(1, "a", "b")},
			cited: map[entities.DocID]float32{"a": 0.5},
		},
		{
			name:  "a link between two seeds chains them",
			links: []entities.Edge{walkEdge("s", "d", 1)},
			cited: map[entities.DocID]float32{"s": 0.5, "d": 0.4},
			want:  [][]entities.DocID{walkIDs("s", "d")},
		},
		{
			name:  "a seed link joins the path leaving its far end",
			paths: []walkPath{rankPath(1, "d", "pr")},
			links: []entities.Edge{walkEdge("s", "d", 1)},
			cited: map[entities.DocID]float32{"s": 0.5, "d": 0.4, "pr": 0.3},
			want:  [][]entities.DocID{walkIDs("s", "d", "pr")},
		},
		{
			name:  "a run of seed links joins into one chain",
			links: []entities.Edge{walkEdge("r", "s", 1), walkEdge("s", "d", 1), walkEdge("d", "e", 1)},
			cited: map[entities.DocID]float32{"r": 0.5, "s": 0.4, "d": 0.3, "e": 0.2},
			want:  [][]entities.DocID{walkIDs("r", "s", "d", "e")},
		},
		{
			name:  "a cyclic pair of seed links repeats no node",
			links: []entities.Edge{walkEdge("s", "d", 1), walkEdge("d", "s", 1)},
			cited: map[entities.DocID]float32{"s": 0.5, "d": 0.4},
			want:  [][]entities.DocID{walkIDs("d", "s"), walkIDs("s", "d")},
		},
		{
			name:  "a seed link into a document the bundle does not carry chains nothing",
			links: []entities.Edge{walkEdge("s", "d", 1)},
			cited: map[entities.DocID]float32{"s": 0.5},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := assembleChains(tt.paths, tt.links, citedNodes(tt.cited))
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("chains = %v, want %v", got, tt.want)
			}
		})
	}
}

func citedNodes(scores map[entities.DocID]float32) []entities.EvidenceNode {
	nodes := make([]entities.EvidenceNode, 0, len(scores))
	for id, score := range scores {
		nodes = append(nodes, entities.EvidenceNode{Doc: rankMeta(string(id), walkEpoch), Score: score})
	}
	slices.SortFunc(nodes, byRank)

	return nodes
}

func TestAssembleChainsCopiesPathNodes(t *testing.T) {
	t.Parallel()

	path := rankPath(1, "a", "b")
	chains := assembleChains([]walkPath{path}, nil, citedNodes(map[entities.DocID]float32{"a": 1, "b": 0.5}))
	path.Nodes[0] = "mutated"

	if chains[0][0] != "a" {
		t.Errorf("chain %v aliases the walk path", chains[0])
	}
}

func TestAssembleChainsCapsDenselyLinkedSeeds(t *testing.T) {
	t.Parallel()

	const seeds = 8

	scores := make(map[entities.DocID]float32, seeds)
	ids := make([]entities.DocID, seeds)
	for i := range seeds {
		ids[i] = entities.DocID(fmt.Sprintf("seed-%d", i))
		scores[ids[i]] = float32(seeds - i)
	}

	var links []entities.Edge
	for _, src := range ids {
		for _, dst := range ids {
			if src != dst {
				links = append(links, entities.Edge{Src: src, Dst: dst, Confidence: 1})
			}
		}
	}

	chains := assembleChains(nil, links, citedNodes(scores))

	if len(chains) > maxChains {
		t.Errorf("chains = %d, want at most %d", len(chains), maxChains)
	}
	if len(chains) == 0 {
		t.Error("chains = 0, want the seed links reported")
	}
}

func TestRankReportsStandaloneSeeds(t *testing.T) {
	t.Parallel()

	alone := rankMeta("jira:ticket:PROJ-4521", walkEpoch)
	alone.Title = "Rate limit the export endpoint"

	got := rank(rankRequest{
		Seeds: []seedHit{rankSeed(alone, rrf(1)), rankSeed(rankMeta("notion:page:decisions", walkEpoch), rrf(2))},
		Walk: walkResult{
			Paths: []walkPath{rankPath(0.9, "notion:page:decisions", "github:pr:12")},
			Metas: rankMetas(rankMeta("github:pr:12", walkEpoch)),
		},
		Now: walkEpoch,
	})

	want := []string{"Rate limit the export endpoint (jira:ticket:PROJ-4521) stands alone; no linked discussion"}
	if !slices.Equal(got.Gaps, want) {
		t.Errorf("gaps = %v, want %v", got.Gaps, want)
	}
}

func TestRankReportsNoStandaloneGapForASeedInsideAChain(t *testing.T) {
	t.Parallel()

	got := rank(rankRequest{
		Seeds: []seedHit{
			rankSeed(rankMeta("ticket", walkEpoch), rrf(1)),
			rankSeed(rankMeta("page", walkEpoch), rrf(2)),
			rankSeed(rankMeta("orphan", walkEpoch), rrf(3)),
		},
		Walk: walkResult{SeedLinks: []entities.Edge{walkEdge("ticket", "page", 1)}},
		Now:  walkEpoch,
	})

	if want := [][]entities.DocID{walkIDs("ticket", "page")}; !reflect.DeepEqual(got.Chains, want) {
		t.Errorf("chains = %v, want %v", got.Chains, want)
	}
	want := []string{"orphan (orphan) stands alone; no linked discussion"}
	if !slices.Equal(got.Gaps, want) {
		t.Errorf("gaps = %v, want %v", got.Gaps, want)
	}
}

func TestRankOrdersGapsBySeedRank(t *testing.T) {
	t.Parallel()

	got := rank(rankRequest{
		Seeds: []seedHit{
			rankSeed(rankMeta("weak", walkEpoch), rrf(9)),
			rankSeed(rankMeta("strong", walkEpoch), rrf(1)),
		},
		Now: walkEpoch,
	})

	want := []string{
		"strong (strong) stands alone; no linked discussion",
		"weak (weak) stands alone; no linked discussion",
	}
	if !slices.Equal(got.Gaps, want) {
		t.Errorf("gaps = %v, want %v", got.Gaps, want)
	}
}

func TestRankIsDeterministic(t *testing.T) {
	t.Parallel()

	req := rankRequest{
		Seeds: []seedHit{
			rankSeed(rankMeta("seed-a", walkEpoch), rrf(1)),
			rankSeed(rankMeta("seed-b", walkEpoch.Add(-rankDay)), rrf(3)),
			rankSeed(rankMeta("seed-c", walkEpoch), 0),
		},
		Walk: walkResult{
			Paths: []walkPath{
				rankPath(0.9, "seed-a", "hop-one"),
				rankPath(0.81, "seed-a", "hop-one", "hop-two"),
				rankPath(0.5, "seed-b", "hop-one"),
				rankPath(0.7, "seed-b", "other"),
			},
			Metas: rankMetas(
				rankMeta("hop-one", walkEpoch),
				rankMeta("hop-two", walkEpoch.Add(-2*rankDay)),
				rankMeta("other", walkEpoch),
			),
		},
		Now: walkEpoch.Add(10 * rankDay),
	}

	first := rank(req)
	if second := rank(req); !reflect.DeepEqual(first, second) {
		t.Errorf("ranking twice differs:\n%+v\n%+v", first, second)
	}
}

func TestRankWithoutSeedsOrPathsRanksNothing(t *testing.T) {
	t.Parallel()

	if got := rank(rankRequest{}); !reflect.DeepEqual(got, ranked{}) {
		t.Errorf("ranked = %+v, want the zero value", got)
	}
}
