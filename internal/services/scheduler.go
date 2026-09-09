package services

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

const (
	roundComplete = "scheduled sync round complete"
	roundPartial  = "scheduled sync round partial: instances did not finish"
)

type Scheduler struct {
	orchestrator SyncOrchestrator
	interval     time.Duration
	log          *slog.Logger
}

func NewScheduler(orchestrator SyncOrchestrator, interval time.Duration, log *slog.Logger) *Scheduler {
	return &Scheduler{orchestrator: orchestrator, interval: interval, log: log}
}

// Blocks until ctx is done; the round in flight is cancelled with it and awaited.
// The first round runs one interval after Run starts, never at startup.
func (s *Scheduler) Run(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// A tick and a cancellation both ready make the select's choice random.
			if ctx.Err() != nil {
				return
			}
			s.round(ctx)
		}
	}
}

func (s *Scheduler) round(ctx context.Context) {
	result, err := s.orchestrator.Sync(ctx, SyncOptions{})
	if ctx.Err() != nil {
		return
	}

	switch {
	case errors.Is(err, ErrSyncLocked):
		s.log.Info("skipped a scheduled sync round: the workspace lease is held", "reason", err)
	case err != nil:
		s.log.Error("scheduled sync round failed", "error", err)
	case len(result.Failures) > 0 && result.TookOverFrom != nil:
		s.log.Error(roundPartial, "failures", instanceCauses(result.Failures),
			"took_over_from", result.TookOverFrom.Holder)
	case len(result.Failures) > 0:
		s.log.Error(roundPartial, "failures", instanceCauses(result.Failures))
	case result.TookOverFrom != nil:
		s.log.Info(roundComplete, "took_over_from", result.TookOverFrom.Holder)
	default:
		s.log.Info(roundComplete)
	}
}

func instanceCauses(failures []InstanceFailure) string {
	causes := make([]string, len(failures))
	for i, failure := range failures {
		causes[i] = fmt.Sprintf("%s: %v", failure.Instance, failure.Err)
	}

	return strings.Join(causes, "; ")
}
