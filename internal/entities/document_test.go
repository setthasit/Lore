package entities_test

import (
	"testing"

	"github.com/setthasit/Lore/internal/entities"
)

func TestNewDocID(t *testing.T) {
	tests := []struct {
		name       string
		source     string
		docType    entities.DocType
		externalID string
		want       entities.DocID
	}{
		{
			name:       "commit",
			source:     "github",
			docType:    entities.DocTypeCommit,
			externalID: "abc123",
			want:       "github:commit:abc123",
		},
		{
			name:       "ticket comment with colons in external id",
			source:     "jira",
			docType:    entities.DocTypeTicketComment,
			externalID: "PROJ-123:10042",
			want:       "jira:ticket_comment:PROJ-123:10042",
		},
		{
			name:       "unknown doc type stays as given",
			source:     "slack",
			docType:    entities.DocType("message"),
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
			docType:    entities.DocTypePage,
			externalID: "",
			want:       "notion:page:",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := entities.NewDocID(tt.source, tt.docType, tt.externalID); got != tt.want {
				t.Errorf("NewDocID(%q, %q, %q) = %q, want %q",
					tt.source, tt.docType, tt.externalID, got, tt.want)
			}
		})
	}
}

func TestDocIDsAreUniquePerSourceAndType(t *testing.T) {
	pr := entities.NewDocID("github", entities.DocTypePR, "1")
	issue := entities.NewDocID("github", entities.DocTypeIssue, "1")
	otherSource := entities.NewDocID("gitlab", entities.DocTypePR, "1")

	if pr == issue {
		t.Errorf("pr and issue with the same external id collide: %q", pr)
	}
	if pr == otherSource {
		t.Errorf("same type across sources collides: %q", pr)
	}
}

func TestDocTypeConstants(t *testing.T) {
	tests := []struct {
		docType entities.DocType
		want    string
	}{
		{entities.DocTypeCommit, "commit"},
		{entities.DocTypePR, "pr"},
		{entities.DocTypePRReview, "pr_review"},
		{entities.DocTypeReviewComment, "review_comment"},
		{entities.DocTypeIssue, "issue"},
		{entities.DocTypeIssueComment, "issue_comment"},
		{entities.DocTypePage, "page"},
		{entities.DocTypeTicket, "ticket"},
		{entities.DocTypeTicketComment, "ticket_comment"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if string(tt.docType) != tt.want {
				t.Errorf("DocType = %q, want %q", tt.docType, tt.want)
			}
		})
	}

	seen := make(map[entities.DocType]struct{}, len(tests))
	for _, tt := range tests {
		if _, dup := seen[tt.docType]; dup {
			t.Errorf("duplicate DocType value %q", tt.docType)
		}
		seen[tt.docType] = struct{}{}
	}
}
