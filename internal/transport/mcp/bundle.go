package mcp

import (
	"encoding/json"
	"time"

	"lore/internal/entities"
)

func EncodeBundle(bundle *entities.EvidenceBundle) ([]byte, error) {
	return json.Marshal(newEvidenceBundle(bundle))
}

// The domain types carry no JSON tags, so the wire names live here.
type evidenceBundle struct {
	Question string         `json:"question"`
	Anchor   anchor         `json:"anchor"`
	Nodes    []evidenceNode `json:"nodes"`
	Chains   [][]string     `json:"chains,omitempty"`
	Gaps     []string       `json:"gaps,omitempty"`
}

type anchor struct {
	Kinds  []string    `json:"kinds"`
	Query  string      `json:"query,omitempty"`
	Code   *codeAnchor `json:"code,omitempty"`
	Doc    *docRef     `json:"doc,omitempty"`
	Window *timeWindow `json:"window,omitempty"`
}

// Line numbers are 1-based, so an omitted line_start/line_end means a whole-file anchor.
type codeAnchor struct {
	Repo       string   `json:"repo"`
	File       string   `json:"file"`
	LineStart  int      `json:"line_start,omitempty"`
	LineEnd    int      `json:"line_end,omitempty"`
	BlamedSHAs []string `json:"blamed_shas,omitempty"`
}

type docRef struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	URL       string    `json:"url"`
	CreatedAt time.Time `json:"created_at"`
}

type timeWindow struct {
	From       time.Time `json:"from"`
	To         time.Time `json:"to"`
	Derivation string    `json:"derivation"`
	AnchoredBy string    `json:"anchored_by,omitempty"`
}

type evidenceNode struct {
	Doc     documentMeta `json:"doc"`
	Excerpt string       `json:"excerpt"`
	Role    string       `json:"role"`
	Score   float32      `json:"score"`
	Via     []edge       `json:"via,omitempty"`
}

type documentMeta struct {
	ID        string    `json:"id"`
	Source    string    `json:"source"`
	Type      string    `json:"type"`
	Title     string    `json:"title"`
	Author    string    `json:"author,omitempty"`
	URL       string    `json:"url"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type edge struct {
	Src        string  `json:"src"`
	Dst        string  `json:"dst"`
	Kind       string  `json:"kind"`
	Confidence float32 `json:"confidence"`
}

func newEvidenceBundle(bundle *entities.EvidenceBundle) evidenceBundle {
	return evidenceBundle{
		Question: bundle.Question,
		Anchor:   newAnchor(bundle.Anchor),
		Nodes:    newEvidenceNodes(bundle.Nodes),
		Chains:   newChains(bundle.Chains),
		Gaps:     bundle.Gaps,
	}
}

func newAnchor(from entities.Anchor) anchor {
	out := anchor{Kinds: anchorKinds(from.Kind), Query: from.Query}
	if from.Code != nil {
		out.Code = &codeAnchor{
			Repo:       from.Code.Repo,
			File:       from.Code.File,
			LineStart:  from.Code.LineStart,
			LineEnd:    from.Code.LineEnd,
			BlamedSHAs: from.Code.BlamedSHAs,
		}
	}
	if from.Doc != nil {
		out.Doc = &docRef{
			ID:        string(from.Doc.ID),
			Title:     from.Doc.Title,
			URL:       from.Doc.URL,
			CreatedAt: from.Doc.CreatedAt,
		}
	}
	if from.Window != nil {
		out.Window = &timeWindow{
			From:       from.Window.From,
			To:         from.Window.To,
			Derivation: from.Window.Derivation,
			AnchoredBy: string(from.Window.AnchoredBy),
		}
	}

	return out
}

var anchorKindNames = []struct {
	kind entities.AnchorKind
	name string
}{
	{entities.AnchorQuery, "query"},
	{entities.AnchorCodeSpan, "code_span"},
	{entities.AnchorDocument, "document"},
	{entities.AnchorTimeWindow, "time_window"},
}

func anchorKinds(kind entities.AnchorKind) []string {
	names := make([]string, 0, len(anchorKindNames))
	for _, candidate := range anchorKindNames {
		if kind&candidate.kind != 0 {
			names = append(names, candidate.name)
		}
	}

	return names
}

func newEvidenceNodes(nodes []entities.EvidenceNode) []evidenceNode {
	out := make([]evidenceNode, len(nodes))
	for i, node := range nodes {
		out[i] = evidenceNode{
			Doc: documentMeta{
				ID:        string(node.Doc.ID),
				Source:    node.Doc.Source,
				Type:      string(node.Doc.Type),
				Title:     node.Doc.Title,
				Author:    node.Doc.Author,
				URL:       node.Doc.URL,
				CreatedAt: node.Doc.CreatedAt,
				UpdatedAt: node.Doc.UpdatedAt,
			},
			Excerpt: node.Excerpt,
			Role:    node.Role,
			Score:   node.Score,
			Via:     newEdges(node.Via),
		}
	}

	return out
}

func newEdges(edges []entities.Edge) []edge {
	if len(edges) == 0 {
		return nil
	}

	out := make([]edge, len(edges))
	for i, from := range edges {
		out[i] = edge{
			Src:        string(from.Src),
			Dst:        string(from.Dst),
			Kind:       string(from.Kind),
			Confidence: from.Confidence,
		}
	}

	return out
}

func newChains(chains [][]entities.DocID) [][]string {
	if len(chains) == 0 {
		return nil
	}

	out := make([][]string, len(chains))
	for i, chain := range chains {
		ids := make([]string, len(chain))
		for j, id := range chain {
			ids[j] = string(id)
		}
		out[i] = ids
	}

	return out
}
