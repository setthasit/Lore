package services

import (
	"context"

	"github.com/setthasit/Lore/internal/connectors/embedder"
	"github.com/setthasit/Lore/internal/entities"
	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/internal/repositories"
)

type StatusService interface {
	// A never-synced index reports zeros and no rows, not an error.
	Status(ctx context.Context) (entities.IndexStats, error)

	// EmbedderIdentity names the vector space the workspace is configured for and
	// the one its index carries; the second is empty before the first sync.
	EmbedderIdentity(ctx context.Context) (entities.EmbedderIdentity, error)
}

type statusService struct {
	store repositories.IndexStore
	emb   embedder.Embedder
}

var _ StatusService = (*statusService)(nil)

func NewStatusService(store repositories.IndexStore, emb embedder.Embedder) StatusService {
	return &statusService{store: store, emb: emb}
}

func (s *statusService) Status(ctx context.Context) (entities.IndexStats, error) {
	stats, err := s.store.Stats(ctx)
	if err != nil {
		return entities.IndexStats{}, internalerror.NewInternalError("reading the index's state failed", err)
	}
	return stats, nil
}

func (s *statusService) EmbedderIdentity(ctx context.Context) (entities.EmbedderIdentity, error) {
	indexed, err := s.store.Meta(ctx, metaKeyEmbedderIdentity)
	if err != nil {
		return entities.EmbedderIdentity{}, internalerror.NewInternalError("reading the index's embedder identity failed", err)
	}
	return entities.EmbedderIdentity{Configured: s.emb.Identity(), Indexed: indexed}, nil
}
