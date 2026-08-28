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

// Workspace is every module a command that works against a workspace needs:
// configuration, index, connectors and services, in the layer order of
// 02-architecture.md. A transport adds itself on top and asks only for services.
func Workspace(configPath string) fx.Option {
	return fx.Options(
		ConfigModule(configPath),
		EmbedderModule,
		RepositoryModule,
		ConnectorModule,
		ServiceModule,
	)
}

// ConfigModule loads lore.yaml from configPath. Loading is a provider rather
// than a constructor argument so a missing or invalid file surfaces as the
// classified error config.Load produced, on the same path as every other
// startup failure.
func ConfigModule(configPath string) fx.Option {
	return fx.Module("config", fx.Provide(func() (*config.Config, error) {
		return config.Load(configPath)
	}))
}

// RepositoryModule provides the workspace index and closes it on shutdown.
var RepositoryModule = fx.Module("repository", fx.Provide(newIndexStore))

// ConnectorModule provides one connector per source the workspace declares.
var ConnectorModule = fx.Module("connectors", fx.Provide(newConnectors))

// EmbedderModule provides the configured embedder and the resolved vector width
// the index is sized for.
var EmbedderModule = fx.Module("embedder", fx.Provide(newEmbedderSpec, newEmbedder))

// ServiceModule provides the service layer.
var ServiceModule = fx.Module("services", fx.Provide(
	services.NewChunker,
	newQueryService,
	services.NewSyncOrchestrator,
	services.NewStatusService,
))

// newIndexStore opens the workspace file named by the configuration.
//
// Two things happen here that the store deliberately refuses to do, because both
// are about whose machine this is rather than about SQLite: the leading "~" the
// configuration stores verbatim is expanded, and the containing directory is
// created. The default index path lives under ~/.lore, which no earlier step
// creates — `lore init` writes a configuration file and never touches the index.
//
// The handle is closed through the lifecycle, so a command that fails halfway
// still releases the file.
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

// resolvePath turns a configured filesystem path into one the store can open.
// Only a leading "~" is special: it is the form the configuration file's own
// default uses, and the loader keeps it verbatim because expanding it there
// would bake one user's home directory into a parsed configuration.
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

// newConnectors registers a connector for every source the workspace declares
// and for no source it does not (02 — D7). Zero connectors is a valid workspace:
// an ask-only one still answers from whatever its index already holds, and a
// sync round over no sources is a round that does nothing.
//
// Tokens are read here, at wiring time, from the variable the configuration
// names — connectors take explicit values and never read the environment
// themselves. Validation already proved the variable is set when the source is
// configured; the check stays because the environment can change between load
// and wiring, and a connector authenticating with an empty token would fail as
// an opaque 401 from the provider instead.
func newConnectors(cfg *config.Config) ([]entities.Connector, error) {
	var connectors []entities.Connector

	if gh := cfg.Sources.GitHub; gh != nil {
		token, err := envValue("sources.github.token_env", gh.TokenEnv)
		if err != nil {
			return nil, err
		}
		connectors = append(connectors, github.NewConnector(token, gh.Repos, ""))
	}

	// A source this build has no connector for is refused rather than ignored:
	// silently never syncing a configured source would look like a source with
	// nothing to say.
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

// envValue reads the secret a configuration field names, reporting the missing
// variable by name: secrets live in the environment only, so an unset one is a
// configuration error the operator can act on directly.
func envValue(field, name string) (string, error) {
	value := os.Getenv(name)
	if value == "" {
		return "", internalerror.NewBadRequestError(field+" names "+name+", but that environment variable is not set", nil)
	}
	return value, nil
}

// EmbedderKeyEnv names the variable the embedder's API key is read from. It is
// exported because `lore init` names it in the scaffold it writes: a workspace
// cannot sync without it.
//
// lore.yaml has no embedder api_key_env (06 — Configuration): an embedding key
// is the one secret every non-local workspace needs, so it uses the provider's
// own conventional variable instead of a per-workspace indirection. Like every
// other secret it lives in the environment only.
const EmbedderKeyEnv = "OPENAI_API_KEY"

// Embedder defaults mirror the configuration example in 06 — Configuration, so
// an `embedder:` block a workspace never touched still wires.
const (
	defaultEmbedderProvider = "openai"
	defaultEmbedderModel    = "text-embedding-3-small"
)

// openAIModelDims is the vector width per OpenAI embedding model. The width is
// not discoverable from the API before the first call, and it has to be known
// earlier than that: it sizes the index's vector table and is half of the
// embedder identity that decides whether stored vectors are still usable. An
// unknown model is therefore refused by name rather than guessed at.
var openAIModelDims = map[string]int{
	"text-embedding-3-small": 1536,
	"text-embedding-3-large": 3072,
	"text-embedding-ada-002": 1536,
}

// embedderSpec is the configured embedder resolved to concrete values. It exists
// because two layers need the same numbers: the embedder produces vectors of
// this width and the store has to size its vector table for exactly them, and
// resolving twice is how those two silently disagree.
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

// newQueryService applies the workspace's retrieval width. A zero top_k cannot
// reach here — the loader fills the default in — but the service normalizes one
// anyway, so this stays a straight pass-through.
func newQueryService(store repositories.IndexStore, emb embedder.Embedder, cfg *config.Config) services.QueryService {
	return services.NewQueryService(store, emb, cfg.Query.TopK)
}
