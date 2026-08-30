package di

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.uber.org/fx"

	"lore/internal/config"
	"lore/internal/connectors/embedder"
	"lore/internal/connectors/embedder/openai"
	"lore/internal/connectors/github"
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
	newQueryService,
	services.NewLinkResolver,
	services.NewSyncOrchestrator,
	services.NewStatusService,
))

func newIndexStore(lc fx.Lifecycle, cfg *config.Config, spec embedderSpec) (repositories.IndexStore, error) {
	path, err := resolvePath(cfg.IndexPath)
	if err != nil {
		return nil, err
	}
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

// Only a leading "~" is expanded.
func resolvePath(path string) (string, error) {
	if path == "" {
		return "", internalerror.NewBadRequestError("index_path must be set", nil)
	}
	if path != "~" && !strings.HasPrefix(path, "~"+string(filepath.Separator)) {
		return path, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", internalerror.NewBadRequestError("index_path "+path+" starts with ~, but this user has no home directory; set an absolute index_path", err)
	}
	return filepath.Join(home, strings.TrimPrefix(path, "~")), nil
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

	// A configured source this build has no connector for is refused, not ignored.
	if cfg.Sources.Notion != nil {
		return nil, unbuiltSource("notion")
	}
	if cfg.Sources.Jira != nil {
		return nil, unbuiltSource("jira")
	}

	return connectors, nil
}

func unbuiltSource(name string) error {
	return internalerror.NewPreconditionError("sources."+name+" is configured, but this build has no "+name+" connector — remove the source or upgrade lore", nil)
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
		return nil, internalerror.NewBadRequestError("cannot configure the "+spec.provider+" embedder", err)
	}
	return emb, nil
}

func newQueryService(store repositories.IndexStore, emb embedder.Embedder, cfg *config.Config) services.QueryService {
	return services.NewQueryService(store, emb, cfg.Query.TopK)
}
