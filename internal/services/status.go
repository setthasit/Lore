package services

import (
	"cmp"
	"context"
	"slices"

	"github.com/setthasit/Lore/internal/entities"
	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/internal/repositories"
)

type StatusService interface {
	Status(ctx context.Context) (entities.IndexStats, error)

	// EmbedderIdentity names the vector space the workspace is configured for and
	// the one its index carries; the second is empty before the first sync.
	EmbedderIdentity(ctx context.Context) (entities.EmbedderIdentity, error)
}

type statusService struct {
	store    repositories.IndexStore
	space    VectorSpace
	declared entities.DeclaredWorkspace
}

var _ StatusService = (*statusService)(nil)

func NewStatusService(store repositories.IndexStore, space VectorSpace, declared entities.DeclaredWorkspace) StatusService {
	return &statusService{store: store, space: space, declared: declared}
}

func (s *statusService) Status(ctx context.Context) (entities.IndexStats, error) {
	stats, err := s.store.Stats(ctx)
	if err != nil {
		return entities.IndexStats{}, internalerror.NewInternalError("reading the index's state failed", err)
	}
	sources := make(map[string]entities.SourceState, len(s.declared.Sources)+len(stats.Sources))
	for _, id := range s.declared.Sources {
		sources[id] = entities.SourceState{ID: id, Configured: true}
	}
	for _, source := range stats.Sources {
		source.Configured = sources[source.ID].Configured
		sources[source.ID] = source
	}
	stats.Sources = nil
	for _, source := range sources {
		stats.Sources = append(stats.Sources, source)
	}
	slices.SortFunc(stats.Sources, func(a, b entities.SourceState) int {
		return cmp.Compare(a.ID, b.ID)
	})
	stats.Clones = nil
	for _, clone := range s.declared.Clones {
		stats.Clones = append(stats.Clones, entities.CloneState(clone))
	}
	return stats, nil
}

func (s *statusService) EmbedderIdentity(ctx context.Context) (entities.EmbedderIdentity, error) {
	indexed, err := s.store.Meta(ctx, metaKeyEmbedderIdentity)
	if err != nil {
		return entities.EmbedderIdentity{}, internalerror.NewInternalError("reading the index's embedder identity failed", err)
	}
	return entities.EmbedderIdentity{Configured: s.space.String(), Indexed: indexed}, nil
}
