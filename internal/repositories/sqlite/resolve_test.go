package sqlite

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/setthasit/Lore/internal/entities"
	"github.com/setthasit/Lore/sdk"
)

const (
	firstSHA      = "1234567aaaaa000000000000000000000000abcd"
	secondSHA     = "1234567bbbbb000000000000000000000000abcd"
	notionID      = "0123456789abcdef0123456789abcdef"
	pageURL       = "https://notion.so/design/retrieval"
	notionPageID  = "3e7a409f-814e-80a9-8644-fc0217b75f69"
	notionPageURL = "https://app.notion.com/p/Still-Got-It-White-Paper-3e7a409f814e80a98644fc0217b75f69"
)

func TestResolveRef(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	firstCommit := lore.NewDocID("github", lore.DocTypeCommit, "acme/lore/commit/"+firstSHA)
	secondCommit := lore.NewDocID("github", lore.DocTypeCommit, "acme/lore/commit/"+secondSHA)
	pr := lore.NewDocID("github", lore.DocTypePR, "acme/lore/pull/42")
	issue := lore.NewDocID("github", lore.DocTypeIssue, "acme/lore/issues/42")
	ticket := lore.NewDocID("jira", lore.DocTypeTicket, "PROJ-123")
	page := lore.NewDocID("notion", lore.DocTypePage, "design/retrieval")
	hexPage := lore.NewDocID("notion", lore.DocTypePage, notionID)

	seedDocuments(t, s, []lore.Document{
		{ID: firstCommit, Source: "github", Type: lore.DocTypeCommit, Title: "Rework the resolver"},
		{ID: secondCommit, Source: "github", Type: lore.DocTypeCommit, Title: "Revert the resolver"},
		{ID: pr, Source: "github", Type: lore.DocTypePR, Title: "Provenance engine"},
		{ID: issue, Source: "github", Type: lore.DocTypeIssue, Title: "Trace answers to sources"},
		{ID: ticket, Source: "jira", Type: lore.DocTypeTicket, Title: "Ship provenance"},
		{ID: page, Source: "notion", Type: lore.DocTypePage, Title: "Retrieval design", URL: pageURL},
		{ID: hexPage, Source: "notion", Type: lore.DocTypePage, Title: "Notion ids look like SHAs"},
	})

	tests := []struct {
		name string
		ref  string
		want []lore.DocID
	}{
		{name: "full sha names one commit", ref: firstSHA, want: []lore.DocID{firstCommit}},
		{name: "uppercase sha resolves too", ref: strings.ToUpper(firstSHA), want: []lore.DocID{firstCommit}},
		{
			name: "shared abbreviation is ambiguous",
			ref:  firstSHA[:7],
			want: []lore.DocID{firstCommit, secondCommit},
		},
		{name: "hex prefix nobody ingested", ref: "deadbee", want: nil},
		{name: "non-commit is never a sha candidate", ref: notionID, want: nil},
		{name: "slug and number", ref: "acme/lore#42", want: []lore.DocID{pr, issue}},
		{name: "hash and number", ref: "#42", want: []lore.DocID{pr, issue}},
		{name: "bare number", ref: "42", want: []lore.DocID{pr, issue}},
		{name: "ticket key", ref: "PROJ-123", want: []lore.DocID{ticket}},
		{name: "exact url", ref: pageURL, want: []lore.DocID{page}},
		{name: "unknown url", ref: "https://notion.so/unknown", want: nil},
		{name: "full doc id", ref: string(pr), want: []lore.DocID{pr}},
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

func TestResolveRefFindsANotionPageFromAnyURLForm(t *testing.T) {
	s := openTestStore(t)
	page := lore.NewDocID("notion", lore.DocTypePage, notionPageID)
	externalPage := lore.NewDocID("web", lore.DocTypePage, "white-paper")
	externalURL := "https://example.com/white-paper-3e7a409f814e80a98644fc0217b75f69"
	seedDocuments(t, s, []lore.Document{
		{ID: page, Source: "notion", Type: lore.DocTypePage, Title: "Still Got It White Paper", URL: notionPageURL},
		{ID: externalPage, Source: "web", Type: lore.DocTypePage, Title: "External white paper", URL: externalURL},
	})

	tests := []struct {
		name string
		ref  string
		want []lore.DocID
	}{
		{name: "bare page id", ref: "https://www.notion.so/3e7a409f814e80a98644fc0217b75f69", want: []lore.DocID{page}},
		{name: "workspace and slug", ref: "https://notion.so/acme/Still-Got-It-White-Paper-3e7a409f814e80a98644fc0217b75f69", want: []lore.DocID{page}},
		{name: "dashed page id", ref: "https://www.notion.so/3e7a409f-814e-80a9-8644-fc0217b75f69", want: []lore.DocID{page}},
		{name: "title-prefixed dashed page id", ref: "https://www.notion.so/White-Paper-3e7a409f-814e-80a9-8644-fc0217b75f69", want: []lore.DocID{page}},
		{name: "query", ref: "https://www.notion.so/3e7a409f814e80a98644fc0217b75f69?pvs=4", want: []lore.DocID{page}},
		{name: "block fragment", ref: "https://www.notion.so/3e7a409f814e80a98644fc0217b75f69#11111111222233334444555555555555", want: []lore.DocID{page}},
		{name: "published site", ref: "https://acme.notion.site/Still-Got-It-White-Paper-3e7a409f814e80a98644fc0217b75f69", want: []lore.DocID{page}},
		{name: "nested published site", ref: "https://docs.acme.notion.site/3e7a409f814e80a98644fc0217b75f69", want: []lore.DocID{page}},
		{name: "app host without slug", ref: "https://app.notion.com/p/3e7a409f814e80a98644fc0217b75f69", want: []lore.DocID{page}},
		{name: "exact stored url deduplicates", ref: notionPageURL, want: []lore.DocID{page}},
		{name: "http", ref: "http://notion.so/3e7a409f814e80a98644fc0217b75f69", want: []lore.DocID{page}},
		{name: "uppercase scheme", ref: "HTTPS://notion.so/3e7a409f814e80a98644fc0217b75f69", want: []lore.DocID{page}},
		{name: "uppercase host and hex", ref: "https://WWW.NOTION.SO/3E7A409F814E80A98644FC0217B75F69", want: []lore.DocID{page}},
		{name: "uppercase dashed hex", ref: "https://notion.so/3E7A409F-814E-80A9-8644-FC0217B75F69", want: []lore.DocID{page}},
		{name: "trailing empty path segments", ref: "https://notion.so/3e7a409f814e80a98644fc0217b75f69///", want: []lore.DocID{page}},
		{name: "escaped path", ref: "https://notion.so/%33e7a409f814e80a98644fc0217b75f69", want: []lore.DocID{page}},
		{name: "unknown page id", ref: "https://notion.so/11111111222233334444555555555555"},
		{name: "unrelated host with same id", ref: "https://example.com/p/3e7a409f814e80a98644fc0217b75f69"},
		{name: "exact non-notion url", ref: externalURL, want: []lore.DocID{externalPage}},
		{name: "non-notion query is not ignored", ref: externalURL + "?pvs=4"},
		{name: "non-notion fragment is not ignored", ref: externalURL + "#block"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.ResolveRef(context.Background(), tc.ref)
			if err != nil {
				t.Fatalf("ResolveRef(%q): %v", tc.ref, err)
			}
			assertResolved(t, tc.ref, got, tc.want)
		})
	}
}

func TestResolveRefRejectsInvalidNotionPageURLs(t *testing.T) {
	s := openTestStore(t)
	seedDocuments(t, s, []lore.Document{
		{ID: lore.NewDocID("notion", lore.DocTypePage, notionPageID), Source: "notion", Type: lore.DocTypePage, URL: notionPageURL},
	})
	tests := []struct {
		name string
		ref  string
	}{
		{name: "unsupported scheme", ref: "ftp://notion.so/3e7a409f814e80a98644fc0217b75f69"},
		{name: "scheme relative", ref: "//notion.so/3e7a409f814e80a98644fc0217b75f69"},
		{name: "missing host", ref: "https:///3e7a409f814e80a98644fc0217b75f69"},
		{name: "invalid port", ref: "https://notion.so:invalid/3e7a409f814e80a98644fc0217b75f69"},
		{name: "host suffix spoof", ref: "https://notion.so.example.com/3e7a409f814e80a98644fc0217b75f69"},
		{name: "unapproved notion subdomain", ref: "https://acme.notion.so/3e7a409f814e80a98644fc0217b75f69"},
		{name: "unapproved notion com host", ref: "https://notion.com/3e7a409f814e80a98644fc0217b75f69"},
		{name: "published site suffix spoof", ref: "https://acme.notion.site.example.com/3e7a409f814e80a98644fc0217b75f69"},
		{name: "published site without subdomain", ref: "https://notion.site/3e7a409f814e80a98644fc0217b75f69"},
		{name: "published site empty subdomain", ref: "https://.notion.site/3e7a409f814e80a98644fc0217b75f69"},
		{name: "published site missing label separator", ref: "https://acmenotion.site/3e7a409f814e80a98644fc0217b75f69"},
		{name: "host in userinfo", ref: "https://notion.so@example.com/3e7a409f814e80a98644fc0217b75f69"},
		{name: "empty path", ref: "https://notion.so/"},
		{name: "id only in query", ref: "https://notion.so/?page=3e7a409f814e80a98644fc0217b75f69"},
		{name: "id only in fragment", ref: "https://notion.so/#3e7a409f814e80a98644fc0217b75f69"},
		{name: "id not in last segment", ref: "https://notion.so/3e7a409f814e80a98644fc0217b75f69/child"},
		{name: "short id", ref: "https://notion.so/3e7a409f814e80a98644fc0217b75f6"},
		{name: "long id", ref: "https://notion.so/03e7a409f814e80a98644fc0217b75f69"},
		{name: "nonhex id", ref: "https://notion.so/ge7a409f814e80a98644fc0217b75f69"},
		{name: "id with suffix", ref: "https://notion.so/3e7a409f814e80a98644fc0217b75f69-extra"},
		{name: "slug without separator", ref: "https://notion.so/title3e7a409f814e80a98644fc0217b75f69"},
		{name: "dashed uuid without title separator", ref: "https://notion.so/White-Paper3e7a409f-814e-80a9-8644-fc0217b75f69"},
		{name: "dashed uuid with extra hex prefix", ref: "https://notion.so/03e7a409f-814e-80a9-8644-fc0217b75f69"},
		{name: "title-prefixed uuid with misplaced dashes", ref: "https://notion.so/White-Paper-3e7a409-f814e8-0a9-8644-fc0217b75f69"},
		{name: "misplaced uuid dashes", ref: "https://notion.so/3e7a409-f814e8-0a9-8644-fc0217b75f69"},
		{name: "nonhex dashed uuid", ref: "https://notion.so/3e7a409g-814e-80a9-8644-fc0217b75f69"},
		{name: "invalid path escape", ref: "https://notion.so/%zz3e7a409f814e80a98644fc0217b75f69"},
		{name: "encoded delimiter after id", ref: "https://notion.so/3e7a409f814e80a98644fc0217b75f69%2Fchild"},
		{name: "encoded nul", ref: "https://notion.so/3e7a409f814e80a98644fc0217b75f69%00"},
		{name: "sql payload", ref: "https://notion.so/3e7a409f814e80a98644fc0217b75f69%27%20OR%201=1--"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.ResolveRef(context.Background(), tc.ref)
			if err != nil {
				t.Fatalf("ResolveRef(%q): %v", tc.ref, err)
			}
			assertResolved(t, tc.ref, got, nil)
		})
	}
}

func TestResolveRefNotionPageCandidatesPreserveInstancesAndExactMatches(t *testing.T) {
	s := openTestStore(t)
	firstPage := lore.NewDocID("notion-eu", lore.DocTypePage, notionPageID)
	secondPage := lore.NewDocID("notion-us", lore.DocTypePage, notionPageID)
	otherPage := lore.NewDocID("archive", lore.DocTypePage, notionPageID)
	ticket := lore.NewDocID("jira", lore.DocTypeTicket, notionPageID)
	exact := lore.NewDocID("web", lore.DocTypeIssue, "exact-url")
	ref := "https://www.notion.so/3e7a409f814e80a98644fc0217b75f69"
	seedDocuments(t, s, []lore.Document{
		{ID: firstPage, Source: "notion-eu", Type: lore.DocTypePage, URL: notionPageURL},
		{ID: secondPage, Source: "notion-us", Type: lore.DocTypePage},
		{ID: otherPage, Source: "archive", Type: lore.DocTypePage},
		{ID: ticket, Source: "jira", Type: lore.DocTypeTicket},
		{ID: exact, Source: "web", Type: lore.DocTypeIssue, URL: ref},
	})
	got, err := s.ResolveRef(context.Background(), ref)
	if err != nil {
		t.Fatalf("ResolveRef(%q): %v", ref, err)
	}
	assertResolved(t, ref, got, []lore.DocID{firstPage, secondPage, otherPage, exact})
}

func TestResolveRefCarriesDocumentMetadata(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	created := time.Date(2025, 4, 8, 11, 0, 0, 0, time.UTC)
	doc := lore.Document{
		ID:        lore.NewDocID("notion", lore.DocTypePage, "design/retrieval"),
		Source:    "notion",
		Type:      lore.DocTypePage,
		Title:     "Retrieval design",
		Body:      "Body text the resolver must not carry.",
		Author:    "architect@example.test",
		URL:       pageURL,
		CreatedAt: created,
		UpdatedAt: created.Add(time.Hour),
	}
	seedDocuments(t, s, []lore.Document{doc})

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

func seedDocuments(t *testing.T, s *Store, docs []lore.Document) {
	t.Helper()

	if err := s.UpsertDocuments(context.Background(), docs); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}
}

func assertResolved(t *testing.T, ref string, got []entities.DocumentMeta, want []lore.DocID) {
	t.Helper()

	ids := make([]lore.DocID, len(got))
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
