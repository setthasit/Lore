package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"lore/internal/entities"
	"lore/internal/repositories"
)

const syncLockID = 1

const (
	selectCursorSQL = `SELECT payload FROM cursors WHERE connector = ?`
	upsertCursorSQL = `
INSERT INTO cursors (connector, payload, updated_at) VALUES (?, ?, ?)
ON CONFLICT(connector) DO UPDATE SET
	payload    = excluded.payload,
	updated_at = excluded.updated_at`

	selectMetaSQL = `SELECT value FROM meta WHERE key = ?`
	upsertMetaSQL = `
INSERT INTO meta (key, value) VALUES (?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value`
)

func (s *Store) Cursor(ctx context.Context, connector string) (entities.Cursor, error) {
	var payload string
	err := s.db.QueryRowContext(ctx, selectCursorSQL, connector).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: read cursor of %q: %w", connector, err)
	}

	var c entities.Cursor
	if err := json.Unmarshal([]byte(payload), &c); err != nil {
		return nil, fmt.Errorf("sqlite: decode cursor of %q: %w", connector, err)
	}
	return c, nil
}

// The stored timestamp is the store clock, not one read out of the Cursor.
func (s *Store) SetCursor(ctx context.Context, connector string, c entities.Cursor) error {
	payload, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("sqlite: encode cursor of %q: %w", connector, err)
	}
	if _, err := s.db.ExecContext(ctx, upsertCursorSQL, connector, string(payload), formatTime(s.now())); err != nil {
		return fmt.Errorf("sqlite: write cursor of %q: %w", connector, err)
	}
	return nil
}

func (s *Store) Meta(ctx context.Context, key string) (string, error) {
	var value string
	err := s.db.QueryRowContext(ctx, selectMetaSQL, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("sqlite: read meta %q: %w", key, err)
	}
	return value, nil
}

// The identity keys are refused: Open validates them against this build, so
// overwriting one would make the file unopenable.
func (s *Store) SetMeta(ctx context.Context, key, value string) error {
	switch key {
	case metaKeySchemaVersion, metaKeyVectorDims:
		return fmt.Errorf("sqlite: meta key %q describes the file's identity and is owned by the store", key)
	}
	if _, err := s.db.ExecContext(ctx, upsertMetaSQL, key, value); err != nil {
		return fmt.Errorf("sqlite: write meta %q: %w", key, err)
	}
	return nil
}

// One statement, so two processes racing for the lease cannot both win: exactly
// one inserts the row or passes the guard, and the loser sees no affected row.
const acquireLeaseSQL = `
INSERT INTO sync_lock (id, holder, acquired_at, heartbeat_at)
VALUES (?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
	holder       = excluded.holder,
	acquired_at  = excluded.acquired_at,
	heartbeat_at = excluded.heartbeat_at
WHERE sync_lock.holder = excluded.holder OR sync_lock.heartbeat_at < ?`

const (
	heartbeatLeaseSQL = `UPDATE sync_lock SET heartbeat_at = ? WHERE id = ? AND holder = ?`
	releaseLeaseSQL   = `DELETE FROM sync_lock WHERE id = ? AND holder = ?`
)

func (s *Store) TryAcquireLease(ctx context.Context, holder string) (bool, error) {
	if holder == "" {
		return false, errors.New("sqlite: lease holder must be named")
	}

	at := s.now()
	now, cutoff := formatTime(at), formatTime(at.Add(-repositories.LeaseTTL))

	res, err := s.db.ExecContext(ctx, acquireLeaseSQL, syncLockID, holder, now, now, cutoff)
	if err != nil {
		return false, fmt.Errorf("sqlite: acquire sync lease for %q: %w", holder, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("sqlite: acquire sync lease for %q: %w", holder, err)
	}
	return affected > 0, nil
}

// No row updated means the lease was lost, wrapping repositories.ErrLeaseLost.
func (s *Store) HeartbeatLease(ctx context.Context, holder string) error {
	res, err := s.db.ExecContext(ctx, heartbeatLeaseSQL, formatTime(s.now()), syncLockID, holder)
	if err != nil {
		return fmt.Errorf("sqlite: heartbeat sync lease for %q: %w", holder, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlite: heartbeat sync lease for %q: %w", holder, err)
	}
	if affected == 0 {
		return fmt.Errorf("sqlite: sync lease is not held by %q: %w", holder, repositories.ErrLeaseLost)
	}
	return nil
}

// The holder check is in the statement, so a deferred release after a takeover
// cannot delete the new holder's lease.
func (s *Store) ReleaseLease(ctx context.Context, holder string) error {
	if _, err := s.db.ExecContext(ctx, releaseLeaseSQL, syncLockID, holder); err != nil {
		return fmt.Errorf("sqlite: release sync lease of %q: %w", holder, err)
	}
	return nil
}
