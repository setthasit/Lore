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

plugins:
  - name: linear
    from: github.com/jdoe/lore-linear@v0.3.1
    pubkey: ./keys/jdoe.pub

sources:
  - use: github
    with:
      token_env: LORE_GITHUB_TOKEN
      repos:
        - acme/myproject
        - acme/myproject-infra
  - id: jira-acme
    use: jira
    with: { base_url: https://acme.atlassian.net, projects: [PROJ, INFRA] }
  - id: linear
    use: linear
    with: { team: PLATFORM, token_env: LORE_LINEAR_TOKEN }

providers:
  - id: openrouter
    use: openai-compatible
    with:
      base_url: https://openrouter.ai/api
      api_key_env: LORE_OPENROUTER_KEY

embedder:
  provider: openai
  model: text-embedding-3-small

llm:
  provider: openrouter
  model: moonshotai/kimi-k2

repos:
  - path: {{REPO}}
    use: git
    remote: github:acme/myproject

query:
  event_window: 30d
  walk_depth: 3
  top_k: 12

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

// minimal is a configuration every rule accepts, so a test that asserts one
// refusal only has to spell the key it is breaking.
const minimal = `
workspace: myproject
sources:
  - use: github
embedder:
  provider: openai
  model: text-embedding-3-small
`

func TestLoad(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr []string
		check   func(*testing.T, *Config)
	}{
		{
			name: "the documented example loads whole",
			yaml: docExample,
			check: func(t *testing.T, cfg *Config) {
				if cfg.Workspace != "myproject" {
					t.Errorf("Workspace = %q", cfg.Workspace)
				}
				if want := homePath(t, ".lore", "myproject.db"); cfg.IndexPath != want {
					t.Errorf("IndexPath = %q, want %q", cfg.IndexPath, want)
				}
				if len(cfg.Plugins) != 1 || cfg.Plugins[0] != (PluginDecl{
					Name: "linear", From: "github.com/jdoe/lore-linear@v0.3.1", PubKey: "./keys/jdoe.pub",
				}) {
					t.Errorf("Plugins = %+v", cfg.Plugins)
				}
				if got := idents(cfg.Sources); !equal(got, []string{"github", "jira-acme", "linear"}) {
					t.Errorf("source idents = %v", got)
				}
				if cfg.Sources[1].Use != "jira" {
					t.Errorf("Sources[1].Use = %q, want the plugin, not the instance id", cfg.Sources[1].Use)
				}
				values, err := cfg.Sources[1].WithValues()
				if err != nil {
					t.Fatalf("WithValues() error = %v", err)
				}
				if values["base_url"] != "https://acme.atlassian.net" {
					t.Errorf("with = %v, want the block captured verbatim", values)
				}
				if len(cfg.Providers) != 1 || cfg.Providers[0].Ident() != "openrouter" ||
					cfg.Providers[0].Use != "openai-compatible" {
					t.Errorf("Providers = %+v", cfg.Providers)
				}
				if cfg.Embedder != (RoleBinding{Provider: "openai", Model: "text-embedding-3-small"}) {
					t.Errorf("Embedder = %+v", cfg.Embedder)
				}
				if cfg.LLM == nil || *cfg.LLM != (RoleBinding{Provider: "openrouter", Model: "moonshotai/kimi-k2"}) {
					t.Errorf("LLM = %+v", cfg.LLM)
				}
				if len(cfg.Repos) != 1 || cfg.Repos[0].Use != "git" || cfg.Repos[0].Remote != "github:acme/myproject" {
					t.Errorf("Repos = %+v", cfg.Repos)
				}
				if cfg.Query.EventWindow != Duration(30*24*time.Hour) || cfg.Query.WalkDepth != 3 || cfg.Query.TopK != 12 {
					t.Errorf("Query = %+v", cfg.Query)
				}
				if cfg.Scheduler.Interval != Duration(30*time.Minute) {
					t.Errorf("Scheduler = %+v", cfg.Scheduler)
				}
				if cfg.Server.HTTPAddr != ":8080" || cfg.Server.GRPCAddr != ":9090" ||
					cfg.Server.MTLS.ClientCA != "./certs/ca.pem" {
					t.Errorf("Server = %+v", cfg.Server)
				}
			},
		},
		{
			name: "an instance id defaults to the plugin it uses",
			yaml: minimal,
			check: func(t *testing.T, cfg *Config) {
				if got := cfg.Sources[0].Ident(); got != "github" {
					t.Errorf("Ident() = %q, want the plugin name", got)
				}
				if cfg.Sources[0].ID != "" {
					t.Errorf("ID = %q, want it left absent", cfg.Sources[0].ID)
				}
			},
		},
		{
			name: "two instances of one plugin with distinct ids load",
			yaml: `
workspace: twosites
sources:
  - id: jira-acme
    use: jira
    with: { base_url: https://acme.atlassian.net }
  - id: jira-legacy
    use: jira
    with: { base_url: https://legacy.atlassian.net }
embedder: { provider: openai, model: text-embedding-3-small }
`,
			check: func(t *testing.T, cfg *Config) {
				if got := idents(cfg.Sources); !equal(got, []string{"jira-acme", "jira-legacy"}) {
					t.Errorf("source idents = %v", got)
				}
			},
		},
		{
			name: "a source with no with: block loads and configures nothing",
			yaml: minimal,
			check: func(t *testing.T, cfg *Config) {
				values, err := cfg.Sources[0].WithValues()
				if err != nil {
					t.Fatalf("WithValues() error = %v", err)
				}
				if values != nil {
					t.Errorf("WithValues() = %v, want nil for an absent block", values)
				}
			},
		},
		{
			name: "zero repos is a valid ask-only workspace",
			yaml: minimal + "repos: []\n",
			check: func(t *testing.T, cfg *Config) {
				if len(cfg.Repos) != 0 {
					t.Fatalf("Repos = %v, want none", cfg.Repos)
				}
			},
		},
		{
			name: "a workspace with repos and no sources loads",
			yaml: `
workspace: anchored
repos:
  - path: ` + repoPlaceholder + `
embedder: { provider: openai, model: text-embedding-3-small }
`,
			check: func(t *testing.T, cfg *Config) {
				if cfg.Repos[0].Use != DefaultRepoPlugin {
					t.Errorf("Repos[0].Use = %q, want %q", cfg.Repos[0].Use, DefaultRepoPlugin)
				}
			},
		},
		{
			name: "query, scheduler and index_path fall back to defaults",
			yaml: minimal,
			check: func(t *testing.T, cfg *Config) {
				if cfg.Query.EventWindow != DefaultEventWindow || cfg.Query.WalkDepth != DefaultWalkDepth ||
					cfg.Query.TopK != DefaultTopK {
					t.Errorf("Query = %+v, want the defaults", cfg.Query)
				}
				if cfg.Scheduler.Interval != DefaultSchedulerInterval {
					t.Errorf("Scheduler.Interval = %s, want the default", cfg.Scheduler.Interval)
				}
				if want := homePath(t, ".lore", "myproject.db"); cfg.IndexPath != want {
					t.Errorf("IndexPath = %q, want %q", cfg.IndexPath, want)
				}
			},
		},
		{
			name:    "an unknown key is refused, naming the line",
			yaml:    "workspace: typo\nsourcs: []\n",
			wantErr: []string{"invalid configuration", "sourcs", "line 2"},
		},
		{
			name:    "a workspace name is required",
			yaml:    "sources:\n  - use: github\n",
			wantErr: []string{"workspace must be set"},
		},
		{
			name:    "a workspace that ingests nothing and anchors nothing is refused",
			yaml:    "workspace: empty\nembedder: { provider: openai }\n",
			wantErr: []string{"at least one of sources or repos must be configured"},
		},
		{
			name: "a source instance must name a plugin",
			yaml: `
workspace: nouse
sources:
  - id: jira-acme
    with: { base_url: https://acme.atlassian.net }
embedder: { provider: openai }
`,
			wantErr: []string{"sources[jira-acme].use must be set"},
		},
		{
			name: "a source instance with neither id nor plugin is reported by position",
			yaml: `
workspace: nouse
sources:
  - use: github
  - with: { base_url: https://acme.atlassian.net }
embedder: { provider: openai }
`,
			wantErr: []string{"sources[1].use must be set"},
		},
		{
			name: "a provider instance must name a plugin",
			yaml: minimal + `
providers:
  - id: openrouter
    with: { base_url: https://openrouter.ai/api }
`,
			wantErr: []string{"providers[openrouter].use must be set"},
		},
		{
			name: "two unnamed instances of one plugin are refused with the fix",
			yaml: `
workspace: twojira
sources:
  - use: jira
    with: { base_url: https://acme.atlassian.net }
  - use: jira
    with: { base_url: https://legacy.atlassian.net }
embedder: { provider: openai }
`,
			wantErr: []string{`sources lists "jira" twice`, "give each instance a distinct id", "id: jira-acme"},
		},
		{
			name: "a repeated source id is refused",
			yaml: `
workspace: dupid
sources:
  - id: jira-acme
    use: jira
  - id: jira-acme
    use: notion
embedder: { provider: openai }
`,
			wantErr: []string{"sources[jira-acme] is declared twice", "every id in sources must be unique"},
		},
		{
			name: "an explicit id that shadows a defaulted one is refused",
			yaml: `
workspace: shadowed
sources:
  - use: jira
  - id: jira
    use: notion
embedder: { provider: openai }
`,
			wantErr: []string{"sources[jira] is declared twice"},
		},
		{
			name: "a repeated provider id is refused",
			yaml: minimal + `
providers:
  - id: openai
    use: openai
  - id: openai
    use: openai-compatible
`,
			wantErr: []string{"providers[openai] is declared twice", "every id in providers must be unique"},
		},
		{
			name:    "an embedder must name a provider",
			yaml:    "workspace: noembed\nsources:\n  - use: github\n",
			wantErr: []string{"embedder.provider must be set"},
		},
		{
			name: "a negative embedder dimension is refused",
			yaml: `
workspace: negdim
sources:
  - use: github
embedder: { provider: ollama, model: nomic-embed-text, dimensions: -1 }
`,
			wantErr: []string{"embedder.dimensions must not be negative, got -1"},
		},
		{
			name:    "a negative event window is refused",
			yaml:    minimal + "query: { event_window: -30m }\n",
			wantErr: []string{"query.event_window must not be negative", "-30m"},
		},
		{
			name:    "a negative walk depth is refused",
			yaml:    minimal + "query: { walk_depth: -1 }\n",
			wantErr: []string{"query.walk_depth must not be negative"},
		},
		{
			name:    "a negative top_k is refused",
			yaml:    minimal + "query: { top_k: -1 }\n",
			wantErr: []string{"query.top_k must not be negative"},
		},
		{
			name:    "a negative scheduler interval is refused",
			yaml:    minimal + "scheduler: { interval: -30m }\n",
			wantErr: []string{"scheduler.interval must not be negative", "-30m"},
		},
		{
			name:    "a repo entry must have a path",
			yaml:    minimal + "repos:\n  - remote: github:acme/myproject\n",
			wantErr: []string{"every entry in repos must have a path"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := test.yaml
			if strings.Contains(body, repoPlaceholder) {
				body = strings.ReplaceAll(body, repoPlaceholder, gitClone(t))
			}

			cfg, err := Load(writeConfig(t, body))

			if len(test.wantErr) > 0 {
				if err == nil {
					t.Fatalf("Load() = %+v, want error containing %q", cfg, test.wantErr)
				}
				if !internalerror.IsBadRequest(err) {
					t.Fatalf("Load() error kind = %s, want bad request", internalerror.KindOf(err))
				}
				for _, want := range test.wantErr {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("Load() error = %q, want it to contain %q", err, want)
					}
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

func TestLoadNamesTheFileItRefused(t *testing.T) {
	path := writeConfig(t, "workspace: typo\nsourcs: []\n")

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load() succeeded, want error")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("Load() error = %q, want it to name %q", err, path)
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

// Decode is what the commands that rewrite a configuration read it with, so it
// must accept a file Load would reject for being incomplete, and must still
// reject a key no field claims.
func TestDecode(t *testing.T) {
	t.Run("an empty document is a configuration of nothing", func(t *testing.T) {
		cfg, err := Decode(strings.NewReader(""))
		if err != nil {
			t.Fatalf("Decode() error = %v, want success", err)
		}
		if cfg == nil || cfg.Workspace != "" || len(cfg.Sources) != 0 {
			t.Errorf("Decode() = %+v, want a zero configuration", cfg)
		}
	})

	t.Run("no defaults and no validation are applied", func(t *testing.T) {
		cfg, err := Decode(strings.NewReader("sources:\n  - use: github\n"))
		if err != nil {
			t.Fatalf("Decode() error = %v, want success", err)
		}
		if cfg.Query.TopK != 0 || cfg.Scheduler.Interval != 0 || cfg.IndexPath != "" {
			t.Errorf("Decode() = %+v, want every default left absent", cfg)
		}
	})

	t.Run("an unknown key is refused", func(t *testing.T) {
		_, err := Decode(strings.NewReader("workspace: typo\nsourcs: []\n"))
		if err == nil {
			t.Fatal("Decode() succeeded, want error")
		}
		if !internalerror.IsBadRequest(err) {
			t.Fatalf("Decode() error kind = %s, want bad request", internalerror.KindOf(err))
		}
		if !strings.Contains(err.Error(), "sourcs") {
			t.Errorf("Decode() error = %q, want it to name the unknown key", err)
		}
	})

	t.Run("a plugin's own with: keys are not checked here", func(t *testing.T) {
		cfg, err := Decode(strings.NewReader("sources:\n  - use: github\n    with: { anything_at_all: 1 }\n"))
		if err != nil {
			t.Fatalf("Decode() error = %v, want the block captured unread", err)
		}
		values, err := cfg.Sources[0].WithValues()
		if err != nil {
			t.Fatalf("WithValues() error = %v", err)
		}
		if values["anything_at_all"] != 1 {
			t.Errorf("with = %v, want the key preserved for the plugin", values)
		}
	})

	t.Run("a misspelled instance key is refused, not ignored", func(t *testing.T) {
		_, err := Decode(strings.NewReader("sources:\n  - usee: github\n"))
		if err == nil {
			t.Fatal("Decode() succeeded, want error")
		}
		for _, want := range []string{"usee", "line 2", "id, use and with"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("Decode() error = %q, want it to contain %q", err, want)
			}
		}
	})

	t.Run("an instance that is not a mapping is refused", func(t *testing.T) {
		_, err := Decode(strings.NewReader("sources:\n  - github\n"))
		if err == nil {
			t.Fatal("Decode() succeeded, want error")
		}
		if !strings.Contains(err.Error(), "an instance must be a mapping that names a plugin with a use key") {
			t.Errorf("Decode() error = %q, want it to say what an instance looks like", err)
		}
	})
}

func TestInstanceIdent(t *testing.T) {
	tests := []struct {
		instance Instance
		want     string
	}{
		{instance: Instance{Use: "github"}, want: "github"},
		{instance: Instance{ID: "jira-acme", Use: "jira"}, want: "jira-acme"},
		{instance: Instance{}, want: ""},
	}
	for _, test := range tests {
		if got := test.instance.Ident(); got != test.want {
			t.Errorf("Instance%+v.Ident() = %q, want %q", test.instance, got, test.want)
		}
	}
}

func TestInstanceWithValues(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
		check   func(*testing.T, map[string]any)
	}{
		{
			name: "an absent block is no configuration",
			yaml: "- use: github\n",
			check: func(t *testing.T, values map[string]any) {
				if values != nil {
					t.Errorf("WithValues() = %v, want nil", values)
				}
			},
		},
		{
			name: "an empty block is no configuration",
			yaml: "- use: github\n  with:\n",
			check: func(t *testing.T, values map[string]any) {
				if values != nil {
					t.Errorf("WithValues() = %v, want nil", values)
				}
			},
		},
		{
			name: "a mapping decodes to generic values the registry can check",
			yaml: "- use: github\n  with: { token_env: LORE_GITHUB_TOKEN, repos: [acme/app], depth: 3 }\n",
			check: func(t *testing.T, values map[string]any) {
				if values["token_env"] != "LORE_GITHUB_TOKEN" || values["depth"] != 3 {
					t.Errorf("WithValues() = %v", values)
				}
				repos, ok := values["repos"].([]any)
				if !ok || len(repos) != 1 || repos[0] != "acme/app" {
					t.Errorf("with.repos = %v", values["repos"])
				}
			},
		},
		{
			name:    "a block that is not a mapping names the instance it belongs to",
			yaml:    "- id: jira-acme\n  use: jira\n  with: [PROJ, INFRA]\n",
			wantErr: `with: for instance "jira-acme" must be a mapping of configuration keys`,
		},
		{
			name:    "a scalar block is refused too",
			yaml:    "- use: notion\n  with: LORE_NOTION_TOKEN\n",
			wantErr: `with: for instance "notion" must be a mapping of configuration keys`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg, err := Decode(strings.NewReader("sources:\n" + indent(test.yaml)))
			if err != nil {
				t.Fatalf("Decode() error = %v", err)
			}

			values, err := cfg.Sources[0].WithValues()

			if test.wantErr != "" {
				if err == nil {
					t.Fatalf("WithValues() = %v, want error containing %q", values, test.wantErr)
				}
				if !internalerror.IsBadRequest(err) {
					t.Fatalf("WithValues() error kind = %s, want bad request", internalerror.KindOf(err))
				}
				if !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("WithValues() error = %q, want it to contain %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("WithValues() error = %v, want success", err)
			}
			test.check(t, values)
		})
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
			name: "a path that cannot be reached is rejected as unreadable, not as missing",
			path: func(t *testing.T) string {
				if os.Geteuid() == 0 {
					t.Skip("root searches a directory whose permissions forbid it")
				}
				parent := t.TempDir()
				clone := filepath.Join(parent, "myproject")
				if err := os.Mkdir(clone, 0o750); err != nil {
					t.Fatalf("seed clone: %v", err)
				}
				if err := os.Chmod(parent, 0o000); err != nil {
					t.Fatalf("seal parent: %v", err)
				}
				t.Cleanup(func() { _ = os.Chmod(parent, 0o750) })
				return clone
			},
			wantErr: "cannot be read",
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

			cfg, err := Load(writeConfig(t, "workspace: anchored\nembedder: { provider: openai }\nrepos:\n  - path: "+path+"\n"))

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

	cfg, err := Load(writeConfig(t,
		"workspace: tilde\nembedder: { provider: openai }\nrepos:\n  - path: ~/dev/myproject\n"))
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

func idents(instances []Instance) []string {
	got := make([]string, 0, len(instances))
	for i := range instances {
		got = append(got, instances[i].Ident())
	}
	return got
}

func equal(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// indent nests a sequence fixture under a top-level key.
func indent(body string) string {
	var out strings.Builder
	for _, line := range strings.Split(strings.TrimSuffix(body, "\n"), "\n") {
		out.WriteString("  " + line + "\n")
	}
	return out.String()
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
