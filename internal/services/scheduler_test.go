package services

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"lore/internal/entities"
	"lore/internal/errors/internalerror"
)

const (
	// Short enough to keep the suite fast; every assertion waits on a channel, never on this.
	schedulerTick = 2 * time.Millisecond

	// Bounds a channel wait so a stalled loop fails by name, not by test-binary timeout.
	schedulerStall = 5 * time.Second

	schedulerQuiet = 100 * time.Millisecond
)

type roundOutcome struct {
	result SyncResult
	err    error
}

// A round parks until the test ends it, so a tick can never outrun the assertions.
type roundCall struct {
	ctx  context.Context
	ends chan roundOutcome
}

func (c roundCall) end(result SyncResult, err error) {
	c.ends <- roundOutcome{result: result, err: err}
}

type scriptedOrchestrator struct {
	calls     chan roundCall
	abandoned chan struct{}
}

var _ SyncOrchestrator = (*scriptedOrchestrator)(nil)

func (o *scriptedOrchestrator) Sync(ctx context.Context, _ SyncOptions) (SyncResult, error) {
	call := roundCall{ctx: ctx, ends: make(chan roundOutcome)}

	select {
	case o.calls <- call:
	case <-o.abandoned:
		return SyncResult{}, ctx.Err()
	}

	select {
	case outcome := <-call.ends:
		return outcome.result, outcome.err
	case <-o.abandoned:
		return SyncResult{}, ctx.Err()
	}
}

// The loop logs from its own goroutine while the test reads.
type syncedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *syncedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

type scheduledLoop struct {
	orchestrator *scriptedOrchestrator
	logs         *syncedBuffer
	stop         context.CancelFunc
	returned     chan struct{}
}

func startScheduler(t *testing.T, interval time.Duration) *scheduledLoop {
	t.Helper()

	loop := &scheduledLoop{
		orchestrator: &scriptedOrchestrator{calls: make(chan roundCall), abandoned: make(chan struct{})},
		logs:         &syncedBuffer{},
		returned:     make(chan struct{}),
	}

	ctx, cancel := context.WithCancel(context.Background())
	loop.stop = cancel

	log := slog.New(slog.NewTextHandler(loop.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	scheduler := NewScheduler(loop.orchestrator, interval, log)

	go func() {
		defer close(loop.returned)
		scheduler.Run(ctx)
	}()

	t.Cleanup(func() {
		cancel()
		close(loop.orchestrator.abandoned)
		loop.awaitReturn(t)
	})

	return loop
}

func (l *scheduledLoop) round(t *testing.T) roundCall {
	t.Helper()

	select {
	case call := <-l.orchestrator.calls:
		return call
	case <-time.After(schedulerStall):
		t.Fatalf("no sync round started within %s", schedulerStall)
		return roundCall{}
	}
}

func (l *scheduledLoop) expectNoRound(t *testing.T) {
	t.Helper()

	select {
	case <-l.orchestrator.calls:
		t.Fatalf("a sync round started within %s, want none", schedulerQuiet)
	case <-time.After(schedulerQuiet):
	}
}

// The next round starting proves every earlier round has been logged.
func (l *scheduledLoop) logsOnceRoundsAreLogged(t *testing.T) string {
	t.Helper()

	call := l.round(t)
	logs := l.logs.String()
	call.end(SyncResult{}, nil)

	return logs
}

func (l *scheduledLoop) awaitReturn(t *testing.T) {
	t.Helper()

	select {
	case <-l.returned:
	case <-time.After(schedulerStall):
		t.Errorf("Run did not return within %s of its context being cancelled", schedulerStall)
	}
}

func leaseHeld() error {
	return internalerror.NewPreconditionError("cannot run a sync round",
		fmt.Errorf("%w — host-b/2002 is already writing this index", ErrSyncLocked))
}

func TestSchedulerRunsARoundOnEveryTick(t *testing.T) {
	loop := startScheduler(t, schedulerTick)

	loop.round(t).end(SyncResult{}, nil)

	logs := loop.logsOnceRoundsAreLogged(t)
	if want := "scheduled sync round complete"; !strings.Contains(logs, want) {
		t.Errorf("logs = %q, want a line containing %q", logs, want)
	}
}

func TestSchedulerRunsNoRoundBeforeTheFirstIntervalElapses(t *testing.T) {
	loop := startScheduler(t, time.Hour)

	loop.expectNoRound(t)
}

func TestSchedulerLogsAHeldLeaseAsASkipAndTicksOn(t *testing.T) {
	loop := startScheduler(t, schedulerTick)

	loop.round(t).end(SyncResult{}, leaseHeld())
	// Reaching a second round proves the skip did not end the loop.
	loop.round(t).end(SyncResult{}, nil)

	logs := loop.logsOnceRoundsAreLogged(t)
	skip := logLine(t, logs, "skipped a scheduled sync round")
	if !strings.Contains(skip, "level=INFO") {
		t.Errorf("the skip line = %q, want it logged at INFO", skip)
	}
	if !strings.Contains(logs, "scheduled sync round complete") {
		t.Errorf("logs = %q, want the round after the skip to have completed", logs)
	}
}

func TestSchedulerLogsAFailedRoundAndTicksOn(t *testing.T) {
	loop := startScheduler(t, schedulerTick)

	loop.round(t).end(SyncResult{}, internalerror.NewInternalError("the index is unreadable", nil))

	failure := logLine(t, loop.logsOnceRoundsAreLogged(t), "scheduled sync round failed")
	if !strings.Contains(failure, "level=ERROR") {
		t.Errorf("the failure line = %q, want it logged at ERROR", failure)
	}
	if !strings.Contains(failure, "the index is unreadable") {
		t.Errorf("the failure line = %q, want it to carry the error", failure)
	}
}

func TestSchedulerNamesTheDeadHolderItTookOverFrom(t *testing.T) {
	loop := startScheduler(t, schedulerTick)

	dead := &entities.LeaseState{Holder: "host-c/3003"}
	loop.round(t).end(SyncResult{TookOverFrom: dead}, nil)

	complete := logLine(t, loop.logsOnceRoundsAreLogged(t), "scheduled sync round complete")
	if !strings.Contains(complete, dead.Holder) {
		t.Errorf("the completion line = %q, want it to name %q", complete, dead.Holder)
	}
}

func TestSchedulerReturnsOnlyOnceTheRoundInFlightHasStopped(t *testing.T) {
	loop := startScheduler(t, schedulerTick)
	call := loop.round(t)

	loop.stop()

	select {
	case <-call.ctx.Done():
	case <-time.After(schedulerStall):
		t.Fatalf("the round in flight was not cancelled within %s of the loop being cancelled", schedulerStall)
	}

	select {
	case <-loop.returned:
		t.Fatal("Run returned while its round was still in flight")
	case <-time.After(schedulerQuiet):
	}

	call.end(SyncResult{}, context.Canceled)
	loop.awaitReturn(t)
}

func logLine(t *testing.T, logs, message string) string {
	t.Helper()

	for line := range strings.SplitSeq(strings.TrimSpace(logs), "\n") {
		if strings.Contains(line, message) {
			return line
		}
	}
	t.Fatalf("logs = %q, want a line containing %q", logs, message)

	return ""
}
