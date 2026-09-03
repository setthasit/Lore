package cli

import (
	"strings"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/setthasit/Lore/internal/entities"
	"github.com/setthasit/Lore/internal/errors/internalerror"
	mock_services "github.com/setthasit/Lore/internal/mocks/services"
)

func mockStatus(t *testing.T, stats entities.IndexStats, err error) *Runtime {
	t.Helper()

	status := mock_services.NewMockStatusService(gomock.NewController(t))
	status.EXPECT().Status(gomock.Any()).Return(stats, err)
	return &Runtime{Status: status}
}

func TestStatusRendersCountsCursorAgesAndLock(t *testing.T) {
	// Timestamps are relative to now, so the humanized ages are deterministic.
	now := time.Now()
	rt := mockStatus(t, entities.IndexStats{
		Documents: 1284,
		Chunks:    9613,
		Edges:     431,
		Cursors: []entities.CursorAge{
			{Connector: "github", UpdatedAt: now.Add(-90 * time.Minute)},
			{Connector: "notion", UpdatedAt: now.Add(-3 * 24 * time.Hour)},
		},
		Lease: &entities.LeaseState{
			Holder:      "host-1/4242",
			AcquiredAt:  time.Date(2025, time.March, 12, 9, 30, 0, 0, time.UTC),
			HeartbeatAt: now.Add(-12 * time.Second),
		},
	}, nil)

	res := run(t, rt, "status")
	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}
	if !res.released {
		t.Error("the workspace was not released")
	}

	for _, want := range []string{
		"documents: 1284",
		"chunks:    9613",
		"edges:     431",
		"github     last checkpoint 1h ago",
		"notion     last checkpoint 3d ago",
		"sync lock: held by host-1/4242 since 2025-03-12T09:30:00Z, heartbeat 12s ago",
	} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("output is missing %q\n--- output ---\n%s", want, res.stdout)
		}
	}
}

func TestStatusOnAnUnsyncedWorkspace(t *testing.T) {
	rt := mockStatus(t, entities.IndexStats{}, nil)

	res := run(t, rt, "status")
	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}
	for _, want := range []string{
		"documents: 0",
		"chunks:    0",
		"edges:     0",
		"none have checkpointed yet",
		"sync lock: free",
	} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("output is missing %q\n--- output ---\n%s", want, res.stdout)
		}
	}
}

func TestStatusReportsAFailedRead(t *testing.T) {
	rt := mockStatus(t, entities.IndexStats{}, internalerror.NewInternalError("reading the index's state failed", errUnclassified))

	res := run(t, rt, "status")
	if res.exitCode != exitInternal {
		t.Fatalf("exit = %d, want %d", res.exitCode, exitInternal)
	}
	if !strings.Contains(res.stderr, "reading the index's state failed") {
		t.Errorf("stderr = %q, want the failure", res.stderr)
	}
}

func TestHumanizeAge(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{-time.Hour, "just now"},
		{2 * time.Second, "just now"},
		{45 * time.Second, "45s ago"},
		{90 * time.Second, "1m ago"},
		{90 * time.Minute, "1h ago"},
		{47 * time.Hour, "47h ago"},
		{72 * time.Hour, "3d ago"},
	}
	for _, c := range cases {
		if got := humanizeAge(c.d); got != c.want {
			t.Errorf("humanizeAge(%s) = %q, want %q", c.d, got, c.want)
		}
	}
}
