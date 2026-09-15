package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/setthasit/Lore/internal/errors/internalerror"
	"github.com/setthasit/Lore/internal/fsx"
	"github.com/setthasit/Lore/sdk"
)

const (
	DefaultEventWindow       = Duration(30 * 24 * time.Hour)
	DefaultWalkDepth         = 3
	DefaultTopK              = 12
	DefaultSchedulerInterval = Duration(30 * time.Minute)
	DefaultRepoPlugin        = "git"
)

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

type PluginDecl struct {
	Name   string `yaml:"name"`
	From   string `yaml:"from"` // "github.com/jdoe/lore-linear@v0.3.1"
	PubKey string `yaml:"pubkey"`
}

type Instance struct {
	ID   string     `yaml:"id"`
	Use  string     `yaml:"use"`
	With *yaml.Node `yaml:"with"`
}

// KnownFields(true) rejects a plugin's own `with:` keys as unknown fields of yaml.Node.
func (i *Instance) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: an instance must be a mapping that names a plugin with a use key", node.Line)
	}

	for pair := 0; pair+1 < len(node.Content); pair += 2 {
		key, value := node.Content[pair], node.Content[pair+1]
		if declaredEarlier(node.Content[:pair], key.Value) {
			return fmt.Errorf("line %d: field %s is declared more than once in an instance",
				key.Line, key.Value)
		}
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

func declaredEarlier(content []*yaml.Node, name string) bool {
	for prior := 0; prior < len(content); prior += 2 {
		if content[prior].Value == name {
			return true
		}
	}
	return false
}

func (i Instance) Ident() string {
	if i.ID != "" {
		return i.ID
	}
	return i.Use
}

func (c *Config) InstancesUsing(plugin string) []string {
	used := instancesUsing(nil, "sources", c.Sources, plugin)
	used = instancesUsing(used, "providers", c.Providers, plugin)
	for _, repo := range c.Repos {
		if repo.Use == plugin {
			used = append(used, "repos["+repo.Path+"]")
		}
	}
	return used
}

func instancesUsing(used []string, section string, instances []Instance, plugin string) []string {
	for _, instance := range instances {
		if instance.Use == plugin {
			used = append(used, section+"["+instance.Ident()+"]")
		}
	}
	return used
}

// An absent or empty `with:` block decodes to a nil map, not an error.
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

type RoleBinding struct {
	Provider string `yaml:"provider"` // a providers[] id, or a provider plugin used with its defaults
	Model    string `yaml:"model"`

	// Vector width for models that do not imply one; zero leaves it to the driver.
	Dimensions int `yaml:"dimensions"`
}

type RepoDecl struct {
	Path   string `yaml:"path"`
	Use    string `yaml:"use"`
	Remote string `yaml:"remote"` // "github:acme/myproject", named after the forge, not the instance
}

type Query struct {
	EventWindow Duration `yaml:"event_window"` // ± window for event resolution
	WalkDepth   int      `yaml:"walk_depth"`
	TopK        int      `yaml:"top_k"`
}

type Scheduler struct {
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

// Duration accepts the whole-day "30d" that time.ParseDuration rejects.
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

func Decode(r io.Reader) (*Config, error) {
	cfg, err := decode(r)
	if err != nil {
		return nil, internalerror.NewBadRequestError("invalid configuration", err)
	}
	return cfg, nil
}

func decode(r io.Reader) (*Config, error) {
	decoder := yaml.NewDecoder(r)
	decoder.KnownFields(true)

	var cfg Config
	if err := decoder.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return &cfg, nil
}

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

func ReadFile(path string) (text string, cfg *Config, err error) {
	content, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil, refuseMissing(path, err)
		}
		return "", nil, internalerror.NewInternalError("cannot read "+path, err)
	}

	parsed, err := Decode(bytes.NewReader(content))
	if err != nil {
		return "", nil, internalerror.NewBadRequestError("cannot parse "+path, err)
	}
	return string(content), parsed, nil
}

func refuseMissing(path string, cause error) error {
	return internalerror.NewNotFoundError("no configuration at "+path+
		" — run `lore init` to create one", cause)
}

type Splice struct {
	From string
	To   string
}

func WriteFile(path string, splice Splice, refusal string) error {
	if _, err := Decode(strings.NewReader(splice.To)); err != nil {
		return internalerror.NewInternalError(refusal, err)
	}
	mode, err := ensureUnchanged(path, splice.From)
	if err != nil {
		return err
	}
	if err := fsx.WriteAtomic(path, []byte(splice.To), mode); err != nil {
		return internalerror.NewInternalError("cannot write "+path, err)
	}
	return nil
}

func ensureUnchanged(path, read string) (fs.FileMode, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return 0, refuseUnreadable(path, err)
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil {
		return 0, refuseUnreadable(path, err)
	}
	if !info.Mode().IsRegular() {
		return 0, refuseNotRegular(path)
	}

	oneByteBeyondRead := int64(len(read)) + 1
	current, err := io.ReadAll(io.LimitReader(file, oneByteBeyondRead))
	if err != nil {
		return 0, refuseUnreadable(path, err)
	}
	if string(current) != read {
		return 0, refuseChanged(path)
	}
	return info.Mode().Perm(), nil
}

func refuseChanged(path string) error {
	return internalerror.NewPreconditionError(path+" changed since it was read"+
		" — re-run the command; the newer file is unchanged", nil)
}

func refuseNotRegular(path string) error {
	return internalerror.NewPreconditionError(path+" is not a regular file"+
		" — re-run the command; nothing was written", nil)
}

func refuseUnreadable(path string, cause error) error {
	if errors.Is(cause, fs.ErrNotExist) {
		return refuseMissing(path, cause)
	}
	return internalerror.NewPreconditionError("cannot re-read the configuration to confirm it is unchanged: "+
		internalerror.MessageOf(cause)+" — re-run the command; nothing was written", cause)
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

	indexPath, err := ExpandHome("index_path", c.IndexPath)
	if err != nil {
		return err
	}
	c.IndexPath = indexPath

	for i := range c.Repos {
		path, err := ExpandHome("repos path", c.Repos[i].Path)
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

func ExpandHome(field, path string) (string, error) {
	if !startsAtHome(path) {
		return path, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", internalerror.NewBadRequestError(field+" "+path+" starts with ~, but this user has no home directory; declare an absolute path", err)
	}
	return filepath.Join(home, strings.TrimPrefix(path, "~")), nil
}

func startsAtHome(path string) bool {
	return path == "~" || (len(path) > 1 && path[0] == '~' && os.IsPathSeparator(path[1]))
}
