package sqlite

import (
	"context"
	"fmt"
	"maps"
	"testing"
	"time"

	"lore/internal/entities"
)

func TestCursorRoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	// A connector that has never checkpointed has no cursor, which is not an
	// error: a nil Cursor is where a full sync starts.
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

	// Cursors are per connector and overwritten in place.
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

	// An empty cursor reads back empty, and overwrites the row rather than
	// dropping it: emptiness is a position, not an absence of one.
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

	// The keys describing the file's own identity are not the caller's to write:
	// Open validates them, so overwriting one would make the file unopenable.
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
	s := openTestStore(t)
	ctx := context.Background()

	start := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	clock := start
	s.now = func() time.Time { return clock }

	// First caller takes the lease.
	acquired, err := s.TryAcquireLease(ctx, "daemon")
	if err != nil {
		t.Fatalf("TryAcquireLease(daemon): %v", err)
	}
	if !acquired {
		t.Fatal("daemon did not get a free lease")
	}
	assertLeaseHolder(t, s, "daemon", start)

	// A second caller loses while the lease is fresh, and cannot heartbeat or
	// release someone else's lease.
	if acquired, err = s.TryAcquireLease(ctx, "cli"); err != nil || acquired {
		t.Fatalf("TryAcquireLease(cli) = %v, %v; want false, nil while the lease is held", acquired, err)
	}
	if err = s.HeartbeatLease(ctx, "cli"); err == nil {
		t.Error("HeartbeatLease accepted a non-holder")
	}
	if err = s.ReleaseLease(ctx, "cli"); err != nil {
		t.Errorf("ReleaseLease by a non-holder = %v, want a no-op", err)
	}
	assertLeaseHolder(t, s, "daemon", start)

	// The holder heartbeats halfway through the TTL, pushing the deadline out.
	clock = start.Add(30 * time.Second)
	if err = s.HeartbeatLease(ctx, "daemon"); err != nil {
		t.Fatalf("HeartbeatLease(daemon): %v", err)
	}
	if acquired, err = s.TryAcquireLease(ctx, "cli"); err != nil || acquired {
		t.Fatalf("TryAcquireLease(cli) = %v, %v; want false, nil 30s into the lease", acquired, err)
	}

	// A heartbeat exactly one TTL old is not yet dead.
	clock = start.Add(30*time.Second + leaseTTL)
	if acquired, err = s.TryAcquireLease(ctx, "cli"); err != nil || acquired {
		t.Fatalf("TryAcquireLease(cli) = %v, %v; want false, nil at exactly the TTL", acquired, err)
	}

	// One second later it is, and the lease is taken over.
	clock = clock.Add(time.Second)
	if acquired, err = s.TryAcquireLease(ctx, "cli"); err != nil || !acquired {
		t.Fatalf("TryAcquireLease(cli) = %v, %v; want true, nil past the TTL", acquired, err)
	}
	assertLeaseHolder(t, s, "cli", clock)

	// The dead holder learns it lost the lease from its next heartbeat, and its
	// deferred release leaves the new holder's lease alone.
	if err = s.HeartbeatLease(ctx, "daemon"); err == nil {
		t.Error("HeartbeatLease accepted the holder whose lease was taken over")
	}
	if err = s.ReleaseLease(ctx, "daemon"); err != nil {
		t.Errorf("ReleaseLease after takeover = %v, want a no-op", err)
	}
	assertLeaseHolder(t, s, "cli", clock)
	if acquired, err = s.TryAcquireLease(ctx, "other"); err != nil || acquired {
		t.Fatalf("TryAcquireLease(other) = %v, %v; want false, nil against the fresh takeover", acquired, err)
	}

	// Re-acquiring a lease one already holds succeeds and restarts its clock.
	clock = clock.Add(5 * time.Second)
	if acquired, err = s.TryAcquireLease(ctx, "cli"); err != nil || !acquired {
		t.Fatalf("TryAcquireLease(cli, held by cli) = %v, %v; want true, nil", acquired, err)
	}
	assertLeaseHolder(t, s, "cli", clock)

	// Releasing frees the lease for the next caller.
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
	if err = s.HeartbeatLease(ctx, "cli"); err == nil {
		t.Error("HeartbeatLease accepted a released lease")
	}
	if acquired, err = s.TryAcquireLease(ctx, "other"); err != nil || !acquired {
		t.Fatalf("TryAcquireLease(other) = %v, %v; want true, nil on a free lease", acquired, err)
	}
	assertLeaseHolder(t, s, "other", clock)

	// An unnamed holder would make every ownership check meaningless.
	if _, err = s.TryAcquireLease(ctx, ""); err == nil {
		t.Error("TryAcquireLease accepted an empty holder")
	}
}

// The lease is what keeps a manual sync and a scheduler tick out of each other's
// way across processes, so acquiring it has to be one atomic statement: with a
// read followed by a write, every racing caller reads "free" and every one of
// them writes itself in.
func TestSyncLeaseAdmitsOneWinnerUnderRace(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	s.now = func() time.Time { return time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC) }

	// Releasing a lease nobody holds is a no-op, not an error.
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
