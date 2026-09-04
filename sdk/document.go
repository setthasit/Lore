package lore

import "time"

// DocID is the globally unique document identity, formatted
// "<source>:<type>:<external_id>".
type DocID string

// NewDocID builds a DocID from its three parts. A connector never spells its
// own name here: the host supplies the instance id through SourceConfig.DocID.
func NewDocID(source string, t DocType, externalID string) DocID {
	return DocID(source + ":" + string(t) + ":" + externalID)
}

// DocType names the kind of thing a Document normalizes. The set is open: a new
// connector may introduce a type, and unknown types get the default chunking
// strategy and rank as ordinary evidence.
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

// Document is the single shape every connector normalizes its source to. The
// tags are the wire format an out-of-process plugin speaks; they are on the
// type rather than on a separate DTO so the two modes cannot drift.
type Document struct {
	ID      DocID   `json:"id"`
	Source  string  `json:"source"` // the instance id that produced it — "github", "jira-acme", …
	Type    DocType `json:"type"`
	RepoRef string  `json:"repo_ref"` // "github:owner/repo"; the key is always present, the value may be empty
	Title   string  `json:"title"`
	Body    string  `json:"body"` // normalized plain text / markdown
	Author  string  `json:"author"`
	URL     string  `json:"url"` // canonical web URL — the citation target

	// Both timestamps are required and non-zero, encoded RFC 3339 with an offset.
	// A source with no true creation time sets CreatedAt equal to UpdatedAt and
	// says so in its manifest summary.
	CreatedAt time.Time `json:"created_at"` // when the thing happened (event time)
	UpdatedAt time.Time `json:"updated_at"` // last edit (freshness / sync watermark)

	Refs []RawRef `json:"refs"` // unresolved references found in the body
}

// RefKind classifies the textual form of an unresolved reference. The
// vocabulary is closed: an unknown kind is rejected at ingest, never dropped.
type RefKind string

const (
	RefKindURL       RefKind = "url"
	RefKindTicketKey RefKind = "ticket_key"
	RefKindCommitSHA RefKind = "commit_sha"
	RefKindFilePath  RefKind = "file_path"
	RefKindPRNumber  RefKind = "pr_number"
)

// RefKinds is the closed vocabulary, in the order errors list it.
func RefKinds() []RefKind {
	return []RefKind{RefKindURL, RefKindTicketKey, RefKindCommitSHA, RefKindFilePath, RefKindPRNumber}
}

// RawRef is a reference emitted by a connector before the host turns it into an
// edge.
type RawRef struct {
	Kind  RefKind `json:"kind"`
	Value string  `json:"value"` // "https://notion.so/…", "PROJ-123", "abc123", "internal/auth/auth.go"
}

// Cursor is an opaque per-instance sync position; only the connector that
// produced it interprets its keys.
type Cursor map[string]string

// Batch is the checkpoint unit of a sync round: Cursor becomes durable once Docs
// are durably committed. Every batch carries a cursor, empty ones included.
type Batch struct {
	Docs   []Document `json:"docs"`
	Cursor Cursor     `json:"cursor"`
}
