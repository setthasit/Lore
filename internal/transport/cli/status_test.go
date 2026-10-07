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

func configuredStatusFixture(now time.Time) entities.IndexStats {
	return entities.IndexStats{
		Documents: 44,
		Chunks:    81,
		Edges:     9,
		Sources: []entities.SourceState{
			{ID: "notion", Configured: true, Documents: 44, LastCheckpoint: now.Add(-1540 * time.Second)},
			{ID: "jira", Configured: true},
		},
		Clones: []entities.CloneState{
			{Name: "github:acme/myproject", Synced: true},
			{Name: "repos[1] (no remote)"},
		},
	}
}

func TestStatusListsConfiguredSourcesAndClonesWithoutLocalPaths(t *testing.T) {
	now := time.Now()
	rt := mockStatus(t, configuredStatusFixture(now), nil)
	res := run(t, rt, "status")
	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}
	for _, want := range []string{
		"documents: 44", "chunks:    81", "edges:     9",
		"notion     44 docs, last checkpoint 25m ago (" + now.Add(-1540*time.Second).UTC().Format(time.RFC3339) + ")",
		"jira       0 docs, never synced",
		"clones:\n  github:acme/myproject synced\n  repos[1] (no remote) not synced by any source",
		"sync lock: free",
	} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("output is missing %q: %s", want, res.stdout)
		}
	}
	for _, field := range strings.Fields(res.stdout) {
		if strings.HasPrefix(field, "/") {
			t.Errorf("output contains a filesystem path %q", field)
		}
	}
	if strings.Contains(res.stdout, "(not configured)") {
		t.Errorf("configured sources are marked unconfigured: %s", res.stdout)
	}
}

func TestStatusMarksIndexedSourcesNoLongerConfigured(t *testing.T) {
	now := time.Now()
	stats := configuredStatusFixture(now)
	stats.Sources[0].Configured = false
	stats.Sources[1].Configured = false
	res := run(t, mockStatus(t, stats, nil), "status")
	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}
	for _, want := range []string{
		"notion     44 docs, last checkpoint 25m ago (" + now.Add(-1540*time.Second).UTC().Format(time.RFC3339) + ") (not configured)",
		"jira       0 docs, never synced (not configured)",
	} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("output is missing %q: %s", want, res.stdout)
		}
	}
}

func TestStatusRendersCountsCheckpointAgesAndLock(t *testing.T) {
	now := time.Now()
	rt := mockStatus(t, entities.IndexStats{
		Documents: 1284,
		Chunks:    9613,
		Edges:     431,
		Sources: []entities.SourceState{
			{ID: "github", Configured: true, Documents: 1240, LastCheckpoint: now.Add(-90 * time.Minute)},
			{ID: "notion", Configured: true, Documents: 44, LastCheckpoint: now.Add(-3 * 24 * time.Hour)},
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
		"github     1240 docs, last checkpoint 1h ago",
		"notion     44 docs, last checkpoint 3d ago",
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
		"sources: none configured or indexed",
		"clones: none registered",
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

func TestStatusRendersSourceIDsAndCloneNamesInert(t *testing.T) {
	const hostile = clearScreen + "\n\t\r\x00\u202e"
	const escaped = clearScreenInert + `\n\t\r\x00\u202e`
	stats := configuredStatusFixture(time.Now())
	stats.Sources[0].ID = "forge" + hostile
	stats.Sources[1].ID = "tracker" + hostile
	stats.Clones[0].Name += hostile
	stats.Clones[1].Name += hostile
	rt := mockStatus(t, stats, nil)

	res := run(t, rt, "status")
	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}
	assertInert(t, res.stdout)
	for _, want := range []string{"forge" + escaped, "tracker" + escaped, "github:acme/myproject" + escaped, "repos[1] (no remote)" + escaped} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("stdout = %q, want %q with control characters escaped", res.stdout, want)
		}
	}
}

func TestStatusRendersALeaseHolderInert(t *testing.T) {
	now := time.Now()
	rt := mockStatus(t, entities.IndexStats{
		Lease: &entities.LeaseState{
			Holder:      "host-1/4242" + clearScreen,
			AcquiredAt:  now.Add(-time.Hour),
			HeartbeatAt: now.Add(-12 * time.Second),
		},
	}, nil)

	res := run(t, rt, "status")
	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}
	assertInert(t, res.stdout)
	if !strings.Contains(res.stdout, "host-1/4242"+clearScreenInert) {
		t.Errorf("stdout = %q, want the holder named with its escape shown", res.stdout)
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
