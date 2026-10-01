package services_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/setthasit/Lore/internal/entities"
	"github.com/setthasit/Lore/internal/services"
	"github.com/setthasit/Lore/sdk"
)

// Mirrors the chunker's own sizing constants, so a drift in either fails here.
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

func docWith(t lore.DocType, id lore.DocID, body string) lore.Document {
	return lore.Document{
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

func paragraph(section, index int) string {
	return strings.TrimSpace(fmt.Sprintf("s%dp%d %s", section, index, strings.Repeat("alpha ", 32)))
}

func paragraphs(section, count int) string {
	var b strings.Builder
	for p := range count {
		b.WriteString(paragraph(section, p))
		b.WriteString("\n\n")
	}

	return b.String()
}

func headedSection(section int, heading string, count int) string {
	return heading + "\n\n" + paragraphs(section, count)
}

// Each section is itself between minChunkTokens and maxChunkTokens.
func headedBody(sections, perSection int) string {
	var b strings.Builder
	for s := range sections {
		b.WriteString(headedSection(s, fmt.Sprintf("## Section %d", s), perSection))
	}

	return b.String()
}

func plainBody(count int) string {
	return strings.TrimSpace(paragraphs(0, count))
}

func assertInvariants(t *testing.T, doc lore.Document, chunks []entities.Chunk) {
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

func carriedOverlap(text string) string {
	head, _, ok := strings.Cut(text, "\n\n")
	if !ok {
		return text
	}

	return head
}

func assertHeadingPaths(t *testing.T, chunks []entities.Chunk, wantPaths []string) []entities.Chunk {
	t.Helper()
	if len(chunks) != len(wantPaths)+1 {
		t.Fatalf("got %d chunks, want %d", len(chunks), len(wantPaths)+1)
	}
	stripped := slices.Clone(chunks)
	for i, want := range wantPaths {
		c := &stripped[i+1]
		if want == "" {
			continue
		}
		text, ok := strings.CutPrefix(c.Text, want+"\n\n")
		if !ok {
			t.Errorf("chunk %d = %q, want it to start with heading path %q", c.Ordinal, c.Text, want)
			continue
		}
		c.Text = text
	}
	assertOverlap(t, stripped)

	return stripped
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
	doc := docWith(lore.DocTypeCommit, "github:commit:abc123", "  "+body+"\n")

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
		docType    lore.DocType
		id         lore.DocID
		body       string
		wantThread string
	}{
		{
			name:       "review comment",
			docType:    lore.DocTypeReviewComment,
			id:         "github:review_comment:acme/lore/pull/42#discussion_r7",
			body:       "The retry budget should be per connector, not global.",
			wantThread: "github:review_comment:acme/lore/pull/42",
		},
		{
			name:       "issue comment",
			docType:    lore.DocTypeIssueComment,
			id:         "github:issue_comment:acme/lore/issues/42#issuecomment-9",
			body:       "Reproduced on the staging workspace.",
			wantThread: "github:issue_comment:acme/lore/issues/42",
		},
		{
			name:       "ticket comment",
			docType:    lore.DocTypeTicketComment,
			id:         "jira:ticket_comment:PROJ-1#10042",
			body:       "Deferred to the next sprint after the incident review.",
			wantThread: "jira:ticket_comment:PROJ-1",
		},
		{
			name:       "comment without a thread fragment is its own thread",
			docType:    lore.DocTypeIssueComment,
			id:         "github:issue_comment:9",
			body:       "Standalone comment.",
			wantThread: "github:issue_comment:9",
		},
		{
			name:       "long comment is still one chunk",
			docType:    lore.DocTypeIssueComment,
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
	doc := docWith(lore.DocTypePage, "notion:page:design-sync", headedBody(sections, 7))

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
	doc := docWith(lore.DocTypePR, "github:pr:acme/lore/42", plainBody(30))

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

	if !strings.HasPrefix(chunks[0].Text, paragraph(0, 0)) {
		t.Errorf("first chunk does not start at the first paragraph: %q", chunks[0].Text)
	}
	if !strings.HasSuffix(chunks[len(chunks)-1].Text, paragraph(0, 29)) {
		t.Errorf("last chunk does not end at the last paragraph: %q", chunks[len(chunks)-1].Text)
	}
}

func TestChunkDefaultStrategyPerDocType(t *testing.T) {
	tests := []lore.DocType{
		lore.DocTypePR,
		lore.DocTypeIssue,
		lore.DocTypeTicket,
		lore.DocTypePage,
		lore.DocTypePRReview,
		lore.DocType("message"), // unknown / future type
	}

	for _, docType := range tests {
		t.Run(string(docType), func(t *testing.T) {
			doc := docWith(docType, lore.NewDocID("github", docType, "1"), headedBody(4, 7))

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
			doc := docWith(lore.DocTypePage, "notion:page:wall-of-text", tt.body)

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
	docTypes := []lore.DocType{
		lore.DocTypeCommit,
		lore.DocTypeIssueComment,
		lore.DocTypePage,
		lore.DocType("message"),
	}

	for name, body := range bodies {
		for _, docType := range docTypes {
			t.Run(name+"/"+string(docType), func(t *testing.T) {
				doc := docWith(docType, lore.NewDocID("github", docType, "1"), body)

				if chunks := services.NewChunker().Chunk(doc); len(chunks) != 0 {
					t.Errorf("got %d chunks, want none: %+v", len(chunks), chunks)
				}
			})
		}
	}
}

const (
	architectureHeading = "# 15 Architecture"
	timeSection         = "15.5"
	timeHeading         = "## " + timeSection + " Time and fast forward"
	timePath            = architectureHeading + "\n" + timeHeading
	schedulingHeading   = "## 15.6 Scheduling"
)

func TestChunkCarriesItsHeadingPathMidSection(t *testing.T) {
	body := architectureHeading + "\n\n" + headedSection(0, timeHeading, 30)
	doc := docWith(lore.DocTypePage, "notion:page:design-architecture", body)

	chunks := services.NewChunker().Chunk(doc)

	if len(chunks) < 3 {
		t.Fatalf("got %d chunks, want the section split into several", len(chunks))
	}
	assertInvariants(t, doc, chunks)
	stripped := assertHeadingPaths(t, chunks, slices.Repeat([]string{timePath}, len(chunks)-1))

	if want := architectureHeading + "\n\n" + timeHeading + "\n\n" + paragraph(0, 0); !strings.HasPrefix(chunks[0].Text, want) {
		t.Errorf("first chunk = %q, want it to start with %q", chunks[0].Text, want)
	}
	for _, c := range stripped[1:] {
		if strings.Contains(c.Text, timeSection) {
			t.Errorf("chunk %d names its section outside the heading path, so the path is untested: %q", c.Ordinal, c.Text)
		}
		if got := tokens(c.Text); got > maxChunkTokens+overlapTokens {
			t.Errorf("chunk %d = %d tokens without its heading path, want <= %d", c.Ordinal, got, maxChunkTokens+overlapTokens)
		}
	}
}

func TestChunkHeadingPathDropsClosedSections(t *testing.T) {
	tests := []struct {
		name      string
		next      string
		wantPaths []string
	}{
		{
			name:      "a sibling section keeps the enclosing section open",
			next:      "## 16 Operations",
			wantPaths: []string{architectureHeading, architectureHeading + "\n## 16 Operations"},
		},
		{
			name:      "a same-level section closes the enclosing section",
			next:      "# 16 Operations",
			wantPaths: []string{"", "# 16 Operations"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := architectureHeading + "\n\n" + headedSection(0, timeHeading, 7) + headedSection(1, tt.next, 14)
			doc := docWith(lore.DocTypePage, "notion:page:design-architecture", body)

			chunks := services.NewChunker().Chunk(doc)

			assertInvariants(t, doc, chunks)
			stripped := assertHeadingPaths(t, chunks, tt.wantPaths)

			if content := strings.TrimPrefix(stripped[1].Text, carriedOverlap(stripped[1].Text)+"\n\n"); !strings.HasPrefix(content, tt.next) {
				t.Errorf("chunk 1 content does not start at %q: %q", tt.next, chunks[1].Text)
			}
			for _, c := range chunks[1:] {
				if strings.Contains(c.Text, timeHeading) {
					t.Errorf("chunk %d under %q still carries the closed %q: %q", c.Ordinal, tt.next, timeHeading, c.Text)
				}
			}
		})
	}
}

func TestChunkDoesNotRepeatTheHeadingItStartsWith(t *testing.T) {
	tests := []struct {
		name     string
		next     string
		wantPath string
	}{
		{name: "sibling section", next: schedulingHeading, wantPath: architectureHeading},
		{name: "nested section", next: "### 15.5.1 Clock skew", wantPath: timePath},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			next := headedSection(1, tt.next, 3)
			body := architectureHeading + "\n\n" + headedSection(0, timeHeading, 7) + next
			doc := docWith(lore.DocTypePage, "notion:page:design-architecture", body)

			chunks := services.NewChunker().Chunk(doc)

			if len(chunks) != 2 {
				t.Fatalf("got %d chunks, want 2", len(chunks))
			}
			assertInvariants(t, doc, chunks)

			want := tt.wantPath + "\n\n" + paragraph(0, 6) + "\n\n" + strings.TrimSpace(next)
			if chunks[1].Text != want {
				t.Errorf("chunk 1 = %q, want %q", chunks[1].Text, want)
			}
		})
	}
}

func TestChunkIgnoresHeadingsInsideCodeFences(t *testing.T) {
	tests := []struct {
		name  string
		fence string
	}{
		{name: "backtick fence", fence: "```go\n# comment\n```"},
		{name: "tilde fence", fence: "~~~\n# comment\n~~~"},
		{name: "a longer run closes", fence: "```\n# comment\n`````"},
		{name: "the other character does not close", fence: "```\n~~~\n# comment\n```"},
		{name: "a shorter run does not close", fence: "````\n```\n# comment\n````"},
		{name: "a run with an info string does not close", fence: "```\n```go\n# comment\n```"},
	}
	wantPaths := []string{timePath, architectureHeading, architectureHeading + "\n" + schedulingHeading}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := architectureHeading + "\n\n" + timeHeading + "\n\n" + tt.fence + "\n\n" + paragraphs(0, 16) +
				headedSection(1, schedulingHeading, 12)
			doc := docWith(lore.DocTypePage, "notion:page:design-architecture", body)

			chunks := services.NewChunker().Chunk(doc)

			assertInvariants(t, doc, chunks)
			assertHeadingPaths(t, chunks, wantPaths)

			if !strings.Contains(chunks[0].Text, tt.fence) {
				t.Errorf("chunk 0 does not hold the fenced block %q intact: %q", tt.fence, chunks[0].Text)
			}
		})
	}
}
