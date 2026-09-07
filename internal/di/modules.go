package di

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"go.uber.org/fx"

	"github.com/setthasit/Lore/internal/config"
	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/internal/registry"
	"github.com/setthasit/Lore/internal/repositories"
	"github.com/setthasit/Lore/internal/repositories/sqlite"
	"github.com/setthasit/Lore/internal/services"
	"github.com/setthasit/Lore/sdk"
)

func Workspace(configPath string, compiled *registry.Registry) fx.Option {
	return fx.Options(
		ConfigModule(configPath),
		fx.Supply(registry.Compiled{Registry: compiled}),
		PluginModule,
		RepositoryModule,
		ServiceModule,
	)
}

type WorkspaceDir string

func ConfigModule(configPath string) fx.Option {
	return fx.Module("config",
		fx.Provide(func() (*config.Config, error) { return config.Load(configPath) }),
		fx.Supply(WorkspaceDir(filepath.Dir(configPath))),
	)
}

var RepositoryModule = fx.Module("repository", fx.Provide(newIndexStore))

var PluginModule = fx.Module("plugins", fx.Provide(
	newExternals,
	newWorkspaceRegistry,
	newSources,
	newProviderInstances,
	newEmbedding,
	newEmbedder,
	newVectorSpace,
	newCompleter,
	newClones,
	newCodeRepos,
	newStartupWarnings,
))

var ServiceModule = fx.Module("services", fx.Provide(
	services.NewChunker,
	newQueryService,
	newWhyService,
	services.NewTraceService,
	newImpactService,
	services.NewHistoryService,
	services.NewLinkResolver,
	services.NewSyncOrchestrator,
	services.NewStatusService,
	services.NewSynthesisService,
))

var SchedulerModule = fx.Module("scheduler",
	fx.Provide(DiagnosticLogger, newScheduler),
	fx.Invoke(func(*services.Scheduler) {}),
)

const schedulerStopReserve = time.Second

// Stdout carries the MCP JSON-RPC stream, so diagnostics belong on stderr.
func DiagnosticLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, nil))
}

func newScheduler(
	lc fx.Lifecycle,
	orchestrator services.SyncOrchestrator,
	cfg *config.Config,
	log *slog.Logger,
) *services.Scheduler {
	scheduler := services.NewScheduler(orchestrator, time.Duration(cfg.Scheduler.Interval), log)

	var (
		stop context.CancelFunc
		done chan struct{}
	)
	lc.Append(fx.Hook{
		// The start context is cancelled once startup finishes, so the loop runs on its own.
		OnStart: func(context.Context) error {
			var loop context.Context
			loop, stop = context.WithCancel(context.Background())
			done = make(chan struct{})

			go func() {
				defer close(done)
				scheduler.Run(loop)
			}()
			return nil
		},
		OnStop: func(ctx context.Context) error {
			stop()

			wait, giveUp := schedulerStopBudget(ctx)
			defer giveUp()

			select {
			case <-done:
				return nil
			case <-wait.Done():
				return internalerror.NewInternalError("the sync scheduler did not stop before the shutdown deadline", wait.Err())
			}
		},
	})

	return scheduler
}

// fx abandons the stop hooks below this one once the shutdown context expires, so the
// loop is given every part of the budget except the tail that closing the index needs.
func schedulerStopBudget(ctx context.Context) (context.Context, context.CancelFunc) {
	deadline, ok := ctx.Deadline()
	remaining := time.Until(deadline)
	if !ok || remaining <= 0 {
		return context.WithCancel(ctx)
	}

	return context.WithDeadline(ctx, deadline.Add(-min(schedulerStopReserve, remaining/2)))
}

// The index's vector column is fixed at creation, so it opens only once a provider reports a width.
func newIndexStore(lc fx.Lifecycle, cfg *config.Config, embedding embedding) (repositories.IndexStore, error) {
	dims := embedding.provider.Dimensions()
	if dims <= 0 {
		return nil, internalerror.NewPreconditionError(
			"embedder provider "+embedding.plugin+" reports a vector width of "+
				"zero, so there is no column the index could store vectors in", nil)
	}

	path := cfg.IndexPath
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, internalerror.NewInternalError("cannot create the index directory "+dir, err)
		}
	}

	store, err := sqlite.Open(path, dims)
	if err != nil {
		return nil, internalerror.NewInternalError("cannot open the workspace index at "+path, err)
	}
	lc.Append(fx.Hook{OnStop: func(context.Context) error { return store.Close() }})
	return store, nil
}

func newSources(cfg *config.Config, reg *registry.Registry) ([]lore.Connector, error) {
	instances, err := sourceInstances(cfg)
	if err != nil {
		return nil, err
	}
	return reg.BuildSources(instances)
}

type embedding struct {
	plugin   string
	model    string
	provider lore.Embedder
}

func newEmbedding(cfg *config.Config, reg *registry.Registry, providers providerInstances) (embedding, error) {
	built, err := reg.BuildProvider(registry.Binding{
		Provider:   cfg.Embedder.Provider,
		Model:      cfg.Embedder.Model,
		Dimensions: cfg.Embedder.Dimensions,
		Capability: lore.CapabilityEmbed,
		Field:      "embedder",
	}, providers)
	if err != nil {
		return embedding{}, err
	}
	return embedding{plugin: built.Plugin, model: cfg.Embedder.Model, provider: built.Value.(lore.Embedder)}, nil
}

func newEmbedder(e embedding) lore.Embedder { return e.provider }

func newVectorSpace(e embedding) services.VectorSpace {
	return services.NewVectorSpace(e.plugin, e.model, e.provider.Dimensions())
}

// A workspace with no llm: block resolves to a nil Completer; only synthesis then fails.
func newCompleter(cfg *config.Config, reg *registry.Registry, providers providerInstances) (lore.Completer, error) {
	if cfg.LLM == nil {
		return nil, nil
	}

	built, err := reg.BuildProvider(registry.Binding{
		Provider:   cfg.LLM.Provider,
		Model:      cfg.LLM.Model,
		Dimensions: cfg.LLM.Dimensions,
		Capability: lore.CapabilityComplete,
		Field:      "llm",
	}, providers)
	if err != nil {
		return nil, err
	}
	return built.Value.(lore.Completer), nil
}

func newCodeRepos(reg *registry.Registry, declared clones) ([]services.CodeRepo, error) {
	built, err := reg.BuildCode(declared)
	if err != nil {
		return nil, err
	}

	repos := make([]services.CodeRepo, 0, len(built))
	for _, code := range built {
		repos = append(repos, services.CodeRepo{Path: code.Path, Remote: code.Remote, Repo: code.Repo})
	}
	return repos, nil
}

func newStartupWarnings(sources []lore.Connector, ext externals, declared clones) registry.Warnings {
	return append(ext.warnings, registry.UnmatchedRemotes(declared, sources)...)
}

func sourceInstances(cfg *config.Config) ([]registry.Instance, error) {
	return instances(cfg.Sources, "sources")
}

type providerInstances []registry.Instance

func newProviderInstances(cfg *config.Config) (providerInstances, error) {
	return instances(cfg.Providers, "providers")
}

func instances(declared []config.Instance, block string) ([]registry.Instance, error) {
	out := make([]registry.Instance, 0, len(declared))
	for _, decl := range declared {
		with, err := decl.WithValues()
		if err != nil {
			return nil, err
		}
		out = append(out, registry.Instance{
			ID:    decl.ID,
			Use:   decl.Use,
			With:  with,
			Field: block + "[" + decl.Ident() + "]",
		})
	}
	return out, nil
}

type clones []registry.LocalClone

func newClones(cfg *config.Config) clones {
	out := make(clones, 0, len(cfg.Repos))
	for i, repo := range cfg.Repos {
		out = append(out, registry.LocalClone{
			Path:   repo.Path,
			Use:    repo.Use,
			Remote: repo.Remote,
			Field:  "repos[" + strconv.Itoa(i) + "]",
		})
	}
	return out
}

func newQueryService(store repositories.IndexStore, emb lore.Embedder, cfg *config.Config) services.QueryService {
	return services.NewQueryService(store, emb, queryConfig(cfg))
}

func newImpactService(store repositories.IndexStore, emb lore.Embedder, cfg *config.Config) services.ImpactService {
	return services.NewImpactService(store, emb, queryConfig(cfg))
}

func newWhyService(
	store repositories.IndexStore,
	emb lore.Embedder,
	cfg *config.Config,
	repos []services.CodeRepo,
) services.WhyService {
	return services.NewWhyService(store, emb, queryConfig(cfg), repos)
}

func queryConfig(cfg *config.Config) services.QueryConfig {
	return services.QueryConfig{
		TopK:        cfg.Query.TopK,
		WalkDepth:   cfg.Query.WalkDepth,
		EventWindow: time.Duration(cfg.Query.EventWindow),
	}
}
