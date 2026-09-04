package lore_test

import (
	"testing"

	"github.com/setthasit/Lore/sdk"
)

func TestNewDocID(t *testing.T) {
	tests := []struct {
		name       string
		source     string
		docType    lore.DocType
		externalID string
		want       lore.DocID
	}{
		{
			name:       "commit",
			source:     "github",
			docType:    lore.DocTypeCommit,
			externalID: "abc123",
			want:       "github:commit:abc123",
		},
		{
			name:       "ticket comment with colons in external id",
			source:     "jira",
			docType:    lore.DocTypeTicketComment,
			externalID: "PROJ-123:10042",
			want:       "jira:ticket_comment:PROJ-123:10042",
		},
		{
			name:       "unknown doc type stays as given",
			source:     "slack",
			docType:    lore.DocType("message"),
			externalID: "C01/1712345678.000100",
			want:       "slack:message:C01/1712345678.000100",
		},
		{
			name:       "empty parts keep their separators",
			source:     "",
			docType:    "",
			externalID: "",
			want:       "::",
		},
		{
			name:       "empty external id",
			source:     "notion",
			docType:    lore.DocTypePage,
			externalID: "",
			want:       "notion:page:",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := lore.NewDocID(tt.source, tt.docType, tt.externalID); got != tt.want {
				t.Errorf("NewDocID(%q, %q, %q) = %q, want %q",
					tt.source, tt.docType, tt.externalID, got, tt.want)
			}
		})
	}
}

func TestDocIDsAreUniquePerSourceAndType(t *testing.T) {
	pr := lore.NewDocID("github", lore.DocTypePR, "1")
	issue := lore.NewDocID("github", lore.DocTypeIssue, "1")
	otherSource := lore.NewDocID("gitlab", lore.DocTypePR, "1")

	if pr == issue {
		t.Errorf("pr and issue with the same external id collide: %q", pr)
	}
	if pr == otherSource {
		t.Errorf("same type across sources collides: %q", pr)
	}
}

func TestDocTypeConstants(t *testing.T) {
	tests := []struct {
		docType lore.DocType
		want    string
	}{
		{lore.DocTypeCommit, "commit"},
		{lore.DocTypePR, "pr"},
		{lore.DocTypePRReview, "pr_review"},
		{lore.DocTypeReviewComment, "review_comment"},
		{lore.DocTypeIssue, "issue"},
		{lore.DocTypeIssueComment, "issue_comment"},
		{lore.DocTypePage, "page"},
		{lore.DocTypeTicket, "ticket"},
		{lore.DocTypeTicketComment, "ticket_comment"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if string(tt.docType) != tt.want {
				t.Errorf("DocType = %q, want %q", tt.docType, tt.want)
			}
		})
	}

	seen := make(map[lore.DocType]struct{}, len(tests))
	for _, tt := range tests {
		if _, dup := seen[tt.docType]; dup {
			t.Errorf("duplicate DocType value %q", tt.docType)
		}
		seen[tt.docType] = struct{}{}
	}
}
