package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	sqlitevec "github.com/asg017/sqlite-vec-go-bindings/ncruces"

	"lore/internal/entities"
)

const testDims = 3

func openTestStore(t *testing.T) *Store {
	t.Helper()

	s, err := Open(filepath.Join(t.TempDir(), "workspace.db"), testDims)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return s
}

func TestOpenCreatesEverySchemaObject(t *testing.T) {
	s := openTestStore(t)

	want := []string{
		"documents", "chunks", "chunks_fts", "chunk_vectors",
		"edges", "pending_refs", "cursors", "sync_lock", "meta",
	}
	for _, name := range want {
		var got string
		err := s.db.QueryRowContext(context.Background(),
			`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&got)
		if err != nil {
			t.Errorf("table %s: %v", name, err)
		}
	}

	var version string
	err := s.db.QueryRowContext(context.Background(),
		`SELECT value FROM meta WHERE key = ?`, metaKeySchemaVersion).Scan(&version)
	if err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	if version != schemaVersion {
		t.Errorf("schema version = %q, want %q", version, schemaVersion)
	}

	var journal string
	if err := s.db.QueryRowContext(context.Background(), `PRAGMA journal_mode`).Scan(&journal); err != nil {
		t.Fatalf("read journal_mode: %v", err)
	}
	if journal != "wal" {
		t.Errorf("journal_mode = %q, want %q", journal, "wal")
	}

	var foreignKeys int
	if err := s.db.QueryRowContext(context.Background(), `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		t.Fatalf("read foreign_keys: %v", err)
	}
	if foreignKeys != 1 {
		t.Errorf("foreign_keys = %d, want 1", foreignKeys)
	}
}

func TestOpenIsIdempotentAndGuardsVectorWidth(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workspace.db")

	first, err := Open(path, testDims)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	again, err := Open(path, testDims)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if err := again.Close(); err != nil {
		t.Fatalf("reopen Close: %v", err)
	}

	if _, err := Open(path, testDims+1); err == nil {
		t.Error("reopening with a different vector width succeeded, want an error")
	}
}

func TestUpsertDocumentsAndReplaceChunks(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	created := time.Date(2025, 3, 12, 9, 30, 0, 0, time.UTC)
	docID := entities.NewDocID("github", entities.DocTypeCommit, "ABCDEF0123456789abcdef0123456789abcdef01")
	doc := entities.Document{
		ID:        docID,
		Source:    "github",
		Type:      entities.DocTypeCommit,
		RepoRef:   "github:acme/lore",
		Title:     "Pick SQLite for the workspace index",
		Body:      "Zero external infra, single-file portability, offline queries.",
		Author:    "dev@example.test",
		URL:       "https://github.com/acme/lore/commit/abcdef0",
		CreatedAt: created,
		UpdatedAt: created.Add(time.Hour),
	}

	if err := s.UpsertDocuments(ctx, []entities.Document{doc}); err != nil {
		t.Fatalf("UpsertDocuments: %v", err)
	}

	// Keyed by DocID: a second write updates in place.
	doc.Title = "Pick SQLite (revised)"
	if err := s.UpsertDocuments(ctx, []entities.Document{doc}); err != nil {
		t.Fatalf("UpsertDocuments (update): %v", err)
	}

	var (
		count       int
		title       string
		externalKey string
		sha         string
		createdAt   string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT count(*) OVER (), title, external_key, sha_prefix, created_at FROM documents`).
		Scan(&count, &title, &externalKey, &sha, &createdAt)
	if err != nil {
		t.Fatalf("read document: %v", err)
	}
	if count != 1 {
		t.Errorf("documents rows = %d, want 1", count)
	}
	if title != "Pick SQLite (revised)" {
		t.Errorf("title = %q, want the updated one", title)
	}
	if want := "ABCDEF0123456789abcdef0123456789abcdef01"; externalKey != want {
		t.Errorf("external_key = %q, want %q", externalKey, want)
	}
	if want := "abcdef012345"; sha != want {
		t.Errorf("sha_prefix = %q, want %q", sha, want)
	}
	if want := "2025-03-12T09:30:00Z"; createdAt != want {
		t.Errorf("created_at = %q, want %q", createdAt, want)
	}

	chunk := entities.Chunk{
		DocID:     docID,
		Ordinal:   0,
		Text:      "Zero external infra, single-file portability, offline queries.",
		Source:    doc.Source,
		RepoRef:   doc.RepoRef,
		DocType:   doc.Type,
		Author:    doc.Author,
		CreatedAt: created,
		UpdatedAt: created,
		Embedding: []float32{0.1, 0.2, 0.3},
	}
	unembedded := chunk
	unembedded.Ordinal = 1
	unembedded.Text = "Benchmark decides if cgo is ever needed."
	unembedded.Embedding = nil

	if err := s.ReplaceChunks(ctx, docID, []entities.Chunk{chunk, unembedded}); err != nil {
		t.Fatalf("ReplaceChunks: %v", err)
	}
	assertCounts(t, s, 2, 2, 1)

	var (
		alignedOrdinal int
		distance       float64
	)
	err = s.db.QueryRowContext(ctx, `
		SELECT c.ordinal, v.distance
		FROM chunk_vectors v JOIN chunks c ON c.id = v.rowid
		WHERE v.embedding MATCH ? AND k = 1`, mustSerialize(t, chunk.Embedding)).
		Scan(&alignedOrdinal, &distance)
	if err != nil {
		t.Fatalf("vector search: %v", err)
	}
	if alignedOrdinal != 0 {
		t.Errorf("nearest chunk ordinal = %d, want 0", alignedOrdinal)
	}
	if distance > 1e-6 {
		t.Errorf("distance to itself = %v, want ~0", distance)
	}

	var ftsOrdinal int
	err = s.db.QueryRowContext(ctx, `
		SELECT c.ordinal FROM chunks_fts f JOIN chunks c ON c.id = f.rowid
		WHERE chunks_fts MATCH 'benchmark'`).Scan(&ftsOrdinal)
	if err != nil {
		t.Fatalf("lexical search: %v", err)
	}
	if ftsOrdinal != 1 {
		t.Errorf("lexical hit ordinal = %d, want 1", ftsOrdinal)
	}

	// Narrowing the chunk set leaves no stale lexical or vector rows.
	if err := s.ReplaceChunks(ctx, docID, []entities.Chunk{chunk}); err != nil {
		t.Fatalf("ReplaceChunks (replace): %v", err)
	}
	assertCounts(t, s, 1, 1, 1)

	if err := s.ReplaceChunks(ctx, docID, nil); err != nil {
		t.Fatalf("ReplaceChunks (clear): %v", err)
	}
	assertCounts(t, s, 0, 0, 0)
}

func TestReplaceChunksRejectsWrongVectorWidthAndUnknownDocument(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	docID := entities.NewDocID("github", entities.DocTypeIssue, "42")

	err := s.ReplaceChunks(ctx, docID, []entities.Chunk{{DocID: docID, Text: "x", Embedding: []float32{1, 2}}})
	if err == nil {
		t.Error("ReplaceChunks accepted a 2-dimension vector in a 3-dimension store")
	}

	err = s.ReplaceChunks(ctx, docID, []entities.Chunk{{DocID: docID, Text: "orphan"}})
	if err == nil {
		t.Error("ReplaceChunks accepted a chunk whose document does not exist")
	}
	assertCounts(t, s, 0, 0, 0)
}

func assertCounts(t *testing.T, s *Store, chunks, texts, vectors int) {
	t.Helper()

	for _, c := range []struct {
		query string
		want  int
	}{
		{`SELECT count(*) FROM chunks`, chunks},
		{`SELECT count(*) FROM chunks_fts`, texts},
		{`SELECT count(*) FROM chunk_vectors`, vectors},
	} {
		var got int
		if err := s.db.QueryRowContext(context.Background(), c.query).Scan(&got); err != nil {
			t.Fatalf("%s: %v", c.query, err)
		}
		if got != c.want {
			t.Errorf("%s = %d, want %d", c.query, got, c.want)
		}
	}
}

func mustSerialize(t *testing.T, v []float32) []byte {
	t.Helper()

	b, err := sqlitevec.SerializeFloat32(v)
	if err != nil {
		t.Fatalf("serialize vector: %v", err)
	}
	return b
}
