package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/setthasit/Lore/internal/registry"
	"github.com/setthasit/Lore/sdk"
)

// The scaffold and the prompts are generated from manifests, so the tests are
// written against stub plugins: they exercise every field type and capability
// combination the renderer has to handle, which no real plugin set happens to.
type stubSource struct{ manifest lore.Manifest }

func (s stubSource) Manifest() lore.Manifest { return s.manifest }

func (stubSource) NewSource(lore.SourceConfig) (lore.Connector, error) { return nil, nil }

type stubProvider struct{ manifest lore.Manifest }

func (p stubProvider) Manifest() lore.Manifest { return p.manifest }

func (stubProvider) NewProvider(lore.ProviderConfig) (lore.Provider, error) { return nil, nil }

// forgePlugin declares one of every field type, so a scaffold and a prompt run
// over it cover required and optional handling for all of them.
func forgePlugin() lore.Plugin {
	return stubSource{lore.Manifest{
		Name:         "forge",
		Kind:         lore.KindSource,
		APIVersion:   lore.APIVersion,
		Summary:      "a stub forge source",
		Capabilities: lore.Capabilities{RepoRemotes: true},
		Fields: []lore.Field{
			{
				Name:     "repos",
				Type:     lore.FieldStringList,
				Required: true,
				Prompt:   "Repositories to ingest, each \"owner/name\"",
				Doc:      "each entry is \"owner/name\"",
			},
			{
				Name:    "base_url",
				Type:    lore.FieldURL,
				Default: "https://forge.example",
				Prompt:  "Forge base URL",
				Doc:     "override to reach a self-managed instance",
			},
			{Name: "since", Type: lore.FieldDuration, Prompt: "How far back to ingest"},
			{Name: "batch", Type: lore.FieldInt, Prompt: "Items per request"},
			{Name: "archived", Type: lore.FieldBool, Prompt: "Include archived repositories"},
		},
		Secrets: []lore.Secret{{
			Key:         "token",
			ConfigField: "token_env",
			DefaultEnv:  "LORE_FORGE_TOKEN",
			Doc:         "a read-only token for the listed repositories",
		}},
	}}
}

// trackerPlugin has a required field with no default, which is the only shape
// that can refuse an empty answer.
func trackerPlugin() lore.Plugin {
	return stubSource{lore.Manifest{
		Name:       "tracker",
		Kind:       lore.KindSource,
		APIVersion: lore.APIVersion,
		Summary:    "a stub issue tracker source",
		Fields: []lore.Field{
			{Name: "base_url", Type: lore.FieldURL, Required: true, Prompt: "Tracker base URL"},
			{Name: "projects", Type: lore.FieldStringList, Required: true, Prompt: "Project keys to sync, comma-separated"},
		},
		Secrets: []lore.Secret{{
			Key:         "api_token",
			ConfigField: "token_env",
			DefaultEnv:  "LORE_TRACKER_TOKEN",
		}},
	}}
}

func vectorsPlugin() lore.Plugin {
	return stubProvider{lore.Manifest{
		Name:          "vectors",
		Kind:          lore.KindProvider,
		APIVersion:    lore.APIVersion,
		Summary:       "a stub embedding provider",
		Capabilities:  lore.Capabilities{Embed: true},
		DefaultModels: map[lore.Capability]string{lore.CapabilityEmbed: "embed-small"},
		Secrets: []lore.Secret{{
			Key:         "api_key",
			ConfigField: "api_key_env",
			DefaultEnv:  "VECTORS_API_KEY",
		}},
	}}
}

func chatterPlugin() lore.Plugin {
	return stubProvider{lore.Manifest{
		Name:          "chatter",
		Kind:          lore.KindProvider,
		APIVersion:    lore.APIVersion,
		Summary:       "a stub completion provider",
		Capabilities:  lore.Capabilities{Complete: true},
		DefaultModels: map[lore.Capability]string{lore.CapabilityComplete: "chat-large"},
		Secrets: []lore.Secret{{
			Key:         "api_key",
			ConfigField: "api_key_env",
			DefaultEnv:  "CHATTER_API_KEY",
		}},
	}}
}

func stubRegistry(t *testing.T, pluginSet ...lore.Plugin) *registry.Registry {
	t.Helper()

	reg := registry.New(lore.Host{})
	if err := reg.Register(pluginSet...); err != nil {
		t.Fatalf("register stub plugins: %v", err)
	}
	return reg
}

type pluginResult struct {
	stdout   string
	stderr   string
	exitCode int
}

// runPlugins drives the two manifest-driven commands over a registry of the
// caller's choosing, which is the whole point of them taking one.
func runPlugins(t *testing.T, reg *registry.Registry, stdin string, args ...string) pluginResult {
	t.Helper()

	var out, errOut bytes.Buffer
	configPath := new(string)

	root := &cobra.Command{Use: "lore", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().StringVar(configPath, "config", defaultConfigPath, "path to lore.yaml")
	root.AddCommand(newInitCommand(configPath, reg), newSourceCommand(configPath, reg))
	root.SetIn(strings.NewReader(stdin))
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs(args)

	res := pluginResult{}
	if err := root.ExecuteContext(context.Background()); err != nil {
		res.exitCode = report(&errOut, err)
	}
	res.stdout, res.stderr = out.String(), errOut.String()
	return res
}

func TestInitRendersTheStarterPluginsFromTheirManifests(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lore.yaml")
	reg := stubRegistry(t, forgePlugin(), trackerPlugin(), vectorsPlugin(), chatterPlugin())

	res := runPlugins(t, reg, "", "init", "--config", path)
	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}
	if !strings.Contains(res.stdout, path) {
		t.Errorf("stdout = %q, want the path written", res.stdout)
	}
	for _, variable := range []string{"LORE_FORGE_TOKEN", "VECTORS_API_KEY"} {
		if !strings.Contains(res.stdout, variable) {
			t.Errorf("stdout = %q, want it to name %s, which the scaffold mentions", res.stdout, variable)
		}
	}
	if strings.Contains(res.stdout, "CHATTER_API_KEY") {
		t.Errorf("stdout = %q, want no variable for the commented-out llm stanza", res.stdout)
	}

	scaffold := readConfigFile(t, path)
	for _, want := range []string{
		"workspace: " + filepath.Base(dir),
		"# index_path: ~/.lore/" + filepath.Base(dir) + ".db",
		"sources:\n  - use: forge\n    with:\n",
		"      token_env: LORE_FORGE_TOKEN",
		"      repos: []",
		"      # base_url: https://forge.example",
		"      # since: \"\"",
		"      # batch: 0",
		"      # archived: false",
		"# each entry is \"owner/name\"",
		"repos: []",
		"#     remote: forge:acme/" + filepath.Base(dir),
		"embedder:\n",
		"  provider: vectors",
		"  model: embed-small",
		"lore sync --reembed",
		"# llm:\n",
		"#   provider: chatter",
		"#   model: chat-large",
		"# scheduler:",
	} {
		if !strings.Contains(scaffold, want) {
			t.Errorf("scaffold is missing %q\n--- scaffold ---\n%s", want, scaffold)
		}
	}
	// The starter source is the first registered one; the second must not appear.
	if strings.Contains(scaffold, "tracker") {
		t.Errorf("scaffold names a plugin that is not the starter\n--- scaffold ---\n%s", scaffold)
	}
	assertNoSecretValues(t, scaffold)

	cfg := decodeConfigFile(t, scaffold)
	if len(cfg.Sources) != 1 || cfg.Sources[0].Use != "forge" {
		t.Fatalf("sources = %+v, want one starter instance of the starter plugin", cfg.Sources)
	}
	values, err := cfg.Sources[0].WithValues()
	if err != nil {
		t.Fatalf("with: does not decode: %v", err)
	}
	if values["token_env"] != "LORE_FORGE_TOKEN" {
		t.Errorf("with.token_env = %v, want the manifest's default variable", values["token_env"])
	}
	if repos, ok := values["repos"].([]any); !ok || len(repos) != 0 {
		t.Errorf("with.repos = %v, want an empty placeholder list", values["repos"])
	}
	if _, present := values["base_url"]; present {
		t.Errorf("with = %v, want the optional keys commented out", values)
	}
	if cfg.Embedder.Provider != "vectors" || cfg.Embedder.Model != "embed-small" {
		t.Errorf("embedder = %+v, want the starter embedding provider and its suggested model", cfg.Embedder)
	}
	if cfg.LLM != nil {
		t.Errorf("llm = %+v, want the stanza to stay commented out so a fresh workspace loads unchanged", cfg.LLM)
	}
}

func TestInitRefusesABuildItCannotScaffold(t *testing.T) {
	tests := []struct {
		name    string
		plugins []lore.Plugin
		wantErr string
	}{
		{
			name:    "no source plugin at all",
			plugins: []lore.Plugin{vectorsPlugin(), chatterPlugin()},
			wantErr: "no source plugin",
		},
		{
			name:    "no provider serving embeddings",
			plugins: []lore.Plugin{forgePlugin(), chatterPlugin()},
			wantErr: "no provider serving embeddings",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "lore.yaml")

			res := runPlugins(t, stubRegistry(t, test.plugins...), "", "init", "--config", path)
			if res.exitCode != exitPrecondition {
				t.Fatalf("exit = %d, want %d (stderr %q)", res.exitCode, exitPrecondition, res.stderr)
			}
			if !strings.Contains(res.stderr, test.wantErr) {
				t.Errorf("stderr = %q, want it to contain %q", res.stderr, test.wantErr)
			}
			// A scaffold that cannot load is worse than none: nothing is written.
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Errorf("stat %s = %v, want no file written", path, err)
			}
		})
	}
}

func TestInitRefusesToOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lore.yaml")
	const existing = "workspace: hand-edited\n"
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatalf("seed configuration: %v", err)
	}

	res := runPlugins(t, stubRegistry(t, forgePlugin(), trackerPlugin(), vectorsPlugin(), chatterPlugin()),
		"", "init", "--config", path)
	if res.exitCode != exitPrecondition {
		t.Fatalf("exit = %d, want %d", res.exitCode, exitPrecondition)
	}
	if !strings.Contains(res.stderr, path) || !strings.Contains(res.stderr, "already exists") {
		t.Errorf("stderr = %q, want it to name the file it refused to touch", res.stderr)
	}
	if got := readConfigFile(t, path); got != existing {
		t.Errorf("configuration = %q, want it untouched", got)
	}
}

// assertNoSecretValues holds the line no generated file may cross: a *_env key
// names a variable, and anything else that looks like a credential key would be
// holding the credential itself.
func assertNoSecretValues(t *testing.T, content string) {
	t.Helper()

	for _, line := range strings.Split(content, "\n") {
		key, _, assigns := strings.Cut(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "#")), ":")
		if !assigns {
			continue
		}
		for _, secretish := range []string{"token", "key", "secret", "password"} {
			if strings.HasSuffix(key, secretish) {
				t.Errorf("line %q assigns a secret directly; only *_env keys are allowed", line)
			}
		}
	}
}
