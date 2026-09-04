package config

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/sdk"
)

// Defaults applied at load when the corresponding key is absent.
const (
	DefaultEventWindow       = Duration(30 * 24 * time.Hour)
	DefaultWalkDepth         = 3
	DefaultTopK              = 12
	DefaultSchedulerInterval = Duration(30 * time.Minute)

	// DefaultRepoPlugin reads a registered clone. Naming a clone without naming
	// a code plugin means the only one a clone can have.
	DefaultRepoPlugin = "git"
)

// Config is a parsed lore.yaml workspace configuration. It names no source and
// no provider: `use:` selects a plugin from the registry and `with:` is that
// plugin's own business, so reaching a new system is a configuration change
// this type never has to learn about.
type Config struct {
	Workspace string       `yaml:"workspace"`
	IndexPath string       `yaml:"index_path"`
	Plugins   []PluginDecl `yaml:"plugins"`
	Sources   []Instance   `yaml:"sources"`
	Providers []Instance   `yaml:"providers"`
	Embedder  RoleBinding  `yaml:"embedder"`
	LLM       *RoleBinding `yaml:"llm"`
	Repos     []RepoDecl   `yaml:"repos"`
	Query     Query        `yaml:"query"`
	Scheduler Scheduler    `yaml:"scheduler"`
	Server    Server       `yaml:"server"`
}

// PluginDecl declares an external plugin this workspace needs. Name is what
// `use:` refers to, From is the module its binary is resolved from, and PubKey,
// when set, is the key that binary's signature must verify against.
type PluginDecl struct {
	Name   string `yaml:"name"`
	From   string `yaml:"from"` // "github.com/jdoe/lore-linear@v0.3.1"
	PubKey string `yaml:"pubkey"`
}

// Instance is one configured use of a plugin: two Jira sites are two instances
// of one plugin, not two plugins. With is captured as a node instead of being
// decoded, because only the plugin the instance names knows which keys exist.
type Instance struct {
	ID   string     `yaml:"id"`
	Use  string     `yaml:"use"`
	With *yaml.Node `yaml:"with"`
}

// UnmarshalYAML reads an instance a key at a time. It exists because a
// yaml.Node field cannot be filled by a decoder in KnownFields mode: the
// decoder walks the node's own struct fields and rejects the plugin's keys as
// unknown. Keys are matched here instead, so an instance still refuses a
// misspelling rather than ignoring it.
func (i *Instance) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: an instance must be a mapping that names a plugin with a use key", node.Line)
	}

	for pair := 0; pair+1 < len(node.Content); pair += 2 {
		key, value := node.Content[pair], node.Content[pair+1]
		switch key.Value {
		case "id":
			if err := value.Decode(&i.ID); err != nil {
				return err
			}
		case "use":
			if err := value.Decode(&i.Use); err != nil {
				return err
			}
		case "with":
			i.With = value
		default:
			return fmt.Errorf("line %d: field %s not found in an instance, which has id, use and with",
				key.Line, key.Value)
		}
	}
	return nil
}

// Ident is the identity the engine keys on: the sync cursor key, the value of
// every document's source, and the document id prefix. It defaults to the
// plugin name, so a workspace with one instance of a plugin never spells an id.
func (i Instance) Ident() string {
	if i.ID != "" {
		return i.ID
	}
	return i.Use
}

// WithValues decodes the captured `with:` block into generic values, which is
// what the registry checks against the plugin's manifest before the plugin
// decodes the same block strictly itself. An absent or empty block is a nil
// map: a plugin whose every field is optional is configured by naming it alone.
func (i Instance) WithValues() (map[string]any, error) {
	if i.With == nil || i.With.Tag == "!!null" {
		return nil, nil
	}

	var values map[string]any
	if err := i.With.Decode(&values); err != nil {
		return nil, internalerror.NewBadRequestError("with: for instance "+strconv.Quote(i.Ident())+
			" must be a mapping of configuration keys", err)
	}
	return values, nil
}

// RoleBinding binds a role — embedding, synthesis — to a provider instance and
// one of its models. Adding a role later adds a key, not plumbing.
type RoleBinding struct {
	Provider string `yaml:"provider"` // a providers[] id, or a provider plugin used with its defaults
	Model    string `yaml:"model"`

	// Dimensions is the vector width, for drivers whose models do not imply one;
	// `ollama show <model>` reports it. Zero leaves the width to the driver.
	Dimensions int `yaml:"dimensions"`
}

// RepoDecl registers a local clone, read for blame and log only. Remote maps
// the clone onto the forge a source ingests, which is what lets a chain reach
// from a line of code to the discussion around it. Zero repos is a valid
// ask-only workspace.
type RepoDecl struct {
	Path   string `yaml:"path"`
	Use    string `yaml:"use"`    // code plugin; DefaultRepoPlugin when absent
	Remote string `yaml:"remote"` // "github:acme/myproject", named after the forge, not the instance
}

type Query struct {
	EventWindow Duration `yaml:"event_window"` // ± window for event resolution
	WalkDepth   int      `yaml:"walk_depth"`
	TopK        int      `yaml:"top_k"`
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

// Duration is a time.Duration in the form the whole system speaks, including
// the whole-day "30d" that time.ParseDuration rejects. The spelling is the
// contract's, not this package's, so a plugin declaring a duration field and the
// workspace configuration cannot drift apart.
type Duration time.Duration

func (d Duration) String() string {
	return time.Duration(d).String()
}

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var raw string
	if err := node.Decode(&raw); err != nil {
		return err
	}
	parsed, err := lore.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("line %d: invalid duration %q: %w", node.Line, raw, err)
	}
	*d = Duration(parsed)
	return nil
}

// Decode reads one configuration document, rejecting a key no field claims. It
// applies no defaults and validates nothing, which is what makes it usable by
// the commands that read a configuration in order to rewrite it.
//
// It is exported because it is the only strict decoder in the repository: a
// second one drifts from this one silently, and a key that Load rejects but an
// editing command accepts writes a file the next Load refuses.
func Decode(r io.Reader) (*Config, error) {
	cfg, err := decode(r)
	if err != nil {
		return nil, internalerror.NewBadRequestError("invalid configuration", err)
	}
	return cfg, nil
}

// decode is the repository's only strict decoder. Load and Decode differ in
// nothing but the error they report, so they must not differ in what they
// accept either.
func decode(r io.Reader) (*Config, error) {
	decoder := yaml.NewDecoder(r)
	decoder.KnownFields(true)

	var cfg Config
	// An empty document decodes to a configuration of nothing rather than
	// failing, because a file about to be filled in is not a syntax error.
	if err := decoder.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return &cfg, nil
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

	cfg, err := decode(file)
	if err != nil {
		return nil, internalerror.NewBadRequestError("invalid configuration at "+path, err)
	}

	if err := cfg.applyDefaults(); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
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

		if c.Repos[i].Use == "" {
			c.Repos[i].Use = DefaultRepoPlugin
		}
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
