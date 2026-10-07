package services

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/setthasit/Lore/internal/entities"
	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/internal/repositories"
	"github.com/setthasit/Lore/sdk"
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
	sourceNames := make([]string, len(stats.Sources))
	for i, source := range stats.Sources {
		sourceNames[i] = source.ID
	}
	for i, label := range localStatusLabels(sourceNames, "sources[%d] (local id)", isLocalSourceID) {
		stats.Sources[i].ID = label
	}
	stats.Clones = nil
	cloneNames := make([]string, len(s.declared.Clones))
	for i, clone := range s.declared.Clones {
		cloneNames[i] = clone.Name
	}
	cloneLabels := localStatusLabels(cloneNames, "repos[%d] (local remote)", isLocalCloneRemote)
	for i, clone := range s.declared.Clones {
		stats.Clones = append(stats.Clones, entities.CloneState{Name: cloneLabels[i], Synced: clone.Synced})
	}
	return stats, nil
}

func localStatusLabels(names []string, labelFormat string, isLocal func(string) bool) []string {
	used := make(map[string]bool, len(names))
	for _, name := range names {
		used[name] = true
	}
	labels := slices.Clone(names)
	for i, name := range names {
		if !isLocal(name) {
			continue
		}
		for index := i; ; index++ {
			label := fmt.Sprintf(labelFormat, index)
			if used[label] {
				continue
			}
			labels[i] = label
			used[label] = true
			break
		}
	}
	return labels
}

func isLocalSourceID(name string) bool {
	name = strings.TrimSpace(name)
	if hasLocalStatusPathPrefix(name) {
		return true
	}
	if len(name) >= 2 && name[1] == ':' && ((name[0] >= 'a' && name[0] <= 'z') || (name[0] >= 'A' && name[0] <= 'Z')) {
		return true
	}
	return strings.Contains(name, "/")
}

func isLocalCloneRemote(name string) bool {
	name = strings.TrimSpace(name)
	if hasLocalStatusPathPrefix(name) {
		return true
	}
	forge, _, remote := lore.SplitRemote(name)
	if remote && !strings.Contains(forge, "/") {
		return false
	}
	return isLocalSourceID(name)
}

func hasLocalStatusPathPrefix(name string) bool {
	return name == "." || name == ".." || strings.HasPrefix(name, "~") || strings.Contains(name, `\`) || strings.HasPrefix(strings.ToLower(name), "file:")
}

func (s *statusService) EmbedderIdentity(ctx context.Context) (entities.EmbedderIdentity, error) {
	indexed, err := s.store.Meta(ctx, metaKeyEmbedderIdentity)
	if err != nil {
		return entities.EmbedderIdentity{}, internalerror.NewInternalError("reading the index's embedder identity failed", err)
	}
	return entities.EmbedderIdentity{Configured: s.space.String(), Indexed: indexed}, nil
}
