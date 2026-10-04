package sqlite

import (
	"context"
	"errors"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/setthasit/Lore/internal/entities"
	"github.com/setthasit/Lore/sdk"
)

// One document with one chunk, so a document id names a hit unambiguously.
type corpusEntry struct {
	id        lore.DocID
	source    string
	docType   lore.DocType
	repoRef   string
	created   time.Time
	text      string
	embedding []float32
}

func day(month, d int) time.Time {
	return time.Date(2025, time.Month(month), d, 12, 0, 0, 0, time.UTC)
}

// "sqlite" is in two of the five chunks so BM25's IDF stays positive, "lore" is in all five,
// and the embeddings give every pair a distinct L2 distance.
var searchCorpus = []corpusEntry{{
	id:        lore.NewDocID("github", lore.DocTypeCommit, "abcdef0123456789"),
	source:    "github",
	docType:   lore.DocTypeCommit,
	repoRef:   "github:acme/lore",
	created:   day(1, 10),
	text:      "lore picked sqlite because sqlite ships everywhere and sqlite needs no server",
	embedding: []float32{1, 0, 0},
}, {
	id:        lore.NewDocID("github", lore.DocTypePR, "12"),
	source:    "github",
	docType:   lore.DocTypePR,
	repoRef:   "github:acme/lore",
	created:   day(2, 20),
	text:      "the lore sqlite decision is recorded in an adr",
	embedding: []float32{0, 1, 0},
}, {
	id:        lore.NewDocID("notion", lore.DocTypePage, "design/storage"),
	source:    "notion",
	docType:   lore.DocTypePage,
	repoRef:   "",
	created:   day(3, 30),
	text:      "lore could run on postgres with pgvector instead",
	embedding: []float32{0, 0, 1},
}, {
	id:        lore.NewDocID("github", lore.DocTypeIssue, "7"),
	source:    "github",
	docType:   lore.DocTypeIssue,
	repoRef:   "github:acme/other",
	created:   day(4, 15),
	text:      "lore chunking strategy for very long documents",
	embedding: []float32{1, 1, 0},
}, {
	id:        lore.NewDocID("jira", lore.DocTypeTicket, "PROJ-1"),
	source:    "jira",
	docType:   lore.DocTypeTicket,
	repoRef:   "",
	created:   day(5, 1),
	text:      "lore onboarding notes for new engineers",
	embedding: []float32{0, 1, 1},
}}

func seedSearchCorpus(t *testing.T, s *Store) {
	t.Helper()
	seedCorpus(t, s, searchCorpus)
}

func seedCorpus(t *testing.T, s *Store, entries []corpusEntry) {
	t.Helper()
	ctx := context.Background()

	for _, e := range entries {
		doc := lore.Document{
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
		if err := s.UpsertDocuments(ctx, []lore.Document{doc}); err != nil {
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

type passage struct {
	text      string
	embedding []float32
}

func seedPage(t *testing.T, s *Store, external string, passages []passage) lore.DocID {
	t.Helper()

	id := lore.NewDocID("notion", lore.DocTypePage, external)
	created := day(6, 1)
	seedDocuments(t, s, []lore.Document{{
		ID: id, Source: "notion", Type: lore.DocTypePage,
		CreatedAt: created, UpdatedAt: created,
	}})

	chunks := make([]entities.Chunk, len(passages))
	for i, p := range passages {
		chunks[i] = entities.Chunk{
			DocID: id, Ordinal: i, Text: p.text, Source: "notion",
			DocType: lore.DocTypePage, CreatedAt: created, UpdatedAt: created,
			Embedding: p.embedding,
		}
	}
	if err := s.ReplaceChunks(context.Background(), id, chunks); err != nil {
		t.Fatalf("ReplaceChunks(%q): %v", id, err)
	}
	return id
}

func hitIDs(hits []entities.ChunkHit) []string {
	ids := make([]string, len(hits))
	for i, h := range hits {
		ids[i] = string(h.DocID)
	}
	return ids
}

func docID(source string, t lore.DocType, external string) string {
	return string(lore.NewDocID(source, t, external))
}

func TestSearchLexicalRanksByRelevance(t *testing.T) {
	s := openTestStore(t)
	seedSearchCorpus(t, s)

	hits, err := s.SearchLexical(context.Background(), "sqlite", entities.Filters{}, 10)
	if err != nil {
		t.Fatalf("SearchLexical: %v", err)
	}

	want := []string{
		docID("github", lore.DocTypeCommit, "abcdef0123456789"),
		docID("github", lore.DocTypePR, "12"),
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

func TestSearchLexicalMatchesADottedNumberAsOnePhrase(t *testing.T) {
	s := openTestStore(t)
	seedSearchCorpus(t, s)
	section15 := corpusEntry{
		id:      lore.NewDocID("notion", lore.DocTypePage, "spec/15.5"),
		source:  "notion",
		docType: lore.DocTypePage,
		created: day(6, 1),
		text:    "15.5 Time and fast forward: advancing the clock is a single process step",
	}
	section16 := corpusEntry{
		id:      lore.NewDocID("notion", lore.DocTypePage, "spec/16.5"),
		source:  "notion",
		docType: lore.DocTypePage,
		created: day(6, 2),
		text: "16.5 Content packs: pack 5 is one process step and pack 15 is another process step; " +
			"installing pack 5 then pack 15 repeats that process step",
	}
	seedCorpus(t, s, []corpusEntry{section15, section16})

	hits, err := s.SearchLexical(context.Background(), "section 15.5 process step", entities.Filters{}, 10)
	if err != nil {
		t.Fatalf("SearchLexical: %v", err)
	}

	want := []string{string(section15.id), string(section16.id)}
	if got := hitIDs(hits); !slices.Equal(got, want) {
		t.Errorf("hits = %v, want %v (the 15.5 section above one that only mentions 15 and 5)", got, want)
	}
}

func TestSearchLexicalMatchesACompoundIdentifierAsOnePhrase(t *testing.T) {
	s := openTestStore(t)
	seedSearchCorpus(t, s)
	compound := corpusEntry{
		id:      lore.NewDocID("github", lore.DocTypeCommit, "fastforward0001"),
		source:  "github",
		docType: lore.DocTypeCommit,
		created: day(6, 1),
		text:    "the fast_forward option moves the branch pointer without a merge commit",
	}
	parts := corpusEntry{
		id:      lore.NewDocID("github", lore.DocTypeIssue, "99"),
		source:  "github",
		docType: lore.DocTypeIssue,
		created: day(6, 2),
		text: "Builds must stay fast. We forward failures to on-call. A fast test suite helps. " +
			"Please forward flaky runs. Fast reviews matter. Forward the summary weekly.",
	}
	seedCorpus(t, s, []corpusEntry{compound, parts})

	hits, err := s.SearchLexical(context.Background(), "explain fast_forward", entities.Filters{}, 10)
	if err != nil {
		t.Fatalf("SearchLexical: %v", err)
	}

	want := []string{string(compound.id)}
	if got := hitIDs(hits); !slices.Equal(got, want) {
		t.Errorf("hits = %v, want %v (not the chunk that only mentions fast and forward apart)", got, want)
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
		docID("github", lore.DocTypeCommit, "abcdef0123456789"),
		docID("notion", lore.DocTypePage, "design/storage"),
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

	if _, err := s.SearchLexical(ctx, `"§15.5" AND NOT * NEAR(`, entities.Filters{}, 10); err != nil {
		t.Errorf("SearchLexical (quoted section and operators): %v", err)
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
		docID("github", lore.DocTypeCommit, "abcdef0123456789"),
		docID("github", lore.DocTypeIssue, "7"),
		docID("github", lore.DocTypePR, "12"),
		docID("notion", lore.DocTypePage, "design/storage"),
		docID("jira", lore.DocTypeTicket, "PROJ-1"),
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

// The query matches the whole corpus, so anything missing was excluded by the filter.
var filterCases = []struct {
	name   string
	filter entities.Filters
	want   []string
}{{
	name:   "unfiltered",
	filter: entities.Filters{},
	want: []string{
		docID("github", lore.DocTypeCommit, "abcdef0123456789"),
		docID("github", lore.DocTypeIssue, "7"),
		docID("github", lore.DocTypePR, "12"),
		docID("jira", lore.DocTypeTicket, "PROJ-1"),
		docID("notion", lore.DocTypePage, "design/storage"),
	},
}, {
	name:   "source",
	filter: entities.Filters{Source: "notion"},
	want:   []string{docID("notion", lore.DocTypePage, "design/storage")},
}, {
	name:   "repo_ref",
	filter: entities.Filters{RepoRef: "github:acme/lore"},
	want: []string{
		docID("github", lore.DocTypeCommit, "abcdef0123456789"),
		docID("github", lore.DocTypePR, "12"),
	},
}, {
	name:   "doc_type",
	filter: entities.Filters{DocType: lore.DocTypeIssue},
	want:   []string{docID("github", lore.DocTypeIssue, "7")},
}, {
	name:   "created_from",
	filter: entities.Filters{CreatedFrom: day(3, 30)}, // inclusive: the notion page is exactly here
	want: []string{
		docID("github", lore.DocTypeIssue, "7"),
		docID("jira", lore.DocTypeTicket, "PROJ-1"),
		docID("notion", lore.DocTypePage, "design/storage"),
	},
}, {
	name:   "created_to",
	filter: entities.Filters{CreatedTo: day(2, 20)}, // inclusive: the PR is exactly here
	want: []string{
		docID("github", lore.DocTypeCommit, "abcdef0123456789"),
		docID("github", lore.DocTypePR, "12"),
	},
}, {
	name:   "created_range",
	filter: entities.Filters{CreatedFrom: day(2, 1), CreatedTo: day(4, 1)},
	want: []string{
		docID("github", lore.DocTypePR, "12"),
		docID("notion", lore.DocTypePage, "design/storage"),
	},
}, {
	name: "every dimension at once",
	filter: entities.Filters{
		Source:      "github",
		RepoRef:     "github:acme/lore",
		DocType:     lore.DocTypePR,
		CreatedFrom: day(1, 1),
		CreatedTo:   day(3, 1),
		DocID:       lore.NewDocID("github", lore.DocTypePR, "12"),
	},
	want: []string{docID("github", lore.DocTypePR, "12")},
}, {
	name:   "contradictory filter excludes everything",
	filter: entities.Filters{Source: "notion", DocType: lore.DocTypeCommit},
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

// Applying the filter after the KNN would spend the single hit on the nearest chunk and return nothing.
func TestSearchVectorFilterAppliesBeforeK(t *testing.T) {
	s := openTestStore(t)
	seedSearchCorpus(t, s)

	nearest := []float32{1, 0, 0} // the github commit chunk, exactly
	hits, err := s.SearchVector(context.Background(), nearest, entities.Filters{Source: "notion"}, 1)
	if err != nil {
		t.Fatalf("SearchVector: %v", err)
	}
	want := []string{docID("notion", lore.DocTypePage, "design/storage")}
	if got := hitIDs(hits); !slices.Equal(got, want) {
		t.Errorf("hits = %v, want %v", got, want)
	}
}

func TestSearchKeepsOnlyTheFilteredDocument(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	target := seedPage(t, s, "runbook/deploy", []passage{{
		text:      "a deploy rollback starts from the previous release tag",
		embedding: []float32{0, 1, 0},
	}, {
		text:      "the on-call engineer announces every rollback in the incident channel",
		embedding: []float32{0, 0, 1},
	}, {
		text:      "after a rollback the canary stays paused until the next review",
		embedding: []float32{0, 1, 1},
	}})
	rival := seedPage(t, s, "runbook/database", []passage{{
		text:      "a schema rollback is a rollback of one migration, and that rollback runs in a transaction",
		embedding: []float32{1, 0, 0},
	}, {
		text:      "a data rollback is a rollback from a snapshot, and that rollback needs a maintenance window",
		embedding: []float32{0.9, 0.1, 0},
	}, {
		text:      "an index rollback is a rollback of one build, and that rollback drops the new index",
		embedding: []float32{0.8, 0.2, 0},
	}})

	const k = 2
	arms := []struct {
		name   string
		search func(entities.Filters) ([]entities.ChunkHit, error)
	}{{
		name: "lexical",
		search: func(f entities.Filters) ([]entities.ChunkHit, error) {
			return s.SearchLexical(ctx, "rollback", f, k)
		},
	}, {
		name: "vector",
		search: func(f entities.Filters) ([]entities.ChunkHit, error) {
			return s.SearchVector(ctx, []float32{1, 0, 0}, f, k)
		},
	}}

	for _, arm := range arms {
		t.Run(arm.name, func(t *testing.T) {
			hits, err := arm.search(entities.Filters{})
			if err != nil {
				t.Fatalf("unfiltered search: %v", err)
			}
			want := []string{string(rival), string(rival)}
			if got := hitIDs(hits); !slices.Equal(got, want) {
				t.Fatalf("unfiltered hits = %v, want %v (the rival page ranks first)", got, want)
			}

			hits, err = arm.search(entities.Filters{DocID: target})
			if err != nil {
				t.Fatalf("filtered search: %v", err)
			}
			want = []string{string(target), string(target)}
			if got := hitIDs(hits); !slices.Equal(got, want) {
				t.Fatalf("filtered hits = %v, want %v", got, want)
			}
			if hits[0].Ordinal == hits[1].Ordinal {
				t.Errorf("both filtered hits are chunk %d, want two different chunks", hits[0].Ordinal)
			}
		})
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

func TestSearchLexicalErrorOmitsCallerText(t *testing.T) {
	const (
		callerText    = "callertypedthis"
		callerSource  = "sourcefromcaller"
		callerRepoRef = "owner/repofromcaller"
		query         = "sqlite " + callerText
	)
	filters := entities.Filters{Source: callerSource, RepoRef: callerRepoRef}

	t.Run("the statement fails", func(t *testing.T) {
		s := openTestStore(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		_, err := s.SearchLexical(ctx, query, filters, 10)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want one wrapping the cancellation", err)
		}
		assertErrorOmits(t, err, callerText, callerSource, callerRepoRef)
	})

	t.Run("a matching row is unreadable", func(t *testing.T) {
		s := openTestStore(t)
		seedSearchCorpus(t, s)
		ctx := context.Background()
		_, err := s.db.ExecContext(ctx,
			`UPDATE chunks SET created_at = 'not a timestamp', source = ?, repo_ref = ?`,
			callerSource, callerRepoRef)
		if err != nil {
			t.Fatalf("corrupt the chunks the filters match: %v", err)
		}

		_, err = s.SearchLexical(ctx, query, filters, 10)
		var parseErr *time.ParseError
		if !errors.As(err, &parseErr) {
			t.Fatalf("error = %v, want one wrapping the timestamp parse failure", err)
		}
		assertErrorOmits(t, err, callerText, callerSource, callerRepoRef)
	})
}

func assertErrorOmits(t *testing.T, err error, callerTexts ...string) {
	t.Helper()

	for _, callerText := range callerTexts {
		if strings.Contains(err.Error(), callerText) {
			t.Errorf("error %q carries the caller text %q", err, callerText)
		}
	}
}

func TestSearchVectorSkipsUnembeddedChunks(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	id := seedPage(t, s, "unembedded", []passage{{text: "vectorless prose about lore"}})

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
