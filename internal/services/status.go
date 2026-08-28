package services

import (
	"context"

	"lore/internal/entities"
	"lore/internal/errors/internalerror"
	"lore/internal/repositories"
)

// StatusService reports the workspace index's operational state: what it holds,
// how fresh each connector's checkpoint is, and whether a sync round is running.
// It is the read behind `lore status` and the sync_status tool.
//
// It is a service rather than a store read the surfaces make themselves because
// transports call services only (02 — Layering), and because this is where the
// scheduler wave's per-connector run history will land — the surfaces keep
// asking one question and get a fuller answer when there is one.
//
// Errors come back classified: reading the index cannot fail for a reason the
// caller can act on, so a failure here is always internal.
type StatusService interface {
	// Status reports the index's current state. An index that has never synced
	// is zeros and no rows, not an error: a workspace nobody has synced yet is
	// an ordinary thing to ask about.
	Status(ctx context.Context) (entities.IndexStats, error)
}

type statusService struct {
	store repositories.IndexStore
}

var _ StatusService = (*statusService)(nil)

// NewStatusService returns the index-backed StatusService.
func NewStatusService(store repositories.IndexStore) StatusService {
	return &statusService{store: store}
}

func (s *statusService) Status(ctx context.Context) (entities.IndexStats, error) {
	stats, err := s.store.Stats(ctx)
	if err != nil {
		return entities.IndexStats{}, internalerror.NewInternalError("reading the index's state failed", err)
	}
	return stats, nil
}
