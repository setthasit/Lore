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
	"lore/internal/connectors/embedder/ollama"
	"lore/internal/connectors/embedder/openai"
	"lore/internal/connectors/github"
	"lore/internal/connectors/gitrepo"
	"lore/internal/connectors/jira"
	"lore/internal/connectors/llm"
	llmanthropic "lore/internal/connectors/llm/anthropic"
	llmollama "lore/internal/connectors/llm/ollama"
	llmopenai "lore/internal/connectors/llm/openai"
	llmzai "lore/internal/connectors/llm/zai"
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
		LLMModule,
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

var LLMModule = fx.Module("llm", fx.Provide(newLLM))

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
	providerOpenAI = "openai"
	providerOllama = "ollama"

	defaultEmbedderProvider = providerOpenAI
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
	baseURL  string
	dims     int
}

func newEmbedderSpec(cfg *config.Config) (embedderSpec, error) {
	spec := embedderSpec{
		provider: cfg.Embedder.Provider,
		model:    cfg.Embedder.Model,
		baseURL:  cfg.Embedder.BaseURL,
		dims:     cfg.Embedder.Dimensions,
	}
	if spec.provider == "" {
		spec.provider = defaultEmbedderProvider
	}
	if spec.model == "" {
		spec.model = defaultEmbedderModel
	}

	switch spec.provider {
	case providerOpenAI:
		return openAISpec(spec)
	case providerOllama:
		return ollamaSpec(spec)
	default:
		return embedderSpec{}, internalerror.NewPreconditionError("embedder.provider "+spec.provider+
			" is configured, but this build only implements "+providerOpenAI+" and "+providerOllama, nil)
	}
}

func openAISpec(spec embedderSpec) (embedderSpec, error) {
	if spec.dims != 0 {
		return embedderSpec{}, internalerror.NewBadRequestError("embedder.dimensions must not be set for the "+
			providerOpenAI+" provider: the vector width follows from embedder.model", nil)
	}

	dims, known := openAIModelDims[spec.model]
	if !known {
		return embedderSpec{}, internalerror.NewBadRequestError("embedder.model "+spec.model+" has no known vector width; supported models: "+strings.Join(knownModels(), ", "), nil)
	}
	spec.dims = dims
	return spec, nil
}

func ollamaSpec(spec embedderSpec) (embedderSpec, error) {
	if spec.dims <= 0 {
		return embedderSpec{}, internalerror.NewBadRequestError("embedder.dimensions must be set to the vector width of "+
			spec.model+" for the "+providerOllama+" provider; `ollama show "+spec.model+"` reports it", nil)
	}
	return spec, nil
}

func knownModels() []string {
	return slices.Sorted(maps.Keys(openAIModelDims))
}

func newEmbedder(spec embedderSpec) (embedder.Embedder, error) {
	if spec.provider == providerOllama {
		emb, err := ollama.New(spec.model, spec.baseURL, spec.dims)
		if err != nil {
			return nil, unconfigurableEmbedder(spec.provider, err)
		}
		return emb, nil
	}

	key := os.Getenv(EmbedderKeyEnv)
	if key == "" {
		return nil, internalerror.NewBadRequestError("the "+spec.provider+" embedder needs an API key in "+EmbedderKeyEnv+", but that environment variable is not set", nil)
	}

	emb, err := openai.New(key, spec.model, spec.baseURL, spec.dims)
	if err != nil {
		return nil, unconfigurableEmbedder(spec.provider, err)
	}
	return emb, nil
}

func unconfigurableEmbedder(provider string, err error) error {
	return internalerror.NewBadRequestError("cannot configure the "+provider+" embedder: "+err.Error(), err)
}

const (
	providerAnthropic = "anthropic"
	providerZAI       = "zai"

	defaultLLMProvider = providerOpenAI
)

var llmProviders = map[string]func(key, model, baseURL string) (llm.LLM, error){
	providerOpenAI:    func(key, model, baseURL string) (llm.LLM, error) { return llmopenai.New(key, model, baseURL) },
	providerAnthropic: func(key, model, baseURL string) (llm.LLM, error) { return llmanthropic.New(key, model, baseURL) },
	providerZAI:       func(key, model, baseURL string) (llm.LLM, error) { return llmzai.New(key, model, baseURL) },
	providerOllama:    func(_, model, baseURL string) (llm.LLM, error) { return llmollama.New(model, baseURL) },
}

// A workspace with no llm: block resolves to a nil LLM: only synthesis then fails, and it says why.
func newLLM(cfg *config.Config) (llm.LLM, error) {
	if cfg.LLM == nil {
		return nil, nil
	}

	provider := cfg.LLM.Provider
	if provider == "" {
		provider = defaultLLMProvider
	}
	build, known := llmProviders[provider]
	if !known {
		return nil, internalerror.NewPreconditionError("llm.provider "+provider+
			" is configured, but this build only implements "+strings.Join(knownLLMProviders(), ", "), nil)
	}

	var key string
	if provider != providerOllama {
		apiKey, err := llmAPIKey(provider, cfg.LLM.APIKeyEnv)
		if err != nil {
			return nil, err
		}
		key = apiKey
	}

	client, err := build(key, cfg.LLM.Model, cfg.LLM.BaseURL)
	if err != nil {
		return nil, internalerror.NewBadRequestError("cannot configure the "+provider+" LLM: "+err.Error(), err)
	}
	return client, nil
}

func llmAPIKey(provider, name string) (string, error) {
	if name == "" {
		return "", internalerror.NewBadRequestError("llm.api_key_env must name the environment variable holding the "+
			provider+" API key", nil)
	}
	return envValue("llm.api_key_env", name)
}

func knownLLMProviders() []string {
	return slices.Sorted(maps.Keys(llmProviders))
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
