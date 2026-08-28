package sqlite

// These benchmarks answer one question: is the pure-Go driver
// (ncruces/go-sqlite3 running SQLite as WASM, plus the sqlite-vec build it
// carries) fast enough to keep cgo out of the project? They measure the three
// operations the daemon repeats — ingest, lexical search, vector search — on a
// corpus shaped like a real workspace, because the correctness fixtures are far
// too small to say anything about cost.

import (
	"context"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"lore/internal/entities"
)

const (
	// benchDims is the width of a small local embedding model (all-MiniLM and
	// its relatives). It is what sizes the vec0 column, and therefore the KNN
	// scan, realistically without bloating the temporary file.
	benchDims = 128

	// benchChunksPerDoc and benchChunkWords approximate what the chunker emits
	// for an average PR or page: ten windows of a few hundred bytes each.
	benchChunksPerDoc = 10
	benchChunkWords   = 60

	// benchRarePerChunk is how many low-frequency tokens a chunk carries.
	benchRarePerChunk = 3

	// benchTitleWords is how much of the body the title repeats.
	benchTitleWords = 8

	// benchIngestBatch is the document batch UpsertDocuments is called with. One
	// batch is one document transaction plus benchIngestBatch chunk
	// transactions, because ReplaceChunks is per document by contract.
	benchIngestBatch = 100

	// benchSearchDocs sizes the search corpus: 1,000 documents, so ~10,000
	// chunks and ~10,000 vectors.
	benchSearchDocs = 1000

	// benchK is the k a hybrid retrieval leg asks one strategy for.
	benchK = 12

	// benchQueryCount is how many distinct queries the search loops rotate
	// through, so a benchmark reports the cost of searching rather than the cost
	// of SQLite's page cache holding one query's postings list.
	benchQueryCount = 16

	// benchSeed fixes every word choice and every vector component. Combined
	// with a per-document generator, it makes document i identical in every
	// benchmark and on every run.
	benchSeed = 20250827
)

// benchEpoch anchors the corpus in time; documents are spaced an hour apart.
var benchEpoch = time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

// benchCommonWords is the high-frequency half of the vocabulary: stop words and
// domain terms. They are what give BM25 long postings lists to walk, which is
// the cost a natural-language query actually pays.
var benchCommonWords = strings.Fields(`
	the a an of to and or in for on with from into that this when why where
	service store index query chunk document embedding vector search request
	response handler retry timeout context error commit review issue ticket
	page thread comment schema migration transaction connection pool cache
	latency deploy rollback config token limit worker queue batch flush`)

// benchRareStems compose the low-frequency vocabulary. They are deliberately
// disjoint from benchCommonWords: a stem that leaked into both halves would stop
// being selective.
var benchRareStems = strings.Fields(`
	auth gate shard lease quorum proxy spool ledger vault beacon cursor digest
	entropy fabric girder harbor jitter lattice mantle nexus oracle parcel
	quarry raster sentinel tundra umbra warden zenith anvil bramble citadel
	delta ember fjord glyph hollow ivory jasper kiln lumen marrow nimbus
	obsidian plume`)

// benchRareWords is every ordered pair of stems, a pool large enough that a
// token lands in a handful of the ten thousand chunks — as selective as a real
// identifier, ticket key or symbol name.
var benchRareWords = func() []string {
	words := make([]string, 0, len(benchRareStems)*len(benchRareStems))
	for _, head := range benchRareStems {
		for _, tail := range benchRareStems {
			words = append(words, head+tail)
		}
	}
	return words
}()

// benchQueryTemplates phrase the queries as questions, the way retrieval
// receives them: mostly stop words, one domain term, one selective token.
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

// benchDoc builds document i and its chunks. Everything is derived from i alone,
// because the generator is seeded per document: the same index means the same
// bytes whatever order or iteration count the benchmark loop settles on.
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

// benchChunkText draws a chunk's words: mostly common vocabulary with a few rare
// tokens sprinkled over it.
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

// benchTitle is the document's opening words, the way a commit subject or page
// heading relates to its body.
func benchTitle(text string) string {
	words := strings.SplitN(text, " ", benchTitleWords+1)
	return strings.Join(words[:benchTitleWords], " ")
}

// benchVector is a chunk or query embedding. The components are plain uniform
// noise: vec0's KNN is a brute-force scan whose cost depends on how many vectors
// there are and how wide they are, not on what they hold.
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

// ingestBatch writes one batch the way the indexer does: the documents in a
// single transaction, then each document's chunks.
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

// seedBenchStore builds the search corpus in a fresh file. Both search
// benchmarks pay for it once and outside the measurement: b.Loop resets the
// timer when the loop starts, so setup before it is not counted.
func seedBenchStore(b *testing.B) *Store {
	b.Helper()

	s := openBenchStore(b)
	for start := 0; start < benchSearchDocs; start += benchIngestBatch {
		docs, chunks := benchBatch(start, benchIngestBatch)
		ingestBatch(b, s, docs, chunks)
	}
	return s
}

// benchQueries are the queries the lexical leg rotates through. They are built
// from the same vocabulary the corpus is, so every one of them hits.
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

// BenchmarkUpsertAndChunk measures a first sync: one batch of benchIngestBatch
// documents with their chunks, embeddings and derived lexical and vector rows.
// ns/op is per batch; the reported ns/doc and ns/chunk are what a connector's
// throughput is read off.
func BenchmarkUpsertAndChunk(b *testing.B) {
	s := openBenchStore(b)

	batches := 0
	for b.Loop() {
		// Each iteration ingests documents the store has never seen: reusing ids
		// would measure in-place updates against a database that stops growing,
		// which is not what a first sync does.
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
		// A short result would mean the corpus, not the query, is what the
		// numbers describe.
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
