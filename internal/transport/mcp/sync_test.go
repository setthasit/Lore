package mcp

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/mock/gomock"

	"github.com/setthasit/Lore/internal/entities"
	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/internal/services"
	"github.com/setthasit/Lore/internal/transport"
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
		Configured               *bool  `json:"configured"`
		Documents                *int64 `json:"documents"`
		LastCheckpointSecondsAgo *int64 `json:"last_checkpoint_seconds_ago"`
		NeverSynced              *bool  `json:"never_synced"`
	} `json:"sources"`
	Clones []struct {
		Name   string `json:"name"`
		Synced *bool  `json:"synced"`
	} `json:"clones"`
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

func TestSyncNowNamesEveryFailedInstanceAndLogsEachCause(t *testing.T) {
	const (
		expired = "the forge token expired at its last checkpoint; re-run lore auth"
		stalled = "write tcp 10.1.2.3:5432: broken pipe"
	)

	f := newToolFixture(t)
	f.sync.EXPECT().Sync(gomock.Any(), gomock.Any()).Return(services.SyncResult{
		Failures: []services.InstanceFailure{
			{Instance: "forge", Err: internalerror.NewPreconditionError(expired, errors.New(testCause))},
			{Instance: "tracker", Err: internalerror.NewInternalError("committing the tracker batch failed", errors.New(stalled))},
		},
	}, nil)

	res := f.callTool(t, syncNowName, map[string]any{})

	assertResultJSON(t, res, `{"synced":"all configured sources","failures":[`+
		`{"instance":"forge","error":"`+expired+`"},`+
		`{"instance":"tracker","error":"`+transport.InternalErrorMessage+`"}]}`)

	logged := f.logs.String()
	for _, want := range []string{
		"level=ERROR", syncNowName + " instance failed", "instance=forge", testCause, "instance=tracker", stalled,
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("log %q does not record %q", logged, want)
		}
	}
}

func TestSyncNowHidesAnInternalInstanceCauseButLogsIt(t *testing.T) {
	f := newToolFixture(t)
	f.sync.EXPECT().Sync(gomock.Any(), gomock.Any()).Return(services.SyncResult{
		Failures: []services.InstanceFailure{
			{Instance: "forge", Err: internalerror.NewInternalError("committing the forge batch failed", errors.New(testCause))},
		},
	}, nil)

	res := f.callTool(t, syncNowName, map[string]any{})

	assertResultJSON(t, res, `{"synced":"all configured sources","failures":[{"instance":"forge","error":"`+
		transport.InternalErrorMessage+`"}]}`)

	wire, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal the tool result: %v", err)
	}
	if strings.Contains(string(wire), testCause) {
		t.Errorf("result %s leaks the cause", wire)
	}
	logged := f.logs.String()
	if !strings.Contains(logged, testCause) {
		t.Errorf("log %q does not record the cause", logged)
	}
	for _, want := range []string{"level=ERROR", syncNowName + " instance failed", "instance=forge"} {
		if !strings.Contains(logged, want) {
			t.Errorf("log %q does not attribute the failure: no %q", logged, want)
		}
	}
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

func TestSyncStatusListsConfiguredSourcesAndClonesWithoutLocalPaths(t *testing.T) {
	f := newToolFixture(t)
	f.status.EXPECT().Status(gomock.Any()).Return(configuredStatusFixture(time.Now()), nil)
	res := f.callTool(t, syncStatusName, map[string]any{})
	status := decodeResult[statusPayload](t, res)
	if status.Documents != 44 || status.Chunks != 81 || status.Edges != 9 {
		t.Errorf("counts = %d/%d/%d, want 44/81/9", status.Documents, status.Chunks, status.Edges)
	}
	if len(status.Sources) != 2 || len(status.Clones) != 2 {
		t.Fatalf("status = %+v, want both sources and both clones", status)
	}
	for i, want := range []struct {
		id        string
		documents int64
	}{{"notion", 44}, {"jira", 0}} {
		source := status.Sources[i]
		if source.Source != want.id || source.Configured == nil || !*source.Configured || source.Documents == nil || *source.Documents != want.documents {
			t.Errorf("source = %+v, want configured %s with %d documents", source, want.id, want.documents)
		}
	}
	assertSecondsAgo(t, "notion.last_checkpoint_seconds_ago", status.Sources[0].LastCheckpointSecondsAgo, 1540)
	if status.Sources[0].NeverSynced != nil {
		t.Errorf("notion never_synced = %v, want omitted", status.Sources[0].NeverSynced)
	}
	jira := status.Sources[1]
	if jira.LastCheckpointSecondsAgo != nil || jira.NeverSynced == nil || !*jira.NeverSynced {
		t.Errorf("jira = %+v, want never_synced true and no age", jira)
	}
	for i, want := range []entities.CloneState{{Name: "github:acme/myproject", Synced: true}, {Name: "repos[1] (no remote)"}} {
		clone := status.Clones[i]
		if clone.Name != want.Name || clone.Synced == nil || *clone.Synced != want.Synced {
			t.Errorf("clone = %+v, want %+v with explicit synced", clone, want)
		}
	}
	wire, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal status: %v", err)
	}
	if strings.Contains(string(wire), `"/`) || strings.Contains(string(wire), `\"/`) {
		t.Errorf("status contains a filesystem path: %s", wire)
	}
}

func TestSyncStatusIncludesZeroAgesAndUnconfiguredSources(t *testing.T) {
	f := newToolFixture(t)
	stats := configuredStatusFixture(time.Now())
	stats.Sources[0].Configured = false
	stats.Sources[0].LastCheckpoint = time.Now()
	stats.Sources[1].Configured = false
	f.status.EXPECT().Status(gomock.Any()).Return(stats, nil)
	status := decodeResult[statusPayload](t, f.callTool(t, syncStatusName, map[string]any{}))
	if len(status.Sources) != 2 {
		t.Fatalf("sources = %+v, want two", status.Sources)
	}
	for _, source := range status.Sources {
		if source.Configured == nil || *source.Configured {
			t.Errorf("source = %+v, want explicit configured false", source)
		}
	}
	assertSecondsAgo(t, "notion.last_checkpoint_seconds_ago", status.Sources[0].LastCheckpointSecondsAgo, 0)
}

func TestSyncStatusPreservesHostileNamesAsJSONData(t *testing.T) {
	const hostile = "\x1b[2J\n\t\r\x00"
	f := newToolFixture(t)
	stats := configuredStatusFixture(time.Now())
	stats.Sources[0].ID += hostile
	stats.Clones[0].Name += hostile
	f.status.EXPECT().Status(gomock.Any()).Return(stats, nil)
	res := f.callTool(t, syncStatusName, map[string]any{})
	status := decodeResult[statusPayload](t, res)
	if len(status.Sources) != 2 || len(status.Clones) != 2 {
		t.Fatalf("status = %+v, want both sources and both clones", status)
	}
	if status.Sources[0].Source != "notion"+hostile || status.Clones[0].Name != "github:acme/myproject"+hostile {
		t.Errorf("status = %+v, want hostile names preserved as data", status)
	}
	wire, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal status: %v", err)
	}
	if strings.ContainsAny(string(wire), "\x1b\n\t\r\x00") {
		t.Errorf("status JSON contains raw control characters: %q", wire)
	}
}

func TestSyncStatusReportsCountsCheckpointsAndTheHeldLock(t *testing.T) {
	f := newToolFixture(t)
	now := time.Now()
	f.status.EXPECT().Status(gomock.Any()).Return(entities.IndexStats{
		Documents: 412,
		Chunks:    3120,
		Edges:     877,
		Sources: []entities.SourceState{
			{ID: "github", Configured: true, LastCheckpoint: now.Add(-30 * time.Second)},
			{ID: "notion", Configured: true, LastCheckpoint: now.Add(-2 * time.Hour)},
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
		`{"documents":0,"chunks":0,"edges":0,"sources":[],"clones":[],"sync_lock":{"held":false}}`)
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
	for _, word := range []string{"sync_now", "seconds", "last_checkpoint_seconds_ago", "never_synced", "configured", "clones", "remote", "sync_lock.held"} {
		if !strings.Contains(reader.Description, word) {
			t.Errorf("description of %s does not explain %q", syncStatusName, word)
		}
	}
}
