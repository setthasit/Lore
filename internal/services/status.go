package services

import (
	"context"

	"lore/internal/entities"
	"lore/internal/errors/internalerror"
	"lore/internal/repositories"
)

type StatusService interface {
	// A never-synced index reports zeros and no rows, not an error.
	Status(ctx context.Context) (entities.IndexStats, error)
}

type statusService struct {
	store repositories.IndexStore
}

var _ StatusService = (*statusService)(nil)

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
