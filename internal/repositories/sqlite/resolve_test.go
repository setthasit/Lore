package sqlite

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/setthasit/Lore/internal/entities"
)

const (
	firstSHA  = "1234567aaaaa000000000000000000000000abcd"
	secondSHA = "1234567bbbbb000000000000000000000000abcd"
	notionID  = "0123456789abcdef0123456789abcdef"
	pageURL   = "https://notion.so/design/retrieval"
)

func TestResolveRef(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	firstCommit := entities.NewDocID("github", entities.DocTypeCommit, "acme/lore/commit/"+firstSHA)
	secondCommit := entities.NewDocID("github", entities.DocTypeCommit, "acme/lore/commit/"+secondSHA)
	pr := entities.NewDocID("github", entities.DocTypePR, "acme/lore/pull/42")
	issue := entities.NewDocID("github", entities.DocTypeIssue, "acme/lore/issues/42")
	ticket := entities.NewDocID("jira", entities.DocTypeTicket, "PROJ-123")
	page := entities.NewDocID("notion", entities.DocTypePage, "design/retrieval")
	hexPage := entities.NewDocID("notion", entities.DocTypePage, notionID)

	seedDocuments(t, s, []entities.Document{
		{ID: firstCommit, Source: "github", Type: entities.DocTypeCommit, Title: "Rework the resolver"},
		{ID: secondCommit, Source: "github", Type: entities.DocTypeCommit, Title: "Revert the resolver"},
		{ID: pr, Source: "github", Type: entities.DocTypePR, Title: "Provenance engine"},
		{ID: issue, Source: "github", Type: entities.DocTypeIssue, Title: "Trace answers to sources"},
		{ID: ticket, Source: "jira", Type: entities.DocTypeTicket, Title: "Ship provenance"},
		{ID: page, Source: "notion", Type: entities.DocTypePage, Title: "Retrieval design", URL: pageURL},
		{ID: hexPage, Source: "notion", Type: entities.DocTypePage, Title: "Notion ids look like SHAs"},
	})

	tests := []struct {
		name string
		ref  string
		want []entities.DocID
	}{
		{name: "full sha names one commit", ref: firstSHA, want: []entities.DocID{firstCommit}},
		{name: "uppercase sha resolves too", ref: strings.ToUpper(firstSHA), want: []entities.DocID{firstCommit}},
		{
			name: "shared abbreviation is ambiguous",
			ref:  firstSHA[:7],
			want: []entities.DocID{firstCommit, secondCommit},
		},
		{name: "hex prefix nobody ingested", ref: "deadbee", want: nil},
		{name: "non-commit is never a sha candidate", ref: notionID, want: nil},
		{name: "slug and number", ref: "acme/lore#42", want: []entities.DocID{pr, issue}},
		{name: "hash and number", ref: "#42", want: []entities.DocID{pr, issue}},
		{name: "bare number", ref: "42", want: []entities.DocID{pr, issue}},
		{name: "ticket key", ref: "PROJ-123", want: []entities.DocID{ticket}},
		{name: "exact url", ref: pageURL, want: []entities.DocID{page}},
		{name: "unknown url", ref: "https://notion.so/unknown", want: nil},
		{name: "full doc id", ref: string(pr), want: []entities.DocID{pr}},
		{name: "prose", ref: "the march outage", want: nil},
		{name: "empty", ref: "", want: nil},
		{name: "whitespace", ref: "  \t ", want: nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.ResolveRef(ctx, tc.ref)
			if err != nil {
				t.Fatalf("ResolveRef(%q): %v", tc.ref, err)
			}
			assertResolved(t, tc.ref, got, tc.want)
		})
	}
}

func TestResolveRefCarriesDocumentMetadata(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	created := time.Date(2025, 4, 8, 11, 0, 0, 0, time.UTC)
	doc := entities.Document{
		ID:        entities.NewDocID("notion", entities.DocTypePage, "design/retrieval"),
		Source:    "notion",
		Type:      entities.DocTypePage,
		Title:     "Retrieval design",
		Body:      "Body text the resolver must not carry.",
		Author:    "architect@example.test",
		URL:       pageURL,
		CreatedAt: created,
		UpdatedAt: created.Add(time.Hour),
	}
	seedDocuments(t, s, []entities.Document{doc})

	got, err := s.ResolveRef(ctx, pageURL)
	if err != nil {
		t.Fatalf("ResolveRef: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ResolveRef returned %+v, want one candidate", got)
	}

	want := entities.DocumentMeta{
		ID:        doc.ID,
		Source:    doc.Source,
		Type:      doc.Type,
		Title:     doc.Title,
		Author:    doc.Author,
		URL:       doc.URL,
		CreatedAt: doc.CreatedAt,
		UpdatedAt: doc.UpdatedAt,
	}
	if got[0] != want {
		t.Errorf("ResolveRef candidate = %+v, want %+v", got[0], want)
	}
}

func seedDocuments(t *testing.T, s *Store, docs []entities.Document) {
	t.Helper()

	if err := s.UpsertDocuments(context.Background(), docs); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}
}

func assertResolved(t *testing.T, ref string, got []entities.DocumentMeta, want []entities.DocID) {
	t.Helper()

	ids := make([]entities.DocID, len(got))
	for i, m := range got {
		ids[i] = m.ID
	}
	if !slices.IsSorted(ids) {
		t.Errorf("ResolveRef(%q) = %v, want candidates in doc id order", ref, ids)
	}

	expected := slices.Clone(want)
	slices.Sort(expected)
	if !slices.Equal(ids, expected) {
		t.Errorf("ResolveRef(%q) = %v, want %v", ref, ids, expected)
	}
}
