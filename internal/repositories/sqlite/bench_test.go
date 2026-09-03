package sqlite

import (
	"context"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/setthasit/Lore/internal/entities"
)

const (
	// Width of a small local embedding model, so the KNN scan is realistic.
	benchDims = 128

	benchChunksPerDoc = 10
	benchChunkWords   = 60
	benchRarePerChunk = 3
	benchTitleWords   = 8

	// One batch is one document transaction plus benchIngestBatch chunk
	// transactions, because ReplaceChunks is per document by contract.
	benchIngestBatch = 100

	// 1,000 documents, so ~10,000 chunks and ~10,000 vectors.
	benchSearchDocs = 1000

	benchK = 12

	// Rotated so a benchmark reports the cost of searching rather than the cost
	// of SQLite's page cache holding one query's postings list.
	benchQueryCount = 16

	// Fixes every word choice and vector component: with a per-document
	// generator, document i is identical in every benchmark and on every run.
	benchSeed = 20250827
)

var benchEpoch = time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

// The high-frequency half of the vocabulary, which is what gives BM25 the long
// postings lists a natural-language query actually pays for.
var benchCommonWords = strings.Fields(`
	the a an of to and or in for on with from into that this when why where
	service store index query chunk document embedding vector search request
	response handler retry timeout context error commit review issue ticket
	page thread comment schema migration transaction connection pool cache
	latency deploy rollback config token limit worker queue batch flush`)

// Disjoint from benchCommonWords: a stem in both halves would stop being
// selective.
var benchRareStems = strings.Fields(`
	auth gate shard lease quorum proxy spool ledger vault beacon cursor digest
	entropy fabric girder harbor jitter lattice mantle nexus oracle parcel
	quarry raster sentinel tundra umbra warden zenith anvil bramble citadel
	delta ember fjord glyph hollow ivory jasper kiln lumen marrow nimbus
	obsidian plume`)

// Every ordered pair of stems, so a token lands in a handful of the ten thousand
// chunks — as selective as a real identifier or symbol name.
var benchRareWords = func() []string {
	words := make([]string, 0, len(benchRareStems)*len(benchRareStems))
	for _, head := range benchRareStems {
		for _, tail := range benchRareStems {
			words = append(words, head+tail)
		}
	}
	return words
}()

var benchQueryTemplates = []string{
	"why did the %s %s change",
	"who reviewed the %s %s migration",
	"where is the %s %s configured",
	"when did the %s %s start failing",
}

var benchDocKinds = []struct {
	source  string
	docType entities.DocType
	repoRef string
}{
	{"github", entities.DocTypeCommit, "github:acme/lore"},
	{"github", entities.DocTypePR, "github:acme/lore"},
	{"github", entities.DocTypeIssue, "github:acme/other"},
	{"notion", entities.DocTypePage, ""},
	{"jira", entities.DocTypeTicket, ""},
}

var benchAuthors = []string{"dev@example.test", "reviewer@example.test", "ops@example.test"}

func openBenchStore(b *testing.B) *Store {
	b.Helper()

	s, err := Open(filepath.Join(b.TempDir(), "workspace.db"), benchDims)
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	b.Cleanup(func() {
		if err := s.Close(); err != nil {
			b.Errorf("Close: %v", err)
		}
	})
	return s
}

// The generator is seeded per document, so index i means the same bytes whatever
// order or iteration count the benchmark loop settles on.
func benchDoc(i int) (entities.Document, []entities.Chunk) {
	rng := rand.New(rand.NewPCG(benchSeed, uint64(i)))
	kind := benchDocKinds[i%len(benchDocKinds)]
	id := entities.NewDocID(kind.source, kind.docType, strconv.Itoa(i))
	created := benchEpoch.Add(time.Duration(i) * time.Hour)
	author := benchAuthors[i%len(benchAuthors)]

	chunks := make([]entities.Chunk, benchChunksPerDoc)
	var body strings.Builder
	for j := range chunks {
		text := benchChunkText(rng)
		body.WriteString(text)
		body.WriteByte('\n')
		chunks[j] = entities.Chunk{
			DocID:     id,
			Ordinal:   j,
			Text:      text,
			Source:    kind.source,
			RepoRef:   kind.repoRef,
			DocType:   kind.docType,
			Author:    author,
			CreatedAt: created,
			UpdatedAt: created.Add(time.Hour),
			Embedding: benchVector(rng),
		}
	}

	doc := entities.Document{
		ID:        id,
		Source:    kind.source,
		Type:      kind.docType,
		RepoRef:   kind.repoRef,
		Title:     benchTitle(chunks[0].Text),
		Body:      body.String(),
		Author:    author,
		URL:       "https://example.test/" + string(id),
		CreatedAt: created,
		UpdatedAt: created.Add(time.Hour),
	}
	return doc, chunks
}

func benchChunkText(rng *rand.Rand) string {
	words := make([]string, benchChunkWords)
	for i := range words {
		words[i] = benchCommonWords[rng.IntN(len(benchCommonWords))]
	}
	for range benchRarePerChunk {
		words[rng.IntN(len(words))] = benchRareWords[rng.IntN(len(benchRareWords))]
	}
	return strings.Join(words, " ")
}

func benchTitle(text string) string {
	words := strings.SplitN(text, " ", benchTitleWords+1)
	return strings.Join(words[:benchTitleWords], " ")
}

// Uniform noise is fine: vec0's KNN is a brute-force scan whose cost depends on
// how many vectors there are and how wide they are, not on what they hold.
func benchVector(rng *rand.Rand) []float32 {
	v := make([]float32, benchDims)
	for i := range v {
		v[i] = rng.Float32()*2 - 1
	}
	return v
}

func benchBatch(start, n int) ([]entities.Document, [][]entities.Chunk) {
	docs := make([]entities.Document, n)
	chunks := make([][]entities.Chunk, n)
	for i := range n {
		docs[i], chunks[i] = benchDoc(start + i)
	}
	return docs, chunks
}

func ingestBatch(b *testing.B, s *Store, docs []entities.Document, chunks [][]entities.Chunk) {
	b.Helper()
	ctx := context.Background()

	if err := s.UpsertDocuments(ctx, docs); err != nil {
		b.Fatalf("UpsertDocuments: %v", err)
	}
	for i := range docs {
		if err := s.ReplaceChunks(ctx, docs[i].ID, chunks[i]); err != nil {
			b.Fatalf("ReplaceChunks %q: %v", docs[i].ID, err)
		}
	}
}

// b.Loop resets the timer when the loop starts, so this setup is not counted.
func seedBenchStore(b *testing.B) *Store {
	b.Helper()

	s := openBenchStore(b)
	for start := 0; start < benchSearchDocs; start += benchIngestBatch {
		docs, chunks := benchBatch(start, benchIngestBatch)
		ingestBatch(b, s, docs, chunks)
	}
	return s
}

// Built from the same vocabulary as the corpus, so every query hits.
func benchQueries() []string {
	rng := rand.New(rand.NewPCG(benchSeed, benchSearchDocs))
	queries := make([]string, benchQueryCount)
	for i := range queries {
		queries[i] = fmt.Sprintf(benchQueryTemplates[i%len(benchQueryTemplates)],
			benchCommonWords[rng.IntN(len(benchCommonWords))],
			benchRareWords[rng.IntN(len(benchRareWords))])
	}
	return queries
}

func benchQueryVectors() [][]float32 {
	rng := rand.New(rand.NewPCG(benchSeed, benchSearchDocs))
	vectors := make([][]float32, benchQueryCount)
	for i := range vectors {
		vectors[i] = benchVector(rng)
	}
	return vectors
}

// ns/op is per batch; ns/doc and ns/chunk are what a connector's throughput is
// read off.
func BenchmarkUpsertAndChunk(b *testing.B) {
	s := openBenchStore(b)

	batches := 0
	for b.Loop() {
		// Fresh ids each iteration: reusing them would measure in-place updates.
		b.StopTimer()
		docs, chunks := benchBatch(batches*benchIngestBatch, benchIngestBatch)
		batches++
		b.StartTimer()

		ingestBatch(b, s, docs, chunks)
	}

	docs := float64(batches * benchIngestBatch)
	elapsed := float64(b.Elapsed().Nanoseconds())
	b.ReportMetric(elapsed/docs, "ns/doc")
	b.ReportMetric(elapsed/(docs*benchChunksPerDoc), "ns/chunk")
}

func BenchmarkSearchLexical(b *testing.B) {
	s := seedBenchStore(b)
	ctx := context.Background()
	queries := benchQueries()

	i := 0
	for b.Loop() {
		query := queries[i%len(queries)]
		i++

		hits, err := s.SearchLexical(ctx, query, entities.Filters{}, benchK)
		if err != nil {
			b.Fatalf("SearchLexical: %v", err)
		}
		if len(hits) != benchK {
			b.Fatalf("SearchLexical(%q) = %d hits, want %d", query, len(hits), benchK)
		}
	}
}

func BenchmarkSearchVector(b *testing.B) {
	s := seedBenchStore(b)
	ctx := context.Background()
	vectors := benchQueryVectors()

	i := 0
	for b.Loop() {
		vector := vectors[i%len(vectors)]
		i++

		hits, err := s.SearchVector(ctx, vector, entities.Filters{}, benchK)
		if err != nil {
			b.Fatalf("SearchVector: %v", err)
		}
		if len(hits) != benchK {
			b.Fatalf("SearchVector = %d hits, want %d", len(hits), benchK)
		}
	}
}
