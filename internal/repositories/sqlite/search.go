package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"unicode"

	sqlitevec "github.com/asg017/sqlite-vec-go-bindings/ncruces"

	"github.com/setthasit/Lore/internal/entities"
	"github.com/setthasit/Lore/sdk"
)

const chunkHitColumns = `c.doc_id, c.ordinal, c.text, c.source, c.repo_ref, c.doc_type,
	c.author, c.created_at, c.updated_at, c.thread_id`

const maxHitPrealloc = 128

// FTS5 returns every matching row, so filtering and limiting in the outer query
// still yields the best k of the filtered set.
const searchLexicalSQL = `
SELECT ` + chunkHitColumns + `, -bm25(chunks_fts) AS score
FROM chunks_fts JOIN chunks c ON c.id = chunks_fts.rowid
WHERE chunks_fts MATCH ?%s
ORDER BY score DESC
LIMIT ?`

// The metadata filter has to be a rowid candidate set: a filter in the outer
// query would prune rows the KNN already counted against k and silently return
// fewer than k hits, whereas vec0 applies a rowid constraint before searching.
const searchVectorSQL = `
SELECT ` + chunkHitColumns + `, -v.distance AS score
FROM chunk_vectors v JOIN chunks c ON c.id = v.rowid
WHERE v.embedding MATCH ? AND k = ?%s
ORDER BY score DESC`

const filterCandidateSQL = ` AND v.rowid IN (SELECT fc.id FROM chunks fc WHERE %s)`

func (s *Store) SearchLexical(ctx context.Context, query string, f entities.Filters, k int) ([]entities.ChunkHit, error) {
	if k <= 0 {
		return nil, fmt.Errorf("sqlite: lexical search k must be positive, got %d", k)
	}

	match := ftsMatchExpr(query)
	if match == "" {
		return nil, nil
	}

	where, args := filterClause("c", f)
	stmt := fmt.Sprintf(searchLexicalSQL, "")
	if where != "" {
		stmt = fmt.Sprintf(searchLexicalSQL, " AND "+where)
	}

	bind := make([]any, 0, len(args)+2)
	bind = append(bind, match)
	bind = append(bind, args...)
	bind = append(bind, k)

	rows, err := s.db.QueryContext(ctx, stmt, bind...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: lexical search for %q: %w", query, err)
	}
	defer func() { _ = rows.Close() }()

	hits, err := scanChunkHits(rows, k)
	if err != nil {
		return nil, fmt.Errorf("sqlite: lexical search for %q: %w", query, err)
	}
	return hits, nil
}

func (s *Store) SearchVector(ctx context.Context, embedding []float32, f entities.Filters, k int) ([]entities.ChunkHit, error) {
	if k <= 0 {
		return nil, fmt.Errorf("sqlite: vector search k must be positive, got %d", k)
	}
	if len(embedding) != s.vectorDims {
		return nil, fmt.Errorf("sqlite: query vector has %d dimensions, store expects %d",
			len(embedding), s.vectorDims)
	}

	vector, err := sqlitevec.SerializeFloat32(embedding)
	if err != nil {
		return nil, fmt.Errorf("sqlite: serialize query vector: %w", err)
	}

	where, args := filterClause("fc", f)
	stmt := fmt.Sprintf(searchVectorSQL, "")
	if where != "" {
		stmt = fmt.Sprintf(searchVectorSQL, fmt.Sprintf(filterCandidateSQL, where))
	}

	bind := make([]any, 0, len(args)+2)
	bind = append(bind, vector, k)
	bind = append(bind, args...)

	rows, err := s.db.QueryContext(ctx, stmt, bind...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: vector search: %w", err)
	}
	defer func() { _ = rows.Close() }()

	hits, err := scanChunkHits(rows, k)
	if err != nil {
		return nil, fmt.Errorf("sqlite: vector search: %w", err)
	}
	return hits, nil
}

// Every token is emitted quoted, which is what keeps arbitrary user text from
// being a syntax error: inside quotes "AND", "NOT", "NEAR" and "*" are ordinary
// terms, and the split treats a quote as a separator so no token can contain
// one. Tokens are OR-ed, not AND-ed as FTS5 defaults to, because a conjoined
// natural-language question matches nothing. Returns "" for text with no
// indexable token — an empty MATCH is the one expression FTS5 rejects.
func ftsMatchExpr(query string) string {
	var b strings.Builder
	for token := range strings.FieldsFuncSeq(query, isTokenBreak) {
		if b.Len() > 0 {
			b.WriteString(" OR ")
		}
		b.WriteByte('"')
		b.WriteString(token)
		b.WriteByte('"')
	}
	return b.String()
}

func isTokenBreak(r rune) bool {
	return !unicode.IsLetter(r) && !unicode.IsNumber(r)
}

// The created_at bounds are inclusive and compare as plain strings.
func filterClause(alias string, f entities.Filters) (string, []any) {
	var (
		b    strings.Builder
		args []any
	)
	add := func(condition string, arg any) {
		if b.Len() > 0 {
			b.WriteString(" AND ")
		}
		b.WriteString(alias)
		b.WriteByte('.')
		b.WriteString(condition)
		args = append(args, arg)
	}

	if f.Source != "" {
		add("source = ?", f.Source)
	}
	if f.RepoRef != "" {
		add("repo_ref = ?", f.RepoRef)
	}
	if f.DocType != "" {
		add("doc_type = ?", string(f.DocType))
	}
	if !f.CreatedFrom.IsZero() {
		add("created_at >= ?", formatTime(f.CreatedFrom))
	}
	if !f.CreatedTo.IsZero() {
		add("created_at <= ?", formatTime(f.CreatedTo))
	}
	return b.String(), args
}

func scanChunkHits(rows *sql.Rows, k int) ([]entities.ChunkHit, error) {
	hits := make([]entities.ChunkHit, 0, min(k, maxHitPrealloc))
	for rows.Next() {
		var (
			h         entities.ChunkHit
			docID     string
			docType   string
			createdAt string
			updatedAt string
			score     float64
		)
		err := rows.Scan(&docID, &h.Ordinal, &h.Text, &h.Source, &h.RepoRef, &docType,
			&h.Author, &createdAt, &updatedAt, &h.ThreadID, &score)
		if err != nil {
			return nil, fmt.Errorf("scan chunk hit: %w", err)
		}

		h.DocID = lore.DocID(docID)
		h.DocType = lore.DocType(docType)
		h.Score = float32(score)
		if h.CreatedAt, err = parseTime(createdAt); err != nil {
			return nil, fmt.Errorf("chunk %d of %q: %w", h.Ordinal, h.DocID, err)
		}
		if h.UpdatedAt, err = parseTime(updatedAt); err != nil {
			return nil, fmt.Errorf("chunk %d of %q: %w", h.Ordinal, h.DocID, err)
		}
		hits = append(hits, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read chunk hits: %w", err)
	}
	return hits, nil
}
