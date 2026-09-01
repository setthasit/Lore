package di

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.uber.org/fx"

	"lore/internal/entities"
	"lore/internal/errors/internalerror"
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
