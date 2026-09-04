package di

import (
	"context"
	"io"
	"iter"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/fx"

	"github.com/setthasit/Lore/internal/config"
	"github.com/setthasit/Lore/internal/entities"
	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/internal/registry"
	"github.com/setthasit/Lore/internal/repositories"
	"github.com/setthasit/Lore/internal/services"
	"github.com/setthasit/Lore/sdk"
)

// The plugins these tests wire are declared here rather than imported: nothing
// under internal/ may reach into plugins/, and the subject is the graph anyway.
// A stub also pins the graph down where a real plugin could not — it reports
// exactly the vector width a case wants, including none at all.
const (
	sourcePlugin = "pigeon"     // a source that claims the remotes it is told to
	dualPlugin   = "abacus"     // a provider that both embeds and completes
	widthPlugin  = "slide-rule" // a provider that embeds at the width it is given
	codePlugin   = "chisel"     // a code plugin over a clone that tracks nothing

	sourceTokenEnv = "LORE_TEST_PIGEON_TOKEN"
	embedderModel  = "beads-v2"
	completerModel = "chalk-v1"

	// The width dualPlugin reports, so a configuration binding it never has to
	// declare one.
	dualWidth = 24

	// The answer dualPlugin's completer gives, so a test can tell the completer
	// the graph resolved from any other value.
	dualReply = "an answer from the stub completer"
)

type stubSourcePlugin struct{}

var _ lore.SourcePlugin = stubSourcePlugin{}

func (stubSourcePlugin) Manifest() lore.Manifest {
	return lore.Manifest{
		Name:         sourcePlugin,
		Kind:         lore.KindSource,
		APIVersion:   lore.APIVersion,
		Summary:      "a source that exists only in this package's tests",
		Capabilities: lore.Capabilities{RepoRemotes: true},
		Fields: []lore.Field{{
			Name:     "seams",
			Type:     lore.FieldStringList,
			Required: true,
			Doc:      "the repo remotes this instance ingests",
		}},
		Secrets: []lore.Secret{{
			Key:         "token",
			ConfigField: "token_env",
			DefaultEnv:  sourceTokenEnv,
			Doc:         "token this instance authenticates with",
		}},
	}
}

func (stubSourcePlugin) NewSource(c lore.SourceConfig) (lore.Connector, error) {
	var with struct {
		Seams []string `json:"seams"`
	}
	if err := c.Decode(&with); err != nil {
		return nil, err
	}

	return stubConnector{name: c.Instance, seams: with.Seams}, nil
}

type stubConnector struct {
	name  string
	seams []string
}

func (c stubConnector) Name() string { return c.name }

func (stubConnector) Changes(context.Context, lore.Cursor) iter.Seq2[lore.Batch, error] {
	return func(func(lore.Batch, error) bool) {}
}

// The manifest claims repo_remotes, so the connector must answer which remotes
// it ingests: that question is the whole of the unmatched-clone warning.
func (c stubConnector) MatchesRemote(remote string) bool { return slices.Contains(c.seams, remote) }

type stubDualPlugin struct{}

var _ lore.ProviderPlugin = stubDualPlugin{}

func (stubDualPlugin) Manifest() lore.Manifest {
	return lore.Manifest{
		Name:         dualPlugin,
		Kind:         lore.KindProvider,
		APIVersion:   lore.APIVersion,
		Summary:      "a provider that exists only in this package's tests",
		Capabilities: lore.Capabilities{Embed: true, Complete: true},
		DefaultModels: map[lore.Capability]string{
			lore.CapabilityEmbed:    embedderModel,
			lore.CapabilityComplete: completerModel,
		},
	}
}

// One value serves both halves; which half it was built for is the capability
// the host asked for and then asserts it against.
func (stubDualPlugin) NewProvider(lore.ProviderConfig) (lore.Provider, error) {
	return stubModel{}, nil
}

type stubModel struct{}

var (
	_ lore.Embedder  = stubModel{}
	_ lore.Completer = stubModel{}
)

func (stubModel) Embed(_ context.Context, texts []string) ([][]float32, error) {
	vectors := make([][]float32, 0, len(texts))
	for range texts {
		vectors = append(vectors, make([]float32, dualWidth))
	}

	return vectors, nil
}

func (stubModel) Dimensions() int { return dualWidth }

func (stubModel) Complete(context.Context, string, string) (string, error) { return dualReply, nil }

type stubWidthPlugin struct{}

var _ lore.ProviderPlugin = stubWidthPlugin{}

func (stubWidthPlugin) Manifest() lore.Manifest {
	return lore.Manifest{
		Name:          widthPlugin,
		Kind:          lore.KindProvider,
		APIVersion:    lore.APIVersion,
		Summary:       "an embedding provider that exists only in this package's tests",
		Capabilities:  lore.Capabilities{Embed: true},
		DefaultModels: map[lore.Capability]string{lore.CapabilityEmbed: embedderModel},
	}
}

// The width the binding declared is reported back verbatim, which is how a case
// drives the graph with a chosen vector width — or with none, as the deny case
// wants.
func (stubWidthPlugin) NewProvider(c lore.ProviderConfig) (lore.Provider, error) {
	return stubEmbedder{dims: c.Dimensions}, nil
}

type stubEmbedder struct{ dims int }

var _ lore.Embedder = stubEmbedder{}

func (e stubEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	vectors := make([][]float32, 0, len(texts))
	for range texts {
		vectors = append(vectors, make([]float32, e.dims))
	}

	return vectors, nil
}

func (e stubEmbedder) Dimensions() int { return e.dims }

type stubCodePlugin struct{}

var _ lore.CodePlugin = stubCodePlugin{}

func (stubCodePlugin) Manifest() lore.Manifest {
	return lore.Manifest{
		Name:       codePlugin,
		Kind:       lore.KindCode,
		APIVersion: lore.APIVersion,
		Summary:    "a code plugin that exists only in this package's tests",
	}
}

func (stubCodePlugin) NewCode(lore.CodeConfig) (lore.CodeRepo, error) { return stubClone{}, nil }

// A clone that tracks no file: the code-anchored verbs then refuse for a reason
// that names the clone, which is what proves the clone reached them.
type stubClone struct{}

var _ lore.CodeRepo = stubClone{}

func (stubClone) Blame(context.Context, string, int, int) ([]lore.BlameSpan, error) {
	return nil, nil
}

func (stubClone) Log(context.Context, string) ([]lore.CommitRef, error) { return nil, nil }

func (stubClone) HasFileAtHEAD(context.Context, string) (bool, error) { return false, nil }

func stubRegistry(t *testing.T) *registry.Registry {
	t.Helper()

	reg := registry.New(lore.Host{})
	if err := reg.Register(stubSourcePlugin{}, stubDualPlugin{}, stubWidthPlugin{}, stubCodePlugin{}); err != nil {
		t.Fatalf("register the stub plugins: %v", err)
	}

	return reg
}

// The embedder is the one role a workspace cannot resolve without, so every
// configuration below carries one; the cases about widths spell their own.
const embedderBlock = `embedder:
  provider: ` + dualPlugin + `
  model: ` + embedderModel + `
`

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

// startWorkspace resolves the graph, runs its startup hooks and populates
// targets. The index is closed on cleanup rather than before returning, so a
// test can still question the store the graph opened; a Close failure surfaces
// there as a test failure.
func startWorkspace(t *testing.T, path string, targets ...any) error {
	t.Helper()

	app := fx.New(fx.NopLogger, Workspace(path, stubRegistry(t)), fx.Populate(targets...))
	if err := app.Err(); err != nil {
		return err
	}

	if err := app.Start(context.Background()); err != nil {
		return err
	}
	t.Cleanup(func() {
		if err := app.Stop(context.Background()); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})

	return nil
}

func TestWorkspaceResolvesOneConnectorPerSourceInstance(t *testing.T) {
	t.Setenv(sourceTokenEnv, "token-example")

	path := writeConfig(t, `sources:
  - use: `+sourcePlugin+`
    with:
      seams: ["pigeon:acme/app"]
  - id: pigeon-archive
    use: `+sourcePlugin+`
    with:
      seams: ["pigeon:acme/archive"]
`+embedderBlock)

	var connectors []lore.Connector
	if err := startWorkspace(t, path, &connectors); err != nil {
		t.Fatalf("resolve workspace: %v", err)
	}

	names := make([]string, 0, len(connectors))
	for _, connector := range connectors {
		names = append(names, connector.Name())
	}
	// Configuration order is sync order, and each connector answers to its
	// instance id, because that id is the cursor key and the document namespace.
	if want := []string{sourcePlugin, "pigeon-archive"}; !slices.Equal(names, want) {
		t.Errorf("connector names = %v, want %v", names, want)
	}
}

func TestWorkspaceWithReposAndNoSourcesResolvesTheCodeAnchoredVerbs(t *testing.T) {
	clone := gitClone(t)

	path := writeConfig(t, `repos:
  - path: `+clone+`
    use: `+codePlugin+`
`+embedderBlock)

	var (
		connectors []lore.Connector
		why        services.WhyService
		history    services.HistoryService
	)
	if err := startWorkspace(t, path, &connectors, &why, &history); err != nil {
		t.Fatalf("resolve workspace: %v", err)
	}
	if len(connectors) != 0 {
		t.Errorf("connectors = %v, want none for a workspace with no sources", connectors)
	}

	ctx := context.Background()
	_, whyErr := why.Why(ctx, services.WhyRequest{File: anchoredFile, LineStart: 10, LineEnd: 20})
	_, historyErr := history.HistoryOf(ctx, services.HistoryRequest{File: anchoredFile})

	// The registered clone reached both verbs: each refusal is about the file
	// and names the clone it looked in, not about there being no clone at all.
	for verb, err := range map[string]error{"why": whyErr, "history_of": historyErr} {
		if got := internalerror.KindOf(err); got != internalerror.KindNotFound {
			t.Fatalf("%s kind = %s, want %s (error %v)", verb, got, internalerror.KindNotFound, err)
		}
		if !strings.Contains(err.Error(), clone) {
			t.Errorf("%s error = %q, want it to name the registered clone %s", verb, err, clone)
		}
	}
}

// widthConfig is a workspace whose embedder reports exactly dims, which is the
// one number the index and the vector-space identity are both built from.
func widthConfig(t *testing.T, dimensions string) string {
	t.Helper()

	return writeConfig(t, `repos:
  - path: `+gitClone(t)+`
    use: `+codePlugin+`
embedder:
  provider: `+widthPlugin+`
  model: `+embedderModel+`
`+dimensions)
}

func TestWorkspaceComposesTheVectorSpaceFromThePluginModelAndWidth(t *testing.T) {
	path := widthConfig(t, "  dimensions: 8\n")

	var (
		embedder lore.Embedder
		space    services.VectorSpace
	)
	if err := startWorkspace(t, path, &embedder, &space); err != nil {
		t.Fatalf("resolve workspace: %v", err)
	}
	if got := embedder.Dimensions(); got != 8 {
		t.Fatalf("Dimensions = %d, want 8", got)
	}
	// The host composes the identity from the plugin's manifest name, the
	// configured model and the width the provider reports; a provider never
	// names a vector space itself, so it cannot claim another's.
	if want := services.VectorSpace(widthPlugin + "/" + embedderModel + "/8"); space != want {
		t.Errorf("vector space = %q, want %q", space, want)
	}
}

func TestWorkspaceOpensTheIndexAtTheWidthTheEmbedderReports(t *testing.T) {
	path := widthConfig(t, "  dimensions: 8\n")

	var store repositories.IndexStore
	if err := startWorkspace(t, path, &store); err != nil {
		t.Fatalf("resolve workspace: %v", err)
	}

	ctx := context.Background()
	if _, err := store.SearchVector(ctx, make([]float32, 8), entities.Filters{}, 1); err != nil {
		t.Errorf("SearchVector at the width the embedder reports: %v", err)
	}
	// The vector column's width is baked into the index at creation, so a query
	// vector of any other width is the proof that it was baked in at this one.
	if _, err := store.SearchVector(ctx, make([]float32, 9), entities.Filters{}, 1); err == nil {
		t.Error("the index accepted a 9-dimension query vector: it was opened at some other width")
	}
}

func TestWorkspaceRefusesAnEmbedderThatReportsNoVectorWidth(t *testing.T) {
	path := widthConfig(t, "")

	var store repositories.IndexStore
	err := startWorkspace(t, path, &store)
	if err == nil {
		t.Fatal("resolve workspace: want an error rather than an index with no vector column")
	}
	if got := internalerror.KindOf(err); got != internalerror.KindPrecondition {
		t.Errorf("kind = %s, want %s", got, internalerror.KindPrecondition)
	}
	for _, want := range []string{widthPlugin, "vector width"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

// `lore mcp`, `lore sync` and `lore status` run on workspaces that never synthesize.
func TestWorkspaceResolvesWithoutAnLLMBlock(t *testing.T) {
	path := writeConfig(t, `repos:
  - path: `+gitClone(t)+`
    use: `+codePlugin+`
`+embedderBlock)

	var (
		synthesis services.SynthesisService
		completer lore.Completer
	)
	if err := startWorkspace(t, path, &synthesis, &completer); err != nil {
		t.Fatalf("resolve workspace: %v", err)
	}
	if completer != nil {
		t.Errorf("llm = %v, want none for a workspace with no llm: block", completer)
	}

	_, err := synthesis.Synthesize(context.Background(), "why option B?", &entities.EvidenceBundle{})
	if got := internalerror.KindOf(err); got != internalerror.KindPrecondition {
		t.Fatalf("kind = %s, want %s (error %v)", got, internalerror.KindPrecondition, err)
	}
	if want := "llm:"; !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not name the %s block", err, want)
	}
}

func TestWorkspaceResolvesTheBoundCompleter(t *testing.T) {
	path := writeConfig(t, `repos:
  - path: `+gitClone(t)+`
    use: `+codePlugin+`
llm:
  provider: `+dualPlugin+`
  model: `+completerModel+`
`+embedderBlock)

	var (
		synthesis services.SynthesisService
		completer lore.Completer
	)
	if err := startWorkspace(t, path, &synthesis, &completer); err != nil {
		t.Fatalf("resolve workspace: %v", err)
	}
	if synthesis == nil || completer == nil {
		t.Fatalf("graph resolved to synthesis=%v llm=%v; want both", synthesis, completer)
	}

	// Answering with the bound plugin's text is what tells this completer apart
	// from any other value the graph could have handed synthesis.
	got, err := completer.Complete(context.Background(), "system", "user")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got != dualReply {
		t.Errorf("Complete = %q, want the plugin the llm: block binds, which answers %q", got, dualReply)
	}
}

// A clone whose remote no source ingests still answers blame, so it is reported
// as a warning the transports print, never as a failure to resolve.
func TestWorkspaceWarnsOnlyAboutACloneNoSourceClaims(t *testing.T) {
	const ingestedRemote = "pigeon:acme/app"

	tests := []struct {
		name   string
		remote string
		want   int
	}{
		{name: "a source claims the remote", remote: ingestedRemote, want: 0},
		{name: "no source claims the remote", remote: "pigeon:acme/elsewhere", want: 1},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv(sourceTokenEnv, "token-example")

			path := writeConfig(t, `sources:
  - use: `+sourcePlugin+`
    with:
      seams: ["`+ingestedRemote+`"]
repos:
  - path: `+gitClone(t)+`
    use: `+codePlugin+`
    remote: "`+test.remote+`"
`+embedderBlock)

			var warnings registry.Warnings
			if err := startWorkspace(t, path, &warnings); err != nil {
				t.Fatalf("resolve workspace: %v", err)
			}
			if len(warnings) != test.want {
				t.Fatalf("warnings = %v, want %d of them", warnings, test.want)
			}
			if test.want > 0 && !strings.Contains(warnings[0], test.remote) {
				t.Errorf("warning = %q, want it to name the unmatched remote %s", warnings[0], test.remote)
			}
		})
	}
}

const anchoredFile = "internal/auth/auth.go"

func TestWorkspaceResolvesTheCodeAnchoredVerbsForAnAskOnlyWorkspace(t *testing.T) {
	t.Setenv(sourceTokenEnv, "token-example")

	path := writeConfig(t, `sources:
  - use: `+sourcePlugin+`
    with:
      seams: ["pigeon:acme/app"]
`+embedderBlock)

	var (
		why     services.WhyService
		history services.HistoryService
	)
	if err := startWorkspace(t, path, &why, &history); err != nil {
		t.Fatalf("resolve workspace: %v", err)
	}

	ctx := context.Background()
	_, whyErr := why.Why(ctx, services.WhyRequest{File: anchoredFile, LineStart: 10, LineEnd: 20})
	_, historyErr := history.HistoryOf(ctx, services.HistoryRequest{File: anchoredFile})

	// A workspace with no clone answers document questions and declines code
	// questions instead of guessing at them.
	for verb, err := range map[string]error{"why": whyErr, "history_of": historyErr} {
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

	path := writeConfig(t, `repos:
  - path: `+gitClone(t)+`
    use: `+codePlugin+`
scheduler:
  interval: `+schedulerTick+`
`+embedderBlock)

	graph := new(startedGraph)
	rt := &graph.runtime
	graph.app = fx.New(
		fx.NopLogger,
		Workspace(path, stubRegistry(t)),
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
