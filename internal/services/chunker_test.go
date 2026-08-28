package services_test

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"lore/internal/entities"
	"lore/internal/services"
)

// The chunker's sizing contract, restated here so the tests fail if the
// implementation's constants drift away from the documented targets.
const (
	bytesPerToken  = 4
	minChunkTokens = 300
	maxChunkTokens = 500
	overlapTokens  = 50
)

func tokens(text string) int { return len(text) / bytesPerToken }

var (
	created = time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)
	updated = time.Date(2024, 3, 2, 9, 30, 0, 0, time.UTC)
)

func docWith(t entities.DocType, id entities.DocID, body string) entities.Document {
	return entities.Document{
		ID:        id,
		Source:    "github",
		Type:      t,
		RepoRef:   "github:acme/lore",
		Title:     "Bounded retries for the sync loop",
		Body:      body,
		Author:    "dev@example.test",
		URL:       "https://github.example.test/acme/lore",
		CreatedAt: created,
		UpdatedAt: updated,
	}
}

// paragraph builds a distinguishable ~50-token single-line paragraph.
func paragraph(section, index int) string {
	return strings.TrimSpace(fmt.Sprintf("s%dp%d %s", section, index, strings.Repeat("alpha ", 32)))
}

// headedBody builds a markdown body of sections sized so that each section is
// itself between minChunkTokens and maxChunkTokens.
func headedBody(sections, perSection int) string {
	var b strings.Builder
	for s := range sections {
		fmt.Fprintf(&b, "## Section %d\n\n", s)
		for p := range perSection {
			b.WriteString(paragraph(s, p))
			b.WriteString("\n\n")
		}
	}

	return b.String()
}

func plainBody(paragraphs int) string {
	parts := make([]string, 0, paragraphs)
	for p := range paragraphs {
		parts = append(parts, paragraph(0, p))
	}

	return strings.Join(parts, "\n\n")
}

// assertInvariants checks what every chunk of every document must satisfy:
// sequential 0-based ordinals, copied metadata, non-empty valid text and no
// embedding (the sync service fills vectors later).
func assertInvariants(t *testing.T, doc entities.Document, chunks []entities.Chunk) {
	t.Helper()
	for i, c := range chunks {
		if c.Ordinal != i {
			t.Errorf("chunk %d: ordinal = %d, want %d", i, c.Ordinal, i)
		}
		if c.DocID != doc.ID || c.Source != doc.Source || c.RepoRef != doc.RepoRef || c.DocType != doc.Type || c.Author != doc.Author {
			t.Errorf("chunk %d: metadata = %+v, want it copied from %+v", i, c, doc)
		}
		if !c.CreatedAt.Equal(doc.CreatedAt) || !c.UpdatedAt.Equal(doc.UpdatedAt) {
			t.Errorf("chunk %d: timestamps = %v / %v, want %v / %v", i, c.CreatedAt, c.UpdatedAt, doc.CreatedAt, doc.UpdatedAt)
		}
		if strings.TrimSpace(c.Text) == "" {
			t.Errorf("chunk %d: empty text", i)
		}
		if !utf8.ValidString(c.Text) {
			t.Errorf("chunk %d: text is not valid UTF-8", i)
		}
		if c.Embedding != nil {
			t.Errorf("chunk %d: embedding = %v, want nil", i, c.Embedding)
		}
	}
}

// carriedOverlap returns the context a chunk carried from its predecessor: the
// text before its first paragraph break.
func carriedOverlap(text string) string {
	head, _, ok := strings.Cut(text, "\n\n")
	if !ok {
		return text
	}

	return head
}

func assertOverlap(t *testing.T, chunks []entities.Chunk) {
	t.Helper()
	for i := 1; i < len(chunks); i++ {
		overlap := carriedOverlap(chunks[i].Text)
		switch {
		case overlap == "":
			t.Errorf("chunk %d carries no overlap from chunk %d", i, i-1)
		case !strings.HasSuffix(chunks[i-1].Text, overlap):
			t.Errorf("chunk %d overlap %q is not the tail of chunk %d", i, overlap, i-1)
		case tokens(overlap) > overlapTokens:
			t.Errorf("chunk %d overlap = %d tokens, want <= %d", i, tokens(overlap), overlapTokens)
		}
	}
}

func TestChunkCommitIsOneChunk(t *testing.T) {
	body := "fix(sync): bound connector retries\n\n" + plainBody(4)
	doc := docWith(entities.DocTypeCommit, "github:commit:abc123", "  "+body+"\n")

	chunks := services.NewChunker().Chunk(doc)

	if len(chunks) != 1 {
		t.Fatalf("got %d chunks, want 1", len(chunks))
	}
	assertInvariants(t, doc, chunks)
	if chunks[0].Text != body {
		t.Errorf("text = %q, want the trimmed message %q", chunks[0].Text, body)
	}
	if strings.Contains(chunks[0].Text, doc.Title) {
		t.Errorf("title %q duplicated into chunk text", doc.Title)
	}
	if chunks[0].ThreadID != "" {
		t.Errorf("thread id = %q, want empty for a commit", chunks[0].ThreadID)
	}
}

func TestChunkCommentIsOneChunkWithThreadID(t *testing.T) {
	tests := []struct {
		name       string
		docType    entities.DocType
		id         entities.DocID
		body       string
		wantThread string
	}{
		{
			name:       "review comment",
			docType:    entities.DocTypeReviewComment,
			id:         "github:review_comment:acme/lore/pull/42#discussion_r7",
			body:       "The retry budget should be per connector, not global.",
			wantThread: "github:review_comment:acme/lore/pull/42",
		},
		{
			name:       "issue comment",
			docType:    entities.DocTypeIssueComment,
			id:         "github:issue_comment:acme/lore/issues/42#issuecomment-9",
			body:       "Reproduced on the staging workspace.",
			wantThread: "github:issue_comment:acme/lore/issues/42",
		},
		{
			name:       "ticket comment",
			docType:    entities.DocTypeTicketComment,
			id:         "jira:ticket_comment:PROJ-1#10042",
			body:       "Deferred to the next sprint after the incident review.",
			wantThread: "jira:ticket_comment:PROJ-1",
		},
		{
			name:       "comment without a thread fragment is its own thread",
			docType:    entities.DocTypeIssueComment,
			id:         "github:issue_comment:9",
			body:       "Standalone comment.",
			wantThread: "github:issue_comment:9",
		},
		{
			name:       "long comment is still one chunk",
			docType:    entities.DocTypeIssueComment,
			id:         "github:issue_comment:acme/lore/issues/7#issuecomment-1",
			body:       headedBody(3, 7),
			wantThread: "github:issue_comment:acme/lore/issues/7",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := docWith(tt.docType, tt.id, tt.body)

			chunks := services.NewChunker().Chunk(doc)

			if len(chunks) != 1 {
				t.Fatalf("got %d chunks, want 1", len(chunks))
			}
			assertInvariants(t, doc, chunks)
			if chunks[0].Text != strings.TrimSpace(tt.body) {
				t.Errorf("text = %q, want the whole comment body", chunks[0].Text)
			}
			if chunks[0].ThreadID != tt.wantThread {
				t.Errorf("thread id = %q, want %q", chunks[0].ThreadID, tt.wantThread)
			}
		})
	}
}

func TestChunkPageSplitsOnHeadings(t *testing.T) {
	const sections = 5
	doc := docWith(entities.DocTypePage, "notion:page:design-sync", headedBody(sections, 7))

	chunks := services.NewChunker().Chunk(doc)

	if len(chunks) != sections {
		t.Fatalf("got %d chunks, want one per section (%d)", len(chunks), sections)
	}
	assertInvariants(t, doc, chunks)
	assertOverlap(t, chunks)

	for i, c := range chunks {
		heading := fmt.Sprintf("## Section %d", i)
		if !strings.Contains(c.Text, heading) {
			t.Errorf("chunk %d does not contain %q", i, heading)
		}
		if i > 0 && !strings.HasPrefix(strings.TrimPrefix(c.Text, carriedOverlap(c.Text)+"\n\n"), heading) {
			t.Errorf("chunk %d does not start its own content at %q: %q", i, heading, c.Text)
		}
		if got := tokens(c.Text); got < minChunkTokens || got > maxChunkTokens+overlapTokens {
			t.Errorf("chunk %d = %d tokens, want %d..%d", i, got, minChunkTokens, maxChunkTokens+overlapTokens)
		}
		if c.ThreadID != "" {
			t.Errorf("chunk %d: thread id = %q, want empty for a page", i, c.ThreadID)
		}
	}
}

func TestChunkFallsBackToParagraphGroups(t *testing.T) {
	doc := docWith(entities.DocTypePR, "github:pr:acme/lore/42", plainBody(30))

	chunks := services.NewChunker().Chunk(doc)

	if len(chunks) < 3 {
		t.Fatalf("got %d chunks, want the body split into several", len(chunks))
	}
	assertInvariants(t, doc, chunks)
	assertOverlap(t, chunks)

	for i, c := range chunks {
		if got := tokens(c.Text); got > maxChunkTokens+overlapTokens {
			t.Errorf("chunk %d = %d tokens, want <= %d", i, got, maxChunkTokens+overlapTokens)
		}
		if i < len(chunks)-1 && tokens(c.Text) < minChunkTokens {
			t.Errorf("chunk %d = %d tokens, want >= %d for a non-final chunk", i, tokens(c.Text), minChunkTokens)
		}
	}

	// Paragraph boundaries are respected: the first paragraph opens the first
	// chunk and the last one closes the last chunk, whole.
	if !strings.HasPrefix(chunks[0].Text, paragraph(0, 0)) {
		t.Errorf("first chunk does not start at the first paragraph: %q", chunks[0].Text)
	}
	if !strings.HasSuffix(chunks[len(chunks)-1].Text, paragraph(0, 29)) {
		t.Errorf("last chunk does not end at the last paragraph: %q", chunks[len(chunks)-1].Text)
	}
}

func TestChunkDefaultStrategyPerDocType(t *testing.T) {
	tests := []entities.DocType{
		entities.DocTypePR,
		entities.DocTypeIssue,
		entities.DocTypeTicket,
		entities.DocTypePage,
		entities.DocTypePRReview,
		entities.DocType("message"), // unknown / future type
	}

	for _, docType := range tests {
		t.Run(string(docType), func(t *testing.T) {
			doc := docWith(docType, entities.NewDocID("github", docType, "1"), headedBody(4, 7))

			chunks := services.NewChunker().Chunk(doc)

			if len(chunks) < 2 {
				t.Fatalf("got %d chunks, want the body split", len(chunks))
			}
			assertInvariants(t, doc, chunks)
			assertOverlap(t, chunks)
			for i, c := range chunks {
				if got := tokens(c.Text); got > maxChunkTokens+overlapTokens {
					t.Errorf("chunk %d = %d tokens, want <= %d", i, got, maxChunkTokens+overlapTokens)
				}
				if c.ThreadID != "" {
					t.Errorf("chunk %d: thread id = %q, want empty for %q", i, c.ThreadID, docType)
				}
			}
		})
	}
}

func TestChunkSplitsOversizedParagraph(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "no paragraph breaks", body: strings.TrimSpace(strings.Repeat("alpha ", 1200))},
		{name: "multibyte text without word breaks", body: strings.Repeat("日本語テキスト", 400)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := docWith(entities.DocTypePage, "notion:page:wall-of-text", tt.body)

			chunks := services.NewChunker().Chunk(doc)

			if len(chunks) < 2 {
				t.Fatalf("got %d chunks, want the oversized paragraph split", len(chunks))
			}
			assertInvariants(t, doc, chunks)
			for i, c := range chunks {
				if got := tokens(c.Text); got > maxChunkTokens+overlapTokens {
					t.Errorf("chunk %d = %d tokens, want <= %d", i, got, maxChunkTokens+overlapTokens)
				}
			}
		})
	}
}

func TestChunkEmptyBodyYieldsNoChunks(t *testing.T) {
	bodies := map[string]string{"empty": "", "whitespace only": "  \n\t\n  "}
	docTypes := []entities.DocType{
		entities.DocTypeCommit,
		entities.DocTypeIssueComment,
		entities.DocTypePage,
		entities.DocType("message"),
	}

	for name, body := range bodies {
		for _, docType := range docTypes {
			t.Run(name+"/"+string(docType), func(t *testing.T) {
				doc := docWith(docType, entities.NewDocID("github", docType, "1"), body)

				if chunks := services.NewChunker().Chunk(doc); len(chunks) != 0 {
					t.Errorf("got %d chunks, want none: %+v", len(chunks), chunks)
				}
			})
		}
	}
}
