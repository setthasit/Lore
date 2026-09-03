package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/setthasit/Lore/internal/errors/internalerror"
)

const repoPlaceholder = "{{REPO}}"

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
  - path: {{REPO}}
    remote: github:acme/myproject

query:
  event_window: 30d
  walk_depth: 3
  top_k: 12

embedder:
  provider: openai
  model: text-embedding-3-small
  base_url: https://api.openai.com

llm:
  provider: anthropic
  model: claude-sonnet-4-5
  api_key_env: LORE_LLM_KEY
  base_url: https://api.anthropic.com

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
			name: "an ask-only workspace with jira and notion but no github validates",
			yaml: `
workspace: asksources
sources:
  notion:
    token_env: LORE_NOTION_TOKEN
    root_pages: ["Engineering Wiki"]
  jira:
    base_url: https://acme.atlassian.net
    email_env: LORE_JIRA_EMAIL
    token_env: LORE_JIRA_TOKEN
    projects: [PROJ]
repos: []
`,
			env: map[string]string{
				"LORE_NOTION_TOKEN": "notion-fake-value",
				"LORE_JIRA_EMAIL":   "someone@example.test",
				"LORE_JIRA_TOKEN":   "jira-fake-value",
			},
			check: func(t *testing.T, cfg *Config) {
				if cfg.Sources.GitHub != nil {
					t.Errorf("sources.github = %+v, want none", cfg.Sources.GitHub)
				}
				if cfg.Sources.Notion == nil || cfg.Sources.Jira == nil {
					t.Fatalf("sources = %+v, want notion and jira", cfg.Sources)
				}
				if got := cfg.Sources.Jira.Projects; len(got) != 1 || got[0] != "PROJ" {
					t.Errorf("jira projects = %v", got)
				}
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
  - path: {{REPO}}
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
			name: "jira email_env naming an unset variable is rejected by name",
			yaml: `
workspace: unsetemail
sources:
  jira:
    base_url: https://acme.atlassian.net
    email_env: LORE_JIRA_EMAIL
    token_env: LORE_JIRA_TOKEN
`,
			env:     map[string]string{"LORE_JIRA_EMAIL": "", "LORE_JIRA_TOKEN": "jira-fake-value"},
			wantErr: "sources.jira.email_env names LORE_JIRA_EMAIL, but that environment variable is not set",
		},
		{
			name: "jira with a blank email_env is rejected",
			yaml: `
workspace: blankemail
sources:
  jira:
    base_url: https://acme.atlassian.net
    email_env: ""
    token_env: LORE_JIRA_TOKEN
`,
			env:     map[string]string{"LORE_JIRA_TOKEN": "jira-fake-value"},
			wantErr: "sources.jira.email_env must name an environment variable",
		},
		{
			name: "jira with an empty base_url is rejected",
			yaml: `
workspace: emptybaseurl
sources:
  jira:
    base_url: ""
    email_env: LORE_JIRA_EMAIL
    token_env: LORE_JIRA_TOKEN
`,
			env:     map[string]string{"LORE_JIRA_EMAIL": "someone@example.test", "LORE_JIRA_TOKEN": "jira-fake-value"},
			wantErr: "sources.jira.base_url must be set",
		},
		{
			name: "notion token_env naming an unset variable is rejected by name",
			yaml: `
workspace: unsetnotion
sources:
  notion:
    token_env: LORE_NOTION_TOKEN
    root_pages: ["Engineering Wiki"]
`,
			env:     map[string]string{"LORE_NOTION_TOKEN": ""},
			wantErr: "sources.notion.token_env names LORE_NOTION_TOKEN, but that environment variable is not set",
		},
		{
			name: "a gitlab source without base_url defaults to gitlab.com",
			yaml: `
workspace: gitlabdefault
sources:
  gitlab:
    token_env: LORE_GITLAB_TOKEN
    projects: [acme/myproject, acme/platform/infra]
`,
			env: map[string]string{"LORE_GITLAB_TOKEN": "gitlab-fake-value"},
			check: func(t *testing.T, cfg *Config) {
				gitlab := cfg.Sources.GitLab
				if gitlab == nil {
					t.Fatalf("sources = %+v, want gitlab", cfg.Sources)
				}
				if gitlab.BaseURL != "" {
					t.Errorf("gitlab base_url = %q, want it left to the connector default", gitlab.BaseURL)
				}
				if got := gitlab.Projects; len(got) != 2 || got[1] != "acme/platform/infra" {
					t.Errorf("gitlab projects = %v, want both namespaced paths", got)
				}
			},
		},
		{
			name: "gitlab with a blank token_env is rejected",
			yaml: `
workspace: blankgitlabtoken
sources:
  gitlab:
    token_env: ""
    projects: [acme/myproject]
`,
			wantErr: "sources.gitlab.token_env must name an environment variable",
		},
		{
			name: "gitlab token_env naming an unset variable is rejected by name",
			yaml: `
workspace: unsetgitlab
sources:
  gitlab:
    token_env: LORE_GITLAB_TOKEN
    projects: [acme/myproject]
`,
			env:     map[string]string{"LORE_GITLAB_TOKEN": ""},
			wantErr: "sources.gitlab.token_env names LORE_GITLAB_TOKEN, but that environment variable is not set",
		},
		{
			name: "gitlab without a project is rejected",
			yaml: `
workspace: noprojects
sources:
  gitlab:
    token_env: LORE_GITLAB_TOKEN
    projects: []
`,
			env:     map[string]string{"LORE_GITLAB_TOKEN": "gitlab-fake-value"},
			wantErr: `sources.gitlab.projects must list at least one "group/project" path`,
		},
		{
			name: "a gitlab base_url that is not absolute is rejected",
			yaml: `
workspace: relativegitlab
sources:
  gitlab:
    base_url: gitlab.acme.dev
    token_env: LORE_GITLAB_TOKEN
    projects: [acme/myproject]
`,
			env:     map[string]string{"LORE_GITLAB_TOKEN": "gitlab-fake-value"},
			wantErr: "sources.gitlab.base_url must be an absolute http(s) URL like https://gitlab.com, got gitlab.acme.dev",
		},
		{
			name: "a gitlab base_url with a foreign scheme is rejected",
			yaml: `
workspace: ftpgitlab
sources:
  gitlab:
    base_url: ftp://gitlab.acme.dev
    token_env: LORE_GITLAB_TOKEN
    projects: [acme/myproject]
`,
			env:     map[string]string{"LORE_GITLAB_TOKEN": "gitlab-fake-value"},
			wantErr: "sources.gitlab.base_url must be an absolute http(s) URL",
		},
		{
			name: "llm api key env must exist when named",
			yaml: `
workspace: withllm
repos:
  - path: {{REPO}}
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
			name: "an unknown embedder key is rejected",
			yaml: `
workspace: typo
repos:
  - path: ~/dev/myproject
embedder:
  dimension: 768
`,
			wantErr: "dimension",
		},
		{
			name: "the embedder block carries a base url and an explicit vector width",
			yaml: `
workspace: local
repos:
  - path: {{REPO}}
embedder:
  provider: ollama
  model: nomic-embed-text
  base_url: http://gpu-box.internal:11434
  dimensions: 768
`,
			check: func(t *testing.T, cfg *Config) {
				want := Embedder{
					Provider:   "ollama",
					Model:      "nomic-embed-text",
					BaseURL:    "http://gpu-box.internal:11434",
					Dimensions: 768,
				}
				if cfg.Embedder != want {
					t.Errorf("embedder = %+v, want %+v", cfg.Embedder, want)
				}
			},
		},
		{
			name: "defaults are applied",
			yaml: `
workspace: demo
repos:
  - path: {{REPO}}
`,
			check: func(t *testing.T, cfg *Config) {
				if want := homePath(t, ".lore", "demo.db"); cfg.IndexPath != want {
					t.Errorf("IndexPath = %q, want %q", cfg.IndexPath, want)
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
				if cfg.Scheduler.Interval != DefaultSchedulerInterval {
					t.Errorf("Scheduler.Interval = %s, want %s", cfg.Scheduler.Interval, DefaultSchedulerInterval)
				}
				if cfg.Embedder.BaseURL != "" || cfg.Embedder.Dimensions != 0 {
					t.Errorf("embedder = %+v, want an unset base_url and width", cfg.Embedder)
				}
			},
		},
		{
			name: "explicit tuning overrides defaults",
			yaml: `
workspace: tuned
index_path: /srv/lore/tuned.db
repos:
  - path: {{REPO}}
query:
  event_window: 12h
  walk_depth: 5
  top_k: 40
scheduler:
  interval: 5m
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
				if want := Duration(5 * time.Minute); cfg.Scheduler.Interval != want {
					t.Errorf("Scheduler.Interval = %s, want %s", cfg.Scheduler.Interval, want)
				}
			},
		},
		{
			name: "a zero scheduler interval reads as absent and takes the default",
			yaml: `
workspace: zerointerval
repos:
  - path: {{REPO}}
scheduler:
  interval: 0s
`,
			check: func(t *testing.T, cfg *Config) {
				if cfg.Scheduler.Interval != DefaultSchedulerInterval {
					t.Errorf("Scheduler.Interval = %s, want %s", cfg.Scheduler.Interval, DefaultSchedulerInterval)
				}
			},
		},
		{
			name: "a negative scheduler interval is rejected",
			yaml: `
workspace: negativeinterval
repos:
  - path: {{REPO}}
scheduler:
  interval: -5m
`,
			wantErr: "scheduler.interval must not be negative",
		},
		{
			name: "negative tuning is rejected",
			yaml: `
workspace: negative
repos:
  - path: {{REPO}}
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
				if want := homePath(t, ".lore", "myproject.db"); cfg.Workspace != "myproject" || cfg.IndexPath != want {
					t.Errorf("workspace/index_path = %q/%q, want myproject/%q", cfg.Workspace, cfg.IndexPath, want)
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
				if cfg.Embedder.Provider != "openai" || cfg.Embedder.Model != "text-embedding-3-small" ||
					cfg.Embedder.BaseURL != "https://api.openai.com" {
					t.Errorf("embedder = %+v", cfg.Embedder)
				}
				if cfg.LLM.Provider != "anthropic" || cfg.LLM.APIKeyEnv != "LORE_LLM_KEY" ||
					cfg.LLM.BaseURL != "https://api.anthropic.com" {
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

			body := test.yaml
			if strings.Contains(body, repoPlaceholder) {
				body = strings.ReplaceAll(body, repoPlaceholder, gitClone(t))
			}

			cfg, err := Load(writeConfig(t, body))

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

func TestValidateListenAddr(t *testing.T) {
	tests := []struct {
		name    string
		addr    string
		mtls    *MTLS
		wantErr string
	}{
		{name: "loopback IPv4 in the clear", addr: "127.0.0.1:8080"},
		{name: "another address in the loopback range", addr: "127.0.0.53:8080"},
		{name: "loopback IPv6 in the clear", addr: "[::1]:8080"},
		{name: "loopback IPv6 with a zone", addr: "[::1%lo0]:8080"},
		{name: "IPv4-mapped loopback", addr: "[::ffff:127.0.0.1]:8080"},
		{name: "port zero is still loopback", addr: "127.0.0.1:0"},
		{
			name:    "a bare port binds every interface",
			addr:    ":8080",
			wantErr: "is not a loopback address",
		},
		{
			name:    "every IPv4 interface",
			addr:    "0.0.0.0:8080",
			wantErr: "is not a loopback address",
		},
		{
			name:    "every IPv6 interface",
			addr:    "[::]:8080",
			wantErr: "is not a loopback address",
		},
		{
			name:    "a routable address",
			addr:    "10.1.2.3:8080",
			wantErr: "is not a loopback address",
		},
		{
			name:    "localhost is a name, not proof",
			addr:    "localhost:8080",
			wantErr: "is not a loopback address",
		},
		{
			name:    "a host that does not resolve",
			addr:    "nowhere.invalid:8080",
			wantErr: "is not a loopback address",
		},
		{
			name:    "no port",
			addr:    "127.0.0.1",
			wantErr: "must be a host:port address",
		},
		{
			name:    "empty",
			wantErr: "must be a host:port address",
		},
		{
			name:    "unbracketed IPv6",
			addr:    "::1:8080",
			wantErr: "must be a host:port address",
		},
		{
			name:    "a malformed address is refused even with TLS",
			addr:    "127.0.0.1:8080:9090",
			mtls:    &MTLS{Cert: "./certs/server.pem", Key: "./certs/server-key.pem"},
			wantErr: "must be a host:port address",
		},
		{
			name: "non-loopback with a certificate and a key",
			addr: "0.0.0.0:8443",
			mtls: &MTLS{Cert: "./certs/server.pem", Key: "./certs/server-key.pem"},
		},
		{
			name:    "non-loopback with a certificate but no key",
			addr:    "0.0.0.0:8443",
			mtls:    &MTLS{Cert: "./certs/server.pem"},
			wantErr: "is not a loopback address",
		},
		{
			name:    "non-loopback with a client CA only",
			addr:    "0.0.0.0:8443",
			mtls:    &MTLS{ClientCA: "./certs/ca.pem"},
			wantErr: "is not a loopback address",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := &Config{Server: Server{MTLS: test.mtls}}

			for _, setting := range []string{"server.http_addr", "server.grpc_addr"} {
				err := cfg.ValidateListenAddr(setting, test.addr)

				if test.wantErr == "" {
					if err != nil {
						t.Fatalf("ValidateListenAddr(%q, %q) = %v, want success", setting, test.addr, err)
					}
					continue
				}
				if err == nil {
					t.Fatalf("ValidateListenAddr(%q, %q) = nil, want error containing %q", setting, test.addr, test.wantErr)
				}
				if !internalerror.IsBadRequest(err) {
					t.Fatalf("ValidateListenAddr(%q, %q) error kind = %s, want bad request",
						setting, test.addr, internalerror.KindOf(err))
				}
				for _, want := range []string{test.wantErr, setting} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("ValidateListenAddr(%q, %q) error = %q, want it to contain %q",
							setting, test.addr, err, want)
					}
				}
			}
		})
	}
}

func TestValidateListenAddrRefusalNamesTheRemedy(t *testing.T) {
	err := new(Config).ValidateListenAddr("server.grpc_addr", "0.0.0.0:8443")
	if err == nil {
		t.Fatal("ValidateListenAddr() = nil, want a refusal")
	}
	for _, want := range []string{"server.grpc_addr", "server.mtls.cert", "server.mtls.key", "127.0.0.1:8443", "TLS"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err, want)
		}
	}
}

func TestLoadValidatesRepoPaths(t *testing.T) {
	tests := []struct {
		name    string
		path    func(*testing.T) string
		wantErr string
	}{
		{
			name: "a clone with a .git directory is accepted",
			path: gitClone,
		},
		{
			name: "a linked worktree with a .git file is accepted",
			path: gitWorktree,
		},
		{
			name:    "a path that does not exist is rejected",
			path:    func(t *testing.T) string { return filepath.Join(t.TempDir(), "absent") },
			wantErr: "does not exist",
		},
		{
			name:    "a directory that is not a git repository is rejected",
			path:    func(t *testing.T) string { return t.TempDir() },
			wantErr: "is not a git repository",
		},
		{
			name: "an unreadable .git entry is rejected as unreadable, not as missing",
			path: func(t *testing.T) string {
				dir := t.TempDir()
				if err := os.Symlink(".git", filepath.Join(dir, ".git")); err != nil {
					t.Fatalf("seed .git symlink loop: %v", err)
				}
				return dir
			},
			wantErr: "has a .git entry that cannot be read",
		},
		{
			name: "a file instead of a directory is rejected",
			path: func(t *testing.T) string {
				path := filepath.Join(t.TempDir(), "myproject")
				if err := os.WriteFile(path, nil, 0o600); err != nil {
					t.Fatalf("seed file: %v", err)
				}
				return path
			},
			wantErr: "is not a directory",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := test.path(t)

			cfg, err := Load(writeConfig(t, "workspace: anchored\nrepos:\n  - path: "+path+"\n"))

			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("Load() error = %v, want success", err)
				}
				if cfg.Repos[0].Path != path {
					t.Errorf("Repos[0].Path = %q, want %q", cfg.Repos[0].Path, path)
				}
				return
			}
			if err == nil {
				t.Fatalf("Load() = %+v, want error containing %q", cfg, test.wantErr)
			}
			if !internalerror.IsBadRequest(err) {
				t.Fatalf("Load() error kind = %s, want bad request", internalerror.KindOf(err))
			}
			for _, want := range []string{test.wantErr, path} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Load() error = %q, want it to contain %q", err, want)
				}
			}
		})
	}
}

func TestLoadExpandsTildePaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	clone := filepath.Join(home, "dev", "myproject")
	if err := os.MkdirAll(filepath.Join(clone, ".git"), 0o750); err != nil {
		t.Fatalf("seed clone: %v", err)
	}

	cfg, err := Load(writeConfig(t, "workspace: tilde\nrepos:\n  - path: ~/dev/myproject\n"))
	if err != nil {
		t.Fatalf("Load() error = %v, want success", err)
	}
	if want := filepath.Join(home, ".lore", "tilde.db"); cfg.IndexPath != want {
		t.Errorf("IndexPath = %q, want %q", cfg.IndexPath, want)
	}
	if cfg.Repos[0].Path != clone {
		t.Errorf("Repos[0].Path = %q, want %q", cfg.Repos[0].Path, clone)
	}
}

func TestExpandHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if got, err := expandHome("index_path", "~"); err != nil || got != home {
		t.Errorf(`expandHome("~") = %q, %v; want %q`, got, err, home)
	}
	for _, path := range []string{"./index.db", "/var/lib/lore/index.db", "index~backup.db", "~notauser/index.db"} {
		if got, err := expandHome("index_path", path); err != nil || got != path {
			t.Errorf("expandHome(%q) = %q, %v; want it unchanged", path, got, err)
		}
	}
}

func gitClone(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o750); err != nil {
		t.Fatalf("seed clone: %v", err)
	}
	return dir
}

func gitWorktree(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: /elsewhere/.git/worktrees/myproject\n"), 0o600); err != nil {
		t.Fatalf("seed worktree: %v", err)
	}
	return dir
}

func homePath(t *testing.T, parts ...string) string {
	t.Helper()

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("user home: %v", err)
	}
	return filepath.Join(append([]string{home}, parts...)...)
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "lore.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}
