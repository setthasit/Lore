package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	sqlitevec "github.com/asg017/sqlite-vec-go-bindings/ncruces"

	"lore/internal/entities"
)

// Long enough to be unique in any real repository, short enough that
// abbreviated SHAs quoted in prose still match.
const shaPrefixLen = 12

const upsertDocumentSQL = `
INSERT INTO documents (
	doc_id, source, type, repo_ref, title, body, author, url,
	created_at, updated_at, external_key, sha_prefix
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(doc_id) DO UPDATE SET
	source       = excluded.source,
	type         = excluded.type,
	repo_ref     = excluded.repo_ref,
	title        = excluded.title,
	body         = excluded.body,
	author       = excluded.author,
	url          = excluded.url,
	created_at   = excluded.created_at,
	updated_at   = excluded.updated_at,
	external_key = excluded.external_key,
	sha_prefix   = excluded.sha_prefix`

func (s *Store) UpsertDocuments(ctx context.Context, docs []entities.Document) error {
	if len(docs) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: begin document upsert: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, upsertDocumentSQL)
	if err != nil {
		return fmt.Errorf("sqlite: prepare document upsert: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for _, d := range docs {
		key := externalID(d.ID)
		_, err := stmt.ExecContext(ctx,
			string(d.ID), d.Source, string(d.Type), d.RepoRef, d.Title, d.Body, d.Author, d.URL,
			formatTime(d.CreatedAt), formatTime(d.UpdatedAt), key, shaPrefix(d.Type, key))
		if err != nil {
			return fmt.Errorf("sqlite: upsert document %q: %w", d.ID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: commit document upsert: %w", err)
	}
	return nil
}

const selectDocumentMetaSQL = `
SELECT doc_id, source, type, title, author, url, created_at, updated_at
FROM documents
WHERE doc_id IN (%s)`

// The body column is deliberately not read.
func (s *Store) DocumentsByID(ctx context.Context, ids []entities.DocID) ([]entities.DocumentMeta, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = string(id)
	}

	stmt := fmt.Sprintf(selectDocumentMetaSQL, placeholders(len(ids)))
	rows, err := s.db.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: read %d documents: %w", len(ids), err)
	}
	defer func() { _ = rows.Close() }()

	metas := make([]entities.DocumentMeta, 0, len(ids))
	for rows.Next() {
		var (
			m         entities.DocumentMeta
			docID     string
			docType   string
			createdAt string
			updatedAt string
		)
		err := rows.Scan(&docID, &m.Source, &docType, &m.Title,
			&m.Author, &m.URL, &createdAt, &updatedAt)
		if err != nil {
			return nil, fmt.Errorf("sqlite: scan document meta: %w", err)
		}

		m.ID = entities.DocID(docID)
		m.Type = entities.DocType(docType)
		if m.CreatedAt, err = parseTime(createdAt); err != nil {
			return nil, fmt.Errorf("sqlite: document %q: %w", m.ID, err)
		}
		if m.UpdatedAt, err = parseTime(updatedAt); err != nil {
			return nil, fmt.Errorf("sqlite: document %q: %w", m.ID, err)
		}
		metas = append(metas, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: read %d documents: %w", len(ids), err)
	}
	return metas, nil
}

// Only the marker count reaches the SQL text; every value stays bound.
func placeholders(n int) string {
	return strings.Repeat(",?", n)[1:]
}

const (
	selectChunkIDsSQL    = `SELECT id FROM chunks WHERE doc_id = ?`
	deleteChunkVectorSQL = `DELETE FROM chunk_vectors WHERE rowid = ?`
	deleteChunkTextSQL   = `DELETE FROM chunks_fts WHERE rowid = ?`
	deleteChunksSQL      = `DELETE FROM chunks WHERE doc_id = ?`

	insertChunkSQL = `
INSERT INTO chunks (
	doc_id, ordinal, text, source, repo_ref, doc_type, author,
	created_at, updated_at, thread_id
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	insertChunkTextSQL   = `INSERT INTO chunks_fts (rowid, text) VALUES (?, ?)`
	insertChunkVectorSQL = `INSERT INTO chunk_vectors (rowid, embedding) VALUES (?, ?)`
)

// chunks_fts and chunk_vectors are keyed by the chunk's rowid, so both derived
// rows are written with the id SQLite assigned the chunk.
func (s *Store) ReplaceChunks(ctx context.Context, docID entities.DocID, chunks []entities.Chunk) error {
	for i := range chunks {
		if n := len(chunks[i].Embedding); n != 0 && n != s.vectorDims {
			return fmt.Errorf("sqlite: chunk %d of %q has %d vector dimensions, store expects %d",
				chunks[i].Ordinal, docID, n, s.vectorDims)
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: begin chunk replace for %q: %w", docID, err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := deleteDerivedChunkRows(ctx, tx, docID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, deleteChunksSQL, string(docID)); err != nil {
		return fmt.Errorf("sqlite: delete chunks of %q: %w", docID, err)
	}
	if err := insertChunks(ctx, tx, docID, chunks); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: commit chunk replace for %q: %w", docID, err)
	}
	return nil
}

// The ids are deleted one by one on purpose: rowid equality is the one
// constraint every virtual-table implementation handles.
func deleteDerivedChunkRows(ctx context.Context, tx *sql.Tx, docID entities.DocID) error {
	ids, err := chunkIDs(ctx, tx, docID)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}

	delVector, err := tx.PrepareContext(ctx, deleteChunkVectorSQL)
	if err != nil {
		return fmt.Errorf("sqlite: prepare vector delete: %w", err)
	}
	defer func() { _ = delVector.Close() }()

	delText, err := tx.PrepareContext(ctx, deleteChunkTextSQL)
	if err != nil {
		return fmt.Errorf("sqlite: prepare lexical delete: %w", err)
	}
	defer func() { _ = delText.Close() }()

	for _, id := range ids {
		if _, err := delVector.ExecContext(ctx, id); err != nil {
			return fmt.Errorf("sqlite: delete vector of %q: %w", docID, err)
		}
		if _, err := delText.ExecContext(ctx, id); err != nil {
			return fmt.Errorf("sqlite: delete lexical row of %q: %w", docID, err)
		}
	}
	return nil
}

func chunkIDs(ctx context.Context, tx *sql.Tx, docID entities.DocID) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, selectChunkIDsSQL, string(docID))
	if err != nil {
		return nil, fmt.Errorf("sqlite: read chunk ids of %q: %w", docID, err)
	}
	defer func() { _ = rows.Close() }()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("sqlite: scan chunk id of %q: %w", docID, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: read chunk ids of %q: %w", docID, err)
	}
	return ids, nil
}

func insertChunks(ctx context.Context, tx *sql.Tx, docID entities.DocID, chunks []entities.Chunk) error {
	if len(chunks) == 0 {
		return nil
	}

	insChunk, err := tx.PrepareContext(ctx, insertChunkSQL)
	if err != nil {
		return fmt.Errorf("sqlite: prepare chunk insert: %w", err)
	}
	defer func() { _ = insChunk.Close() }()

	insText, err := tx.PrepareContext(ctx, insertChunkTextSQL)
	if err != nil {
		return fmt.Errorf("sqlite: prepare lexical insert: %w", err)
	}
	defer func() { _ = insText.Close() }()

	insVector, err := tx.PrepareContext(ctx, insertChunkVectorSQL)
	if err != nil {
		return fmt.Errorf("sqlite: prepare vector insert: %w", err)
	}
	defer func() { _ = insVector.Close() }()

	for i := range chunks {
		c := &chunks[i]
		res, err := insChunk.ExecContext(ctx,
			string(docID), c.Ordinal, c.Text, c.Source, c.RepoRef, string(c.DocType), c.Author,
			formatTime(c.CreatedAt), formatTime(c.UpdatedAt), c.ThreadID)
		if err != nil {
			return fmt.Errorf("sqlite: insert chunk %d of %q: %w", c.Ordinal, docID, err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("sqlite: chunk %d of %q got no row id: %w", c.Ordinal, docID, err)
		}

		if _, err := insText.ExecContext(ctx, id, c.Text); err != nil {
			return fmt.Errorf("sqlite: index chunk %d of %q for search: %w", c.Ordinal, docID, err)
		}
		if len(c.Embedding) == 0 {
			continue
		}
		vector, err := sqlitevec.SerializeFloat32(c.Embedding)
		if err != nil {
			return fmt.Errorf("sqlite: serialize vector of chunk %d of %q: %w", c.Ordinal, docID, err)
		}
		if _, err := insVector.ExecContext(ctx, id, vector); err != nil {
			return fmt.Errorf("sqlite: insert vector of chunk %d of %q: %w", c.Ordinal, docID, err)
		}
	}
	return nil
}

var wipeChunkTableSQL = [...]string{
	`DELETE FROM chunk_vectors`,
	`DELETE FROM chunks_fts`,
	`DELETE FROM chunks`,
}

func (s *Store) WipeChunks(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: begin chunk wipe: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, stmt := range wipeChunkTableSQL {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("sqlite: %s: %w", stmt, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: commit chunk wipe: %w", err)
	}
	return nil
}

// Returns the third segment of a "<source>:<type>:<external_id>" DocID. The
// external id may itself contain colons (URLs, Notion paths), so only the first
// two separators count; a DocID without them yields "".
func externalID(id entities.DocID) string {
	rest := string(id)
	for range 2 {
		i := strings.IndexByte(rest, ':')
		if i < 0 {
			return ""
		}
		rest = rest[i+1:]
	}
	return rest
}

// Lowercased so a prefix lookup does not depend on how a source cased its hex.
// Empty for every document type but commits.
func shaPrefix(t entities.DocType, externalKey string) string {
	if t != entities.DocTypeCommit {
		return ""
	}
	if len(externalKey) > shaPrefixLen {
		externalKey = externalKey[:shaPrefixLen]
	}
	return strings.ToLower(externalKey)
}

func formatTime(t time.Time) string {
	return t.UTC().Format(timeLayout)
}

// Only rows this store wrote reach here, so a parse failure is reported rather
// than papered over with a zero time.
func parseTime(s string) (time.Time, error) {
	t, err := time.Parse(timeLayout, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("timestamp %q is not %s: %w", s, timeLayout, err)
	}
	return t.UTC(), nil
}
