package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
)

// schemaVersion is the generation of the on-disk schema. Because the index is
// derived data, an incompatible bump is resolved by rebuilding the workspace
// file, never by migrating it.
const schemaVersion = "1"

// Keys the store itself owns in the meta table. The embedder identity keys are
// written by the service layer.
const (
	metaKeySchemaVersion = "schema_version"
	metaKeyVectorDims    = "vector_dims"
)

// schemaDDL creates every table the workspace file needs. It is idempotent, so
// every Open runs it.
//
// Conventions:
//
//   - Timestamps are TEXT in RFC 3339 UTC with second precision
//     ("2006-01-02T15:04:05Z"). Fixed width means lexicographic order is
//     chronological order, so time-range filters push straight into SQL as
//     string comparisons. The columns are declared TEXT so the driver hands
//     them back as strings rather than guessing a time format.
//   - Real tables are STRICT: a column's declared type is enforced, so a typo
//     in Go cannot quietly store an integer in a text column.
//   - chunks.id is an explicit INTEGER PRIMARY KEY (a rowid alias) because
//     chunks_fts and chunk_vectors are rowid-aligned with it, and VACUUM may
//     renumber implicit rowids but never an INTEGER PRIMARY KEY.
//   - chunks.doc_id deliberately does not cascade. A future document delete
//     must go through ReplaceChunks(docID, nil) first; otherwise a cascade
//     would drop chunk rows while leaving orphaned lexical and vector rows
//     behind, since virtual tables see no foreign keys.
//   - chunks_fts is a self-contained FTS5 table, not an external-content or
//     contentless one. It costs a second copy of the chunk text in a file that
//     is a rebuildable cache anyway, and buys ordinary INSERT/DELETE semantics
//     with no index-sync invariant to violate. Tokenizer tuning is left to the
//     retrieval wave.
//   - edges is WITHOUT ROWID keyed by (src, dst, kind); that primary-key index
//     already serves src-first walks, so only dst needs its own index.
const schemaDDL = `
CREATE TABLE IF NOT EXISTS documents (
	doc_id       TEXT PRIMARY KEY,
	source       TEXT NOT NULL,
	type         TEXT NOT NULL,
	repo_ref     TEXT NOT NULL,
	title        TEXT NOT NULL,
	body         TEXT NOT NULL,
	author       TEXT NOT NULL,
	url          TEXT NOT NULL,
	created_at   TEXT NOT NULL,
	updated_at   TEXT NOT NULL,
	external_key TEXT NOT NULL,
	sha_prefix   TEXT NOT NULL
) STRICT;

CREATE INDEX IF NOT EXISTS documents_url_idx ON documents(url);
CREATE INDEX IF NOT EXISTS documents_external_key_idx ON documents(external_key);
CREATE INDEX IF NOT EXISTS documents_sha_prefix_idx ON documents(sha_prefix);
CREATE INDEX IF NOT EXISTS documents_source_type_idx ON documents(source, type);

CREATE TABLE IF NOT EXISTS chunks (
	id         INTEGER PRIMARY KEY,
	doc_id     TEXT NOT NULL REFERENCES documents(doc_id),
	ordinal    INTEGER NOT NULL,
	text       TEXT NOT NULL,
	source     TEXT NOT NULL,
	repo_ref   TEXT NOT NULL,
	doc_type   TEXT NOT NULL,
	author     TEXT NOT NULL,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	thread_id  TEXT NOT NULL
) STRICT;

CREATE INDEX IF NOT EXISTS chunks_doc_id_idx ON chunks(doc_id, ordinal);

CREATE VIRTUAL TABLE IF NOT EXISTS chunks_fts USING fts5(text);

CREATE TABLE IF NOT EXISTS edges (
	src        TEXT NOT NULL,
	dst        TEXT NOT NULL,
	kind       TEXT NOT NULL,
	confidence REAL NOT NULL,
	PRIMARY KEY (src, dst, kind)
) WITHOUT ROWID, STRICT;

CREATE INDEX IF NOT EXISTS edges_dst_idx ON edges(dst);

CREATE TABLE IF NOT EXISTS pending_refs (
	src_doc TEXT NOT NULL,
	kind    TEXT NOT NULL,
	value   TEXT NOT NULL,
	PRIMARY KEY (src_doc, kind, value)
) WITHOUT ROWID, STRICT;

CREATE TABLE IF NOT EXISTS cursors (
	connector TEXT PRIMARY KEY,
	payload   TEXT NOT NULL
) WITHOUT ROWID, STRICT;

CREATE TABLE IF NOT EXISTS sync_lock (
	id           INTEGER PRIMARY KEY CHECK (id = 1),
	holder       TEXT NOT NULL,
	acquired_at  TEXT NOT NULL,
	heartbeat_at TEXT NOT NULL
) STRICT;

CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
) WITHOUT ROWID, STRICT;
`

// vectorTableDDL is separate because a vec0 column's dimension count is baked
// into the declaration and cannot be a bind parameter.
const vectorTableDDL = `CREATE VIRTUAL TABLE IF NOT EXISTS chunk_vectors USING vec0(embedding float[%d]);`

// bootstrap creates the schema if absent and refuses a file whose schema
// generation or vector width does not match this build. CREATE TABLE IF NOT
// EXISTS would silently keep an existing vec0 table at its original dimensions,
// so the recorded width is what actually guards vector inserts.
func (s *Store) bootstrap(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin schema bootstrap: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, schemaDDL); err != nil {
		return fmt.Errorf("create schema: %w", err)
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(vectorTableDDL, s.vectorDims)); err != nil {
		return fmt.Errorf("create chunk_vectors with %d dimensions: %w", s.vectorDims, err)
	}
	if err := ensureMeta(ctx, tx, metaKeySchemaVersion, schemaVersion); err != nil {
		return err
	}
	if err := ensureMeta(ctx, tx, metaKeyVectorDims, strconv.Itoa(s.vectorDims)); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit schema bootstrap: %w", err)
	}
	return nil
}

// ensureMeta records want under key on a fresh file and reports a mismatch on an
// existing one.
func ensureMeta(ctx context.Context, tx *sql.Tx, key, want string) error {
	const insert = `INSERT INTO meta (key, value) VALUES (?, ?) ON CONFLICT(key) DO NOTHING`
	if _, err := tx.ExecContext(ctx, insert, key, want); err != nil {
		return fmt.Errorf("record meta %q: %w", key, err)
	}

	var got string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&got); err != nil {
		return fmt.Errorf("read meta %q: %w", key, err)
	}
	if got != want {
		return fmt.Errorf("database %s is %q, this store expects %q", key, got, want)
	}
	return nil
}
