package di

import (
	"context"
	"os"
	"path/filepath"
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
  - path: /tmp/does-not-need-to-exist
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
  - path: /tmp/does-not-need-to-exist
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
  - path: /tmp/does-not-need-to-exist
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

func TestWorkspaceGraphRejectsUnbuiltSource(t *testing.T) {
	t.Setenv("LORE_TEST_NOTION_TOKEN", "secret_example")
	t.Setenv(EmbedderKeyEnv, "sk-example")

	path := writeConfig(t, `sources:
  notion:
    token_env: LORE_TEST_NOTION_TOKEN
    root_pages: ["Engineering Wiki"]
`)

	_, err := resolveWorkspace(t, path)
	if err == nil {
		t.Fatal("resolve workspace: want an error naming the unbuilt source")
	}
	if got := internalerror.KindOf(err); got != internalerror.KindPrecondition {
		t.Errorf("kind = %s, want %s", got, internalerror.KindPrecondition)
	}
	if !strings.Contains(err.Error(), "sources.notion") {
		t.Errorf("error %q does not name the source", err)
	}
}

func TestResolvePathExpandsHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	got, err := resolvePath("~/.lore/myproject.db")
	if err != nil {
		t.Fatalf("resolvePath: %v", err)
	}
	if want := filepath.Join(home, ".lore", "myproject.db"); got != want {
		t.Errorf("resolvePath = %q, want %q", got, want)
	}

	for _, path := range []string{"./index.db", "/var/lib/lore/index.db", "index~backup.db"} {
		if got, err := resolvePath(path); err != nil || got != path {
			t.Errorf("resolvePath(%q) = %q, %v; want it unchanged", path, got, err)
		}
	}

	if _, err := resolvePath(""); internalerror.KindOf(err) != internalerror.KindBadRequest {
		t.Errorf("resolvePath(\"\") = %v, want a bad request", err)
	}
}
