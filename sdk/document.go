package lore

import (
	"encoding/json"
	"maps"
	"time"
)

// DocID is formatted "<source>:<type>:<external_id>".
type DocID string

// A connector uses SourceConfig.DocID rather than spelling its own name here.
func NewDocID(source string, t DocType, externalID string) DocID {
	return DocID(source + ":" + string(t) + ":" + externalID)
}

// The set is open: a connector may introduce a type, and an unknown type gets
// the default chunking strategy and ranks as ordinary evidence.
type DocType string

const (
	DocTypeCommit        DocType = "commit"
	DocTypePR            DocType = "pr"
	DocTypePRReview      DocType = "pr_review"
	DocTypeReviewComment DocType = "review_comment"
	DocTypeIssue         DocType = "issue"
	DocTypeIssueComment  DocType = "issue_comment"
	DocTypePage          DocType = "page"
	DocTypeTicket        DocType = "ticket"
	DocTypeTicketComment DocType = "ticket_comment"
)

// The json tags are the wire format an out-of-process plugin speaks.
type Document struct {
	ID      DocID   `json:"id"`
	Source  string  `json:"source"` // the instance id that produced it
	Type    DocType `json:"type"`
	RepoRef string  `json:"repo_ref"` // "github:owner/repo"; the key is always present, the value may be empty
	Title   string  `json:"title"`
	Body    string  `json:"body"` // normalized plain text / markdown
	Author  string  `json:"author"`
	URL     string  `json:"url"` // canonical web URL — the citation target

	// Both are required and non-zero, encoded RFC 3339 with an offset. A source
	// with no true creation time sets CreatedAt equal to UpdatedAt.
	CreatedAt time.Time `json:"created_at"` // event time
	UpdatedAt time.Time `json:"updated_at"` // last edit — the freshness watermark

	Refs []RawRef `json:"refs"`
}

// The vocabulary is closed: an unknown kind is rejected at ingest, never dropped.
type RefKind string

const (
	RefKindURL       RefKind = "url"
	RefKindTicketKey RefKind = "ticket_key"
	RefKindCommitSHA RefKind = "commit_sha"
	RefKindFilePath  RefKind = "file_path"
	RefKindPRNumber  RefKind = "pr_number"
)

// RefKinds lists the vocabulary in the order errors list it.
func RefKinds() []RefKind {
	return []RefKind{RefKindURL, RefKindTicketKey, RefKindCommitSHA, RefKindFilePath, RefKindPRNumber}
}

type RawRef struct {
	Kind  RefKind `json:"kind"`
	Value string  `json:"value"`
}

// Cursor is opaque and per-instance: only the connector that produced it
// interprets its keys.
type Cursor map[string]string

// Clone never returns nil; a nil or empty receiver yields a fresh empty Cursor.
func (c Cursor) Clone() Cursor {
	if len(c) == 0 {
		return Cursor{}
	}
	return maps.Clone(c)
}

// Cursor becomes durable once Docs are durably committed, and every batch
// carries a cursor, empty ones included.
type Batch struct {
	Docs   []Document `json:"docs"`
	Cursor Cursor     `json:"cursor"`
}

// docs is never null: docs/v3/09-plugin-protocol.md types it as a list.
func (b Batch) MarshalJSON() ([]byte, error) {
	type wire Batch
	if b.Docs == nil {
		b.Docs = []Document{}
	}
	return json.Marshal(wire(b))
}
