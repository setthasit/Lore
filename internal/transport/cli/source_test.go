package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/setthasit/Lore/internal/config"
	"github.com/setthasit/Lore/internal/registry"
)

// seeded is a hand-written configuration with comments in it, which is what
// `source add` has to leave alone: the splice exists so a round trip through the
// YAML encoder never reflows a file a human wrote.
const seeded = `workspace: myproject

# Sources say what to INGEST: one item per instance, in sync order.
sources:
  - use: forge                             # the starter instance
    with:
      token_env: LORE_FORGE_TOKEN
      repos: []

# Local clones, for blame and file history only.
repos: []

embedder:
  provider: vectors
  model: embed-small
`

// The answers a full run over forgePlugin's manifest needs: the token variable
// defaulted, one repository, the base URL defaulted, and the three optional
// fields left out.
const forgeAnswers = "\nacme/app\n\n\n\n\n"

// trackerAnswers covers the other shape: a required field with no default.
const trackerAnswers = "\nhttps://tracker.example\nPROJ, INFRA\n"

func TestSourceAddAppendsASequenceItem(t *testing.T) {
	path := writeConfigFile(t, seeded)

	res := runPlugins(t, sourceRegistry(t), trackerAnswers, "source", "add", "tracker", "--config", path)
	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}

	const want = `workspace: myproject

# Sources say what to INGEST: one item per instance, in sync order.
sources:
  - use: forge                             # the starter instance
    with:
      token_env: LORE_FORGE_TOKEN
      repos: []
  - use: tracker
    with:
      token_env: LORE_TRACKER_TOKEN
      base_url: https://tracker.example
      projects:
        - PROJ
        - INFRA

# Local clones, for blame and file history only.
repos: []

embedder:
  provider: vectors
  model: embed-small
`
	after := readConfigFile(t, path)
	if after != want {
		t.Errorf("file =\n%s\nwant\n%s", after, want)
	}
	assertOriginalLinesKept(t, seeded, after)

	cfg := decodeConfigFile(t, after)
	if len(cfg.Sources) != 2 || cfg.Sources[1].Ident() != "tracker" {
		t.Fatalf("sources = %+v, want the new instance appended after the existing one", cfg.Sources)
	}
	values, err := cfg.Sources[1].WithValues()
	if err != nil {
		t.Fatalf("with: does not decode: %v", err)
	}
	if values["base_url"] != "https://tracker.example" {
		t.Errorf("with.base_url = %v, want the answer given", values["base_url"])
	}
	if projects, ok := values["projects"].([]any); !ok || len(projects) != 2 || projects[1] != "INFRA" {
		t.Errorf("with.projects = %v, want both keys", values["projects"])
	}

	// The transcript is the manifest read aloud: one question per declared
	// secret and field, in declaration order, and a credential question that
	// asks for a variable name.
	const transcript = "name of the environment variable holding the tracker api token" +
		" — the name, never the value [LORE_TRACKER_TOKEN]: " +
		"Tracker base URL: " +
		"Project keys to sync, comma-separated: "
	if !strings.HasPrefix(res.stdout, transcript) {
		t.Errorf("stdout = %q, want it to open with the prompt sequence\n%q", res.stdout, transcript)
	}
	if !strings.Contains(res.stdout, path) || !strings.Contains(res.stdout, "export LORE_TRACKER_TOKEN") {
		t.Errorf("stdout = %q, want the path written and the variable to export", res.stdout)
	}
	if strings.Contains(res.stdout, "needs its own id") {
		t.Errorf("stdout = %q, want no id prompt for the first instance of a plugin", res.stdout)
	}
	assertPromptsAskForNamesOnly(t, res.stdout)
}

// A sequence indented differently is still one sequence, and an item spliced at
// the wrong indentation would read as a nested one.
func TestSourceAddAppendsIntoAMultiItemSequenceAtItsOwnIndent(t *testing.T) {
	const multi = `workspace: myproject
sources:
    - use: forge
      with:
        token_env: LORE_FORGE_TOKEN
        repos: [acme/app]
    - id: forge-infra
      use: forge
      with:
        token_env: LORE_FORGE_TOKEN
        repos: [acme/infra]
repos: []
`
	path := writeConfigFile(t, multi)

	res := runPlugins(t, sourceRegistry(t), trackerAnswers, "source", "add", "tracker", "--config", path)
	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}

	after := readConfigFile(t, path)
	assertOriginalLinesKept(t, multi, after)
	const item = `    - use: tracker
      with:
        token_env: LORE_TRACKER_TOKEN
        base_url: https://tracker.example
        projects:
          - PROJ
          - INFRA
`
	if !strings.Contains(after, item) {
		t.Errorf("file =\n%s\nwant it to hold, at the sequence's own indent\n%s", after, item)
	}
	if !strings.Contains(after, item+"repos: []\n") {
		t.Errorf("file =\n%s\nwant the item appended as the block's last one", after)
	}

	cfg := decodeConfigFile(t, after)
	if len(cfg.Sources) != 3 || cfg.Sources[2].Ident() != "tracker" {
		t.Errorf("sources = %+v, want three instances with the new one last", cfg.Sources)
	}
}

func TestSourceAddAsksForAnIDWhenThePluginAlreadyHasAnInstance(t *testing.T) {
	path := writeConfigFile(t, seeded)

	res := runPlugins(t, sourceRegistry(t), "forge-infra\n"+forgeAnswers, "source", "add", "forge", "--config", path)
	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}
	// Every declared field is asked for, optional ones included: the operator
	// sees what the plugin accepts even when the answer is to skip it.
	const transcript = "sources already has an instance called forge," +
		" so this one needs its own id, for example forge-2: " +
		"name of the environment variable holding the forge token" +
		" — the name, never the value [LORE_FORGE_TOKEN]: " +
		"Repositories to ingest, each \"owner/name\": " +
		"Forge base URL [https://forge.example]: " +
		"How far back to ingest: " +
		"Items per request: " +
		"Include archived repositories: "
	if !strings.HasPrefix(res.stdout, transcript) {
		t.Errorf("stdout = %q, want it to open with the prompt sequence\n%q", res.stdout, transcript)
	}

	after := readConfigFile(t, path)
	assertOriginalLinesKept(t, seeded, after)
	const item = `  - id: forge-infra
    use: forge
    with:
      token_env: LORE_FORGE_TOKEN
      repos:
        - acme/app
      base_url: https://forge.example
`
	if !strings.Contains(after, item) {
		t.Errorf("file =\n%s\nwant it to hold\n%s", after, item)
	}
	if strings.Contains(after, "since") || strings.Contains(after, "batch") || strings.Contains(after, "archived") {
		t.Errorf("file =\n%s\nwant the unanswered optional fields left out", after)
	}

	cfg := decodeConfigFile(t, after)
	if len(cfg.Sources) != 2 || cfg.Sources[1].Ident() != "forge-infra" {
		t.Errorf("sources = %+v, want a second instance under its own id", cfg.Sources)
	}
	if err := cfg.Validate(); err != nil {
		// Two instances of one plugin are legitimate; only a shared identity is not.
		t.Errorf("the file no longer validates: %v", err)
	}
}

func TestSourceAddRefusesAnIDAlreadyInUse(t *testing.T) {
	path := writeConfigFile(t, seeded)

	res := runPlugins(t, sourceRegistry(t), "forge\n"+forgeAnswers, "source", "add", "forge", "--config", path)
	if res.exitCode != exitBadRequest {
		t.Fatalf("exit = %d, want %d (stderr %q)", res.exitCode, exitBadRequest, res.stderr)
	}
	if !strings.Contains(res.stderr, "already has an instance called forge") {
		t.Errorf("stderr = %q, want it to name the identity that is taken", res.stderr)
	}
	if after := readConfigFile(t, path); after != seeded {
		t.Errorf("file = %q, want it untouched after the refusal", after)
	}
}

func TestSourceAddOnAnUnknownPluginListsTheRegisteredSources(t *testing.T) {
	path := writeConfigFile(t, seeded)

	res := runPlugins(t, sourceRegistry(t), "", "source", "add", "nosuchforge", "--config", path)
	if res.exitCode != exitBadRequest {
		t.Fatalf("exit = %d, want %d (stderr %q)", res.exitCode, exitBadRequest, res.stderr)
	}
	for _, want := range []string{"nosuchforge", "forge", "tracker", "lore plugin list"} {
		if !strings.Contains(res.stderr, want) {
			t.Errorf("stderr = %q, want it to contain %q", res.stderr, want)
		}
	}
	// A provider is registered but is not a source, so it must not be offered.
	if strings.Contains(res.stderr, "vectors") {
		t.Errorf("stderr = %q, want only the source plugins listed", res.stderr)
	}
	if after := readConfigFile(t, path); after != seeded {
		t.Errorf("file = %q, want it untouched after the refusal", after)
	}
}

func TestSourceAddCreatesTheSourcesSection(t *testing.T) {
	const askOnly = `workspace: askonly

# sources:
#   - use: forge
repos: []
`
	path := writeConfigFile(t, askOnly)

	res := runPlugins(t, sourceRegistry(t), trackerAnswers, "source", "add", "tracker", "--config", path)
	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}

	after := readConfigFile(t, path)
	assertOriginalLinesKept(t, askOnly, after)
	if !strings.HasPrefix(after, askOnly) {
		t.Errorf("file =\n%s\nwant the new section appended after everything that was there", after)
	}
	if !strings.HasSuffix(after, "sources:\n  - use: tracker\n    with:\n"+
		"      token_env: LORE_TRACKER_TOKEN\n"+
		"      base_url: https://tracker.example\n"+
		"      projects:\n        - PROJ\n        - INFRA\n") {
		t.Errorf("file =\n%s\nwant a sources: block created at the end", after)
	}
	if len(decodeConfigFile(t, after).Sources) != 1 {
		t.Errorf("file =\n%s\nwant exactly the new instance", after)
	}
}

// `sources: []` says "no instances" in a shape nothing can be appended to, so
// the key is reopened rather than the command refusing a valid configuration.
func TestSourceAddReopensAnEmptyFlowSequence(t *testing.T) {
	const empty = `workspace: askonly
sources: []                                # nothing yet
repos: []
`
	path := writeConfigFile(t, empty)

	res := runPlugins(t, sourceRegistry(t), trackerAnswers, "source", "add", "tracker", "--config", path)
	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}

	const want = `workspace: askonly
sources: # nothing yet
  - use: tracker
    with:
      token_env: LORE_TRACKER_TOKEN
      base_url: https://tracker.example
      projects:
        - PROJ
        - INFRA
repos: []
`
	if after := readConfigFile(t, path); after != want {
		t.Errorf("file =\n%s\nwant\n%s", after, want)
	}
}

// An inline flow sequence is a valid configuration no text splice can extend,
// so the command refuses it and says what to do rather than reflowing the file.
func TestSourceAddRefusesAnInlineSourcesValue(t *testing.T) {
	const inline = `workspace: askonly
sources: [{use: forge, with: {repos: [acme/app]}}]
repos: []
`
	path := writeConfigFile(t, inline)

	res := runPlugins(t, sourceRegistry(t), trackerAnswers, "source", "add", "tracker", "--config", path)
	if res.exitCode != exitPrecondition {
		t.Fatalf("exit = %d, want %d, stderr = %q", res.exitCode, exitPrecondition, res.stderr)
	}
	if !strings.Contains(res.stderr, "inline value") {
		t.Errorf("stderr = %q, want it to name the inline value", res.stderr)
	}
	if after := readConfigFile(t, path); after != inline {
		t.Errorf("file = %q, want it untouched after the refusal", after)
	}
}

// A user who pastes a credential where a variable name was asked for must not
// see it echoed, and must not find it in the file either.
func TestSourceAddNeverWritesOrEchoesASecretValue(t *testing.T) {
	const pasted = "glpat-Pasted!Credential"
	path := writeConfigFile(t, seeded)

	res := runPlugins(t, sourceRegistry(t), pasted+"\n"+trackerAnswers, "source", "add", "tracker", "--config", path)
	if res.exitCode != exitBadRequest {
		t.Fatalf("exit = %d, want %d (stderr %q)", res.exitCode, exitBadRequest, res.stderr)
	}
	for name, stream := range map[string]string{"stderr": res.stderr, "stdout": res.stdout} {
		if strings.Contains(stream, pasted) {
			t.Errorf("%s = %q, want the rejected answer absent", name, stream)
		}
	}

	after := readConfigFile(t, path)
	if after != seeded {
		t.Errorf("file = %q, want it untouched after the refusal", after)
	}
	if strings.Contains(after, pasted) {
		t.Errorf("file = %q, want no pasted credential in it", after)
	}
}

func TestSourceAddWritesOnlyVariableNamesForSecrets(t *testing.T) {
	path := writeConfigFile(t, seeded)

	res := runPlugins(t, sourceRegistry(t), "TRACKER_PAT\nhttps://tracker.example\nPROJ\n",
		"source", "add", "tracker", "--config", path)
	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}

	after := readConfigFile(t, path)
	if !strings.Contains(after, "token_env: TRACKER_PAT") {
		t.Errorf("file =\n%s\nwant the variable name the operator gave", after)
	}
	assertNoSecretValues(t, after)
	assertPromptsAskForNamesOnly(t, res.stdout)
}

func TestSourceAddRefusesBadAnswersAndLeavesTheFileAlone(t *testing.T) {
	tests := []struct {
		name    string
		plugin  string
		answers string
		wantErr string
	}{
		{
			name:    "a required field left empty",
			plugin:  "tracker",
			answers: "\n\n",
			wantErr: "sources[tracker].with.base_url must be set",
		},
		{
			name:    "a required list left empty",
			plugin:  "tracker",
			answers: "\nhttps://tracker.example\n\n",
			wantErr: "sources[tracker].with.projects must list at least one entry",
		},
		{
			name:    "a url that is not absolute http",
			plugin:  "tracker",
			answers: "\ntracker.example\n",
			wantErr: "sources[tracker].with.base_url must be an absolute http(s) URL",
		},
		{
			// A second instance of a plugin already in the file is asked for an
			// id first, so these answers open with one.
			name:    "a url whose default is offered as the example",
			plugin:  "forge",
			answers: "forge-2\n\nacme/app\nftp://forge.acme.dev\n",
			wantErr: "sources[forge-2].with.base_url must be an absolute http(s) URL like https://forge.example",
		},
		{
			name:    "a non-numeric int",
			plugin:  "forge",
			answers: "forge-2\n\nacme/app\n\n\nseven\n",
			wantErr: `sources[forge-2].with.batch must be a whole number, got "seven"`,
		},
		{
			name:    "a bool that is neither",
			plugin:  "forge",
			answers: "forge-2\n\nacme/app\n\n\n50\nperhaps\n",
			wantErr: `sources[forge-2].with.archived must be true or false, got "perhaps"`,
		},
		{
			name:    "a duration that does not parse",
			plugin:  "forge",
			answers: "forge-2\n\nacme/app\n\nlast tuesday\n",
			wantErr: `sources[forge-2].with.since must be a duration like 30m or 30d, got "last tuesday"`,
		},
		{
			name:    "an env var name that is not a variable name",
			plugin:  "tracker",
			answers: "not a name!\n\n",
			wantErr: "sources[tracker].with.token_env must be an environment variable name like LORE_TRACKER_TOKEN",
		},
		{
			name:    "no plugin at all",
			answers: "",
			wantErr: "name the source plugin to add",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := writeConfigFile(t, seeded)

			args := []string{"source", "add"}
			if test.plugin != "" {
				args = append(args, test.plugin)
			}
			res := runPlugins(t, sourceRegistry(t), test.answers, append(args, "--config", path)...)

			if res.exitCode != exitBadRequest {
				t.Fatalf("exit = %d, want %d (stderr %q)", res.exitCode, exitBadRequest, res.stderr)
			}
			if !strings.Contains(res.stderr, test.wantErr) {
				t.Errorf("stderr = %q, want it to contain %q", res.stderr, test.wantErr)
			}
			if after := readConfigFile(t, path); after != seeded {
				t.Errorf("file = %q, want it untouched after the refusal", after)
			}
		})
	}
}

func TestSourceAddWithoutAConfigFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lore.yaml")

	res := runPlugins(t, sourceRegistry(t), trackerAnswers, "source", "add", "tracker", "--config", path)
	if res.exitCode != exitNotFound {
		t.Fatalf("exit = %d, want %d (stderr %q)", res.exitCode, exitNotFound, res.stderr)
	}
	if !strings.Contains(res.stderr, path) || !strings.Contains(res.stderr, "lore init") {
		t.Errorf("stderr = %q, want it to name the path and point at `lore init`", res.stderr)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("stat %s = %v, want the file still absent", path, err)
	}
}

func TestSourceAddUsageListsTheRegisteredSourcePlugins(t *testing.T) {
	res := runPlugins(t, sourceRegistry(t), "", "source", "add", "--help")
	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}
	if !strings.Contains(res.stdout, "add <forge|tracker>") {
		t.Errorf("help = %q, want the usage line built from the registry", res.stdout)
	}
}

// sourceRegistry is the build these tests pretend to be: two source plugins so
// the usage line and the refusals have something to list, and one provider so
// the seeded configuration's embedder names something real.
func sourceRegistry(t *testing.T) *registry.Registry {
	t.Helper()

	return stubRegistry(t, forgePlugin(), trackerPlugin(), vectorsPlugin())
}

func writeConfigFile(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "lore.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("seed configuration: %v", err)
	}
	return path
}

func readConfigFile(t *testing.T, path string) string {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read configuration: %v", err)
	}
	return string(raw)
}

// decodeConfigFile goes through the one strict decoder the repository has, so a
// spliced file that this package accepts is one the next `lore` run accepts too.
func decodeConfigFile(t *testing.T, content string) *config.Config {
	t.Helper()

	cfg, err := config.Decode(strings.NewReader(content))
	if err != nil {
		t.Fatalf("the file does not decode: %v\n--- file ---\n%s", err, content)
	}
	return cfg
}

func assertOriginalLinesKept(t *testing.T, before, after string) {
	t.Helper()

	kept := map[string]int{}
	for _, line := range strings.Split(after, "\n") {
		kept[line]++
	}
	for _, line := range strings.Split(before, "\n") {
		kept[line]--
		if kept[line] < 0 {
			t.Fatalf("line %q was dropped or rewritten\n--- file ---\n%s", line, after)
		}
	}
}

func assertPromptsAskForNamesOnly(t *testing.T, prompts string) {
	t.Helper()

	if !strings.Contains(prompts, "name of the environment variable") || !strings.Contains(prompts, "never the value") {
		t.Errorf("prompts = %q, want every credential prompt to ask for an environment variable name", prompts)
	}
	for _, asking := range []string{"password", "paste", "value of", "credential:"} {
		if strings.Contains(strings.ToLower(prompts), asking) {
			t.Errorf("prompts = %q, want no prompt for %q", prompts, asking)
		}
	}
}
