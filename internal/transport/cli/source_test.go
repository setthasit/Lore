package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/setthasit/Lore/internal/config"
)

func TestSourceAddNotionWithDefaults(t *testing.T) {
	path := scaffolded(t)
	before := readConfigFile(t, path)

	res := runWithInput(t, nil, "\n\n", "source", "add", "notion", "--config", path)
	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}

	after := readConfigFile(t, path)
	assertInserted(t, before, after, "  notion:\n    token_env: LORE_NOTION_TOKEN\n")
	if strings.Contains(after, "root_pages") {
		t.Errorf("file = %q, want no root_pages key when no page was named", after)
	}

	cfg := decodeConfigFile(t, after)
	if cfg.Sources.Notion == nil || cfg.Sources.Notion.TokenEnv != "LORE_NOTION_TOKEN" {
		t.Fatalf("sources.notion = %+v, want the default variable name", cfg.Sources.Notion)
	}
	if len(cfg.Sources.Notion.RootPages) != 0 {
		t.Errorf("root_pages = %v, want none", cfg.Sources.Notion.RootPages)
	}
	if cfg.Sources.GitHub == nil {
		t.Error("the scaffolded github source went missing")
	}
	if !strings.Contains(res.stdout, path) || !strings.Contains(res.stdout, "export LORE_NOTION_TOKEN") {
		t.Errorf("stdout = %q, want the path written and the variable to export", res.stdout)
	}
	assertPromptsAskForNamesOnly(t, res.stdout)
}

func TestSourceAddNotionWithExplicitAnswers(t *testing.T) {
	path := scaffolded(t)
	before := readConfigFile(t, path)

	res := runWithInput(t, nil, "NOTION_PAT\nEngineering Wiki, Design Docs\n", "source", "add", "notion", "--config", path)
	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}

	after := readConfigFile(t, path)
	assertInserted(t, before, after, "  notion:\n"+
		"    token_env: NOTION_PAT\n"+
		"    root_pages:\n"+
		"      - Engineering Wiki\n"+
		"      - Design Docs\n")

	notion := decodeConfigFile(t, after).Sources.Notion
	if notion == nil || notion.TokenEnv != "NOTION_PAT" {
		t.Fatalf("sources.notion = %+v", notion)
	}
	if len(notion.RootPages) != 2 || notion.RootPages[1] != "Design Docs" {
		t.Errorf("root_pages = %v, want both pages", notion.RootPages)
	}
}

func TestSourceAddJira(t *testing.T) {
	path := scaffolded(t)
	before := readConfigFile(t, path)

	res := runWithInput(t, nil, "https://acme.atlassian.net\n\n\nPROJ, INFRA\n", "source", "add", "jira", "--config", path)
	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}

	after := readConfigFile(t, path)
	assertInserted(t, before, after, "  jira:\n"+
		"    base_url: https://acme.atlassian.net\n"+
		"    email_env: LORE_JIRA_EMAIL\n"+
		"    token_env: LORE_JIRA_TOKEN\n"+
		"    projects:\n"+
		"      - PROJ\n"+
		"      - INFRA\n")

	jira := decodeConfigFile(t, after).Sources.Jira
	if jira == nil || jira.BaseURL != "https://acme.atlassian.net" {
		t.Fatalf("sources.jira = %+v", jira)
	}
	if jira.EmailEnv != "LORE_JIRA_EMAIL" || jira.TokenEnv != "LORE_JIRA_TOKEN" {
		t.Errorf("jira env names = %q/%q, want the defaults", jira.EmailEnv, jira.TokenEnv)
	}
	if len(jira.Projects) != 2 || jira.Projects[1] != "INFRA" {
		t.Errorf("projects = %v, want both keys", jira.Projects)
	}
	if !strings.Contains(res.stdout, "export LORE_JIRA_EMAIL and LORE_JIRA_TOKEN") {
		t.Errorf("stdout = %q, want both variables to export", res.stdout)
	}
	assertPromptsAskForNamesOnly(t, res.stdout)
}

func TestSourceAddGitLabTakesTheGitLabComDefault(t *testing.T) {
	path := scaffolded(t)
	before := readConfigFile(t, path)

	res := runWithInput(t, nil, "\n\nacme/myproject, acme/platform/infra\n", "source", "add", "gitlab", "--config", path)
	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}

	after := readConfigFile(t, path)
	// base_url is written even when defaulted: a later move to a self-managed
	// instance is then an edit rather than a new key nobody knows about.
	assertInserted(t, before, after, "  gitlab:\n"+
		"    base_url: https://gitlab.com\n"+
		"    token_env: LORE_GITLAB_TOKEN\n"+
		"    projects:\n"+
		"      - acme/myproject\n"+
		"      - acme/platform/infra\n")

	gitlab := decodeConfigFile(t, after).Sources.GitLab
	if gitlab == nil || gitlab.BaseURL != "https://gitlab.com" {
		t.Fatalf("sources.gitlab = %+v", gitlab)
	}
	if gitlab.TokenEnv != "LORE_GITLAB_TOKEN" {
		t.Errorf("gitlab token_env = %q, want the default", gitlab.TokenEnv)
	}
	if len(gitlab.Projects) != 2 || gitlab.Projects[1] != "acme/platform/infra" {
		t.Errorf("projects = %v, want both namespaced paths", gitlab.Projects)
	}
	if !strings.Contains(res.stdout, "export LORE_GITLAB_TOKEN") {
		t.Errorf("stdout = %q, want the variable to export", res.stdout)
	}
	assertPromptsAskForNamesOnly(t, res.stdout)
}

func TestSourceAddGitLabAcceptsASelfManagedRoot(t *testing.T) {
	path := scaffolded(t)
	before := readConfigFile(t, path)

	res := runWithInput(t, nil, "https://gitlab.acme.dev/\nACME_GITLAB_PAT\nacme/widgets\n",
		"source", "add", "gitlab", "--config", path)
	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}

	after := readConfigFile(t, path)
	assertInserted(t, before, after, "  gitlab:\n"+
		"    base_url: https://gitlab.acme.dev/\n"+
		"    token_env: ACME_GITLAB_PAT\n"+
		"    projects:\n"+
		"      - acme/widgets\n")

	t.Setenv("LORE_GITHUB_TOKEN", "github-fake-value")
	t.Setenv("ACME_GITLAB_PAT", "gitlab-fake-value")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("the file no longer loads: %v", err)
	}
	if cfg.Sources.GitLab == nil || cfg.Sources.GitLab.TokenEnv != "ACME_GITLAB_PAT" {
		t.Errorf("sources.gitlab = %+v", cfg.Sources.GitLab)
	}
}

func TestSourceAddBothSourcesStillLoads(t *testing.T) {
	path := scaffolded(t)
	before := readConfigFile(t, path)

	if res := runWithInput(t, nil, "\n\n", "source", "add", "notion", "--config", path); res.exitCode != exitOK {
		t.Fatalf("adding notion: exit = %d, stderr = %q", res.exitCode, res.stderr)
	}
	if res := runWithInput(t, nil, "https://acme.atlassian.net\n\n\nPROJ\n", "source", "add", "jira", "--config", path); res.exitCode != exitOK {
		t.Fatalf("adding jira: exit = %d, stderr = %q", res.exitCode, res.stderr)
	}

	assertOriginalLinesKept(t, before, readConfigFile(t, path))

	t.Setenv("LORE_GITHUB_TOKEN", "github-fake-value")
	t.Setenv("LORE_NOTION_TOKEN", "notion-fake-value")
	t.Setenv("LORE_JIRA_EMAIL", "someone@example.test")
	t.Setenv("LORE_JIRA_TOKEN", "jira-fake-value")

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("the file no longer loads: %v", err)
	}
	if cfg.Sources.Notion == nil || cfg.Sources.Jira == nil || cfg.Sources.GitHub == nil {
		t.Errorf("sources = %+v, want all three", cfg.Sources)
	}
}

func TestSourceAddRefusesADuplicate(t *testing.T) {
	path := scaffolded(t)
	if res := runWithInput(t, nil, "\n\n", "source", "add", "notion", "--config", path); res.exitCode != exitOK {
		t.Fatalf("adding notion: exit = %d, stderr = %q", res.exitCode, res.stderr)
	}
	before := readConfigFile(t, path)

	res := runWithInput(t, nil, "OTHER_PAT\n\n", "source", "add", "notion", "--config", path)
	if res.exitCode != exitPrecondition {
		t.Fatalf("exit = %d, want %d (stderr %q)", res.exitCode, exitPrecondition, res.stderr)
	}
	if !strings.Contains(res.stderr, "sources.notion") || !strings.Contains(res.stderr, path) {
		t.Errorf("stderr = %q, want it to name the source and the file to edit", res.stderr)
	}
	if after := readConfigFile(t, path); after != before {
		t.Errorf("file = %q, want it untouched after the refusal", after)
	}
}

func TestSourceAddCreatesTheSourcesSection(t *testing.T) {
	const trimmed = `workspace: askonly

# sources:
#   github:
#     token_env: LORE_GITHUB_TOKEN
repos: []

embedder:
  provider: openai
  model: text-embedding-3-small
`
	path := writeConfigFile(t, trimmed)

	res := runWithInput(t, nil, "https://acme.atlassian.net\n\n\n\n", "source", "add", "jira", "--config", path)
	if res.exitCode != exitOK {
		t.Fatalf("exit = %d, stderr = %q", res.exitCode, res.stderr)
	}

	after := readConfigFile(t, path)
	assertInserted(t, trimmed, after, "sources:\n"+
		"  jira:\n"+
		"    base_url: https://acme.atlassian.net\n"+
		"    email_env: LORE_JIRA_EMAIL\n"+
		"    token_env: LORE_JIRA_TOKEN\n")
	if !strings.HasSuffix(after, "    token_env: LORE_JIRA_TOKEN\n") {
		t.Errorf("file = %q, want the new section appended at the end", after)
	}
	if strings.Contains(after, "projects") {
		t.Errorf("file = %q, want no projects key when no project was named", after)
	}
	if jira := decodeConfigFile(t, after).Sources.Jira; jira == nil || len(jira.Projects) != 0 {
		t.Errorf("sources.jira = %+v, want a source scoped to every visible project", jira)
	}
}

func TestSourceAddRefusesAnInlineSourcesValue(t *testing.T) {
	const inline = `workspace: askonly
sources: {}
repos: []
`
	path := writeConfigFile(t, inline)

	res := runWithInput(t, nil, "\n\n", "source", "add", "notion", "--config", path)
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

// A user who pastes a credential where a variable name was asked for must not see it echoed.
func TestSourceAddNeverEchoesARejectedAnswer(t *testing.T) {
	const pasted = "secret_pasted!credential"
	path := scaffolded(t)

	res := runWithInput(t, nil, pasted+"\n\n", "source", "add", "notion", "--config", path)
	if res.exitCode != exitBadRequest {
		t.Fatalf("exit = %d, want %d", res.exitCode, exitBadRequest)
	}
	for name, stream := range map[string]string{"stderr": res.stderr, "stdout": res.stdout} {
		if strings.Contains(stream, pasted) {
			t.Errorf("%s = %q, want the rejected answer absent", name, stream)
		}
	}
}

func TestSourceAddRejectsBadInput(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		answers  string
		wantErr  string
		wantExit int
	}{
		{
			name:     "an env var name that is not an identifier",
			args:     []string{"notion"},
			answers:  "not a name!\n\n",
			wantErr:  "sources.notion.token_env must be an environment variable name",
			wantExit: exitBadRequest,
		},
		{
			name:     "a base url with no default and no answer",
			args:     []string{"jira"},
			answers:  "\n",
			wantErr:  "sources.jira.base_url must be set",
			wantExit: exitBadRequest,
		},
		{
			name:     "a base url that is not http",
			args:     []string{"jira"},
			answers:  "ftp://acme.atlassian.net\n",
			wantErr:  "sources.jira.base_url must be an absolute http(s) URL",
			wantExit: exitBadRequest,
		},
		{
			name:     "a base url with no host",
			args:     []string{"jira"},
			answers:  "acme.atlassian.net\n",
			wantErr:  "sources.jira.base_url must be an absolute http(s) URL",
			wantExit: exitBadRequest,
		},
		{
			name:     "a gitlab base url that is not http",
			args:     []string{"gitlab"},
			answers:  "ftp://gitlab.acme.dev\n",
			wantErr:  "sources.gitlab.base_url must be an absolute http(s) URL like https://gitlab.com",
			wantExit: exitBadRequest,
		},
		{
			name:     "a gitlab source that names no project",
			args:     []string{"gitlab"},
			answers:  "\n\n\n",
			wantErr:  "sources.gitlab.projects must list at least one entry",
			wantExit: exitBadRequest,
		},
		{
			name:     "an unknown source",
			args:     []string{"bitbucket"},
			wantErr:  "unknown source bitbucket",
			wantExit: exitBadRequest,
		},
		{
			name:     "github, which lore init scaffolds",
			args:     []string{"github"},
			wantErr:  "lore init",
			wantExit: exitBadRequest,
		},
		{
			name:     "no source at all",
			wantErr:  "name the source to add",
			wantExit: exitBadRequest,
		},
		{
			name:     "more than one source",
			args:     []string{"notion", "jira"},
			wantErr:  "accepts at most 1 arg",
			wantExit: exitBadRequest,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := scaffolded(t)
			before := readConfigFile(t, path)

			args := append([]string{"source", "add"}, test.args...)
			res := runWithInput(t, nil, test.answers, append(args, "--config", path)...)

			if res.exitCode != test.wantExit {
				t.Fatalf("exit = %d, want %d (stderr %q)", res.exitCode, test.wantExit, res.stderr)
			}
			if !strings.Contains(res.stderr, test.wantErr) {
				t.Errorf("stderr = %q, want it to contain %q", res.stderr, test.wantErr)
			}
			if after := readConfigFile(t, path); after != before {
				t.Errorf("file = %q, want it untouched after the refusal", after)
			}
		})
	}
}

func TestSourceAddWithoutAConfigFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lore.yaml")

	res := runWithInput(t, nil, "\n\n", "source", "add", "notion", "--config", path)
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

func scaffolded(t *testing.T) string {
	t.Helper()

	return writeConfigFile(t, scaffold("myproject"))
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

func decodeConfigFile(t *testing.T, content string) *config.Config {
	t.Helper()

	decoder := yaml.NewDecoder(strings.NewReader(content))
	decoder.KnownFields(true)

	var cfg config.Config
	if err := decoder.Decode(&cfg); err != nil {
		t.Fatalf("the file does not decode: %v\n--- file ---\n%s", err, content)
	}
	return &cfg
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

func assertInserted(t *testing.T, before, after, block string) {
	t.Helper()

	assertOriginalLinesKept(t, before, after)

	anchored := "\nsources:\n" + strings.TrimPrefix(block, "sources:\n")
	if !strings.Contains(after, anchored) {
		t.Errorf("file does not hold, as the first child of a top-level sources:\n%s--- file ---\n%s", block, after)
	}
	if want := strings.Count(before, "\n") + strings.Count(block, "\n"); strings.Count(after, "\n") != want {
		t.Errorf("file holds %d lines, want %d — the block is not the only addition\n--- file ---\n%s",
			strings.Count(after, "\n"), want, after)
	}
}

func assertPromptsAskForNamesOnly(t *testing.T, prompts string) {
	t.Helper()

	if !strings.Contains(prompts, "name of the environment variable") || !strings.Contains(prompts, "never the value") {
		t.Errorf("prompts = %q, want every credential prompt to ask for an environment variable name", prompts)
	}
	for _, asking := range []string{"password", "paste", "secret", "api key", "credential:"} {
		if strings.Contains(strings.ToLower(prompts), asking) {
			t.Errorf("prompts = %q, want no prompt for %q", prompts, asking)
		}
	}
}
