package entities

import "time"

// EvidenceBundle is the one result shape every query tool returns.
type EvidenceBundle struct {
	Question string         // normalized restatement of the query
	Anchor   Anchor         // how the question was grounded
	Nodes    []EvidenceNode // ordered by relevance (impact_of: chronological)
	Chains   [][]DocID      // provenance paths, e.g. [ticket, page, pr, commit]
	Gaps     []string       // "trail ends at PROJ-4521; no linked follow-up"
}

// AnchorKind is a bit set: a code span or a document combines with a time
// window (AnchorCodeSpan|AnchorTimeWindow, AnchorDocument|AnchorTimeWindow).
type AnchorKind uint8

const (
	AnchorQuery AnchorKind = 1 << iota
	AnchorCodeSpan
	AnchorDocument
	AnchorTimeWindow
)

// Anchor records how a question was grounded. The pointer fields are populated
// for exactly the kinds present in Kind.
type Anchor struct {
	Kind   AnchorKind
	Query  string      // find_decision: the question as used for retrieval
	Code   *CodeAnchor // why/history_of
	Doc    *DocRef     // trace/impact_of: the resolved anchor document
	Window *TimeWindow // event resolution result
}

// CodeAnchor is a blamed line span in a registered local clone.
type CodeAnchor struct {
	Repo       string
	File       string
	LineStart  int
	LineEnd    int
	BlamedSHAs []string
}

// DocRef is the anchor document of a trace or impact_of query. CreatedAt is the
// anchor time impact_of filters consequences against.
type DocRef struct {
	ID        DocID
	Title     string
	URL       string
	CreatedAt time.Time
}

// TimeWindow is a resolved event window. AnchoredBy names the document that
// dated a free-text event and is empty when the window came from an ISO date.
type TimeWindow struct {
	From       time.Time
	To         time.Time
	Derivation string // "date 2025-03-12 ± 30d", "event 'incident X' via INC-201"
	AnchoredBy DocID
}

// Node roles as reported in EvidenceNode.Role.
const (
	RoleSeed          = "seed"
	RoleBlamedCommit  = "blamed_commit"
	RoleReviewThread  = "review_thread"
	RoleLinkedTicket  = "linked_ticket"
	RoleDesignDoc     = "design_doc"
	RoleFollowUp      = "follow_up"
	RoleSemanticMatch = "semantic_match"
)

// EvidenceNode is one cited document in a bundle. Every node carries a real
// URL: no URL means it is not evidence and is never returned.
type EvidenceNode struct {
	Doc     DocumentMeta
	Excerpt string // the relevant span, not the whole body
	Role    string
	Score   float32
	Via     []Edge // how this node was reached; empty for pure retrieval hits
}

// DocumentMeta is a Document without its body.
type DocumentMeta struct {
	ID        DocID
	Source    string
	Type      DocType
	Title     string
	Author    string
	URL       string
	CreatedAt time.Time
	UpdatedAt time.Time
}
