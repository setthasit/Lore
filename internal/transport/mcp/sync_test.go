package mcp

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/mock/gomock"

	"lore/internal/entities"
	"lore/internal/errors/internalerror"
	"lore/internal/services"
	"lore/internal/transport"
)

var syncToolNames = []string{syncNowName, syncStatusName}

// The handler reads its own clock after the case fixed the timestamp, so an age may have ticked on.
const secondsAgoSlack = 2

// The payloads mirror the wire contract rather than the Go types, so a renamed tag fails here.
// Every age is a pointer: a dropped field must not read back as an age of zero.
type acknowledgmentPayload struct {
	Synced       string `json:"synced"`
	TookOverFrom struct {
		Holder                  string `json:"holder"`
		LastHeartbeatSecondsAgo *int64 `json:"last_heartbeat_seconds_ago"`
	} `json:"took_over_from"`
}

type statusPayload struct {
	Documents int64 `json:"documents"`
	Chunks    int64 `json:"chunks"`
	Edges     int64 `json:"edges"`
	Sources   []struct {
		Source                   string `json:"source"`
		LastCheckpointSecondsAgo *int64 `json:"last_checkpoint_seconds_ago"`
	} `json:"sources"`
	SyncLock struct {
		Held                    bool   `json:"held"`
		Holder                  string `json:"holder"`
		HeldForSeconds          *int64 `json:"held_for_seconds"`
		LastHeartbeatSecondsAgo *int64 `json:"last_heartbeat_seconds_ago"`
	} `json:"sync_lock"`
}

func decodeResult[T any](t *testing.T, res *sdk.CallToolResult) T {
	t.Helper()

	if res.IsError {
		t.Fatalf("unexpected tool error: %s", errorText(t, res))
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}

	var decoded T
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal structured content: %v", err)
	}

	return decoded
}

func assertSecondsAgo(t *testing.T, field string, got *int64, want int64) {
	t.Helper()

	if got == nil {
		t.Fatalf("%s is absent from the payload, want an age of %d", field, want)
	}
	if *got < want || *got > want+secondsAgoSlack {
		t.Errorf("%s = %d, want %d", field, *got, want)
	}
}

func TestSyncNowRunsEverySourceWhenNoneIsNamed(t *testing.T) {
	f := newToolFixture(t)
	f.sync.EXPECT().Sync(gomock.Any(), services.SyncOptions{}).Return(services.SyncResult{}, nil)

	assertResultJSON(t, f.callTool(t, syncNowName, map[string]any{}), `{"synced":"all configured sources"}`)
}

func TestSyncNowPassesTheNamedSourceThrough(t *testing.T) {
	f := newToolFixture(t)
	f.sync.EXPECT().Sync(gomock.Any(), services.SyncOptions{Source: "notion"}).Return(services.SyncResult{}, nil)

	assertResultJSON(t, f.callTool(t, syncNowName, map[string]any{"source": "notion"}), `{"synced":"notion"}`)
}

func TestSyncNowReportsTheHolderItDisplaced(t *testing.T) {
	f := newToolFixture(t)
	f.sync.EXPECT().Sync(gomock.Any(), gomock.Any()).Return(services.SyncResult{
		TookOverFrom: &entities.LeaseState{
			Holder:      "host-9/1234",
			HeartbeatAt: time.Now().Add(-4 * time.Minute),
		},
	}, nil)

	ack := decodeResult[acknowledgmentPayload](t, f.callTool(t, syncNowName, map[string]any{}))

	if ack.TookOverFrom.Holder != "host-9/1234" {
		t.Errorf("took_over_from.holder = %q, want host-9/1234", ack.TookOverFrom.Holder)
	}
	assertSecondsAgo(t, "took_over_from.last_heartbeat_seconds_ago", ack.TookOverFrom.LastHeartbeatSecondsAgo, 240)
}

func TestSyncNowSurfacesTheHolderOfAHeldLock(t *testing.T) {
	f := newToolFixture(t)
	f.sync.EXPECT().Sync(gomock.Any(), gomock.Any()).Return(services.SyncResult{},
		internalerror.NewPreconditionError(
			"cannot run a sync round — host-9/1234 (last heartbeat 1m30s ago) is already writing this index",
			services.ErrSyncLocked))

	got := errorText(t, f.callTool(t, syncNowName, map[string]any{}))

	if !strings.Contains(got, "host-9/1234") {
		t.Errorf("error = %q, want it to name the holder", got)
	}
}

func TestSyncNowRejectsAnUnknownSource(t *testing.T) {
	f := newToolFixture(t)
	f.sync.EXPECT().Sync(gomock.Any(), services.SyncOptions{Source: "gitlab"}).Return(services.SyncResult{},
		internalerror.NewBadRequestError(`unknown source "gitlab"; this workspace has github, notion`, nil))

	got := errorText(t, f.callTool(t, syncNowName, map[string]any{"source": "gitlab"}))

	want := `invalid argument: unknown source "gitlab"; this workspace has github, notion`
	if got != want {
		t.Errorf("error = %q, want %q", got, want)
	}
}

func TestSyncStatusReportsCountsCursorsAndTheHeldLock(t *testing.T) {
	f := newToolFixture(t)
	now := time.Now()
	f.status.EXPECT().Status(gomock.Any()).Return(entities.IndexStats{
		Documents: 412,
		Chunks:    3120,
		Edges:     877,
		Cursors: []entities.CursorAge{
			{Connector: "github", UpdatedAt: now.Add(-30 * time.Second)},
			{Connector: "notion", UpdatedAt: now.Add(-2 * time.Hour)},
		},
		Lease: &entities.LeaseState{
			Holder:      "host-1/4242",
			AcquiredAt:  now.Add(-5 * time.Minute),
			HeartbeatAt: now.Add(-10 * time.Second),
		},
	}, nil)

	status := decodeResult[statusPayload](t, f.callTool(t, syncStatusName, map[string]any{}))

	if status.Documents != 412 || status.Chunks != 3120 || status.Edges != 877 {
		t.Errorf("counts = %d/%d/%d, want 412/3120/877", status.Documents, status.Chunks, status.Edges)
	}
	if len(status.Sources) != 2 {
		t.Fatalf("sources = %d, want 2", len(status.Sources))
	}
	if status.Sources[0].Source != "github" || status.Sources[1].Source != "notion" {
		t.Errorf("sources = %+v, want github then notion", status.Sources)
	}
	assertSecondsAgo(t, "sources[0].last_checkpoint_seconds_ago", status.Sources[0].LastCheckpointSecondsAgo, 30)
	assertSecondsAgo(t, "sources[1].last_checkpoint_seconds_ago", status.Sources[1].LastCheckpointSecondsAgo, 7200)

	if !status.SyncLock.Held {
		t.Error("sync_lock.held = false, want true")
	}
	if status.SyncLock.Holder != "host-1/4242" {
		t.Errorf("sync_lock.holder = %q, want host-1/4242", status.SyncLock.Holder)
	}
	assertSecondsAgo(t, "sync_lock.held_for_seconds", status.SyncLock.HeldForSeconds, 300)
	assertSecondsAgo(t, "sync_lock.last_heartbeat_seconds_ago", status.SyncLock.LastHeartbeatSecondsAgo, 10)
}

func TestSyncStatusReportsTheAgesOfALockTakenThisSecond(t *testing.T) {
	f := newToolFixture(t)
	now := time.Now()
	f.status.EXPECT().Status(gomock.Any()).Return(entities.IndexStats{
		Lease: &entities.LeaseState{Holder: "host-1/4242", AcquiredAt: now, HeartbeatAt: now},
	}, nil)

	status := decodeResult[statusPayload](t, f.callTool(t, syncStatusName, map[string]any{}))

	assertSecondsAgo(t, "sync_lock.held_for_seconds", status.SyncLock.HeldForSeconds, 0)
	assertSecondsAgo(t, "sync_lock.last_heartbeat_seconds_ago", status.SyncLock.LastHeartbeatSecondsAgo, 0)
}

func TestSyncStatusReportsAFreeLockAndANeverSyncedIndex(t *testing.T) {
	f := newToolFixture(t)
	f.status.EXPECT().Status(gomock.Any()).Return(entities.IndexStats{}, nil)

	assertResultJSON(t, f.callTool(t, syncStatusName, map[string]any{}),
		`{"documents":0,"chunks":0,"edges":0,"sources":[],"sync_lock":{"held":false}}`)
}

func TestSyncStatusHidesAStoreFailure(t *testing.T) {
	f := newToolFixture(t)
	f.status.EXPECT().Status(gomock.Any()).
		Return(entities.IndexStats{}, internalerror.NewInternalError("reading the index's state failed", errors.New(testCause)))

	got := errorText(t, f.callTool(t, syncStatusName, map[string]any{}))

	if got != transport.InternalErrorMessage {
		t.Errorf("error = %q, want %q", got, transport.InternalErrorMessage)
	}
	if logged := f.logs.String(); !strings.Contains(logged, testCause) {
		t.Errorf("log %q does not record the cause", logged)
	}
}

func TestSyncToolDeclarations(t *testing.T) {
	f := newToolFixture(t)

	writer := f.declaration(t, syncNowName)
	if writer.Annotations != nil && writer.Annotations.ReadOnlyHint {
		t.Errorf("annotations of %s = %+v, want no readOnlyHint: the round writes the index",
			syncNowName, writer.Annotations)
	}
	reader := f.declaration(t, syncStatusName)
	if reader.Annotations == nil || !reader.Annotations.ReadOnlyHint {
		t.Errorf("annotations of %s = %+v, want readOnlyHint", syncStatusName, reader.Annotations)
	}

	for _, word := range []string{"sync_status", "stale", "lock"} {
		if !strings.Contains(writer.Description, word) {
			t.Errorf("description of %s does not route on %q", syncNowName, word)
		}
	}
	for _, word := range []string{"sync_now", "seconds", "last_checkpoint_seconds_ago", "sync_lock.held"} {
		if !strings.Contains(reader.Description, word) {
			t.Errorf("description of %s does not explain %q", syncStatusName, word)
		}
	}
}
