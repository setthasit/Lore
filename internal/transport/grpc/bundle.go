package grpc

import (
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	lorev1 "github.com/setthasit/Lore/api/proto/lore/v1"
	"github.com/setthasit/Lore/internal/entities"
	"github.com/setthasit/Lore/sdk"
)

func newEvidenceBundle(bundle *entities.EvidenceBundle) *lorev1.EvidenceBundle {
	return &lorev1.EvidenceBundle{
		Question: bundle.Question,
		Anchor:   newAnchor(bundle.Anchor),
		Nodes:    newEvidenceNodes(bundle.Nodes),
		Chains:   newChains(bundle.Chains),
		Gaps:     bundle.Gaps,
	}
}

func newAnchor(from entities.Anchor) *lorev1.Anchor {
	out := &lorev1.Anchor{Kinds: anchorKinds(from.Kind), Query: from.Query}
	if from.Code != nil {
		out.Code = &lorev1.CodeAnchor{
			Repo:       from.Code.Repo,
			File:       from.Code.File,
			LineStart:  int32(from.Code.LineStart),
			LineEnd:    int32(from.Code.LineEnd),
			BlamedShas: from.Code.BlamedSHAs,
		}
	}
	if from.Doc != nil {
		out.Doc = &lorev1.DocRef{
			Id:        string(from.Doc.ID),
			Title:     from.Doc.Title,
			Url:       from.Doc.URL,
			CreatedAt: newTimestamp(from.Doc.CreatedAt),
		}
	}
	if from.Window != nil {
		out.Window = &lorev1.TimeWindow{
			From:       newTimestamp(from.Window.From),
			To:         newTimestamp(from.Window.To),
			Derivation: from.Window.Derivation,
			AnchoredBy: string(from.Window.AnchoredBy),
		}
	}

	return out
}

var anchorKindCodes = []struct {
	kind entities.AnchorKind
	code lorev1.AnchorKind
}{
	{entities.AnchorQuery, lorev1.AnchorKind_ANCHOR_KIND_QUERY},
	{entities.AnchorCodeSpan, lorev1.AnchorKind_ANCHOR_KIND_CODE_SPAN},
	{entities.AnchorDocument, lorev1.AnchorKind_ANCHOR_KIND_DOCUMENT},
	{entities.AnchorTimeWindow, lorev1.AnchorKind_ANCHOR_KIND_TIME_WINDOW},
}

func anchorKinds(kind entities.AnchorKind) []lorev1.AnchorKind {
	kinds := make([]lorev1.AnchorKind, 0, len(anchorKindCodes))
	for _, candidate := range anchorKindCodes {
		if kind&candidate.kind != 0 {
			kinds = append(kinds, candidate.code)
		}
	}

	return kinds
}

var nodeRoleCodes = map[string]lorev1.NodeRole{
	entities.RoleSeed:          lorev1.NodeRole_NODE_ROLE_SEED,
	entities.RoleBlamedCommit:  lorev1.NodeRole_NODE_ROLE_BLAMED_COMMIT,
	entities.RoleReviewThread:  lorev1.NodeRole_NODE_ROLE_REVIEW_THREAD,
	entities.RoleLinkedTicket:  lorev1.NodeRole_NODE_ROLE_LINKED_TICKET,
	entities.RoleDesignDoc:     lorev1.NodeRole_NODE_ROLE_DESIGN_DOC,
	entities.RoleFollowUp:      lorev1.NodeRole_NODE_ROLE_FOLLOW_UP,
	entities.RoleSemanticMatch: lorev1.NodeRole_NODE_ROLE_SEMANTIC_MATCH,
	entities.RoleLinkedChange:  lorev1.NodeRole_NODE_ROLE_LINKED_CHANGE,
}

var edgeKindCodes = map[entities.EdgeKind]lorev1.EdgeKind{
	entities.EdgeKindCommitInPR:     lorev1.EdgeKind_EDGE_KIND_COMMIT_IN_PR,
	entities.EdgeKindPRClosesIssue:  lorev1.EdgeKind_EDGE_KIND_PR_CLOSES_ISSUE,
	entities.EdgeKindReferencesDoc:  lorev1.EdgeKind_EDGE_KIND_REFERENCES_DOC,
	entities.EdgeKindMentionsCommit: lorev1.EdgeKind_EDGE_KIND_MENTIONS_COMMIT,
	entities.EdgeKindMentionsPath:   lorev1.EdgeKind_EDGE_KIND_MENTIONS_PATH,
	entities.EdgeKindSupersedes:     lorev1.EdgeKind_EDGE_KIND_SUPERSEDES,
}

func newEvidenceNodes(nodes []entities.EvidenceNode) []*lorev1.EvidenceNode {
	out := make([]*lorev1.EvidenceNode, len(nodes))
	for i, node := range nodes {
		out[i] = &lorev1.EvidenceNode{
			Doc: &lorev1.DocumentMeta{
				Id:        string(node.Doc.ID),
				Source:    node.Doc.Source,
				Type:      string(node.Doc.Type),
				Title:     node.Doc.Title,
				Author:    node.Doc.Author,
				Url:       node.Doc.URL,
				CreatedAt: newTimestamp(node.Doc.CreatedAt),
				UpdatedAt: newTimestamp(node.Doc.UpdatedAt),
			},
			Excerpt: node.Excerpt,
			Role:    nodeRoleCodes[node.Role],
			Score:   node.Score,
			Via:     newEdges(node.Via),
		}
	}

	return out
}

func newEdges(edges []entities.Edge) []*lorev1.Edge {
	if len(edges) == 0 {
		return nil
	}

	out := make([]*lorev1.Edge, len(edges))
	for i, from := range edges {
		out[i] = &lorev1.Edge{
			Src:        string(from.Src),
			Dst:        string(from.Dst),
			Kind:       edgeKindCodes[from.Kind],
			Confidence: from.Confidence,
		}
	}

	return out
}

func newChains(chains [][]lore.DocID) []*lorev1.Chain {
	if len(chains) == 0 {
		return nil
	}

	out := make([]*lorev1.Chain, len(chains))
	for i, chain := range chains {
		ids := make([]string, len(chain))
		for j, id := range chain {
			ids[j] = string(id)
		}
		out[i] = &lorev1.Chain{DocIds: ids}
	}

	return out
}

func newTimestamp(at time.Time) *timestamppb.Timestamp {
	if at.IsZero() {
		return nil
	}

	return timestamppb.New(at)
}
