package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"lore/internal/config"
	"lore/internal/di"
)

func TestInitWritesALoadableScaffold(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lore.yaml")

	res := run(t, nil, "init", "--config", path)
	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}
	if !strings.Contains(res.stdout, path) || !strings.Contains(res.stdout, di.EmbedderKeyEnv) {
		t.Errorf("stdout = %q, want the path written and the key variable to export", res.stdout)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 1 {
		t.Errorf("directory holds %v (err %v), want lore.yaml alone", entries, err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read scaffold: %v", err)
	}
	scaffold := string(raw)

	for _, want := range []string{
		"workspace: " + filepath.Base(dir),
		"token_env: LORE_GITHUB_TOKEN",
		"provider: openai",
		"model: text-embedding-3-small",
		"repos: []",
	} {
		if !strings.Contains(scaffold, want) {
			t.Errorf("scaffold is missing %q\n--- scaffold ---\n%s", want, scaffold)
		}
	}
	for _, line := range strings.Split(scaffold, "\n") {
		key, _, assigns := strings.Cut(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "#")), ":")
		if !assigns {
			continue
		}
		for _, secretish := range []string{"token", "key", "secret", "password"} {
			if strings.HasSuffix(key, secretish) {
				t.Errorf("scaffold line %q assigns a secret directly; only *_env keys are allowed", line)
			}
		}
	}

	t.Setenv("LORE_GITHUB_TOKEN", "ghp_example")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("the scaffold does not load: %v", err)
	}
	if cfg.Workspace != filepath.Base(dir) {
		t.Errorf("workspace = %q, want %q", cfg.Workspace, filepath.Base(dir))
	}
	if cfg.Sources.GitHub == nil || len(cfg.Sources.GitHub.Repos) != 1 {
		t.Errorf("sources.github = %+v, want one placeholder repository", cfg.Sources.GitHub)
	}
	if cfg.Embedder.Model != "text-embedding-3-small" {
		t.Errorf("embedder.model = %q, want the default model", cfg.Embedder.Model)
	}

	var tree map[string]any
	if err := yaml.Unmarshal(raw, &tree); err != nil {
		t.Fatalf("the scaffold is not valid YAML: %v", err)
	}
}

func TestInitRefusesToOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lore.yaml")
	const existing = "workspace: hand-edited\n"
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatalf("seed configuration: %v", err)
	}

	res := run(t, nil, "init", "--config", path)
	if res.exitCode != exitPrecondition {
		t.Fatalf("exit = %d, want %d", res.exitCode, exitPrecondition)
	}
	if !strings.Contains(res.stderr, path) || !strings.Contains(res.stderr, "already exists") {
		t.Errorf("stderr = %q, want it to name the file it refused to touch", res.stderr)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read configuration: %v", err)
	}
	if string(got) != existing {
		t.Errorf("configuration = %q, want it untouched", got)
	}
}
