package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"lore/internal/entities"
)

const (
	countDocumentsSQL = `SELECT count(*) FROM documents`
	countChunksSQL    = `SELECT count(*) FROM chunks`

	// Ordered by connector so a rendered report is stable between runs.
	selectCursorAgesSQL = `SELECT connector, updated_at FROM cursors ORDER BY connector`

	selectLeaseSQL = `SELECT holder, acquired_at, heartbeat_at FROM sync_lock WHERE id = ?`
)

func (s *Store) Stats(ctx context.Context) (entities.IndexStats, error) {
	var stats entities.IndexStats

	if err := s.db.QueryRowContext(ctx, countDocumentsSQL).Scan(&stats.Documents); err != nil {
		return entities.IndexStats{}, fmt.Errorf("sqlite: count documents: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, countChunksSQL).Scan(&stats.Chunks); err != nil {
		return entities.IndexStats{}, fmt.Errorf("sqlite: count chunks: %w", err)
	}

	ages, err := s.cursorAges(ctx)
	if err != nil {
		return entities.IndexStats{}, err
	}
	stats.Cursors = ages

	lease, err := s.lease(ctx)
	if err != nil {
		return entities.IndexStats{}, err
	}
	stats.Lease = lease

	return stats, nil
}

func (s *Store) cursorAges(ctx context.Context) ([]entities.CursorAge, error) {
	rows, err := s.db.QueryContext(ctx, selectCursorAgesSQL)
	if err != nil {
		return nil, fmt.Errorf("sqlite: read cursor ages: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var ages []entities.CursorAge
	for rows.Next() {
		var (
			connector string
			updatedAt string
		)
		if err := rows.Scan(&connector, &updatedAt); err != nil {
			return nil, fmt.Errorf("sqlite: scan cursor age: %w", err)
		}
		at, err := parseTime(updatedAt)
		if err != nil {
			return nil, fmt.Errorf("sqlite: cursor age of %q: %w", connector, err)
		}
		ages = append(ages, entities.CursorAge{Connector: connector, UpdatedAt: at})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: read cursor ages: %w", err)
	}
	return ages, nil
}

// A lapsed heartbeat is reported, not filtered: it evidences a crashed round.
func (s *Store) lease(ctx context.Context) (*entities.LeaseState, error) {
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
