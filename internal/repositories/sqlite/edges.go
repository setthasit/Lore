package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"lore/internal/entities"
)

const upsertEdgeSQL = `INSERT OR IGNORE INTO edges (src, dst, kind, confidence) VALUES (?, ?, ?, ?)`

const selectEdgesSQL = `SELECT src, dst, kind, confidence FROM edges WHERE `

const (
	upsertPendingRefSQL  = `INSERT OR IGNORE INTO pending_refs (src_doc, kind, value) VALUES (?, ?, ?)`
	deletePendingRefSQL  = `DELETE FROM pending_refs WHERE src_doc = ? AND kind = ? AND value = ?`
	selectPendingRefsSQL = `SELECT src_doc, kind, value FROM pending_refs ORDER BY src_doc, kind, value`
)

func (s *Store) UpsertEdges(ctx context.Context, edges []entities.Edge) error {
	if len(edges) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: begin edge upsert: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, upsertEdgeSQL)
	if err != nil {
		return fmt.Errorf("sqlite: prepare edge upsert: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for _, e := range edges {
		_, err := stmt.ExecContext(ctx, string(e.Src), string(e.Dst), string(e.Kind), e.Confidence)
		if err != nil {
			return fmt.Errorf("sqlite: upsert edge %q -> %q: %w", e.Src, e.Dst, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: commit edge upsert: %w", err)
	}
	return nil
}

func (s *Store) Neighbors(
	ctx context.Context,
	ids []entities.DocID,
	kinds []entities.EdgeKind,
	dir entities.Direction,
) ([]entities.Edge, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	columns := endpointColumns(dir)
	branches := make([]string, 0, len(columns))
	args := make([]any, 0, len(columns)*(len(ids)+len(kinds)))
	for _, column := range columns {
		clause := column + " IN (" + placeholders(len(ids)) + ")"
		for _, id := range ids {
			args = append(args, string(id))
		}
		if len(kinds) > 0 {
			clause += " AND kind IN (" + placeholders(len(kinds)) + ")"
			for _, k := range kinds {
				args = append(args, string(k))
			}
		}
		branches = append(branches, selectEdgesSQL+clause)
	}

	query := strings.Join(branches, " UNION ") + " ORDER BY src, dst, kind"
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: read neighbors of %d documents: %w", len(ids), err)
	}
	defer func() { _ = rows.Close() }()

	return scanEdges(rows)
}

func endpointColumns(dir entities.Direction) []string {
	switch dir {
	case entities.DirIn:
		return []string{"dst"}
	case entities.DirBoth:
		return []string{"src", "dst"}
	default:
		return []string{"src"}
	}
}

func scanEdges(rows *sql.Rows) ([]entities.Edge, error) {
	var edges []entities.Edge
	for rows.Next() {
		var (
			src        string
			dst        string
			kind       string
			confidence float64
		)
		if err := rows.Scan(&src, &dst, &kind, &confidence); err != nil {
			return nil, fmt.Errorf("sqlite: scan edge: %w", err)
		}
		edges = append(edges, entities.Edge{
			Src:        entities.DocID(src),
			Dst:        entities.DocID(dst),
			Kind:       entities.EdgeKind(kind),
			Confidence: float32(confidence),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: read edges: %w", err)
	}
	return edges, nil
}

func (s *Store) PendingRefs(ctx context.Context) ([]entities.PendingRef, error) {
	rows, err := s.db.QueryContext(ctx, selectPendingRefsSQL)
	if err != nil {
		return nil, fmt.Errorf("sqlite: read pending refs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var refs []entities.PendingRef
	for rows.Next() {
		var srcDoc, kind, value string
		if err := rows.Scan(&srcDoc, &kind, &value); err != nil {
			return nil, fmt.Errorf("sqlite: scan pending ref: %w", err)
		}
		refs = append(refs, entities.PendingRef{
			SourceDoc: entities.DocID(srcDoc),
			Ref:       entities.RawRef{Kind: entities.RefKind(kind), Value: value},
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: read pending refs: %w", err)
	}
	return refs, nil
}

func (s *Store) UpsertPendingRefs(ctx context.Context, refs []entities.PendingRef) error {
	return s.writePendingRefs(ctx, "upsert", upsertPendingRefSQL, refs)
}

func (s *Store) DeletePendingRefs(ctx context.Context, refs []entities.PendingRef) error {
	return s.writePendingRefs(ctx, "delete", deletePendingRefSQL, refs)
}

func (s *Store) writePendingRefs(ctx context.Context, verb, query string, refs []entities.PendingRef) error {
	if len(refs) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: begin pending ref %s: %w", verb, err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, query)
	if err != nil {
		return fmt.Errorf("sqlite: prepare pending ref %s: %w", verb, err)
	}
	defer func() { _ = stmt.Close() }()

	for _, r := range refs {
		_, err := stmt.ExecContext(ctx, string(r.SourceDoc), string(r.Ref.Kind), r.Ref.Value)
		if err != nil {
			return fmt.Errorf("sqlite: %s pending ref %q of %q: %w", verb, r.Ref.Value, r.SourceDoc, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: commit pending ref %s: %w", verb, err)
	}
	return nil
}
