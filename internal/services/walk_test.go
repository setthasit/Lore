package services

import (
	"cmp"
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"lore/internal/entities"
)

var (
	errWalkEdges = errors.New("edges are unreadable")
	errWalkDocs  = errors.New("documents are unreadable")
)

var walkEpoch = time.Date(2025, time.March, 12, 9, 0, 0, 0, time.UTC)

type walkAsk struct {
	ids   []entities.DocID
	kinds []entities.EdgeKind
	dir   entities.Direction
}

type walkFake struct {
	edges        []entities.Edge
	metas        map[entities.DocID]entities.DocumentMeta
	asked        []walkAsk
	loaded       [][]entities.DocID
	edgesErr     error
	docsErr      error
	docsErrAfter int
}

func walkEdgeOrder(a, b entities.Edge) int {
	return cmp.Or(cmp.Compare(a.Src, b.Src), cmp.Compare(a.Dst, b.Dst), cmp.Compare(a.Kind, b.Kind))
}

func walkTypedEdge(src, dst string, kind entities.EdgeKind, confidence float32) entities.Edge {
	return entities.Edge{Src: entities.DocID(src), Dst: entities.DocID(dst), Kind: kind, Confidence: confidence}
}

func walkEdge(src, dst string, confidence float32) entities.Edge {
	return walkTypedEdge(src, dst, entities.EdgeKindReferencesDoc, confidence)
}

func walkMeta(id entities.DocID, createdAt time.Time) entities.DocumentMeta {
	return entities.DocumentMeta{
		ID:        id,
		Source:    "github",
		Type:      entities.DocTypePage,
		Title:     string(id),
		URL:       "https://example.test/" + string(id),
		CreatedAt: createdAt,
		UpdatedAt: createdAt,
	}
}

func newWalkFake(edges ...entities.Edge) *walkFake {
	slices.SortFunc(edges, walkEdgeOrder)

	metas := make(map[entities.DocID]entities.DocumentMeta, 2*len(edges))
	for _, e := range edges {
		for _, id := range [...]entities.DocID{e.Src, e.Dst} {
			metas[id] = walkMeta(id, walkEpoch)
		}
	}

	return &walkFake{edges: edges, metas: metas}
}

func (f *walkFake) Neighbors(
	_ context.Context,
	ids []entities.DocID,
	kinds []entities.EdgeKind,
	dir entities.Direction,
) ([]entities.Edge, error) {
	f.asked = append(f.asked, walkAsk{ids: slices.Clone(ids), kinds: slices.Clone(kinds), dir: dir})
	if f.edgesErr != nil {
		return nil, f.edgesErr
	}

	asked := make(map[entities.DocID]bool, len(ids))
	for _, id := range ids {
		asked[id] = true
	}

	var out []entities.Edge
	for _, e := range f.edges {
		if len(kinds) > 0 && !slices.Contains(kinds, e.Kind) {
			continue
		}
		if walkTouches(e, asked, dir) {
			out = append(out, e)
		}
	}

	return out, nil
}

func walkTouches(e entities.Edge, asked map[entities.DocID]bool, dir entities.Direction) bool {
	switch dir {
	case entities.DirIn:
		return asked[e.Dst]
	case entities.DirBoth:
		return asked[e.Src] || asked[e.Dst]
	default:
		return asked[e.Src]
	}
}

func (f *walkFake) DocumentsByID(_ context.Context, ids []entities.DocID) ([]entities.DocumentMeta, error) {
	f.loaded = append(f.loaded, slices.Clone(ids))
	if f.docsErr != nil && len(f.loaded) > f.docsErrAfter {
		return nil, f.docsErr
	}

	var out []entities.DocumentMeta
	for _, id := range ids {
		if meta, known := f.metas[id]; known {
			out = append(out, meta)
		}
	}

	return out, nil
}

func walkFrom(t *testing.T, f *walkFake, seed string, opts walkOptions) walkResult {
	t.Helper()

	res, err := walkGraph(context.Background(), f, []entities.DocID{entities.DocID(seed)}, opts)
	if err != nil {
		t.Fatalf("walkGraph: %v", err)
	}

	return res
}

func assertReached(t *testing.T, res walkResult, want ...string) {
	t.Helper()

	got := make([]string, 0, len(res.Paths))
	for _, path := range res.Paths {
		got = append(got, string(lastNode(path)))
	}

	sorted := slices.Clone(got)
	slices.Sort(sorted)
	slices.Sort(want)
	if !slices.Equal(sorted, want) {
		t.Errorf("walk reached %v, want exactly %v", got, want)
	}
}

func assertMetas(t *testing.T, res walkResult, want ...string) {
	t.Helper()

	got := make([]string, 0, len(res.Metas))
	for id := range res.Metas {
		got = append(got, string(id))
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("walk loaded metadata for %v, want exactly %v", got, want)
	}
}

func assertCalls(t *testing.T, what string, got, want [][]entities.DocID) {
	t.Helper()

	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s calls = %v, want %v", what, got, want)
	}
}

func assertAskedLayers(t *testing.T, f *walkFake, want [][]entities.DocID) {
	t.Helper()

	ids := make([][]entities.DocID, len(f.asked))
	for i, ask := range f.asked {
		ids[i] = ask.ids
	}
	assertCalls(t, "Neighbors", ids, want)
}

func assertAskedFor(t *testing.T, f *walkFake, kinds []entities.EdgeKind, dir entities.Direction) {
	t.Helper()

	if len(f.asked) == 0 {
		t.Fatal("Neighbors was never asked")
	}
	for _, ask := range f.asked {
		if !slices.Equal(ask.kinds, kinds) || ask.dir != dir {
			t.Errorf("Neighbors asked kinds %v direction %v, want %v and %v", ask.kinds, ask.dir, kinds, dir)
		}
	}
}

func assertConfidence(t *testing.T, got, want float32) {
	t.Helper()

	if diff := got - want; diff > scoreEpsilon || diff < -scoreEpsilon {
		t.Errorf("path confidence = %v, want %v", got, want)
	}
}

func pathTo(t *testing.T, res walkResult, node string) walkPath {
	t.Helper()

	for _, path := range res.Paths {
		if lastNode(path) == entities.DocID(node) {
			return path
		}
	}
	t.Fatalf("no path reaches %q in %+v", node, res.Paths)

	return walkPath{}
}

func walkIDs(names ...string) []entities.DocID {
	ids := make([]entities.DocID, len(names))
	for i, name := range names {
		ids[i] = entities.DocID(name)
	}

	return ids
}

func TestWalkGraphWithoutSeedsTouchesNothing(t *testing.T) {
	t.Parallel()

	f := newWalkFake(walkEdge("a", "b", 1))

	res, err := walkGraph(context.Background(), f, nil, walkOptions{})
	if err != nil {
		t.Fatalf("walkGraph: %v", err)
	}
	if res.Paths != nil || res.Metas != nil {
		t.Errorf("walkGraph without seeds = %+v, want the zero result", res)
	}
	if f.asked != nil || f.loaded != nil {
		t.Errorf("walkGraph without seeds asked %v and loaded %v, want neither", f.asked, f.loaded)
	}
}

func walkDiamondFake() *walkFake {
	return newWalkFake(
		walkEdge("a", "a", 1),
		walkEdge("a", "b", 0.8),
		walkEdge("a", "c", 0.9),
		walkEdge("b", "d", 0.5),
		walkEdge("c", "d", 0.5),
		walkEdge("d", "a", 1),
	)
}

func TestWalkGraphReachesEachDocumentOnceOnItsStrongestShortestPath(t *testing.T) {
	t.Parallel()

	f := walkDiamondFake()

	res := walkFrom(t, f, "a", walkOptions{})

	assertReached(t, res, "b", "c", "d")
	assertMetas(t, res, "a", "b", "c", "d")

	path := pathTo(t, res, "d")
	if want := walkIDs("a", "c", "d"); !slices.Equal(path.Nodes, want) {
		t.Errorf("path to d = %v, want %v", path.Nodes, want)
	}
	if want := []entities.Edge{walkEdge("a", "c", 0.9), walkEdge("c", "d", 0.5)}; !slices.Equal(path.Edges, want) {
		t.Errorf("edges to d = %+v, want %+v", path.Edges, want)
	}
	assertConfidence(t, path.Confidence, 0.9*0.5)

	layers := [][]entities.DocID{walkIDs("a"), walkIDs("b", "c"), walkIDs("d")}
	assertAskedLayers(t, f, layers)
	assertCalls(t, "DocumentsByID", f.loaded, layers)
}

func walkChainFake() *walkFake {
	return newWalkFake(
		walkEdge("a", "b", 1),
		walkEdge("b", "c", 1),
		walkEdge("c", "d", 1),
		walkEdge("d", "e", 1),
	)
}

func TestWalkGraphHonorsTheDepthCap(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		depth int
		want  []string
	}{
		{name: "one hop reaches immediate neighbours only", depth: 1, want: []string{"b"}},
		{name: "three hops reach three layers", depth: 3, want: []string{"b", "c", "d"}},
		{name: "no depth given caps at three hops", want: []string{"b", "c", "d"}},
		{name: "a deeper cap reaches the tail", depth: 5, want: []string{"b", "c", "d", "e"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			res := walkFrom(t, walkChainFake(), "a", walkOptions{Depth: tc.depth})

			assertReached(t, res, tc.want...)
		})
	}
}

func TestWalkGraphPrunesPathsBelowTheConfidenceFloor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		minConfidence float32
		want          []string
		wantAsked     [][]entities.DocID
	}{
		{
			name:      "the default floor stops the chain where the product crosses it",
			want:      []string{"b", "c"},
			wantAsked: [][]entities.DocID{walkIDs("a"), walkIDs("b"), walkIDs("c")},
		},
		{
			name:          "a lower floor lets the tail through",
			minConfidence: 0.2,
			want:          []string{"b", "c", "d", "e"},
			wantAsked: [][]entities.DocID{
				walkIDs("a"), walkIDs("b"), walkIDs("c"), walkIDs("d"), walkIDs("e"),
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newWalkFake(
				walkEdge("a", "b", 0.9),
				walkEdge("b", "c", 0.7),
				walkEdge("c", "d", 0.4),
				walkEdge("d", "e", 1),
			)

			res := walkFrom(t, f, "a", walkOptions{Depth: 5, MinConfidence: tc.minConfidence})

			assertReached(t, res, tc.want...)
			assertAskedLayers(t, f, tc.wantAsked)
			assertConfidence(t, pathTo(t, res, "c").Confidence, 0.9*0.7)
		})
	}
}

func TestWalkGraphHonorsDirection(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		dir  entities.Direction
		want []string
	}{
		{name: "outward follows Src to Dst", dir: entities.DirOut, want: []string{"q", "r"}},
		{name: "inward follows Dst to Src", dir: entities.DirIn, want: []string{"p", "s"}},
		{name: "both unions the two", dir: entities.DirBoth, want: []string{"p", "q", "r", "s"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newWalkFake(
				walkEdge("s", "p", 1),
				walkEdge("p", "m", 1),
				walkEdge("m", "q", 1),
				walkEdge("q", "r", 1),
				walkEdge("p", "q", 1),
			)

			res := walkFrom(t, f, "m", walkOptions{Direction: tc.dir})

			assertReached(t, res, tc.want...)
			assertAskedFor(t, f, nil, tc.dir)
		})
	}
}

func TestWalkGraphPrunesTraversalThroughDocumentsOlderThanTimeAfter(t *testing.T) {
	t.Parallel()

	f := newWalkFake(
		walkEdge("a", "b", 1),
		walkEdge("b", "c", 1),
		walkEdge("a", "d", 1),
		walkEdge("a", "e", 1),
	)
	f.metas["b"] = walkMeta("b", walkEpoch.Add(-24*time.Hour))
	f.metas["c"] = walkMeta("c", walkEpoch.Add(48*time.Hour))
	f.metas["d"] = walkMeta("d", walkEpoch.Add(time.Hour))

	res := walkFrom(t, f, "a", walkOptions{TimeAfter: &walkEpoch})

	assertReached(t, res, "d")
	assertAskedLayers(t, f, [][]entities.DocID{walkIDs("a"), walkIDs("d")})
	assertCalls(t, "DocumentsByID", f.loaded, [][]entities.DocID{walkIDs("a"), walkIDs("b", "d", "e")})
}

func TestWalkGraphFollowsOnlyTheRequestedKinds(t *testing.T) {
	t.Parallel()

	f := newWalkFake(
		walkTypedEdge("a", "b", entities.EdgeKindReferencesDoc, 1),
		walkTypedEdge("a", "c", entities.EdgeKindMentionsPath, 1),
	)

	kinds := []entities.EdgeKind{entities.EdgeKindReferencesDoc}

	res := walkFrom(t, f, "a", walkOptions{Kinds: kinds})

	assertReached(t, res, "b")
	assertAskedFor(t, f, kinds, entities.DirOut)
}

func TestWalkGraphSkipsEdgesIntoUnindexedDocuments(t *testing.T) {
	t.Parallel()

	f := newWalkFake(walkEdge("a", "x", 1), walkEdge("x", "y", 1))
	delete(f.metas, "x")

	res := walkFrom(t, f, "a", walkOptions{})

	assertReached(t, res)
	assertMetas(t, res, "a")
	assertAskedLayers(t, f, [][]entities.DocID{walkIDs("a")})
}

func TestWalkGraphIsDeterministic(t *testing.T) {
	t.Parallel()

	first := walkFrom(t, walkDiamondFake(), "a", walkOptions{})
	second := walkFrom(t, walkDiamondFake(), "a", walkOptions{})

	if !reflect.DeepEqual(first.Paths, second.Paths) {
		t.Errorf("repeated walk returned %+v then %+v", first.Paths, second.Paths)
	}
	if !reflect.DeepEqual(first.Metas, second.Metas) {
		t.Errorf("repeated walk loaded %+v then %+v", first.Metas, second.Metas)
	}
}

func TestWalkGraphPathsShareNoBackingArray(t *testing.T) {
	t.Parallel()

	f := newWalkFake(
		walkEdge("a", "b", 1),
		walkEdge("b", "c", 1),
		walkEdge("c", "d", 1),
		walkEdge("c", "e", 1),
	)

	res := walkFrom(t, f, "a", walkOptions{})

	assertReached(t, res, "b", "c", "d", "e")
	if want := walkIDs("a", "b", "c", "e"); !slices.Equal(pathTo(t, res, "e").Nodes, want) {
		t.Fatalf("path to e = %v, want %v", pathTo(t, res, "e").Nodes, want)
	}

	deep := pathTo(t, res, "d")
	for i := range deep.Nodes {
		deep.Nodes[i] = "mutated"
	}
	for i := range deep.Edges {
		deep.Edges[i] = walkEdge("mutated", "mutated", 0)
	}

	for node, want := range map[string][]entities.DocID{
		"b": walkIDs("a", "b"),
		"c": walkIDs("a", "b", "c"),
		"e": walkIDs("a", "b", "c", "e"),
	} {
		path := pathTo(t, res, node)
		if !slices.Equal(path.Nodes, want) {
			t.Errorf("path to %s = %v after mutating a sibling, want %v", node, path.Nodes, want)
		}
		if path.Edges[0].Src != "a" {
			t.Errorf("first edge to %s is %+v after mutating a sibling", node, path.Edges[0])
		}
	}
}

func TestWalkGraphSurfacesStoreFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		arrange func(f *walkFake)
		want    error
	}{
		{
			name:    "reading edges fails",
			arrange: func(f *walkFake) { f.edgesErr = errWalkEdges },
			want:    errWalkEdges,
		},
		{
			name:    "loading the seed metadata fails",
			arrange: func(f *walkFake) { f.docsErr = errWalkDocs },
			want:    errWalkDocs,
		},
		{
			name:    "loading a layer's metadata fails",
			arrange: func(f *walkFake) { f.docsErr, f.docsErrAfter = errWalkDocs, 1 },
			want:    errWalkDocs,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := walkChainFake()
			tc.arrange(f)

			res, err := walkGraph(context.Background(), f, walkIDs("a"), walkOptions{})
			if !errors.Is(err, tc.want) {
				t.Fatalf("walkGraph error = %v, want %v", err, tc.want)
			}
			if res.Paths != nil || res.Metas != nil {
				t.Errorf("failed walk returned %+v, want the zero result", res)
			}
		})
	}
}
