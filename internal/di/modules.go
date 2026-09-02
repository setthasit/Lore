package di

import (
	"context"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"go.uber.org/fx"

	"lore/internal/config"
	"lore/internal/connectors/embedder"
	"lore/internal/connectors/embedder/openai"
	"lore/internal/connectors/github"
	"lore/internal/connectors/gitrepo"
	"lore/internal/connectors/jira"
	"lore/internal/connectors/notion"
	"lore/internal/entities"
	"lore/internal/errors/internalerror"
	"lore/internal/repositories"
	"lore/internal/repositories/sqlite"
	"lore/internal/services"
)

func Workspace(configPath string) fx.Option {
	return fx.Options(
		ConfigModule(configPath),
		EmbedderModule,
		RepositoryModule,
		ConnectorModule,
		ServiceModule,
	)
}

func ConfigModule(configPath string) fx.Option {
	return fx.Module("config", fx.Provide(func() (*config.Config, error) {
		return config.Load(configPath)
	}))
}

var RepositoryModule = fx.Module("repository", fx.Provide(newIndexStore))

var ConnectorModule = fx.Module("connectors", fx.Provide(newConnectors))

var EmbedderModule = fx.Module("embedder", fx.Provide(newEmbedderSpec, newEmbedder))

var ServiceModule = fx.Module("services", fx.Provide(
	services.NewChunker,
	newCodeRepos,
	newQueryService,
	newWhyService,
	services.NewTraceService,
	newImpactService,
	services.NewHistoryService,
	services.NewLinkResolver,
	services.NewSyncOrchestrator,
	services.NewStatusService,
))

var SchedulerModule = fx.Module("scheduler",
	fx.Provide(newDiagnosticLogger, newScheduler),
	fx.Invoke(func(*services.Scheduler) {}),
)

const schedulerStopReserve = time.Second

// Stdout carries the MCP JSON-RPC stream, so diagnostics belong on stderr.
func newDiagnosticLogger() *slog.Logger {
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

func newIndexStore(lc fx.Lifecycle, cfg *config.Config, spec embedderSpec) (repositories.IndexStore, error) {
	path := cfg.IndexPath
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, internalerror.NewInternalError("cannot create the index directory "+dir, err)
		}
	}

	store, err := sqlite.Open(path, spec.dims)
	if err != nil {
		return nil, internalerror.NewInternalError("cannot open the workspace index at "+path, err)
	}
	lc.Append(fx.Hook{OnStop: func(context.Context) error { return store.Close() }})
	return store, nil
}

func newConnectors(cfg *config.Config) ([]entities.Connector, error) {
	var connectors []entities.Connector

	if gh := cfg.Sources.GitHub; gh != nil {
		token, err := envValue("sources.github.token_env", gh.TokenEnv)
		if err != nil {
			return nil, err
		}
		connectors = append(connectors, github.NewConnector(token, gh.Repos, ""))
	}

	if n := cfg.Sources.Notion; n != nil {
		token, err := envValue("sources.notion.token_env", n.TokenEnv)
		if err != nil {
			return nil, err
		}
		connectors = append(connectors, notion.NewConnector(token, n.RootPages, ""))
	}

	if j := cfg.Sources.Jira; j != nil {
		email, err := envValue("sources.jira.email_env", j.EmailEnv)
		if err != nil {
			return nil, err
		}
		token, err := envValue("sources.jira.token_env", j.TokenEnv)
		if err != nil {
			return nil, err
		}
		connectors = append(connectors, jira.NewConnector(j.BaseURL, email, token, j.Projects))
	}

	return connectors, nil
}

func envValue(field, name string) (string, error) {
	value := os.Getenv(name)
	if value == "" {
		return "", internalerror.NewBadRequestError(field+" names "+name+", but that environment variable is not set", nil)
	}
	return value, nil
}

const EmbedderKeyEnv = "OPENAI_API_KEY"

const (
	defaultEmbedderProvider = "openai"
	defaultEmbedderModel    = "text-embedding-3-small"
)

var openAIModelDims = map[string]int{
	"text-embedding-3-small": 1536,
	"text-embedding-3-large": 3072,
	"text-embedding-ada-002": 1536,
}

type embedderSpec struct {
	provider string
	model    string
	dims     int
}

func newEmbedderSpec(cfg *config.Config) (embedderSpec, error) {
	spec := embedderSpec{provider: cfg.Embedder.Provider, model: cfg.Embedder.Model}
	if spec.provider == "" {
		spec.provider = defaultEmbedderProvider
	}
	if spec.model == "" {
		spec.model = defaultEmbedderModel
	}

	if spec.provider != defaultEmbedderProvider {
		return embedderSpec{}, internalerror.NewPreconditionError("embedder.provider "+spec.provider+" is configured, but this build only implements "+defaultEmbedderProvider, nil)
	}

	dims, known := openAIModelDims[spec.model]
	if !known {
		return embedderSpec{}, internalerror.NewBadRequestError("embedder.model "+spec.model+" has no known vector width; supported models: "+strings.Join(knownModels(), ", "), nil)
	}
	spec.dims = dims
	return spec, nil
}

func knownModels() []string {
	return slices.Sorted(maps.Keys(openAIModelDims))
}

func newEmbedder(spec embedderSpec) (embedder.Embedder, error) {
	key := os.Getenv(EmbedderKeyEnv)
	if key == "" {
		return nil, internalerror.NewBadRequestError("the "+spec.provider+" embedder needs an API key in "+EmbedderKeyEnv+", but that environment variable is not set", nil)
	}

	emb, err := openai.New(key, spec.model, "", spec.dims)
	if err != nil {
		return nil, internalerror.NewBadRequestError(
			"cannot configure the "+spec.provider+" embedder: "+err.Error(), err)
	}
	return emb, nil
}

func newQueryService(store repositories.IndexStore, emb embedder.Embedder, cfg *config.Config) services.QueryService {
	return services.NewQueryService(store, emb, services.QueryConfig{
		TopK:        cfg.Query.TopK,
		WalkDepth:   cfg.Query.WalkDepth,
		EventWindow: time.Duration(cfg.Query.EventWindow),
	})
}

func newImpactService(store repositories.IndexStore, emb embedder.Embedder, cfg *config.Config) services.ImpactService {
	return services.NewImpactService(store, emb, services.QueryConfig{
		TopK:        cfg.Query.TopK,
		WalkDepth:   cfg.Query.WalkDepth,
		EventWindow: time.Duration(cfg.Query.EventWindow),
	})
}

func newCodeRepos(cfg *config.Config) []services.CodeRepo {
	repos := make([]services.CodeRepo, 0, len(cfg.Repos))
	for _, repo := range cfg.Repos {
		repos = append(repos, services.CodeRepo{
			Path:   repo.Path,
			Remote: repo.Remote,
			Git:    gitrepo.New(repo.Path),
		})
	}

	return repos
}

func newWhyService(
	store repositories.IndexStore,
	emb embedder.Embedder,
	cfg *config.Config,
	repos []services.CodeRepo,
) services.WhyService {
	return services.NewWhyService(store, emb, services.QueryConfig{
		TopK:        cfg.Query.TopK,
		WalkDepth:   cfg.Query.WalkDepth,
		EventWindow: time.Duration(cfg.Query.EventWindow),
	}, repos)
}
