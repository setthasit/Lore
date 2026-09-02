package services

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

const roundComplete = "scheduled sync round complete"

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
	case result.TookOverFrom != nil:
		s.log.Info(roundComplete, "took_over_from", result.TookOverFrom.Holder)
	default:
		s.log.Info(roundComplete)
	}
}
