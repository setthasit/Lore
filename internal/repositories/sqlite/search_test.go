package sqlite

import (
	"context"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/setthasit/Lore/internal/entities"
)

// One document with one chunk, so a document id names a hit unambiguously.
type corpusEntry struct {
	id        entities.DocID
	source    string
	docType   entities.DocType
	repoRef   string
	created   time.Time
	text      string
	embedding []float32
}

func day(month, d int) time.Time {
	return time.Date(2025, time.Month(month), d, 12, 0, 0, 0, time.UTC)
}

// Shaped for ranking assertions: "sqlite" is in two of the five chunks so BM25's
// IDF stays positive, "lore" is in all five so a filter test sees only the filter
// exclude rows, and the embeddings give every pair a distinct L2 distance.
var searchCorpus = []corpusEntry{{
	id:        entities.NewDocID("github", entities.DocTypeCommit, "abcdef0123456789"),
	source:    "github",
	docType:   entities.DocTypeCommit,
	repoRef:   "github:acme/lore",
	created:   day(1, 10),
	text:      "lore picked sqlite because sqlite ships everywhere and sqlite needs no server",
	embedding: []float32{1, 0, 0},
}, {
	id:        entities.NewDocID("github", entities.DocTypePR, "12"),
	source:    "github",
	docType:   entities.DocTypePR,
	repoRef:   "github:acme/lore",
	created:   day(2, 20),
	text:      "the lore sqlite decision is recorded in an adr",
	embedding: []float32{0, 1, 0},
}, {
	id:        entities.NewDocID("notion", entities.DocTypePage, "design/storage"),
	source:    "notion",
	docType:   entities.DocTypePage,
	repoRef:   "",
	created:   day(3, 30),
	text:      "lore could run on postgres with pgvector instead",
	embedding: []float32{0, 0, 1},
}, {
	id:        entities.NewDocID("github", entities.DocTypeIssue, "7"),
	source:    "github",
	docType:   entities.DocTypeIssue,
	repoRef:   "github:acme/other",
	created:   day(4, 15),
	text:      "lore chunking strategy for very long documents",
	embedding: []float32{1, 1, 0},
}, {
	id:        entities.NewDocID("jira", entities.DocTypeTicket, "PROJ-1"),
	source:    "jira",
	docType:   entities.DocTypeTicket,
	repoRef:   "",
	created:   day(5, 1),
	text:      "lore onboarding notes for new engineers",
	embedding: []float32{0, 1, 1},
}}

func seedSearchCorpus(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()

	for _, e := range searchCorpus {
		doc := entities.Document{
			ID:        e.id,
			Source:    e.source,
			Type:      e.docType,
			RepoRef:   e.repoRef,
			Title:     string(e.id),
			Body:      e.text,
			Author:    "dev@example.test",
			URL:       "https://example.test/" + string(e.id),
			CreatedAt: e.created,
			UpdatedAt: e.created.Add(time.Hour),
		}
		if err := s.UpsertDocuments(ctx, []entities.Document{doc}); err != nil {
			t.Fatalf("seed document %q: %v", e.id, err)
		}
		chunk := entities.Chunk{
			DocID:     e.id,
			Ordinal:   0,
			Text:      e.text,
			Source:    e.source,
			RepoRef:   e.repoRef,
			DocType:   e.docType,
			Author:    doc.Author,
			CreatedAt: e.created,
			UpdatedAt: doc.UpdatedAt,
			ThreadID:  "thread-" + string(e.docType),
			Embedding: e.embedding,
		}
		if err := s.ReplaceChunks(ctx, e.id, []entities.Chunk{chunk}); err != nil {
			t.Fatalf("seed chunk of %q: %v", e.id, err)
		}
	}
}

func hitIDs(hits []entities.ChunkHit) []string {
	ids := make([]string, len(hits))
	for i, h := range hits {
		ids[i] = string(h.DocID)
	}
	return ids
}

func docID(source string, t entities.DocType, external string) string {
	return string(entities.NewDocID(source, t, external))
}

func TestSearchLexicalRanksByRelevance(t *testing.T) {
	s := openTestStore(t)
	seedSearchCorpus(t, s)

	hits, err := s.SearchLexical(context.Background(), "sqlite", entities.Filters{}, 10)
	if err != nil {
		t.Fatalf("SearchLexical: %v", err)
	}

	want := []string{
		docID("github", entities.DocTypeCommit, "abcdef0123456789"),
		docID("github", entities.DocTypePR, "12"),
	}
	if got := hitIDs(hits); !slices.Equal(got, want) {
		t.Fatalf("hits = %v, want %v (three mentions before one, non-matching chunks absent)", got, want)
	}

	if hits[0].Score <= hits[1].Score {
		t.Errorf("scores = %v, %v; want the first hit to score higher", hits[0].Score, hits[1].Score)
	}
	if hits[1].Score <= 0 {
		t.Errorf("score = %v, want a positive relevance", hits[1].Score)
	}

	top := hits[0]
	e := searchCorpus[0]
	if top.Text != e.text || top.Ordinal != 0 || top.Source != e.source ||
		top.RepoRef != e.repoRef || top.DocType != e.docType {
		t.Errorf("hit metadata = %+v, want the seeded chunk", top.Chunk)
	}
	if top.Author != "dev@example.test" || top.ThreadID != "thread-commit" {
		t.Errorf("author = %q, thread = %q, want the seeded values", top.Author, top.ThreadID)
	}
	if !top.CreatedAt.Equal(e.created) || !top.UpdatedAt.Equal(e.created.Add(time.Hour)) {
		t.Errorf("timestamps = %v / %v, want %v / %v",
			top.CreatedAt, top.UpdatedAt, e.created, e.created.Add(time.Hour))
	}
	if top.Embedding != nil {
		t.Errorf("hit carries an embedding of %d dimensions, want none", len(top.Embedding))
	}

	hits, err = s.SearchLexical(context.Background(), "sqlite", entities.Filters{}, 1)
	if err != nil {
		t.Fatalf("SearchLexical (k=1): %v", err)
	}
	if got := hitIDs(hits); !slices.Equal(got, want[:1]) {
		t.Errorf("k=1 hits = %v, want %v", got, want[:1])
	}
}

func TestSearchLexicalAcceptsAnyUserText(t *testing.T) {
	s := openTestStore(t)
	seedSearchCorpus(t, s)
	ctx := context.Background()

	// Operator words, wildcards and unbalanced quotes are terms, not syntax.
	questions := []string{
		`Why did we pick "SQLite" AND NOT postgres? (see ADR-3)`,
		`sqlite OR`,
		`"unbalanced quote about sqlite`,
		`NEAR(sqlite postgres, 2) ^ * -- ;DROP`,
		`sqlite*`,
		`postgres NOT lore`,
	}
	for _, q := range questions {
		hits, err := s.SearchLexical(ctx, q, entities.Filters{}, 10)
		if err != nil {
			t.Fatalf("SearchLexical(%q): %v", q, err)
		}
		if len(hits) == 0 {
			t.Errorf("SearchLexical(%q) found nothing; every question mentions an indexed word", q)
		}
	}

	hits, err := s.SearchLexical(ctx, `Why did we pick "SQLite" AND NOT postgres? (see ADR-3)`, entities.Filters{}, 10)
	if err != nil {
		t.Fatalf("SearchLexical: %v", err)
	}
	got := hitIDs(hits)
	for _, want := range []string{
		docID("github", entities.DocTypeCommit, "abcdef0123456789"),
		docID("notion", entities.DocTypePage, "design/storage"),
	} {
		if !slices.Contains(got, want) {
			t.Errorf("hits = %v, want %q among them", got, want)
		}
	}

	hits, err = s.SearchLexical(ctx, "?!! *** ...", entities.Filters{}, 10)
	if err != nil {
		t.Fatalf("SearchLexical (punctuation only): %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("hits = %v, want none", hitIDs(hits))
	}
}

func TestSearchVectorRanksByDistance(t *testing.T) {
	s := openTestStore(t)
	seedSearchCorpus(t, s)

	// Distances from {0.9, 0.1, 0}: 0.1414, 0.9055, 1.2728, 1.3491, 1.6186.
	hits, err := s.SearchVector(context.Background(), []float32{0.9, 0.1, 0}, entities.Filters{}, 5)
	if err != nil {
		t.Fatalf("SearchVector: %v", err)
	}

	want := []string{
		docID("github", entities.DocTypeCommit, "abcdef0123456789"),
		docID("github", entities.DocTypeIssue, "7"),
		docID("github", entities.DocTypePR, "12"),
		docID("notion", entities.DocTypePage, "design/storage"),
		docID("jira", entities.DocTypeTicket, "PROJ-1"),
	}
	if got := hitIDs(hits); !slices.Equal(got, want) {
		t.Fatalf("hits = %v, want %v (nearest first)", got, want)
	}

	if got, exp := float64(hits[0].Score), -math.Sqrt(0.01+0.01); math.Abs(got-exp) > 1e-5 {
		t.Errorf("top score = %v, want %v", got, exp)
	}
	for i := 1; i < len(hits); i++ {
		if hits[i].Score >= hits[i-1].Score {
			t.Errorf("score %d = %v is not below %v", i, hits[i].Score, hits[i-1].Score)
		}
	}
	if hits[0].Embedding != nil {
		t.Error("hit carries an embedding, want none")
	}

	hits, err = s.SearchVector(context.Background(), []float32{0, 0, 1}, entities.Filters{}, 1)
	if err != nil {
		t.Fatalf("SearchVector (exact): %v", err)
	}
	if len(hits) != 1 || hits[0].Score != 0 {
		t.Errorf("exact-match hits = %+v, want one hit scoring 0", hitIDs(hits))
	}
}

// The query matches the whole corpus, so anything missing was excluded by the
// filter.
var filterCases = []struct {
	name   string
	filter entities.Filters
	want   []string
}{{
	name:   "unfiltered",
	filter: entities.Filters{},
	want: []string{
		docID("github", entities.DocTypeCommit, "abcdef0123456789"),
		docID("github", entities.DocTypeIssue, "7"),
		docID("github", entities.DocTypePR, "12"),
		docID("jira", entities.DocTypeTicket, "PROJ-1"),
		docID("notion", entities.DocTypePage, "design/storage"),
	},
}, {
	name:   "source",
	filter: entities.Filters{Source: "notion"},
	want:   []string{docID("notion", entities.DocTypePage, "design/storage")},
}, {
	name:   "repo_ref",
	filter: entities.Filters{RepoRef: "github:acme/lore"},
	want: []string{
		docID("github", entities.DocTypeCommit, "abcdef0123456789"),
		docID("github", entities.DocTypePR, "12"),
	},
}, {
	name:   "doc_type",
	filter: entities.Filters{DocType: entities.DocTypeIssue},
	want:   []string{docID("github", entities.DocTypeIssue, "7")},
}, {
	name:   "created_from",
	filter: entities.Filters{CreatedFrom: day(3, 30)}, // inclusive: the notion page is exactly here
	want: []string{
		docID("github", entities.DocTypeIssue, "7"),
		docID("jira", entities.DocTypeTicket, "PROJ-1"),
		docID("notion", entities.DocTypePage, "design/storage"),
	},
}, {
	name:   "created_to",
	filter: entities.Filters{CreatedTo: day(2, 20)}, // inclusive: the PR is exactly here
	want: []string{
		docID("github", entities.DocTypeCommit, "abcdef0123456789"),
		docID("github", entities.DocTypePR, "12"),
	},
}, {
	name:   "created_range",
	filter: entities.Filters{CreatedFrom: day(2, 1), CreatedTo: day(4, 1)},
	want: []string{
		docID("github", entities.DocTypePR, "12"),
		docID("notion", entities.DocTypePage, "design/storage"),
	},
}, {
	name: "every dimension at once",
	filter: entities.Filters{
		Source:      "github",
		RepoRef:     "github:acme/lore",
		DocType:     entities.DocTypePR,
		CreatedFrom: day(1, 1),
		CreatedTo:   day(3, 1),
	},
	want: []string{docID("github", entities.DocTypePR, "12")},
}, {
	name:   "contradictory filter excludes everything",
	filter: entities.Filters{Source: "notion", DocType: entities.DocTypeCommit},
	want:   nil,
}}

func TestSearchFiltersPushDown(t *testing.T) {
	s := openTestStore(t)
	seedSearchCorpus(t, s)
	ctx := context.Background()

	for _, c := range filterCases {
		t.Run("lexical/"+c.name, func(t *testing.T) {
			hits, err := s.SearchLexical(ctx, "lore", c.filter, 10)
			if err != nil {
				t.Fatalf("SearchLexical: %v", err)
			}
			assertHitSet(t, hits, c.want)
		})

		t.Run("vector/"+c.name, func(t *testing.T) {
			hits, err := s.SearchVector(ctx, []float32{0.5, 0.5, 0.5}, c.filter, 10)
			if err != nil {
				t.Fatalf("SearchVector: %v", err)
			}
			assertHitSet(t, hits, c.want)
		})
	}
}

// A filtered vector search must return the k best chunks *of the filtered set*:
// if the filter were applied after the KNN, asking for one hit would spend it on
// the nearest chunk and then throw it away, returning nothing.
func TestSearchVectorFilterAppliesBeforeK(t *testing.T) {
	s := openTestStore(t)
	seedSearchCorpus(t, s)

	nearest := []float32{1, 0, 0} // the github commit chunk, exactly
	hits, err := s.SearchVector(context.Background(), nearest, entities.Filters{Source: "notion"}, 1)
	if err != nil {
		t.Fatalf("SearchVector: %v", err)
	}
	want := []string{docID("notion", entities.DocTypePage, "design/storage")}
	if got := hitIDs(hits); !slices.Equal(got, want) {
		t.Errorf("hits = %v, want %v", got, want)
	}
}

func TestSearchRejectsBadArguments(t *testing.T) {
	s := openTestStore(t)
	seedSearchCorpus(t, s)
	ctx := context.Background()

	if _, err := s.SearchLexical(ctx, "sqlite", entities.Filters{}, 0); err == nil {
		t.Error("SearchLexical accepted k=0")
	}
	if _, err := s.SearchVector(ctx, []float32{1, 0, 0}, entities.Filters{}, -1); err == nil {
		t.Error("SearchVector accepted k=-1")
	}
	if _, err := s.SearchVector(ctx, []float32{1, 0}, entities.Filters{}, 5); err == nil {
		t.Error("SearchVector accepted a 2-dimension query in a 3-dimension store")
	}
	if _, err := s.SearchVector(ctx, nil, entities.Filters{}, 5); err == nil {
		t.Error("SearchVector accepted an empty query vector")
	}
}

// A chunk indexed without an embedding is lexically retrievable and invisible to
// vector search.
func TestSearchVectorSkipsUnembeddedChunks(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	id := entities.NewDocID("notion", entities.DocTypePage, "unembedded")
	created := day(6, 1)
	if err := s.UpsertDocuments(ctx, []entities.Document{{
		ID: id, Source: "notion", Type: entities.DocTypePage,
		CreatedAt: created, UpdatedAt: created,
	}}); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}
	if err := s.ReplaceChunks(ctx, id, []entities.Chunk{{
		DocID: id, Text: "vectorless prose about lore", Source: "notion",
		DocType: entities.DocTypePage, CreatedAt: created, UpdatedAt: created,
	}}); err != nil {
		t.Fatalf("ReplaceChunks: %v", err)
	}

	hits, err := s.SearchLexical(ctx, "vectorless", entities.Filters{}, 5)
	if err != nil {
		t.Fatalf("SearchLexical: %v", err)
	}
	if got := hitIDs(hits); !slices.Equal(got, []string{string(id)}) {
		t.Errorf("lexical hits = %v, want the unembedded chunk", got)
	}

	hits, err = s.SearchVector(ctx, []float32{1, 1, 1}, entities.Filters{}, 5)
	if err != nil {
		t.Fatalf("SearchVector: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("vector hits = %v, want none", hitIDs(hits))
	}
}

func assertHitSet(t *testing.T, hits []entities.ChunkHit, want []string) {
	t.Helper()

	got := hitIDs(hits)
	slices.Sort(got)
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !slices.Equal(got, want) {
		t.Errorf("hits = %v, want %v", got, want)
	}
}
