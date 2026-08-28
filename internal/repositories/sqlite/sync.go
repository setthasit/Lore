package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"lore/internal/entities"
)

// leaseTTL is how long a lease survives without a heartbeat. Past it the holder
// is presumed dead and any process may take the lease over, so a crashed sync
// never wedges the scheduler.
const leaseTTL = 60 * time.Second

// syncLockID is the id of the one row sync_lock is allowed to hold.
const syncLockID = 1

const (
	selectCursorSQL = `SELECT payload FROM cursors WHERE connector = ?`
	upsertCursorSQL = `
INSERT INTO cursors (connector, payload) VALUES (?, ?)
ON CONFLICT(connector) DO UPDATE SET payload = excluded.payload`

	selectMetaSQL = `SELECT value FROM meta WHERE key = ?`
	upsertMetaSQL = `
INSERT INTO meta (key, value) VALUES (?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value`
)

// Cursor returns the connector's stored sync position, or a nil Cursor when the
// connector has never checkpointed (or checkpointed a nil cursor) — to a
// connector both mean "start from the beginning".
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

// SetCursor stores the connector's sync position. The Cursor is opaque to the
// store, so it is persisted as a JSON object and handed back unchanged.
func (s *Store) SetCursor(ctx context.Context, connector string, c entities.Cursor) error {
	payload, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("sqlite: encode cursor of %q: %w", connector, err)
	}
	if _, err := s.db.ExecContext(ctx, upsertCursorSQL, connector, string(payload)); err != nil {
		return fmt.Errorf("sqlite: write cursor of %q: %w", connector, err)
	}
	return nil
}

// Meta returns the value stored under key, or "" when the key is unset.
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

// SetMeta stores value under key.
//
// The keys describing the file's own identity — schema generation and vector
// width — are refused: Open compares them against this build and a file whose
// recorded identity no longer matches its contents is unopenable, which is not
// something a caller writing embedder identity should be able to do by accident.
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

// acquireLeaseSQL takes the lease in one statement, so two processes racing for
// it cannot both win: exactly one of them inserts the row or passes the DO UPDATE
// guard, and the loser sees no affected row. Reading the lease first and writing
// it after would leave exactly that race open.
//
// The guard admits two cases: the caller already holds the lease (re-entering
// with a fresh acquired_at is how a new round starts), or the current lease's
// heartbeat has fallen behind the TTL cutoff and is presumed dead.
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

// TryAcquireLease takes the sync lease for holder, reporting whether it now holds
// it. A lease whose heartbeat is older than the TTL is taken over.
func (s *Store) TryAcquireLease(ctx context.Context, holder string) (bool, error) {
	if holder == "" {
		return false, errors.New("sqlite: lease holder must be named")
	}

	at := s.now()
	now, cutoff := formatTime(at), formatTime(at.Add(-leaseTTL))

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

// HeartbeatLease refreshes the lease's expiry. It fails when holder is not the
// current holder — which is how a holder learns its lease was taken over and its
// round must stop.
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
		return fmt.Errorf("sqlite: sync lease is not held by %q", holder)
	}
	return nil
}

// ReleaseLease drops the lease if holder still owns it, and does nothing
// otherwise: the holder check is in the statement, so a deferred release after a
// takeover cannot delete the new holder's lease, and a release that finds nothing
// to drop is the expected outcome rather than an error the caller must handle.
func (s *Store) ReleaseLease(ctx context.Context, holder string) error {
	if _, err := s.db.ExecContext(ctx, releaseLeaseSQL, syncLockID, holder); err != nil {
		return fmt.Errorf("sqlite: release sync lease of %q: %w", holder, err)
	}
	return nil
}
