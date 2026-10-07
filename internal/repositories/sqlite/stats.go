package sqlite

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"github.com/setthasit/Lore/internal/entities"
)

const (
	hasEdgesSQL = `SELECT EXISTS(SELECT 1 FROM edges)`

	countIndexRowsSQL = `SELECT
	(SELECT count(*) FROM documents),
	(SELECT count(*) FROM chunks),
	(SELECT count(*) FROM edges)`

	selectSourceCheckpointsSQL = `SELECT connector, updated_at FROM cursors ORDER BY connector`

	selectDocumentsPerSourceSQL = `SELECT source, count(*) FROM documents GROUP BY source`

	selectLeaseSQL = `SELECT holder, acquired_at, heartbeat_at FROM sync_lock WHERE id = ?`
)

func (s *Store) HasEdges(ctx context.Context) (bool, error) {
	var hasEdges bool
	if err := s.db.QueryRowContext(ctx, hasEdgesSQL).Scan(&hasEdges); err != nil {
		return false, fmt.Errorf("sqlite: has edges: %w", err)
	}
	return hasEdges, nil
}

func (s *Store) Stats(ctx context.Context) (entities.IndexStats, error) {
	var stats entities.IndexStats

	err := s.db.QueryRowContext(ctx, countIndexRowsSQL).Scan(&stats.Documents, &stats.Chunks, &stats.Edges)
	if err != nil {
		return entities.IndexStats{}, fmt.Errorf("sqlite: count index rows: %w", err)
	}

	checkpoints, err := s.sourceCheckpoints(ctx)
	if err != nil {
		return entities.IndexStats{}, err
	}
	sources, err := s.sourceStates(ctx, checkpoints)
	if err != nil {
		return entities.IndexStats{}, err
	}
	stats.Sources = sources

	lease, err := s.Lease(ctx)
	if err != nil {
		return entities.IndexStats{}, err
	}
	stats.Lease = lease

	return stats, nil
}

func (s *Store) sourceCheckpoints(ctx context.Context) ([]entities.SourceState, error) {
	rows, err := s.db.QueryContext(ctx, selectSourceCheckpointsSQL)
	if err != nil {
		return nil, fmt.Errorf("sqlite: read source checkpoints: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var sources []entities.SourceState
	for rows.Next() {
		var (
			connector string
			updatedAt string
		)
		if err := rows.Scan(&connector, &updatedAt); err != nil {
			return nil, fmt.Errorf("sqlite: scan source checkpoint: %w", err)
		}
		at, err := parseTime(updatedAt)
		if err != nil {
			return nil, fmt.Errorf("sqlite: checkpoint of %q: %w", connector, err)
		}
		sources = append(sources, entities.SourceState{ID: connector, LastCheckpoint: at})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: read source checkpoints: %w", err)
	}
	return sources, nil
}

func (s *Store) sourceStates(ctx context.Context, checkpoints []entities.SourceState) ([]entities.SourceState, error) {
	byID := make(map[string]entities.SourceState)
	for _, source := range checkpoints {
		byID[source.ID] = source
	}

	rows, err := s.db.QueryContext(ctx, selectDocumentsPerSourceSQL)
	if err != nil {
		return nil, fmt.Errorf("sqlite: count documents per source: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			id        string
			documents int64
		)
		if err := rows.Scan(&id, &documents); err != nil {
			return nil, fmt.Errorf("sqlite: scan document count per source: %w", err)
		}
		source := byID[id]
		source.ID = id
		source.Documents = documents
		byID[id] = source
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: count documents per source: %w", err)
	}

	var sources []entities.SourceState
	for _, source := range byID {
		sources = append(sources, source)
	}
	slices.SortFunc(sources, func(a, b entities.SourceState) int {
		return cmp.Compare(a.ID, b.ID)
	})
	return sources, nil
}

func (s *Store) Lease(ctx context.Context) (*entities.LeaseState, error) {
	var (
		holder      string
		acquiredAt  string
		heartbeatAt string
	)
	err := s.db.QueryRowContext(ctx, selectLeaseSQL, syncLockID).Scan(&holder, &acquiredAt, &heartbeatAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: read sync lease: %w", err)
	}

	acquired, err := parseTime(acquiredAt)
	if err != nil {
		return nil, fmt.Errorf("sqlite: sync lease acquired_at: %w", err)
	}
	heartbeat, err := parseTime(heartbeatAt)
	if err != nil {
		return nil, fmt.Errorf("sqlite: sync lease heartbeat_at: %w", err)
	}
	return &entities.LeaseState{Holder: holder, AcquiredAt: acquired, HeartbeatAt: heartbeat}, nil
}
