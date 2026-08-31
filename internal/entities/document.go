package entities

import "time"

// DocID is the globally unique document identity, formatted
// "<source>:<type>:<external_id>".
type DocID string

// NewDocID builds a DocID from its three parts.
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

// Document is the single shape every connector normalizes its source to.
type Document struct {
	ID        DocID
	Source    string // "github", "notion", "jira", …
	Type      DocType
	RepoRef   string // "github:owner/repo"; empty for non-repo documents
	Title     string
	Body      string // normalized plain text / markdown
	Author    string
	URL       string    // canonical web URL — the citation target
	CreatedAt time.Time // when the thing happened (event time)
	UpdatedAt time.Time // last edit (freshness / sync watermark)
	Refs      []RawRef  // unresolved references found in the body
}

// RefKind classifies the textual form of an unresolved reference.
type RefKind string

const (
	RefKindURL       RefKind = "url"
	RefKindTicketKey RefKind = "ticket_key"
	RefKindCommitSHA RefKind = "commit_sha"
	RefKindFilePath  RefKind = "file_path"
	RefKindPRNumber  RefKind = "pr_number"
)

// RawRef is a reference emitted by a connector before the LinkResolver turns it
// into an Edge.
type RawRef struct {
	Kind  RefKind
	Value string // "https://notion.so/…", "PROJ-123", "abc123", "internal/auth/auth.go"
}
