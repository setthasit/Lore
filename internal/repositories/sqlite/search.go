package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"unicode"

	sqlitevec "github.com/asg017/sqlite-vec-go-bindings/ncruces"

	"lore/internal/entities"
)

// chunkHitColumns is the chunk metadata every hit carries, read from the chunks
// table (aliased c) that both virtual tables are rowid-aligned with. The
// embedding column is deliberately absent: callers rank and cite chunks, they
// never re-embed them, and a vector per hit is pure copying.
const chunkHitColumns = `c.doc_id, c.ordinal, c.text, c.source, c.repo_ref, c.doc_type,
	c.author, c.created_at, c.updated_at, c.thread_id`

// maxHitPrealloc bounds the result slice's initial capacity so an absurd k does
// not turn into an absurd allocation before a single row is read.
const maxHitPrealloc = 128

// searchLexicalSQL ranks by BM25, negated so a larger Score is a better hit (see
// the IndexStore contract). FTS5 returns every matching row, so filtering and
// limiting in the outer query still yields the best k of the filtered set.
//
// %s is where filterClause's conditions go.
const searchLexicalSQL = `
SELECT ` + chunkHitColumns + `, -bm25(chunks_fts) AS score
FROM chunks_fts JOIN chunks c ON c.id = chunks_fts.rowid
WHERE chunks_fts MATCH ?%s
ORDER BY score DESC
LIMIT ?`

// searchVectorSQL is a vec0 KNN query: the k constraint is the row limit, so no
// LIMIT clause is needed or wanted.
//
// %s carries the metadata filter as a rowid candidate set. That is the one place
// it can go: a filter in the outer query would prune rows the KNN already
// counted against k and silently return fewer than k hits, whereas the rowid
// constraint is taken by the vec0 module itself and applies before the search.
const searchVectorSQL = `
SELECT ` + chunkHitColumns + `, -v.distance AS score
FROM chunk_vectors v JOIN chunks c ON c.id = v.rowid
WHERE v.embedding MATCH ? AND k = ?%s
ORDER BY score DESC`

// filterCandidateSQL restricts the KNN to the chunks a filter admits.
const filterCandidateSQL = ` AND v.rowid IN (SELECT fc.id FROM chunks fc WHERE %s)`

// SearchLexical ranks chunks by BM25 over the FTS5 index. Score is negated BM25:
// higher is more relevant.
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

// SearchVector ranks chunks by vector distance. Score is the negated distance:
// higher is more relevant, 0 is identical.
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

// ftsMatchExpr turns arbitrary user text into an FTS5 MATCH expression that
// cannot be a syntax error, whatever the user typed.
//
// The text is split the way the unicode61 tokenizer splits it — runs of letters
// and digits — and every token is emitted as a quoted FTS5 string. Quoting is
// what makes the result safe: inside quotes, "AND", "NOT", "NEAR" and "*" are
// ordinary terms rather than operators, and a token can never contain a quote of
// its own because the split treats one as a separator. So a pasted stack trace or
// a question full of punctuation searches for its words instead of failing.
//
// Tokens are OR-ed, not AND-ed as FTS5 does by default: this is the lexical leg
// of a hybrid search, where a natural-language question conjoined term by term
// would match nothing. BM25 still ranks the chunk covering most of the question
// first. Text with no indexable token yields "", which callers treat as "no
// hits" — an empty MATCH string is the one expression FTS5 rejects.
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

// filterClause renders f as AND-ed conditions over the chunks table under the
// given alias, together with the values they bind. An unconstrained Filters
// yields "" and no arguments, so callers leave the condition out entirely.
//
// The created_at bounds are inclusive and compare as plain strings: the column is
// fixed-width RFC 3339 UTC (see formatTime), so lexicographic order is
// chronological order and the range pushes straight into SQL.
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

// scanChunkHits reads rows selected with chunkHitColumns plus a trailing score.
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

		h.DocID = entities.DocID(docID)
		h.DocType = entities.DocType(docType)
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
