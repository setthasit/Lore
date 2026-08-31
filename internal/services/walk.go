package services

import (
	"context"
	"time"

	"lore/internal/entities"
)

const (
	defaultWalkDepth       = 3
	defaultConfidenceFloor = 0.3
)

type graphSource interface {
	Neighbors(ctx context.Context, ids []entities.DocID, kinds []entities.EdgeKind, dir entities.Direction) ([]entities.Edge, error)
	DocumentsByID(ctx context.Context, ids []entities.DocID) ([]entities.DocumentMeta, error)
}

type walkOptions struct {
	Depth         int
	Direction     entities.Direction
	Kinds         []entities.EdgeKind
	MinConfidence float32
	TimeAfter     *time.Time
}

type walkPath struct {
	Nodes      []entities.DocID
	Edges      []entities.Edge
	Confidence float32
}

type walkResult struct {
	Paths     []walkPath
	SeedLinks []entities.Edge
	Metas     map[entities.DocID]entities.DocumentMeta
}

// Seeds are never reached nodes: they open the paths, and no path ends on one.
func walkGraph(ctx context.Context, g graphSource, seeds []entities.DocID, opts walkOptions) (walkResult, error) {
	if len(seeds) == 0 {
		return walkResult{}, nil
	}

	w := &walker{
		graph:   g,
		opts:    opts,
		floor:   opts.confidenceFloor(),
		seeds:   make(map[entities.DocID]bool, len(seeds)),
		visited: make(map[entities.DocID]bool, len(seeds)),
		metas:   make(map[entities.DocID]entities.DocumentMeta, len(seeds)),
	}

	frontier := make([]walkPath, 0, len(seeds))
	for _, seed := range seeds {
		if w.visited[seed] {
			continue
		}
		w.seeds[seed], w.visited[seed] = true, true
		frontier = append(frontier, walkPath{Nodes: []entities.DocID{seed}, Confidence: 1})
	}
	if err := w.loadMetas(ctx, frontierIDs(frontier)); err != nil {
		return walkResult{}, err
	}

	var paths []walkPath
	for hop := opts.hops(); hop > 0 && len(frontier) > 0; hop-- {
		reached, err := w.step(ctx, frontier)
		if err != nil {
			return walkResult{}, err
		}
		paths = append(paths, reached...)
		frontier = reached
	}

	return walkResult{Paths: paths, SeedLinks: w.links, Metas: w.metas}, nil
}

type walker struct {
	graph   graphSource
	opts    walkOptions
	floor   float32
	seeds   map[entities.DocID]bool
	visited map[entities.DocID]bool
	links   []entities.Edge
	metas   map[entities.DocID]entities.DocumentMeta
}

func (w *walker) step(ctx context.Context, frontier []walkPath) ([]walkPath, error) {
	edges, err := w.graph.Neighbors(ctx, frontierIDs(frontier), w.opts.Kinds, w.opts.Direction)
	if err != nil {
		return nil, err
	}
	w.collectSeedLinks(edges)

	candidates := w.candidates(frontier, edges)
	if len(candidates) == 0 {
		return nil, nil
	}
	if err := w.loadMetas(ctx, candidateIDs(candidates)); err != nil {
		return nil, err
	}

	reached := make([]walkPath, 0, len(candidates))
	for _, c := range candidates {
		if !w.admits(c.node) {
			continue
		}
		w.visited[c.node] = true
		reached = append(reached, extend(frontier[c.parent], c))
	}

	return reached, nil
}

// Only the seed layer is asked about a seed, so a seed-to-seed edge arrives once.
func (w *walker) collectSeedLinks(edges []entities.Edge) {
	for _, e := range edges {
		if e.Src != e.Dst && w.seeds[e.Src] && w.seeds[e.Dst] {
			w.links = append(w.links, e)
		}
	}
}

type candidate struct {
	parent     int
	node       entities.DocID
	edge       entities.Edge
	confidence float32
}

// One candidate per reached node, the highest-confidence one; ties keep first-seen order.
func (w *walker) candidates(frontier []walkPath, edges []entities.Edge) []candidate {
	steps := stepsByNode(edges, w.opts.Direction)

	candidates := make([]candidate, 0, len(edges))
	at := make(map[entities.DocID]int, len(edges))
	for i, path := range frontier {
		for _, s := range steps[lastNode(path)] {
			confidence := path.Confidence * s.edge.Confidence
			if w.visited[s.node] || confidence < w.floor {
				continue
			}
			c := candidate{parent: i, node: s.node, edge: s.edge, confidence: confidence}
			if j, seen := at[s.node]; seen {
				if confidence > candidates[j].confidence {
					candidates[j] = c
				}
				continue
			}
			at[s.node] = len(candidates)
			candidates = append(candidates, c)
		}
	}

	return candidates
}

type walkStep struct {
	node entities.DocID
	edge entities.Edge
}

// Neighbors answers a whole frontier at once, so an edge may belong to either endpoint.
func stepsByNode(edges []entities.Edge, dir entities.Direction) map[entities.DocID][]walkStep {
	outward, inward := followed(dir)

	steps := make(map[entities.DocID][]walkStep, len(edges))
	for _, e := range edges {
		if e.Src == e.Dst {
			continue
		}
		if outward {
			steps[e.Src] = append(steps[e.Src], walkStep{node: e.Dst, edge: e})
		}
		if inward {
			steps[e.Dst] = append(steps[e.Dst], walkStep{node: e.Src, edge: e})
		}
	}

	return steps
}

func followed(dir entities.Direction) (outward, inward bool) {
	switch dir {
	case entities.DirIn:
		return false, true
	case entities.DirBoth:
		return true, true
	default:
		return true, false
	}
}

// Inadmissible nodes are not walked through either: every node of a returned path is itself evidence.
func (w *walker) admits(id entities.DocID) bool {
	meta, known := w.metas[id]
	if !known {
		return false
	}

	return w.opts.TimeAfter == nil || meta.CreatedAt.After(*w.opts.TimeAfter)
}

func (w *walker) loadMetas(ctx context.Context, ids []entities.DocID) error {
	metas, err := w.graph.DocumentsByID(ctx, ids)
	if err != nil {
		return err
	}
	for _, meta := range metas {
		w.metas[meta.ID] = meta
	}

	return nil
}

func extend(parent walkPath, c candidate) walkPath {
	nodes := make([]entities.DocID, len(parent.Nodes)+1)
	copy(nodes, parent.Nodes)
	nodes[len(parent.Nodes)] = c.node

	edges := make([]entities.Edge, len(parent.Edges)+1)
	copy(edges, parent.Edges)
	edges[len(parent.Edges)] = c.edge

	return walkPath{Nodes: nodes, Edges: edges, Confidence: c.confidence}
}

func lastNode(path walkPath) entities.DocID {
	return path.Nodes[len(path.Nodes)-1]
}

func frontierIDs(frontier []walkPath) []entities.DocID {
	ids := make([]entities.DocID, len(frontier))
	for i, path := range frontier {
		ids[i] = lastNode(path)
	}

	return ids
}

func candidateIDs(candidates []candidate) []entities.DocID {
	ids := make([]entities.DocID, len(candidates))
	for i, c := range candidates {
		ids[i] = c.node
	}

	return ids
}

func (o walkOptions) hops() int {
	if o.Depth <= 0 {
		return defaultWalkDepth
	}

	return o.Depth
}

func (o walkOptions) confidenceFloor() float32 {
	if o.MinConfidence <= 0 {
		return defaultConfidenceFloor
	}

	return o.MinConfidence
}
