package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lore/internal/errors/internalerror"
)

const docExample = `
workspace: myproject
index_path: ~/.lore/myproject.db

sources:
  github:
    token_env: LORE_GITHUB_TOKEN
    repos:
      - acme/myproject
      - acme/myproject-infra
  notion:
    token_env: LORE_NOTION_TOKEN
    root_pages:
      - "Engineering Wiki"
  jira:
    base_url: https://acme.atlassian.net
    email_env: LORE_JIRA_EMAIL
    token_env: LORE_JIRA_TOKEN
    projects: [PROJ, INFRA]

repos:
  - path: ~/dev/myproject
    remote: github:acme/myproject

query:
  event_window: 30d
  walk_depth: 3
  top_k: 12

embedder:
  provider: openai
  model: text-embedding-3-small

llm:
  provider: anthropic
  model: claude-sonnet-4-5
  api_key_env: LORE_LLM_KEY

scheduler:
  interval: 30m

server:
  http_addr: ":8080"
  grpc_addr: ":9090"
  mtls:
    cert: ./certs/server.pem
    key: ./certs/server-key.pem
    client_ca: ./certs/ca.pem
`

func TestLoad(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		env     map[string]string
		wantErr string
		check   func(*testing.T, *Config)
	}{
		{
			name: "zero repos is a valid ask-only workspace",
			yaml: `
workspace: askonly
sources:
  notion:
    token_env: LORE_NOTION_TOKEN
    root_pages: ["Engineering Wiki"]
repos: []
`,
			env: map[string]string{"LORE_NOTION_TOKEN": "notion-fake-value"},
			check: func(t *testing.T, cfg *Config) {
				if len(cfg.Repos) != 0 {
					t.Fatalf("Repos = %v, want none", cfg.Repos)
				}
			},
		},
		{
			name: "local clones without any source are valid",
			yaml: `
workspace: clonesonly
repos:
  - path: ~/dev/myproject
    remote: github:acme/myproject
`,
			check: func(t *testing.T, cfg *Config) {
				if cfg.Repos[0].Remote != "github:acme/myproject" {
					t.Fatalf("Repos[0].Remote = %q", cfg.Repos[0].Remote)
				}
			},
		},
		{
			name: "neither sources nor repos is rejected",
			yaml: `
workspace: empty
sources: {}
repos: []
`,
			wantErr: "at least one of sources or repos",
		},
		{
			name:    "workspace is required",
			yaml:    "repos:\n  - path: ~/dev/myproject\n",
			wantErr: "workspace must be set",
		},
		{
			name: "missing env var is rejected by name",
			yaml: `
workspace: jiraonly
sources:
  jira:
    base_url: https://acme.atlassian.net
    email_env: LORE_JIRA_EMAIL
    token_env: LORE_JIRA_TOKEN
    projects: [PROJ]
`,
			env:     map[string]string{"LORE_JIRA_EMAIL": "someone@example.test", "LORE_JIRA_TOKEN": ""},
			wantErr: "LORE_JIRA_TOKEN",
		},
		{
			name: "configured source without an env var name is rejected",
			yaml: `
workspace: nogithubtoken
sources:
  github:
    repos: [acme/myproject]
`,
			wantErr: "sources.github.token_env must name an environment variable",
		},
		{
			name: "jira source without base_url is rejected",
			yaml: `
workspace: nobaseurl
sources:
  jira:
    email_env: LORE_JIRA_EMAIL
    token_env: LORE_JIRA_TOKEN
`,
			env:     map[string]string{"LORE_JIRA_EMAIL": "someone@example.test", "LORE_JIRA_TOKEN": "jira-fake-value"},
			wantErr: "sources.jira.base_url must be set",
		},
		{
			name: "llm api key env must exist when named",
			yaml: `
workspace: withllm
repos:
  - path: ~/dev/myproject
llm:
  provider: anthropic
  model: claude-sonnet-4-5
  api_key_env: LORE_LLM_KEY
`,
			env:     map[string]string{"LORE_LLM_KEY": ""},
			wantErr: "LORE_LLM_KEY",
		},
		{
			name: "unknown key is rejected",
			yaml: `
workspace: typo
repos:
  - path: ~/dev/myproject
query:
  tp_k: 12
`,
			wantErr: "tp_k",
		},
		{
			name: "defaults are applied",
			yaml: `
workspace: demo
repos:
  - path: ~/dev/demo
`,
			check: func(t *testing.T, cfg *Config) {
				if cfg.IndexPath != "~/.lore/demo.db" {
					t.Errorf("IndexPath = %q, want ~/.lore/demo.db", cfg.IndexPath)
				}
				if cfg.Query.EventWindow != DefaultEventWindow {
					t.Errorf("EventWindow = %s, want %s", cfg.Query.EventWindow, DefaultEventWindow)
				}
				if cfg.Query.WalkDepth != DefaultWalkDepth {
					t.Errorf("WalkDepth = %d, want %d", cfg.Query.WalkDepth, DefaultWalkDepth)
				}
				if cfg.Query.TopK != DefaultTopK {
					t.Errorf("TopK = %d, want %d", cfg.Query.TopK, DefaultTopK)
				}
			},
		},
		{
			name: "explicit tuning overrides defaults",
			yaml: `
workspace: tuned
index_path: /srv/lore/tuned.db
repos:
  - path: ~/dev/tuned
query:
  event_window: 12h
  walk_depth: 5
  top_k: 40
`,
			check: func(t *testing.T, cfg *Config) {
				if cfg.IndexPath != "/srv/lore/tuned.db" {
					t.Errorf("IndexPath = %q", cfg.IndexPath)
				}
				if want := Duration(12 * time.Hour); cfg.Query.EventWindow != want {
					t.Errorf("EventWindow = %s, want %s", cfg.Query.EventWindow, want)
				}
				if cfg.Query.WalkDepth != 5 || cfg.Query.TopK != 40 {
					t.Errorf("WalkDepth/TopK = %d/%d, want 5/40", cfg.Query.WalkDepth, cfg.Query.TopK)
				}
			},
		},
		{
			name: "negative tuning is rejected",
			yaml: `
workspace: negative
repos:
  - path: ~/dev/negative
query:
  event_window: -5d
`,
			wantErr: "query.event_window must not be negative",
		},
		{
			name: "unparseable duration is rejected",
			yaml: `
workspace: badduration
repos:
  - path: ~/dev/bad
scheduler:
  interval: 30 sundays
`,
			wantErr: `invalid duration "30 sundays"`,
		},
		{
			name: "repo entry without a path is rejected",
			yaml: `
workspace: pathless
repos:
  - remote: github:acme/myproject
`,
			wantErr: "every entry in repos must have a path",
		},
		{
			name: "documented example loads",
			yaml: docExample,
			env: map[string]string{
				"LORE_GITHUB_TOKEN": "github-fake-value",
				"LORE_NOTION_TOKEN": "notion-fake-value",
				"LORE_JIRA_EMAIL":   "someone@example.test",
				"LORE_JIRA_TOKEN":   "jira-fake-value",
				"LORE_LLM_KEY":      "llm-fake-value",
			},
			check: func(t *testing.T, cfg *Config) {
				if cfg.Workspace != "myproject" || cfg.IndexPath != "~/.lore/myproject.db" {
					t.Errorf("workspace/index_path = %q/%q", cfg.Workspace, cfg.IndexPath)
				}
				if got := cfg.Sources.GitHub.Repos; len(got) != 2 || got[0] != "acme/myproject" {
					t.Errorf("github repos = %v", got)
				}
				if got := cfg.Sources.Notion.RootPages; len(got) != 1 || got[0] != "Engineering Wiki" {
					t.Errorf("notion root_pages = %v", got)
				}
				if cfg.Sources.Jira.BaseURL != "https://acme.atlassian.net" {
					t.Errorf("jira base_url = %q", cfg.Sources.Jira.BaseURL)
				}
				if got := cfg.Sources.Jira.Projects; len(got) != 2 || got[1] != "INFRA" {
					t.Errorf("jira projects = %v", got)
				}
				if cfg.Query.EventWindow != DefaultEventWindow {
					t.Errorf("event_window = %s, want %s", cfg.Query.EventWindow, DefaultEventWindow)
				}
				if cfg.Embedder.Provider != "openai" || cfg.Embedder.Model != "text-embedding-3-small" {
					t.Errorf("embedder = %+v", cfg.Embedder)
				}
				if cfg.LLM.Provider != "anthropic" || cfg.LLM.APIKeyEnv != "LORE_LLM_KEY" {
					t.Errorf("llm = %+v", cfg.LLM)
				}
				if want := Duration(30 * time.Minute); cfg.Scheduler.Interval != want {
					t.Errorf("scheduler.interval = %s, want %s", cfg.Scheduler.Interval, want)
				}
				if cfg.Server.HTTPAddr != ":8080" || cfg.Server.GRPCAddr != ":9090" {
					t.Errorf("server = %+v", cfg.Server)
				}
				if cfg.Server.MTLS.ClientCA != "./certs/ca.pem" {
					t.Errorf("mtls = %+v", cfg.Server.MTLS)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for name, value := range test.env {
				t.Setenv(name, value)
			}

			cfg, err := Load(writeConfig(t, test.yaml))

			if test.wantErr != "" {
				if err == nil {
					t.Fatalf("Load() = %+v, want error containing %q", cfg, test.wantErr)
				}
				if !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("Load() error = %q, want it to contain %q", err, test.wantErr)
				}
				if !internalerror.IsBadRequest(err) {
					t.Fatalf("Load() error kind = %s, want bad request", internalerror.KindOf(err))
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() error = %v, want success", err)
			}
			test.check(t, cfg)
		})
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "absent.yaml"))
	if err == nil {
		t.Fatal("Load() succeeded, want error")
	}
	if !internalerror.IsNotFound(err) {
		t.Fatalf("Load() error kind = %s, want not found", internalerror.KindOf(err))
	}
	if !strings.Contains(err.Error(), "absent.yaml") {
		t.Fatalf("Load() error = %q, want it to name the path", err)
	}
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "lore.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}
