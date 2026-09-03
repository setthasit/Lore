package cli

import (
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
