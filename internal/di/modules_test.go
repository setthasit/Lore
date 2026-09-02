package di

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/fx"

	"lore/internal/config"
	"lore/internal/connectors/embedder"
	"lore/internal/connectors/llm"
	"lore/internal/entities"
	"lore/internal/errors/internalerror"
	"lore/internal/repositories"
	"lore/internal/services"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "lore.yaml")
	index := filepath.Join(dir, "index", "workspace.db")
	content := "workspace: testws\nindex_path: " + index + "\n" + body
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write configuration: %v", err)
	}
	return path
}

func gitClone(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o750); err != nil {
		t.Fatalf("seed clone: %v", err)
	}
	return dir
}

func resolveWorkspace(t *testing.T, path string) ([]entities.Connector, error) {
	t.Helper()

	var (
		query      services.QueryService
		orch       services.SyncOrchestrator
		connectors []entities.Connector
	)
	app := fx.New(
		fx.NopLogger,
		Workspace(path),
		fx.Populate(&query, &orch, &connectors),
	)
	if err := app.Err(); err != nil {
		return nil, err
	}

	ctx := context.Background()
	if err := app.Start(ctx); err != nil {
		return nil, err
	}
	if query == nil || orch == nil {
		t.Fatalf("graph resolved to query=%v orchestrator=%v; want both", query, orch)
	}
	// Stop runs the lifecycle hook that closes the index; a Close failure surfaces here.
	if err := app.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	return connectors, nil
}

func TestWorkspaceGraphWithGitHubSource(t *testing.T) {
	t.Setenv("LORE_TEST_GH_TOKEN", "ghp_example")
	t.Setenv(EmbedderKeyEnv, "sk-example")

	path := writeConfig(t, `sources:
  github:
    token_env: LORE_TEST_GH_TOKEN
    repos: [acme/lore]
embedder:
  provider: openai
  model: text-embedding-3-small
`)

	connectors, err := resolveWorkspace(t, path)
	if err != nil {
		t.Fatalf("resolve workspace: %v", err)
	}
	if len(connectors) != 1 {
		t.Fatalf("connectors = %d, want 1", len(connectors))
	}
	if got := connectors[0].Name(); got != "github" {
		t.Errorf("connector name = %q, want %q", got, "github")
	}
}

func TestWorkspaceGraphWithoutSources(t *testing.T) {
	t.Setenv(EmbedderKeyEnv, "sk-example")

	path := writeConfig(t, `repos:
  - path: `+gitClone(t)+`
`)

	connectors, err := resolveWorkspace(t, path)
	if err != nil {
		t.Fatalf("resolve workspace: %v", err)
	}
	if len(connectors) != 0 {
		t.Errorf("connectors = %v, want none", connectors)
	}
}

func TestWorkspaceGraphRejectsUnknownEmbedderModel(t *testing.T) {
	t.Setenv(EmbedderKeyEnv, "sk-example")

	path := writeConfig(t, `repos:
  - path: `+gitClone(t)+`
embedder:
  model: text-embedding-4-imaginary
`)

	_, err := resolveWorkspace(t, path)
	if err == nil {
		t.Fatal("resolve workspace: want an error naming the model")
	}
	if got := internalerror.KindOf(err); got != internalerror.KindBadRequest {
		t.Errorf("kind = %s, want %s", got, internalerror.KindBadRequest)
	}
	if !strings.Contains(err.Error(), "text-embedding-4-imaginary") {
		t.Errorf("error %q does not name the configured model", err)
	}
	if !strings.Contains(err.Error(), defaultEmbedderModel) {
		t.Errorf("error %q does not list the supported models", err)
	}
}

func TestWorkspaceGraphRejectsMissingEmbedderKey(t *testing.T) {
	t.Setenv(EmbedderKeyEnv, "")

	path := writeConfig(t, `repos:
  - path: `+gitClone(t)+`
`)

	_, err := resolveWorkspace(t, path)
	if err == nil {
		t.Fatal("resolve workspace: want an error naming the key variable")
	}
	if got := internalerror.KindOf(err); got != internalerror.KindBadRequest {
		t.Errorf("kind = %s, want %s", got, internalerror.KindBadRequest)
	}
	if !strings.Contains(err.Error(), EmbedderKeyEnv) {
		t.Errorf("error %q does not name %s", err, EmbedderKeyEnv)
	}
}

func resolveEmbedder(t *testing.T, path string) (embedder.Embedder, error) {
	t.Helper()

	var emb embedder.Embedder
	app := fx.New(fx.NopLogger, Workspace(path), fx.Populate(&emb))
	if err := app.Err(); err != nil {
		return nil, err
	}

	ctx := context.Background()
	if err := app.Start(ctx); err != nil {
		return nil, err
	}
	if err := app.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	return emb, nil
}

func TestWorkspaceGraphResolvesTheOllamaEmbedder(t *testing.T) {
	// The local daemon is unauthenticated: no key variable is consulted.
	t.Setenv(EmbedderKeyEnv, "")

	path := writeConfig(t, `repos:
  - path: `+gitClone(t)+`
embedder:
  provider: ollama
  model: nomic-embed-text
  base_url: http://127.0.0.1:11434
  dimensions: 768
`)

	emb, err := resolveEmbedder(t, path)
	if err != nil {
		t.Fatalf("resolve workspace: %v", err)
	}
	if want := "ollama/nomic-embed-text/768"; emb.Identity() != want {
		t.Errorf("Identity = %q, want %q", emb.Identity(), want)
	}
}

func TestWorkspaceGraphRejectsOllamaWithoutDimensions(t *testing.T) {
	path := writeConfig(t, `repos:
  - path: `+gitClone(t)+`
embedder:
  provider: ollama
  model: nomic-embed-text
`)

	_, err := resolveEmbedder(t, path)
	if err == nil {
		t.Fatal("resolve workspace: want an error naming embedder.dimensions")
	}
	if got := internalerror.KindOf(err); got != internalerror.KindBadRequest {
		t.Errorf("kind = %s, want %s", got, internalerror.KindBadRequest)
	}
	want := "embedder.dimensions must be set to the vector width of nomic-embed-text for the ollama provider; `ollama show nomic-embed-text` reports it"
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not contain %q", err, want)
	}
}

func TestWorkspaceGraphRejectsUnknownEmbedderProvider(t *testing.T) {
	t.Setenv(EmbedderKeyEnv, "sk-example")

	path := writeConfig(t, `repos:
  - path: `+gitClone(t)+`
embedder:
  provider: cohere
  model: embed-english-v3
  dimensions: 1024
`)

	_, err := resolveEmbedder(t, path)
	if err == nil {
		t.Fatal("resolve workspace: want an error naming the provider")
	}
	if got := internalerror.KindOf(err); got != internalerror.KindPrecondition {
		t.Errorf("kind = %s, want %s", got, internalerror.KindPrecondition)
	}
	for _, want := range []string{"cohere", providerOpenAI, providerOllama} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

func TestWorkspaceGraphRejectsDimensionsForOpenAI(t *testing.T) {
	t.Setenv(EmbedderKeyEnv, "sk-example")

	path := writeConfig(t, `repos:
  - path: `+gitClone(t)+`
embedder:
  provider: openai
  model: text-embedding-3-small
  dimensions: 512
`)

	_, err := resolveEmbedder(t, path)
	if err == nil {
		t.Fatal("resolve workspace: want an error rejecting a width openai derives itself")
	}
	if got := internalerror.KindOf(err); got != internalerror.KindBadRequest {
		t.Errorf("kind = %s, want %s", got, internalerror.KindBadRequest)
	}
	if want := "embedder.dimensions must not be set"; !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not contain %q", err, want)
	}
}

const llmKeyEnv = "LORE_TEST_LLM_KEY"

func resolveSynthesis(t *testing.T, path string) (services.SynthesisService, llm.LLM, error) {
	t.Helper()

	var (
		svc   services.SynthesisService
		model llm.LLM
	)
	app := fx.New(fx.NopLogger, Workspace(path), fx.Populate(&svc, &model))
	if err := app.Err(); err != nil {
		return nil, nil, err
	}

	ctx := context.Background()
	if err := app.Start(ctx); err != nil {
		return nil, nil, err
	}
	if err := app.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	return svc, model, nil
}

// `lore mcp`, `lore sync` and `lore status` run on workspaces that never synthesize.
func TestWorkspaceGraphResolvesWithoutAnLLMBlock(t *testing.T) {
	path := writeConfig(t, `repos:
  - path: `+gitClone(t)+`
`)

	svc, model, err := resolveSynthesis(t, path)
	if err != nil {
		t.Fatalf("resolve workspace: %v", err)
	}
	if model != nil {
		t.Errorf("llm = %v, want none for a workspace with no llm: block", model)
	}

	_, err = svc.Synthesize(context.Background(), "why option B?", &entities.EvidenceBundle{})
	if got := internalerror.KindOf(err); got != internalerror.KindPrecondition {
		t.Fatalf("kind = %s, want %s (error %v)", got, internalerror.KindPrecondition, err)
	}
	if want := "llm:"; !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not name the %s block", err, want)
	}
}

func TestWorkspaceGraphResolvesTheConfiguredLLM(t *testing.T) {
	t.Setenv(llmKeyEnv, "sk-example")

	path := writeConfig(t, `repos:
  - path: `+gitClone(t)+`
llm:
  provider: anthropic
  model: claude-sonnet-4-5
  api_key_env: `+llmKeyEnv+`
`)

	svc, model, err := resolveSynthesis(t, path)
	if err != nil {
		t.Fatalf("resolve workspace: %v", err)
	}
	if svc == nil || model == nil {
		t.Fatalf("graph resolved to synthesis=%v llm=%v; want both", svc, model)
	}
}

func TestWorkspaceGraphResolvesTheOllamaLLMWithoutAKey(t *testing.T) {
	path := writeConfig(t, `repos:
  - path: `+gitClone(t)+`
llm:
  provider: ollama
  model: qwen2.5
  base_url: http://127.0.0.1:11434
`)

	_, model, err := resolveSynthesis(t, path)
	if err != nil {
		t.Fatalf("resolve workspace: %v", err)
	}
	if model == nil {
		t.Error("llm = none, want the local provider, which needs no key")
	}
}

func TestWorkspaceGraphRejectsUnknownLLMProvider(t *testing.T) {
	t.Setenv(llmKeyEnv, "sk-example")

	path := writeConfig(t, `repos:
  - path: `+gitClone(t)+`
llm:
  provider: gemini
  model: gemini-2.5-pro
  api_key_env: `+llmKeyEnv+`
`)

	_, _, err := resolveSynthesis(t, path)
	if err == nil {
		t.Fatal("resolve workspace: want an error naming the provider")
	}
	if got := internalerror.KindOf(err); got != internalerror.KindPrecondition {
		t.Errorf("kind = %s, want %s", got, internalerror.KindPrecondition)
	}
	for _, want := range []string{"gemini", providerOpenAI, providerAnthropic, providerZAI, providerOllama} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

// The unset variable is refused while lore.yaml loads, before any provider is built.
func TestWorkspaceGraphRejectsAnUnsetLLMKeyVariableAtLoad(t *testing.T) {
	t.Setenv(llmKeyEnv, "")

	path := writeConfig(t, `repos:
  - path: `+gitClone(t)+`
llm:
  provider: openai
  model: gpt-5
  api_key_env: `+llmKeyEnv+`
`)

	_, _, err := resolveSynthesis(t, path)
	if err == nil {
		t.Fatal("resolve workspace: want an error naming the key variable")
	}
	if got := internalerror.KindOf(err); got != internalerror.KindBadRequest {
		t.Errorf("kind = %s, want %s", got, internalerror.KindBadRequest)
	}
	if !strings.Contains(err.Error(), llmKeyEnv) {
		t.Errorf("error %q does not name %s", err, llmKeyEnv)
	}
}

func TestWorkspaceGraphRejectsAKeyedLLMWithoutAKeyVariable(t *testing.T) {
	path := writeConfig(t, `repos:
  - path: `+gitClone(t)+`
llm:
  provider: zai
  model: glm-4.6
`)

	_, _, err := resolveSynthesis(t, path)
	if err == nil {
		t.Fatal("resolve workspace: want an error naming llm.api_key_env")
	}
	if got := internalerror.KindOf(err); got != internalerror.KindBadRequest {
		t.Errorf("kind = %s, want %s", got, internalerror.KindBadRequest)
	}
	if want := "llm.api_key_env must name the environment variable holding the zai API key"; !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not contain %q", err, want)
	}
}

func TestWorkspaceGraphWithAskOnlySources(t *testing.T) {
	t.Setenv("LORE_TEST_NOTION_TOKEN", "secret_example")
	t.Setenv("LORE_TEST_JIRA_EMAIL", "bot@example.invalid")
	t.Setenv("LORE_TEST_JIRA_TOKEN", "jira_example")
	t.Setenv(EmbedderKeyEnv, "sk-example")

	path := writeConfig(t, `sources:
  notion:
    token_env: LORE_TEST_NOTION_TOKEN
    root_pages: ["Engineering Wiki"]
  jira:
    base_url: https://acme.atlassian.net
    email_env: LORE_TEST_JIRA_EMAIL
    token_env: LORE_TEST_JIRA_TOKEN
    projects: [PROJ]
repos: []
`)

	connectors, err := resolveWorkspace(t, path)
	if err != nil {
		t.Fatalf("resolve workspace: %v", err)
	}
	var names []string
	for _, c := range connectors {
		names = append(names, c.Name())
	}
	if want := []string{"notion", "jira"}; !slices.Equal(names, want) {
		t.Errorf("connector names = %v, want %v", names, want)
	}
}

func TestWorkspaceGraphRejectsMissingJiraEmail(t *testing.T) {
	t.Setenv("LORE_TEST_JIRA_EMAIL", "")
	t.Setenv("LORE_TEST_JIRA_TOKEN", "jira_example")
	t.Setenv(EmbedderKeyEnv, "sk-example")

	path := writeConfig(t, `sources:
  jira:
    base_url: https://acme.atlassian.net
    email_env: LORE_TEST_JIRA_EMAIL
    token_env: LORE_TEST_JIRA_TOKEN
`)

	_, err := resolveWorkspace(t, path)
	if err == nil {
		t.Fatal("resolve workspace: want an error naming the email variable")
	}
	if got := internalerror.KindOf(err); got != internalerror.KindBadRequest {
		t.Errorf("kind = %s, want %s", got, internalerror.KindBadRequest)
	}
	if !strings.Contains(err.Error(), "LORE_TEST_JIRA_EMAIL") {
		t.Errorf("error %q does not name the variable", err)
	}
}

func TestWorkspaceGraphResolvesTheCodeAnchoredVerbsForAnAskOnlyWorkspace(t *testing.T) {
	t.Setenv("LORE_TEST_NOTION_TOKEN", "secret_example")
	t.Setenv(EmbedderKeyEnv, "sk-example")

	path := writeConfig(t, `sources:
  notion:
    token_env: LORE_TEST_NOTION_TOKEN
    root_pages: ["Engineering Wiki"]
repos: []
`)

	var (
		why     services.WhyService
		history services.HistoryService
	)
	app := fx.New(fx.NopLogger, Workspace(path), fx.Populate(&why, &history))
	if err := app.Err(); err != nil {
		t.Fatalf("build graph: %v", err)
	}

	ctx := context.Background()
	if err := app.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		if err := app.Stop(ctx); err != nil {
			t.Errorf("Stop: %v", err)
		}
	}()

	const anchoredFile = "internal/auth/auth.go"

	_, whyErr := why.Why(ctx, services.WhyRequest{File: anchoredFile, LineStart: 10, LineEnd: 20})
	_, historyErr := history.HistoryOf(ctx, services.HistoryRequest{File: anchoredFile})

	refusals := map[string]error{"why": whyErr, "history_of": historyErr}
	for verb, err := range refusals {
		if got := internalerror.KindOf(err); got != internalerror.KindPrecondition {
			t.Fatalf("%s kind = %s, want %s (error %v)", verb, got, internalerror.KindPrecondition, err)
		}
		if want := "no repositories registered"; !strings.Contains(err.Error(), want) {
			t.Errorf("%s error = %q, want it to name %q", verb, err, want)
		}
	}
}

const (
	schedulerTick = "5ms"

	schedulerStall = 5 * time.Second

	schedulerQuiet = 250 * time.Millisecond

	schedulerStopTimeout = 200 * time.Millisecond
)

// A round parks until the test releases it, so a graph can be stopped mid-round.
type scheduledSync struct {
	rounds   chan struct{}
	released chan struct{}
}

var _ services.SyncOrchestrator = (*scheduledSync)(nil)

func newScheduledSync() *scheduledSync {
	orchestrator := newParkedSync()
	close(orchestrator.released)

	return orchestrator
}

func newParkedSync() *scheduledSync {
	return &scheduledSync{rounds: make(chan struct{}, 1), released: make(chan struct{})}
}

func (o *scheduledSync) Sync(context.Context, services.SyncOptions) (services.SyncResult, error) {
	select {
	case o.rounds <- struct{}{}:
	default:
	}
	<-o.released

	return services.SyncResult{}, nil
}

func (o *scheduledSync) Subscribe() (<-chan entities.SyncEvent, func()) {
	events := make(chan entities.SyncEvent)

	return events, sync.OnceFunc(func() { close(events) })
}

func (o *scheduledSync) awaitRound(t *testing.T) {
	t.Helper()

	select {
	case <-o.rounds:
	case <-time.After(schedulerStall):
		t.Fatalf("no sync round ran within %s", schedulerStall)
	}
}

// Mirrors the set every CLI command populates in internal/transport/cli/runtime.go.
type commandRuntime struct {
	config  *config.Config
	query   services.QueryService
	why     services.WhyService
	trace   services.TraceService
	impact  services.ImpactService
	history services.HistoryService
	sync    services.SyncOrchestrator
	status  services.StatusService
	store   repositories.IndexStore
}

type startedGraph struct {
	app     *fx.App
	runtime commandRuntime
	stopped bool
}

func startGraph(t *testing.T, orchestrator services.SyncOrchestrator, extra fx.Option) *startedGraph {
	t.Helper()

	t.Setenv(EmbedderKeyEnv, "sk-example")
	path := writeConfig(t, `repos:
  - path: `+gitClone(t)+`
scheduler:
  interval: `+schedulerTick+`
`)

	graph := new(startedGraph)
	rt := &graph.runtime
	graph.app = fx.New(
		fx.NopLogger,
		Workspace(path),
		extra,
		fx.Decorate(func(services.SyncOrchestrator) services.SyncOrchestrator { return orchestrator }),
		fx.Populate(&rt.config, &rt.query, &rt.why, &rt.trace, &rt.impact, &rt.history, &rt.sync, &rt.status, &rt.store),
	)
	if err := graph.app.Err(); err != nil {
		t.Fatalf("build graph: %v", err)
	}
	if err := graph.app.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		if graph.stopped {
			return
		}
		if err := graph.app.Stop(context.Background()); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})

	return graph
}

func (g *startedGraph) stop(t *testing.T, timeout time.Duration) error {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	g.stopped = true

	return g.app.Stop(ctx)
}

func schedulerWithQuietLogger() fx.Option {
	return fx.Options(
		SchedulerModule,
		fx.Decorate(func(*slog.Logger) *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }),
	)
}

func TestWorkspaceAloneNeverSchedulesASyncRound(t *testing.T) {
	orchestrator := newScheduledSync()
	startGraph(t, orchestrator, fx.Options())

	select {
	case <-orchestrator.rounds:
		t.Fatal("a sync round ran behind a one-shot command: the scheduler must not be part of Workspace")
	case <-time.After(schedulerQuiet):
	}
}

func TestSchedulerModuleSchedulesSyncRounds(t *testing.T) {
	orchestrator := newScheduledSync()
	startGraph(t, orchestrator, schedulerWithQuietLogger())

	orchestrator.awaitRound(t)
}

func TestStoppingMidRoundStillClosesTheIndex(t *testing.T) {
	orchestrator := newParkedSync()
	t.Cleanup(func() { close(orchestrator.released) })

	graph := startGraph(t, orchestrator, schedulerWithQuietLogger())
	orchestrator.awaitRound(t)

	if err := graph.stop(t, schedulerStopTimeout); err == nil {
		t.Fatal("Stop reported success while a round was still in flight")
	}
	if _, err := graph.runtime.store.Stats(context.Background()); err == nil {
		t.Error("the index store still answers queries after Stop: its hook never ran")
	}
}
