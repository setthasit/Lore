package sqlite

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"testing"
	"time"

	"lore/internal/entities"
	"lore/internal/repositories"
)

func TestCursorRoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	got, err := s.Cursor(ctx, "github")
	if err != nil {
		t.Fatalf("Cursor (missing): %v", err)
	}
	if got != nil {
		t.Errorf("Cursor (missing) = %v, want nil", got)
	}

	want := entities.Cursor{
		"since":  "2025-03-12T09:30:00Z",
		"page":   "3",
		"opaque": `{"nested":"value with spaces, commas & \"quotes\""}`,
	}
	if err := s.SetCursor(ctx, "github", want); err != nil {
		t.Fatalf("SetCursor: %v", err)
	}
	if got, err = s.Cursor(ctx, "github"); err != nil {
		t.Fatalf("Cursor: %v", err)
	}
	if !maps.Equal(got, want) {
		t.Errorf("Cursor = %v, want %v", got, want)
	}

	if got, err = s.Cursor(ctx, "notion"); err != nil || got != nil {
		t.Errorf("Cursor(notion) = %v, %v; want nil, nil", got, err)
	}
	next := entities.Cursor{"since": "2025-04-01T00:00:00Z"}
	if err := s.SetCursor(ctx, "github", next); err != nil {
		t.Fatalf("SetCursor (update): %v", err)
	}
	if got, err = s.Cursor(ctx, "github"); err != nil {
		t.Fatalf("Cursor (after update): %v", err)
	}
	if !maps.Equal(got, next) {
		t.Errorf("Cursor = %v, want %v", got, next)
	}

	var rows int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM cursors`).Scan(&rows); err != nil {
		t.Fatalf("count cursors: %v", err)
	}
	if rows != 1 {
		t.Errorf("cursors rows = %d, want 1", rows)
	}

	// Emptiness is a position, not the absence of one: the row stays.
	if err := s.SetCursor(ctx, "github", nil); err != nil {
		t.Fatalf("SetCursor (nil): %v", err)
	}
	if got, err = s.Cursor(ctx, "github"); err != nil {
		t.Fatalf("Cursor (after nil): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Cursor = %v, want empty", got)
	}
	if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM cursors`).Scan(&rows); err != nil {
		t.Fatalf("count cursors: %v", err)
	}
	if rows != 1 {
		t.Errorf("cursors rows = %d after an empty cursor, want 1", rows)
	}
}

func TestMetaRoundTripAndReservedKeys(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	got, err := s.Meta(ctx, "embedder_model")
	if err != nil {
		t.Fatalf("Meta (missing): %v", err)
	}
	if got != "" {
		t.Errorf("Meta (missing) = %q, want empty", got)
	}

	if err := s.SetMeta(ctx, "embedder_model", "text-embedding-3-small"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	if got, err = s.Meta(ctx, "embedder_model"); err != nil || got != "text-embedding-3-small" {
		t.Fatalf("Meta = %q, %v; want the stored model", got, err)
	}

	if err := s.SetMeta(ctx, "embedder_model", "nomic-embed-text"); err != nil {
		t.Fatalf("SetMeta (update): %v", err)
	}
	if got, err = s.Meta(ctx, "embedder_model"); err != nil || got != "nomic-embed-text" {
		t.Fatalf("Meta = %q, %v; want the updated model", got, err)
	}

	for _, key := range []string{metaKeySchemaVersion, metaKeyVectorDims} {
		if err := s.SetMeta(ctx, key, "999"); err == nil {
			t.Errorf("SetMeta accepted the store-owned key %q", key)
		}
		if got, err = s.Meta(ctx, key); err != nil || got == "999" {
			t.Errorf("Meta(%q) = %q, %v; want the value bootstrap wrote", key, got, err)
		}
	}
}

func TestSyncLeaseExcludesUntilTTLExpires(t *testing.T) {
	start := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	clock := start
	s := openTestStore(t, WithClock(func() time.Time { return clock }))
	ctx := context.Background()

	acquired, err := s.TryAcquireLease(ctx, "daemon")
	if err != nil {
		t.Fatalf("TryAcquireLease(daemon): %v", err)
	}
	if !acquired {
		t.Fatal("daemon did not get a free lease")
	}
	assertLeaseHolder(t, s, "daemon", start)

	if acquired, err = s.TryAcquireLease(ctx, "cli"); err != nil || acquired {
		t.Fatalf("TryAcquireLease(cli) = %v, %v; want false, nil while the lease is held", acquired, err)
	}
	if err = s.HeartbeatLease(ctx, "cli"); !errors.Is(err, repositories.ErrLeaseLost) {
		t.Errorf("HeartbeatLease(non-holder) = %v, want it to wrap ErrLeaseLost", err)
	}
	if err = s.ReleaseLease(ctx, "cli"); err != nil {
		t.Errorf("ReleaseLease by a non-holder = %v, want a no-op", err)
	}
	assertLeaseHolder(t, s, "daemon", start)

	clock = start.Add(30 * time.Second)
	if err = s.HeartbeatLease(ctx, "daemon"); err != nil {
		t.Fatalf("HeartbeatLease(daemon): %v", err)
	}
	if acquired, err = s.TryAcquireLease(ctx, "cli"); err != nil || acquired {
		t.Fatalf("TryAcquireLease(cli) = %v, %v; want false, nil 30s into the lease", acquired, err)
	}

	clock = start.Add(30*time.Second + repositories.LeaseTTL)
	if acquired, err = s.TryAcquireLease(ctx, "cli"); err != nil || acquired {
		t.Fatalf("TryAcquireLease(cli) = %v, %v; want false, nil at exactly the TTL", acquired, err)
	}

	clock = clock.Add(time.Second)
	if acquired, err = s.TryAcquireLease(ctx, "cli"); err != nil || !acquired {
		t.Fatalf("TryAcquireLease(cli) = %v, %v; want true, nil past the TTL", acquired, err)
	}
	assertLeaseHolder(t, s, "cli", clock)

	if err = s.HeartbeatLease(ctx, "daemon"); !errors.Is(err, repositories.ErrLeaseLost) {
		t.Errorf("HeartbeatLease after takeover = %v, want it to wrap ErrLeaseLost", err)
	}
	if err = s.ReleaseLease(ctx, "daemon"); err != nil {
		t.Errorf("ReleaseLease after takeover = %v, want a no-op", err)
	}
	assertLeaseHolder(t, s, "cli", clock)
	if acquired, err = s.TryAcquireLease(ctx, "other"); err != nil || acquired {
		t.Fatalf("TryAcquireLease(other) = %v, %v; want false, nil against the fresh takeover", acquired, err)
	}

	clock = clock.Add(5 * time.Second)
	if acquired, err = s.TryAcquireLease(ctx, "cli"); err != nil || !acquired {
		t.Fatalf("TryAcquireLease(cli, held by cli) = %v, %v; want true, nil", acquired, err)
	}
	assertLeaseHolder(t, s, "cli", clock)

	if err = s.ReleaseLease(ctx, "cli"); err != nil {
		t.Fatalf("ReleaseLease(cli): %v", err)
	}
	var rows int
	if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM sync_lock`).Scan(&rows); err != nil {
		t.Fatalf("count sync_lock: %v", err)
	}
	if rows != 0 {
		t.Errorf("sync_lock rows = %d after release, want 0", rows)
	}
	if err = s.HeartbeatLease(ctx, "cli"); !errors.Is(err, repositories.ErrLeaseLost) {
		t.Errorf("HeartbeatLease on a released lease = %v, want it to wrap ErrLeaseLost", err)
	}
	if acquired, err = s.TryAcquireLease(ctx, "other"); err != nil || !acquired {
		t.Fatalf("TryAcquireLease(other) = %v, %v; want true, nil on a free lease", acquired, err)
	}
	assertLeaseHolder(t, s, "other", clock)

	if _, err = s.TryAcquireLease(ctx, ""); err == nil {
		t.Error("TryAcquireLease accepted an empty holder")
	}
}

func TestLeaseReportsTheCurrentHolder(t *testing.T) {
	start := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	clock := start
	s := openTestStore(t, WithClock(func() time.Time { return clock }))
	ctx := context.Background()

	got, err := s.Lease(ctx)
	if err != nil {
		t.Fatalf("Lease (free): %v", err)
	}
	if got != nil {
		t.Errorf("Lease (free) = %+v, want nil", got)
	}

	if acquired, err := s.TryAcquireLease(ctx, "daemon"); err != nil || !acquired {
		t.Fatalf("TryAcquireLease(daemon) = %v, %v; want true, nil", acquired, err)
	}

	clock = start.Add(30 * time.Second)
	if err := s.HeartbeatLease(ctx, "daemon"); err != nil {
		t.Fatalf("HeartbeatLease(daemon): %v", err)
	}

	clock = start.Add(2 * repositories.LeaseTTL)
	if got, err = s.Lease(ctx); err != nil {
		t.Fatalf("Lease (held): %v", err)
	}
	want := entities.LeaseState{Holder: "daemon", AcquiredAt: start, HeartbeatAt: start.Add(30 * time.Second)}
	if got == nil || *got != want {
		t.Errorf("Lease (lapsed but unreleased) = %+v, want %+v", got, want)
	}

	if err := s.ReleaseLease(ctx, "daemon"); err != nil {
		t.Fatalf("ReleaseLease(daemon): %v", err)
	}
	if got, err = s.Lease(ctx); err != nil || got != nil {
		t.Errorf("Lease (released) = %+v, %v; want nil, nil", got, err)
	}
}

// The failure mode this guards: with a read followed by a write, every racing
// caller reads "free" and every one of them writes itself in.
func TestSyncLeaseAdmitsOneWinnerUnderRace(t *testing.T) {
	frozen := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	s := openTestStore(t, WithClock(func() time.Time { return frozen }))
	ctx := context.Background()

	if err := s.ReleaseLease(ctx, "nobody"); err != nil {
		t.Fatalf("ReleaseLease on a free lease: %v", err)
	}

	const contenders = 8
	results := make(chan bool, contenders)
	errs := make(chan error, contenders)
	start := make(chan struct{})

	for i := range contenders {
		go func() {
			<-start
			acquired, err := s.TryAcquireLease(ctx, fmt.Sprintf("contender-%d", i))
			if err != nil {
				errs <- err
				return
			}
			results <- acquired
		}()
	}
	close(start)

	winners := 0
	for range contenders {
		select {
		case err := <-errs:
			t.Fatalf("TryAcquireLease: %v", err)
		case acquired := <-results:
			if acquired {
				winners++
			}
		}
	}
	if winners != 1 {
		t.Errorf("%d of %d contenders acquired the lease, want exactly 1", winners, contenders)
	}

	var rows int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM sync_lock`).Scan(&rows); err != nil {
		t.Fatalf("count sync_lock: %v", err)
	}
	if rows != 1 {
		t.Errorf("sync_lock rows = %d, want the single lease row", rows)
	}
}

func assertLeaseHolder(t *testing.T, s *Store, holder string, acquired time.Time) {
	t.Helper()

	var gotHolder, gotAcquired, gotHeartbeat string
	err := s.db.QueryRowContext(context.Background(),
		`SELECT holder, acquired_at, heartbeat_at FROM sync_lock WHERE id = ?`, syncLockID).
		Scan(&gotHolder, &gotAcquired, &gotHeartbeat)
	if err != nil {
		t.Fatalf("read sync_lock: %v", err)
	}
	if gotHolder != holder {
		t.Errorf("lease holder = %q, want %q", gotHolder, holder)
	}
	if want := formatTime(acquired); gotAcquired != want {
		t.Errorf("acquired_at = %q, want %q", gotAcquired, want)
	}
	if gotHeartbeat < gotAcquired {
		t.Errorf("heartbeat_at = %q, want it no older than acquired_at %q", gotHeartbeat, gotAcquired)
	}
}
