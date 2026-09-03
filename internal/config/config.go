package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/setthasit/Lore/internal/errors/internalerror"
)

// Defaults applied at load when the corresponding key is absent.
const (
	DefaultEventWindow       = Duration(30 * 24 * time.Hour)
	DefaultWalkDepth         = 3
	DefaultTopK              = 12
	DefaultSchedulerInterval = Duration(30 * time.Minute)
)

// Config is a parsed lore.yaml workspace configuration.
type Config struct {
	Workspace string    `yaml:"workspace"`
	IndexPath string    `yaml:"index_path"`
	Sources   Sources   `yaml:"sources"`
	Repos     []Repo    `yaml:"repos"`
	Query     Query     `yaml:"query"`
	Embedder  Embedder  `yaml:"embedder"`
	LLM       *LLM      `yaml:"llm"`
	Scheduler Scheduler `yaml:"scheduler"`
	Server    Server    `yaml:"server"`
}

// Sources declares what to ingest. Every source is optional; an absent source
// is never synced and never required.
type Sources struct {
	GitHub *GitHubSource `yaml:"github"`
	GitLab *GitLabSource `yaml:"gitlab"`
	Notion *NotionSource `yaml:"notion"`
	Jira   *JiraSource   `yaml:"jira"`
}

// GitHubSource ingests commits, PRs, reviews and issues for Repos. Independent
// of local clones: no repository on disk is needed.
type GitHubSource struct {
	TokenEnv string   `yaml:"token_env"`
	Repos    []string `yaml:"repos"` // "acme/myproject"
}

// GitLabSource ingests commits, merge requests, discussion threads, issues and
// notes for Projects. BaseURL is optional: absent means gitlab.com, and a
// self-managed instance names its own root.
type GitLabSource struct {
	BaseURL  string   `yaml:"base_url"`
	TokenEnv string   `yaml:"token_env"`
	Projects []string `yaml:"projects"` // "acme/myproject", "acme/platform/myproject"
}

type NotionSource struct {
	TokenEnv  string   `yaml:"token_env"`
	RootPages []string `yaml:"root_pages"`
}

type JiraSource struct {
	BaseURL  string   `yaml:"base_url"`
	EmailEnv string   `yaml:"email_env"`
	TokenEnv string   `yaml:"token_env"`
	Projects []string `yaml:"projects"`
}

// Repo registers a local clone used for blame and log only. Zero repos is a
// valid ask-only workspace.
type Repo struct {
	Path   string `yaml:"path"`
	Remote string `yaml:"remote"` // "github:acme/myproject"; maps the clone onto a source repo
}

type Query struct {
	EventWindow Duration `yaml:"event_window"` // ± window for event resolution
	WalkDepth   int      `yaml:"walk_depth"`
	TopK        int      `yaml:"top_k"`
}

type Embedder struct {
	Provider string `yaml:"provider"` // openai | ollama
	Model    string `yaml:"model"`
	BaseURL  string `yaml:"base_url"` // default: the provider's own endpoint

	// Dimensions is the vector width. Required for ollama, whose models do not
	// imply one; `ollama show <model>` reports it.
	Dimensions int `yaml:"dimensions"`
}

// LLM configures synthesis for the CLI and gRPC surfaces. Optional: MCP never
// needs it.
type LLM struct {
	Provider  string `yaml:"provider"` // openai | anthropic | zai | ollama
	Model     string `yaml:"model"`
	APIKeyEnv string `yaml:"api_key_env"`
	BaseURL   string `yaml:"base_url"` // default: the provider's own endpoint
}

type Scheduler struct {
	// Zero is absent, not "never sync": Load fills it with DefaultSchedulerInterval.
	Interval Duration `yaml:"interval"`
}

type Server struct {
	HTTPAddr string `yaml:"http_addr"`
	GRPCAddr string `yaml:"grpc_addr"`
	MTLS     *MTLS  `yaml:"mtls"`
}

type MTLS struct {
	Cert     string `yaml:"cert"`
	Key      string `yaml:"key"`
	ClientCA string `yaml:"client_ca"`
}

// Duration is a time.Duration that additionally accepts a whole-day "30d" form,
// which time.ParseDuration rejects.
type Duration time.Duration

func (d Duration) String() string {
	return time.Duration(d).String()
}

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var raw string
	if err := node.Decode(&raw); err != nil {
		return err
	}
	parsed, err := parseDuration(raw)
	if err != nil {
		return fmt.Errorf("line %d: invalid duration %q: %w", node.Line, raw, err)
	}
	*d = Duration(parsed)
	return nil
}

func parseDuration(raw string) (time.Duration, error) {
	if days, ok := strings.CutSuffix(raw, "d"); ok {
		if n, err := strconv.Atoi(days); err == nil {
			return time.Duration(n) * 24 * time.Hour, nil
		}
	}
	return time.ParseDuration(raw)
}

// Load reads lore.yaml from path, rejects unknown keys, applies defaults and
// validates the result.
func Load(path string) (*Config, error) {
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, internalerror.NewNotFoundError("no configuration at "+path+" — run `lore init` to create one", err)
		}
		return nil, internalerror.NewInternalError("cannot read configuration at "+path, err)
	}
	defer func() { _ = file.Close() }()

	decoder := yaml.NewDecoder(file)
	decoder.KnownFields(true)

	var cfg Config
	if err := decoder.Decode(&cfg); err != nil {
		return nil, internalerror.NewBadRequestError("invalid configuration at "+path+": "+err.Error(), err)
	}

	if err := cfg.applyDefaults(); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() error {
	if c.IndexPath == "" && c.Workspace != "" {
		c.IndexPath = "~/.lore/" + c.Workspace + ".db"
	}
	if c.Query.EventWindow == 0 {
		c.Query.EventWindow = DefaultEventWindow
	}
	if c.Query.WalkDepth == 0 {
		c.Query.WalkDepth = DefaultWalkDepth
	}
	if c.Query.TopK == 0 {
		c.Query.TopK = DefaultTopK
	}
	if c.Scheduler.Interval == 0 {
		c.Scheduler.Interval = DefaultSchedulerInterval
	}

	indexPath, err := expandHome("index_path", c.IndexPath)
	if err != nil {
		return err
	}
	c.IndexPath = indexPath

	for i := range c.Repos {
		path, err := expandHome("repos path", c.Repos[i].Path)
		if err != nil {
			return err
		}
		c.Repos[i].Path = path
	}
	return nil
}

// Only a leading "~" is expanded.
func expandHome(field, path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~"+string(filepath.Separator)) {
		return path, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", internalerror.NewBadRequestError(field+" "+path+" starts with ~, but this user has no home directory; set an absolute "+field, err)
	}
	return filepath.Join(home, strings.TrimPrefix(path, "~")), nil
}
