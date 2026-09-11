package cli

import (
	"errors"
	"strings"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/setthasit/Lore/internal/entities"
	"github.com/setthasit/Lore/internal/errors/internalerror"
	mock_services "github.com/setthasit/Lore/internal/mocks/services"
	"github.com/setthasit/Lore/internal/services"
)

func mockSync(t *testing.T) (*Runtime, *mock_services.MockSyncOrchestrator) {
	t.Helper()

	orchestrator := mock_services.NewMockSyncOrchestrator(gomock.NewController(t))
	return &Runtime{Sync: orchestrator}, orchestrator
}

func TestSyncRunsARound(t *testing.T) {
	rt, orchestrator := mockSync(t)
	orchestrator.EXPECT().Sync(gomock.Any(), services.SyncOptions{Reembed: false}).Return(services.SyncResult{}, nil)

	res := run(t, rt, "sync")
	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}
	if !strings.Contains(res.stdout, "sync complete") {
		t.Errorf("stdout = %q, want a completion line", res.stdout)
	}
	if !res.released {
		t.Error("the workspace was not released")
	}
}

func TestSyncPassesReembedThrough(t *testing.T) {
	rt, orchestrator := mockSync(t)
	orchestrator.EXPECT().Sync(gomock.Any(), services.SyncOptions{Reembed: true}).Return(services.SyncResult{}, nil)

	res := run(t, rt, "sync", "--reembed")
	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}
}

func TestSyncPassesTheSourceSelectorThrough(t *testing.T) {
	rt, orchestrator := mockSync(t)
	orchestrator.EXPECT().Sync(gomock.Any(), services.SyncOptions{Source: "jira"}).Return(services.SyncResult{}, nil)

	res := run(t, rt, "sync", "--source", "jira")
	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}
}

func TestSyncRejectsAScopedReembed(t *testing.T) {
	rt, orchestrator := mockSync(t)
	refused := internalerror.NewBadRequestError("cannot re-embed a single source", nil)
	orchestrator.EXPECT().Sync(gomock.Any(), services.SyncOptions{Source: "jira", Reembed: true}).
		Return(services.SyncResult{}, refused)

	res := run(t, rt, "sync", "--source", "jira", "--reembed")
	if res.exitCode != exitBadRequest {
		t.Fatalf("exit = %d, want %d", res.exitCode, exitBadRequest)
	}
	if !strings.Contains(res.stderr, "cannot re-embed a single source") {
		t.Errorf("stderr = %q, want the refusal", res.stderr)
	}
}

func TestSyncReportsAHeldLockAsAPrecondition(t *testing.T) {
	rt, orchestrator := mockSync(t)
	held := internalerror.NewPreconditionError("another process holds the sync lock", nil)
	orchestrator.EXPECT().Sync(gomock.Any(), gomock.Any()).Return(services.SyncResult{}, held)

	res := run(t, rt, "sync")
	if res.exitCode != exitPrecondition {
		t.Fatalf("exit = %d, want %d", res.exitCode, exitPrecondition)
	}
	if !strings.Contains(res.stderr, "holds the sync lock") {
		t.Errorf("stderr = %q, want the lock message", res.stderr)
	}
	if strings.Contains(res.stdout, "sync complete") {
		t.Errorf("stdout = %q, want no completion line for a round that never ran", res.stdout)
	}
}

func TestSyncReportsATakeover(t *testing.T) {
	rt, orchestrator := mockSync(t)
	took := services.SyncResult{TookOverFrom: &entities.LeaseState{
		Holder:      "host-9/1234",
		AcquiredAt:  time.Now().Add(-5 * time.Minute),
		HeartbeatAt: time.Now().Add(-3 * time.Minute),
	}}
	orchestrator.EXPECT().Sync(gomock.Any(), gomock.Any()).Return(took, nil)

	res := run(t, rt, "sync")
	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}
	for _, want := range []string{"took over", "host-9/1234"} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("stdout = %q, want it to name %q", res.stdout, want)
		}
	}
}

func TestSyncStaysSilentWithoutATakeover(t *testing.T) {
	rt, orchestrator := mockSync(t)
	orchestrator.EXPECT().Sync(gomock.Any(), gomock.Any()).Return(services.SyncResult{}, nil)

	res := run(t, rt, "sync")
	if strings.Contains(res.stdout, "took over") {
		t.Errorf("stdout = %q, want no takeover line", res.stdout)
	}
}

func TestSyncCountsTheInstancesThatDidNotFinish(t *testing.T) {
	for _, tc := range []struct {
		name     string
		failures []services.InstanceFailure
		want     string
	}{
		{
			name:     "one",
			failures: []services.InstanceFailure{{Instance: "forge", Err: errors.New("read timed out")}},
			want:     "1 source did not finish this round",
		},
		{
			name: "two",
			failures: []services.InstanceFailure{
				{Instance: "forge", Err: errors.New("read timed out")},
				{Instance: "tracker", Err: errors.New("read timed out")},
			},
			want: "2 sources did not finish this round",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt, orchestrator := mockSync(t)
			orchestrator.EXPECT().Sync(gomock.Any(), gomock.Any()).
				Return(services.SyncResult{Failures: tc.failures}, nil)

			res := run(t, rt, "sync")
			if res.exitCode != exitInternal {
				t.Fatalf("exit = %d, want %d", res.exitCode, exitInternal)
			}
			if !strings.Contains(res.stderr, tc.want) {
				t.Errorf("stderr = %q, want %q", res.stderr, tc.want)
			}
		})
	}
}

func TestSyncNamesEveryInstanceThatFailed(t *testing.T) {
	rt, orchestrator := mockSync(t)
	orchestrator.EXPECT().Sync(gomock.Any(), gomock.Any()).Return(services.SyncResult{
		Failures: []services.InstanceFailure{
			{Instance: "forge", Err: errors.New("read timed out")},
			{Instance: "tracker", Err: internalerror.NewPreconditionError("token expired", nil)},
		},
	}, nil)

	res := run(t, rt, "sync")
	if res.exitCode != exitInternal {
		t.Fatalf("exit = %d, want %d, stdout = %q", res.exitCode, exitInternal, res.stdout)
	}
	for _, want := range []string{
		"forge failed at its last checkpoint — read timed out",
		"tracker failed at its last checkpoint — token expired",
		"the remaining sources are committed",
	} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("stdout = %q, want it to contain %q", res.stdout, want)
		}
	}
}
