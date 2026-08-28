package cli

import (
	"strings"
	"testing"

	"go.uber.org/mock/gomock"

	"lore/internal/errors/internalerror"
	mock_services "lore/internal/mocks/services"
	"lore/internal/services"
)

func mockSync(t *testing.T) (*Runtime, *mock_services.MockSyncOrchestrator) {
	t.Helper()

	orchestrator := mock_services.NewMockSyncOrchestrator(gomock.NewController(t))
	return &Runtime{Sync: orchestrator}, orchestrator
}

func TestSyncRunsARound(t *testing.T) {
	rt, orchestrator := mockSync(t)
	orchestrator.EXPECT().Sync(gomock.Any(), services.SyncOptions{Reembed: false}).Return(nil)

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
	orchestrator.EXPECT().Sync(gomock.Any(), services.SyncOptions{Reembed: true}).Return(nil)

	res := run(t, rt, "sync", "--reembed")
	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}
}

func TestSyncReportsAHeldLockAsAPrecondition(t *testing.T) {
	rt, orchestrator := mockSync(t)
	held := internalerror.NewPreconditionError("another process holds the sync lock", nil)
	orchestrator.EXPECT().Sync(gomock.Any(), gomock.Any()).Return(held)

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
